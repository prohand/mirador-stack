// SPDX-License-Identifier: GPL-3.0-or-later

package webpush

import (
	"context"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"
)

func decoder(t *testing.T, s string) []byte {
	t.Helper()
	b, err := b64.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Exemple de la RFC 8291 (§ 5 et annexe A) : avec les mêmes clés et le même
// sel, le corps chiffré doit être celui de la RFC, à l'octet près.
func TestVecteurRFC8291(t *testing.T) {
	appareil, err := ecdh.P256().NewPublicKey(decoder(t, "BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4"))
	if err != nil {
		t.Fatal(err)
	}
	ephemere, err := ecdh.P256().NewPrivateKey(decoder(t, "yfWPiYE-n46HLnH0KqZOF1fJJU3MYrct3AELtAQ-oRw"))
	if err != nil {
		t.Fatal(err)
	}
	corps, err := chiffrerAvec([]byte("When I grow up, I want to be a watermelon"), appareil, decoder(t, "BTBZMqHH6r4Tts7J_aSIgg"),
		ephemere, decoder(t, "DGv6ra1nlYgDCS1FRnbzlw"))
	if err != nil {
		t.Fatal(err)
	}
	attendu := "DGv6ra1nlYgDCS1FRnbzlwAAEABBBP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A_" +
		"yl95bQpu6cVPTpK4Mqgkf1CXztLVBSt2Ks3oZwbuwXPXLWyouBWLVWGNWQexSgSxsj_Qulcy4a-fN"
	if g := b64.EncodeToString(corps); g != attendu {
		t.Fatalf("corps chiffré différent de la RFC :\n obtenu  %s\n attendu %s", g, attendu)
	}
}

// dechiffrer : le texte clair vu par l'appareil.
func dechiffrer(t *testing.T, corps []byte, appareil *ecdh.PrivateKey, auth []byte) []byte {
	t.Helper()
	clair, err := Dechiffrer(corps, appareil, auth)
	if err != nil {
		t.Fatalf("déchiffrement : %v", err)
	}
	return clair
}

func nouvelAppareil(t *testing.T, adresse string) (*Abonnement, *ecdh.PrivateKey) {
	t.Helper()
	cle, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth := make([]byte, 16)
	_, _ = rand.Read(auth)
	return &Abonnement{Adresse: adresse, P256dh: cle.PublicKey().Bytes(), Auth: auth}, cle
}

func TestChiffrerPuisDechiffrer(t *testing.T) {
	a, cle := nouvelAppareil(t, "https://fcm.googleapis.com/fcm/send/abc")
	message := []byte(`{"titre":"AgentMuet · pve1","corps":"pve1 ne remonte plus de métriques","pastille":2}`)
	c1, err := Chiffrer(message, a)
	if err != nil {
		t.Fatal(err)
	}
	if string(dechiffrer(t, c1, cle, a.Auth)) != string(message) {
		t.Fatal("message déchiffré différent")
	}
	// Chaque envoi a sa clé éphémère et son sel : deux corps différents.
	c2, _ := Chiffrer(message, a)
	if string(c1) == string(c2) {
		t.Error("deux envois du même message donnent le même corps")
	}
	if strings.Contains(string(c1), "AgentMuet") {
		t.Error("le texte clair apparaît dans le corps chiffré")
	}
	if _, err := Chiffrer(make([]byte, TailleMaxMessage+1), a); err == nil {
		t.Error("message trop long accepté")
	}
}

func TestValiderAbonnement(t *testing.T) {
	bon, _ := nouvelAppareil(t, "https://web.push.apple.com/QOabc")
	if err := bon.Valider(); err != nil {
		t.Fatalf("abonnement Apple refusé : %v", err)
	}
	for _, adresse := range []string{
		"https://fcm.googleapis.com/fcm/send/x", "https://updates.push.services.mozilla.com/wpush/v2/x",
		"https://wns2-par02p.notify.windows.com/w/?token=x", "https://FCM.googleapis.com./wp/x",
	} {
		a := *bon
		a.Adresse = adresse
		if err := a.Valider(); err != nil {
			t.Errorf("%s refusé : %v", adresse, err)
		}
	}
	// Adresses refusées : le serveur n'appelle que les services de push connus.
	for _, adresse := range []string{
		"http://fcm.googleapis.com/x", "https://127.0.0.1/x", "https://mirador-olvid:50051/x", "https://exemple.fr/push",
		"https://fcm.googleapis.com.attaquant.example/x", "https://attaquantpush.apple.com/x", "https://u:p@fcm.googleapis.com/x",
		"https://fcm.googleapis.com:8443/x", "", "https://" + strings.Repeat("a", 1100) + ".push.apple.com/",
	} {
		a := *bon
		a.Adresse = adresse
		if err := a.Valider(); !errors.Is(err, ErrAbonnement) {
			t.Errorf("%q accepté (%v)", adresse, err)
		}
	}
	// Clés mal formées.
	a := *bon
	a.Auth = a.Auth[:15]
	if a.Valider() == nil {
		t.Error("secret de 15 octets accepté")
	}
	a = *bon
	a.P256dh = append([]byte{4}, make([]byte, 64)...) // pas un point de la courbe
	if a.Valider() == nil {
		t.Error("clé hors de la courbe acceptée")
	}
}

func TestJetonVAPID(t *testing.T) {
	cle, err := NouvelleCle()
	if err != nil {
		t.Fatal(err)
	}
	fin := time.Unix(1_800_000_000, 0)
	jeton, err := Jeton(cle, "https://fcm.googleapis.com", "https://mirador.exemple.fr", fin)
	if err != nil {
		t.Fatal(err)
	}
	p := strings.Split(jeton, ".")
	if len(p) != 3 {
		t.Fatalf("jeton en %d parties", len(p))
	}
	var entete, corps map[string]any
	_ = json.Unmarshal(decoder(t, p[0]), &entete)
	_ = json.Unmarshal(decoder(t, p[1]), &corps)
	if entete["alg"] != "ES256" || entete["typ"] != "JWT" || corps["aud"] != "https://fcm.googleapis.com" ||
		corps["sub"] != "https://mirador.exemple.fr" || corps["exp"] != float64(fin.Unix()) {
		t.Fatalf("contenu du jeton : %v %v", entete, corps)
	}
	sig := decoder(t, p[2])
	if len(sig) != 64 {
		t.Fatalf("signature de %d octets (64 attendus)", len(sig))
	}
	h := sha256.Sum256([]byte(p[0] + "." + p[1]))
	if !ecdsa.Verify(&cle.PublicKey, h[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		t.Fatal("signature du jeton fausse")
	}
	pub, _ := ClePublique(cle)
	if b := decoder(t, pub); len(b) != 65 || b[0] != 4 {
		t.Fatalf("clé publique : %d octets", len(b))
	}
}

// fauxService : service de push simulé (aucune requête ne sort).
type fauxService struct {
	code  int
	recue *http.Request
	corps []byte
}

func (f *fauxService) RoundTrip(r *http.Request) (*http.Response, error) {
	f.recue = r
	f.corps, _ = io.ReadAll(r.Body)
	return &http.Response{StatusCode: f.code, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}}, nil
}

func TestEnvoyer(t *testing.T) {
	a, cleAppareil := nouvelAppareil(t, "https://fcm.googleapis.com/fcm/send/appareil-secret")
	cle, _ := NouvelleCle()
	o := Options{Cle: cle, Sujet: "https://mirador.exemple.fr", Urgent: true, Groupe: "abc", Instant: time.Unix(1_800_000_000, 0)}
	f := &fauxService{code: http.StatusCreated}
	if err := Envoyer(context.Background(), &http.Client{Transport: f}, a, []byte("bonjour"), o); err != nil {
		t.Fatal(err)
	}
	h := f.recue.Header
	if f.recue.Method != http.MethodPost || h.Get("Content-Encoding") != "aes128gcm" || h.Get("TTL") != "43200" || h.Get("Urgency") != "high" ||
		h.Get("Topic") != "abc" || !strings.HasPrefix(h.Get("Authorization"), "vapid t=") || !strings.Contains(h.Get("Authorization"), ", k=") {
		t.Fatalf("en-têtes : %v", h)
	}
	if string(dechiffrer(t, f.corps, cleAppareil, a.Auth)) != "bonjour" {
		t.Fatal("corps reçu par le service illisible pour l'appareil")
	}
	// Abonnement disparu : à effacer.
	for _, code := range []int{http.StatusNotFound, http.StatusGone} {
		f.code = code
		if err := Envoyer(context.Background(), &http.Client{Transport: f}, a, []byte("x"), o); !errors.Is(err, ErrExpire) {
			t.Errorf("HTTP %d : %v", code, err)
		}
	}
	// Autre erreur : à réessayer ; l'adresse de l'abonnement n'est jamais dans le message.
	f.code = http.StatusTooManyRequests
	err := Envoyer(context.Background(), &http.Client{Transport: f}, a, []byte("x"), o)
	if err == nil || errors.Is(err, ErrExpire) || strings.Contains(err.Error(), "appareil-secret") {
		t.Errorf("HTTP 429 : %v", err)
	}
	// Service inconnu : rien n'est envoyé.
	f.recue = nil
	mauvais := *a
	mauvais.Adresse = "https://127.0.0.1/x"
	if err := Envoyer(context.Background(), &http.Client{Transport: f}, &mauvais, []byte("x"), o); !errors.Is(err, ErrAbonnement) || f.recue != nil {
		t.Errorf("adresse interne : %v (requête partie : %v)", err, f.recue != nil)
	}
}
