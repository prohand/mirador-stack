// SPDX-License-Identifier: GPL-3.0-or-later

// Package webpush envoie une notification à un navigateur ou à l'application
// installée sur un téléphone (« Web Push », SPEC § 35), sans dépendance :
//
//   - RFC 8291 : le message est chiffré pour l'appareil (ECDH P-256, HKDF,
//     AES-128-GCM). Le service de push de Google, Apple ou Mozilla, par
//     lequel il passe, ne peut pas le lire ;
//   - RFC 8188 : forme du corps chiffré (« aes128gcm ») ;
//   - RFC 8292 : « VAPID », un jeton signé (ES256) qui dit au service de
//     push quel serveur envoie.
//
// Écrit ici (~200 lignes, bibliothèque standard seule) plutôt qu'une
// dépendance ; vérifié contre une autre implémentation (SPEC § 35.5).
package webpush

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Abonnement : ce que le navigateur donne quand l'utilisateur accepte les
// notifications sur un appareil.
type Abonnement struct {
	Adresse string // adresse du service de push pour cet appareil (« endpoint »)
	P256dh  []byte // clé publique de l'appareil (P-256, 65 octets)
	Auth    []byte // secret partagé (16 octets)
}

// Bornes.
const (
	// TailleMaxMessage : texte clair le plus long. Les services de push
	// refusent un corps de plus de 4096 octets ; on reste bien en dessous.
	TailleMaxMessage  = 3000
	tailleBloc        = 4096 // « record size » annoncé (un seul bloc)
	tailleCleAppareil = 65
	tailleAuth        = 16
)

// hotesPush : services de push connus. L'adresse d'un abonnement vient du
// navigateur, donc de l'utilisateur : le serveur n'appelle que ces hôtes
// (protection SSRF, en plus du contrôle de l'adresse réellement jointe).
// Un point devant = tous les sous-domaines.
var hotesPush = []string{
	"fcm.googleapis.com",         // Chrome, Edge, Brave, Opera, Samsung (Android et ordinateur)
	"android.googleapis.com",     // ancienne adresse de Google
	"jmt17.google.com",           // adresse de Google dans certains pays
	".push.apple.com",            // Safari, iPhone et iPad (web.push.apple.com)
	".push.services.mozilla.com", // Firefox
	".notify.windows.com",        // Edge sous Windows
}

// ErrAbonnement : abonnement refusé (forme ou service de push inconnu).
var ErrAbonnement = errors.New("abonnement refusé")

// ErrExpire : le service de push dit que l'abonnement n'existe plus
// (application désinstallée, notifications coupées) : à effacer.
var ErrExpire = errors.New("abonnement expiré")

// Valider vérifie un abonnement venu du navigateur : adresse https d'un
// service de push connu, clé publique sur la courbe, secret de 16 octets.
func (a *Abonnement) Valider() error {
	if len(a.Adresse) > 1024 {
		return fmt.Errorf("%w : adresse trop longue", ErrAbonnement)
	}
	u, err := url.Parse(a.Adresse)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" && u.Port() != "443" {
		return fmt.Errorf("%w : adresse https attendue", ErrAbonnement)
	}
	if !HoteConnu(u.Hostname()) {
		return fmt.Errorf("%w : service de push « %s » inconnu de mirador", ErrAbonnement, u.Hostname())
	}
	if len(a.P256dh) != tailleCleAppareil || len(a.Auth) != tailleAuth {
		return fmt.Errorf("%w : clés de l'appareil mal formées", ErrAbonnement)
	}
	if _, err := ecdh.P256().NewPublicKey(a.P256dh); err != nil {
		return fmt.Errorf("%w : clé de l'appareil hors de la courbe", ErrAbonnement)
	}
	return nil
}

// HoteConnu : l'hôte est-il un service de push de la liste ?
func HoteConnu(hote string) bool {
	hote = strings.ToLower(strings.TrimSuffix(hote, "."))
	for _, h := range hotesPush {
		if hote == h || (strings.HasPrefix(h, ".") && strings.HasSuffix(hote, h)) {
			return true
		}
	}
	return false
}

// Chiffrer rend le corps de la requête (« aes128gcm », un seul bloc) pour
// cet appareil. Chaque message a sa propre clé éphémère et son propre sel.
func Chiffrer(clair []byte, a *Abonnement) ([]byte, error) {
	if len(clair) > TailleMaxMessage {
		return nil, errors.New("message trop long")
	}
	appareil, err := ecdh.P256().NewPublicKey(a.P256dh)
	if err != nil || len(a.Auth) != tailleAuth {
		return nil, fmt.Errorf("%w : clés de l'appareil mal formées", ErrAbonnement)
	}
	ephemere, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	sel := make([]byte, 16)
	if _, err := rand.Read(sel); err != nil {
		return nil, err
	}
	return chiffrerAvec(clair, appareil, a.Auth, ephemere, sel)
}

// chiffrerAvec : le calcul, avec la clé éphémère et le sel donnés (tests).
func chiffrerAvec(clair []byte, appareil *ecdh.PublicKey, auth []byte, ephemere *ecdh.PrivateKey, sel []byte) ([]byte, error) {
	partage, err := ephemere.ECDH(appareil)
	if err != nil {
		return nil, err
	}
	pubEphemere := ephemere.PublicKey().Bytes()
	// RFC 8291 § 3.4 : clé de départ, liée au secret de l'appareil et aux
	// deux clés publiques.
	prk, err := hkdf.Extract(sha256.New, partage, auth)
	if err != nil {
		return nil, err
	}
	info := "WebPush: info\x00" + string(appareil.Bytes()) + string(pubEphemere)
	ikm, err := hkdf.Expand(sha256.New, prk, info, 32)
	if err != nil {
		return nil, err
	}
	// RFC 8188 § 2.2 et 2.3 : clé du message et nonce.
	prk2, err := hkdf.Extract(sha256.New, ikm, sel)
	if err != nil {
		return nil, err
	}
	cle, err := hkdf.Expand(sha256.New, prk2, "Content-Encoding: aes128gcm\x00", 16)
	if err != nil {
		return nil, err
	}
	nonce, err := hkdf.Expand(sha256.New, prk2, "Content-Encoding: nonce\x00", 12)
	if err != nil {
		return nil, err
	}
	bloc, err := aes.NewCipher(cle)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(bloc)
	if err != nil {
		return nil, err
	}
	// Dernier (et seul) bloc : le texte suivi de l'octet 0x02.
	chiffre := gcm.Seal(nil, nonce, append(bytes.Clone(clair), 0x02), nil)
	// En-tête : sel (16) | taille de bloc (4) | longueur de la clé (1) | clé éphémère (65).
	corps := make([]byte, 0, 16+4+1+len(pubEphemere)+len(chiffre))
	corps = append(corps, sel...)
	corps = binary.BigEndian.AppendUint32(corps, tailleBloc)
	if len(pubEphemere) != tailleCleAppareil {
		return nil, errors.New("clé éphémère de taille inattendue")
	}
	corps = append(corps, tailleCleAppareil)
	corps = append(corps, pubEphemere...)
	return append(corps, chiffre...), nil
}

// Dechiffrer fait ce que fait l'appareil à la réception (RFC 8291) : il
// retrouve le texte clair avec la clé privée de l'appareil. Le serveur n'en
// a jamais besoin (il n'a pas ces clés privées) : sert aux tests, ici et
// dans les paquets qui envoient.
func Dechiffrer(corps []byte, appareil *ecdh.PrivateKey, auth []byte) ([]byte, error) {
	illisible := errors.New("corps chiffré illisible")
	const entete = 16 + 4 + 1 + tailleCleAppareil
	if len(corps) < entete+17 || binary.BigEndian.Uint32(corps[16:20]) != tailleBloc || corps[20] != tailleCleAppareil {
		return nil, illisible
	}
	sel, pubEphemere := corps[:16], corps[21:entete]
	ephemere, err := ecdh.P256().NewPublicKey(pubEphemere)
	if err != nil {
		return nil, illisible
	}
	partage, err := appareil.ECDH(ephemere)
	if err != nil {
		return nil, illisible
	}
	prk, _ := hkdf.Extract(sha256.New, partage, auth)
	ikm, _ := hkdf.Expand(sha256.New, prk, "WebPush: info\x00"+string(appareil.PublicKey().Bytes())+string(pubEphemere), 32)
	prk2, _ := hkdf.Extract(sha256.New, ikm, sel)
	cle, _ := hkdf.Expand(sha256.New, prk2, "Content-Encoding: aes128gcm\x00", 16)
	nonce, _ := hkdf.Expand(sha256.New, prk2, "Content-Encoding: nonce\x00", 12)
	bloc, err := aes.NewCipher(cle)
	if err != nil {
		return nil, illisible
	}
	gcm, err := cipher.NewGCM(bloc)
	if err != nil {
		return nil, illisible
	}
	clair, err := gcm.Open(nil, nonce, corps[entete:], nil)
	if err != nil || len(clair) == 0 || clair[len(clair)-1] != 0x02 {
		return nil, illisible
	}
	return clair[:len(clair)-1], nil
}

var b64 = base64.RawURLEncoding

// ClePublique : clé publique VAPID telle que l'attend le navigateur
// (point non compressé, 65 octets, en base64 « URL »).
func ClePublique(cle *ecdsa.PrivateKey) (string, error) {
	p, err := cle.PublicKey.ECDH()
	if err != nil {
		return "", err
	}
	return b64.EncodeToString(p.Bytes()), nil
}

// NouvelleCle : paire de clés VAPID du serveur (P-256).
func NouvelleCle() (*ecdsa.PrivateKey, error) {
	return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
}

// Jeton fabrique le jeton VAPID (JWT signé ES256, RFC 8292 § 2) :
// audience = service de push (schéma et hôte de l'adresse de l'abonnement),
// sujet = adresse de contact du serveur (mailto: ou https:), fin = au plus
// 24 h plus tard.
func Jeton(cle *ecdsa.PrivateKey, audience, sujet string, fin time.Time) (string, error) {
	entete := b64.EncodeToString([]byte(`{"typ":"JWT","alg":"ES256"}`))
	brut, err := json.Marshal(map[string]any{"aud": audience, "exp": fin.Unix(), "sub": sujet})
	if err != nil {
		return "", err
	}
	aSigner := entete + "." + b64.EncodeToString(brut)
	h := sha256.Sum256([]byte(aSigner))
	r, s, err := ecdsa.Sign(rand.Reader, cle, h[:])
	if err != nil {
		return "", err
	}
	// Signature « brute » : r puis s, 32 octets chacun.
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return aSigner + "." + b64.EncodeToString(sig), nil
}

// Options d'un envoi.
type Options struct {
	Cle     *ecdsa.PrivateKey // clé VAPID du serveur
	Sujet   string            // contact du serveur (https://… de l'interface)
	Urgent  bool              // à livrer tout de suite, même téléphone en veille
	Duree   time.Duration     // gardé par le service si l'appareil est éteint (0 = 12 h)
	Groupe  string            // « Topic » (32 signes base64 au plus) : un message plus récent du même groupe remplace celui pas encore livré
	Instant time.Time         // heure de l'envoi (fin du jeton = + 12 h)
}

// Envoyer chiffre le message et le remet au service de push. ErrExpire :
// l'abonnement n'existe plus. Toute autre erreur : à réessayer plus tard.
// Le client doit refuser les redirections et les adresses internes
// (notif.ClientPush : contrôle à la connexion avec la liste commune
// adresses.InterditeServeur).
func Envoyer(ctx context.Context, client *http.Client, a *Abonnement, clair []byte, o Options) error {
	if err := a.Valider(); err != nil {
		return err
	}
	u, _ := url.Parse(a.Adresse)
	corps, err := Chiffrer(clair, a)
	if err != nil {
		return err
	}
	if o.Instant.IsZero() {
		o.Instant = time.Now()
	}
	jeton, err := Jeton(o.Cle, u.Scheme+"://"+u.Host, o.Sujet, o.Instant.Add(12*time.Hour))
	if err != nil {
		return err
	}
	pub, err := ClePublique(o.Cle)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.Adresse, bytes.NewReader(corps))
	if err != nil {
		return errors.New("push : requête impossible")
	}
	if o.Duree <= 0 {
		o.Duree = 12 * time.Hour
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("TTL", strconv.Itoa(int(o.Duree.Seconds())))
	req.Header.Set("Urgency", map[bool]string{true: "high", false: "normal"}[o.Urgent])
	if o.Groupe != "" {
		req.Header.Set("Topic", o.Groupe)
	}
	req.Header.Set("Authorization", "vapid t="+jeton+", k="+pub)
	rep, err := client.Do(req)
	if err != nil {
		// Jamais l'adresse de l'abonnement dans l'erreur (elle vaut un secret).
		return fmt.Errorf("service de push %s injoignable", u.Hostname())
	}
	defer rep.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(rep.Body, 16<<10))
	switch {
	case rep.StatusCode/100 == 2:
		return nil
	case rep.StatusCode == http.StatusNotFound || rep.StatusCode == http.StatusGone:
		return ErrExpire
	}
	return fmt.Errorf("service de push %s : HTTP %d", u.Hostname(), rep.StatusCode)
}
