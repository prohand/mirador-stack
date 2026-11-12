// SPDX-License-Identifier: GPL-3.0-or-later

package auth

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net"
	"strings"
	"time"
)

// Passkeys (WebAuthn niveau 2, SPEC.md § 20). Une passkey remplace le code
// 2FA à la connexion, après le mot de passe. Le serveur ne garde qu'une clé
// publique ; la clé privée reste dans l'appareil ou le gestionnaire de mots
// de passe.
//
// Ce qui est vérifié, à la création comme à la connexion :
//   - le défi (aléatoire, à usage unique, gardé dans la session) ;
//   - l'origine : la page qui a parlé à l'authentificateur est bien cette
//     interface (protection contre l'hameçonnage) ;
//   - le domaine (empreinte de rpId) et la présence de l'utilisateur ;
//   - à la connexion : la signature, avec la clé publique enregistrée.
//
// L'attestation du fabricant n'est pas demandée (« none ») : elle sert à
// imposer un modèle de clé, ce qui n'a pas d'intérêt ici.

// Algorithmes de signature acceptés (numéros COSE).
const (
	algES256 = -7   // ECDSA P-256 + SHA-256 (le plus courant)
	algEdDSA = -8   // Ed25519
	algRS256 = -257 // RSA PKCS#1 v1.5 + SHA-256 (Windows Hello)
)

// AlgorithmesPasskey : proposés au navigateur, par ordre de préférence.
var AlgorithmesPasskey = []int{algES256, algEdDSA, algRS256}

// PasskeysMax : passkeys par compte.
const PasskeysMax = 10

// DureeDefiPasskey : validité d'un défi.
const DureeDefiPasskey = 5 * time.Minute

// Passkey : une clé enregistrée.
type Passkey struct {
	ID            int64
	UtilisateurID int64
	Identifiant   []byte
	ClePublique   []byte // PKIX (DER)
	Algorithme    int
	Compteur      uint32
	Domaine, Nom  string
	CreeLe        int64
	UtiliseLe     int64
}

// IdentifiantB64 : identifiant en base64 « URL » (ce qu'attend le navigateur).
func (p Passkey) IdentifiantB64() string { return base64.RawURLEncoding.EncodeToString(p.Identifiant) }

// ErrPasskey : erreur montrée à l'utilisateur ; le détail va dans le journal.
var ErrPasskey = errors.New("passkey refusée")

func refus(format string, args ...any) error {
	return fmt.Errorf("%w : %s", ErrPasskey, fmt.Sprintf(format, args...))
}

// NouveauDefiPasskey : 32 octets d'aléa, en base64 « URL ».
func NouveauDefiPasskey() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// DomaineEtOrigine : domaine (rpId) et origine attendus pour une requête
// reçue sous cet hôte. WebAuthn n'existe qu'en HTTPS, sauf sur localhost.
// Une adresse IP n'est pas un domaine valable : les navigateurs la refusent.
func DomaineEtOrigine(hote string) (domaine, origine string, err error) {
	domaine = hote
	if h, _, e := net.SplitHostPort(hote); e == nil {
		domaine = h
	}
	domaine = strings.ToLower(domaine)
	if domaine == "" || len(hote) > 255 || strings.ContainsAny(hote, "/ \\@?#") {
		return "", "", errors.New("hôte invalide")
	}
	if domaine == "localhost" {
		return domaine, "http://" + hote, nil
	}
	return domaine, "https://" + hote, nil
}

// PasskeyPossible : faux pour une adresse IP (les navigateurs refusent).
func PasskeyPossible(domaine string) bool {
	return domaine != "" && net.ParseIP(strings.Trim(domaine, "[]")) == nil
}

func decoderB64(s string, max int) ([]byte, error) {
	if len(s) > max*4/3+4 {
		return nil, errors.New("donnée trop longue")
	}
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
}

// donneesClient : le JSON signé par le navigateur.
type donneesClient struct {
	Type        string `json:"type"`
	Challenge   string `json:"challenge"`
	Origin      string `json:"origin"`
	CrossOrigin bool   `json:"crossOrigin"`
}

// verifierClient contrôle le JSON du navigateur : bon type d'opération, bon
// défi, bonne origine, et pas dans une page d'un autre site.
func verifierClient(brut []byte, typ, defi, origine string) error {
	var d donneesClient
	if err := json.Unmarshal(brut, &d); err != nil {
		return refus("réponse du navigateur illisible")
	}
	if d.Type != typ {
		return refus("type %q inattendu", d.Type)
	}
	if defi == "" || subtle.ConstantTimeCompare([]byte(d.Challenge), []byte(defi)) != 1 {
		return refus("défi inattendu ou expiré")
	}
	if d.Origin != origine {
		return refus("origine %q au lieu de %q", d.Origin, origine)
	}
	if d.CrossOrigin {
		return refus("demande venue d'un autre site")
	}
	return nil
}

// Drapeaux des données de l'authentificateur.
const (
	drapeauPresent  = 0x01 // l'utilisateur a touché la clé
	drapeauVerifie  = 0x04 // code PIN ou biométrie vérifiés
	drapeauAtteste  = 0x40 // données de la clé créée présentes
	tailleEnteteAut = 37   // empreinte du domaine (32) + drapeaux (1) + compteur (4)
)

// lireAuthentificateur contrôle l'en-tête des données de l'authentificateur
// et renvoie les drapeaux, le compteur de signatures et la suite.
func lireAuthentificateur(b []byte, domaine string) (drapeaux byte, compteur uint32, suite []byte, err error) {
	if len(b) < tailleEnteteAut {
		return 0, 0, nil, refus("données de l'authentificateur trop courtes")
	}
	h := sha256.Sum256([]byte(domaine))
	if subtle.ConstantTimeCompare(b[:32], h[:]) != 1 {
		return 0, 0, nil, refus("clé créée pour un autre domaine que %q", domaine)
	}
	drapeaux = b[32]
	if drapeaux&drapeauPresent == 0 {
		return 0, 0, nil, refus("présence de l'utilisateur non confirmée")
	}
	return drapeaux, binary.BigEndian.Uint32(b[33:37]), b[tailleEnteteAut:], nil
}

func entierCOSE(m map[any]any, cle int64) (int64, bool) {
	v, ok := m[cle].(int64)
	return v, ok
}

func octetsCOSE(m map[any]any, cle int64, min, max int) ([]byte, bool) {
	v, ok := m[cle].([]byte)
	return v, ok && len(v) >= min && len(v) <= max
}

// cleCOSE lit la clé publique (format COSE) et la rend au format PKIX, avec
// son algorithme. Seules les trois familles ci-dessus sont acceptées.
func cleCOSE(m map[any]any) ([]byte, int, error) {
	kty, _ := entierCOSE(m, 1)
	alg, _ := entierCOSE(m, 3)
	var pub any
	switch {
	case kty == 2 && alg == algES256: // courbe P-256
		crv, _ := entierCOSE(m, -1)
		x, okx := octetsCOSE(m, -2, 32, 32)
		y, oky := octetsCOSE(m, -3, 32, 32)
		if crv != 1 || !okx || !oky {
			return nil, 0, refus("clé ES256 mal formée")
		}
		k := &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}
		if _, err := k.ECDH(); err != nil { // refuse un point hors de la courbe
			return nil, 0, refus("clé ES256 hors de la courbe")
		}
		pub = k
	case kty == 1 && alg == algEdDSA: // Ed25519
		crv, _ := entierCOSE(m, -1)
		x, okx := octetsCOSE(m, -2, ed25519.PublicKeySize, ed25519.PublicKeySize)
		if crv != 6 || !okx {
			return nil, 0, refus("clé Ed25519 mal formée")
		}
		pub = ed25519.PublicKey(x)
	case kty == 3 && alg == algRS256: // RSA, 2048 bits au moins
		n, okn := octetsCOSE(m, -1, 256, 1024)
		e, oke := octetsCOSE(m, -2, 1, 4)
		if !okn || !oke {
			return nil, 0, refus("clé RSA mal formée ou trop courte")
		}
		exp := int(new(big.Int).SetBytes(e).Int64())
		if exp < 3 || exp%2 == 0 {
			return nil, 0, refus("exposant RSA invalide")
		}
		pub = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: exp}
	default:
		return nil, 0, refus("algorithme de clé non accepté (type %d, algorithme %d)", kty, alg)
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return nil, 0, refus("clé publique inutilisable")
	}
	return der, int(alg), nil
}

// VerifierCreation contrôle la réponse du navigateur à la création d'une
// passkey (navigator.credentials.create) et renvoie la clé à enregistrer.
// clientB64 et attestationB64 : clientDataJSON et attestationObject.
func VerifierCreation(clientB64, attestationB64, defi, domaine, origine string) (*Passkey, error) {
	client, err := decoderB64(clientB64, 4<<10)
	if err != nil {
		return nil, refus("réponse illisible")
	}
	if err := verifierClient(client, "webauthn.create", defi, origine); err != nil {
		return nil, err
	}
	att, err := decoderB64(attestationB64, 64<<10)
	if err != nil {
		return nil, refus("attestation illisible")
	}
	v, _, err := lireCBOR(att, 0)
	objet, ok := v.(map[any]any)
	if err != nil || !ok {
		return nil, refus("attestation illisible")
	}
	aut, ok := objet["authData"].([]byte)
	if !ok {
		return nil, refus("attestation sans données de l'authentificateur")
	}
	drapeaux, compteur, suite, err := lireAuthentificateur(aut, domaine)
	if err != nil {
		return nil, err
	}
	// Suite : identifiant du modèle (16), longueur (2), identifiant, clé COSE.
	if drapeaux&drapeauAtteste == 0 || len(suite) < 18 {
		return nil, refus("aucune clé dans la réponse")
	}
	n := int(binary.BigEndian.Uint16(suite[16:18]))
	if n == 0 || n > 1023 || len(suite) < 18+n {
		return nil, refus("identifiant de clé invalide")
	}
	identifiant := bytes.Clone(suite[18 : 18+n])
	c, _, err := lireCBOR(suite[18+n:], 0)
	cose, ok := c.(map[any]any)
	if err != nil || !ok {
		return nil, refus("clé publique illisible")
	}
	der, alg, err := cleCOSE(cose)
	if err != nil {
		return nil, err
	}
	return &Passkey{Identifiant: identifiant, ClePublique: der, Algorithme: alg, Compteur: compteur, Domaine: domaine}, nil
}

// VerifierConnexion contrôle la réponse du navigateur à une demande de
// connexion (navigator.credentials.get) pour la passkey p. Elle renvoie le
// nouveau compteur de signatures.
func VerifierConnexion(p *Passkey, clientB64, authentificateurB64, signatureB64, defi, domaine, origine string) (uint32, error) {
	if p.Domaine != domaine {
		return 0, refus("clé créée pour %q, pas pour %q", p.Domaine, domaine)
	}
	client, err := decoderB64(clientB64, 4<<10)
	if err != nil {
		return 0, refus("réponse illisible")
	}
	if err := verifierClient(client, "webauthn.get", defi, origine); err != nil {
		return 0, err
	}
	aut, err := decoderB64(authentificateurB64, 4<<10)
	if err != nil {
		return 0, refus("données de l'authentificateur illisibles")
	}
	_, compteur, _, err := lireAuthentificateur(aut, domaine)
	if err != nil {
		return 0, err
	}
	signature, err := decoderB64(signatureB64, 1<<10)
	if err != nil {
		return 0, refus("signature illisible")
	}
	// Message signé : données de l'authentificateur + SHA-256 du JSON client.
	h := sha256.Sum256(client)
	message := append(bytes.Clone(aut), h[:]...)
	pub, err := x509.ParsePKIXPublicKey(p.ClePublique)
	if err != nil {
		return 0, refus("clé enregistrée illisible")
	}
	valide := false
	switch k := pub.(type) {
	case *ecdsa.PublicKey:
		d := sha256.Sum256(message)
		valide = p.Algorithme == algES256 && ecdsa.VerifyASN1(k, d[:], signature)
	case ed25519.PublicKey:
		valide = p.Algorithme == algEdDSA && ed25519.Verify(k, message, signature)
	case *rsa.PublicKey:
		d := sha256.Sum256(message)
		valide = p.Algorithme == algRS256 && rsa.VerifyPKCS1v15(k, crypto.SHA256, d[:], signature) == nil
	}
	if !valide {
		return 0, refus("signature fausse")
	}
	// Compteur : s'il est utilisé (non nul), il doit augmenter. Sinon la clé
	// a peut-être été copiée. Beaucoup de passkeys le laissent à zéro.
	if (compteur != 0 || p.Compteur != 0) && compteur <= p.Compteur {
		return 0, refus("compteur de signatures en recul (clé copiée ?)")
	}
	return compteur, nil
}

// --- Base ---------------------------------------------------------------------------

// ListerPasskeys : passkeys d'un compte, de la plus ancienne à la plus récente.
func ListerPasskeys(ctx context.Context, db *sql.DB, utilisateur int64) ([]Passkey, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, utilisateur_id, identifiant, cle_publique, algorithme, compteur, domaine, nom, cree_le, utilise_le
		FROM passkeys WHERE utilisateur_id = ? ORDER BY id`, utilisateur)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Passkey
	for rows.Next() {
		var p Passkey
		if err := rows.Scan(&p.ID, &p.UtilisateurID, &p.Identifiant, &p.ClePublique, &p.Algorithme, &p.Compteur, &p.Domaine, &p.Nom,
			&p.CreeLe, &p.UtiliseLe); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// NomPasskey nettoie le nom donné à une passkey (affiché dans l'interface).
func NomPasskey(nom string) string {
	nom = strings.TrimSpace(nom)
	if nom == "" {
		nom = "Passkey"
	}
	if r := []rune(nom); len(r) > 40 {
		nom = string(r[:40])
	}
	return nom
}

// AjouterPasskey enregistre une passkey vérifiée, avec une ligne d'audit.
func AjouterPasskey(ctx context.Context, db *sql.DB, u Utilisateur, p *Passkey, nom, adresse string, t time.Time) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // sans effet après Commit
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM passkeys WHERE utilisateur_id = ?`, u.ID).Scan(&n); err != nil {
		return err
	}
	if n >= PasskeysMax {
		return fmt.Errorf("%d passkeys au plus par compte", PasskeysMax)
	}
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM passkeys WHERE identifiant = ?`, p.Identifiant).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return errors.New("cette passkey est déjà enregistrée")
	}
	nom = NomPasskey(nom)
	if _, err := tx.ExecContext(ctx, `INSERT INTO passkeys(utilisateur_id, identifiant, cle_publique, algorithme, compteur, domaine, nom, cree_le)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, u.ID, p.Identifiant, p.ClePublique, p.Algorithme, p.Compteur, p.Domaine, nom, t.Unix()); err != nil {
		return err
	}
	if err := auditer(ctx, tx, "web:"+u.Nom, "utilisateur_passkey_ajoutee", u.Nom, "", nom+" ("+p.Domaine+")", adresse, t); err != nil {
		return err
	}
	return tx.Commit()
}

// SupprimerPasskey retire une passkey du compte (rien si elle n'est pas à lui).
func SupprimerPasskey(ctx context.Context, db *sql.DB, u Utilisateur, id int64, adresse string, t time.Time) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // sans effet après Commit
	var nom string
	err = tx.QueryRowContext(ctx, `SELECT nom FROM passkeys WHERE id = ? AND utilisateur_id = ?`, id, u.ID).Scan(&nom)
	if errors.Is(err, sql.ErrNoRows) {
		return errors.New("passkey inconnue")
	}
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM passkeys WHERE id = ? AND utilisateur_id = ?`, id, u.ID); err != nil {
		return err
	}
	if err := auditer(ctx, tx, "web:"+u.Nom, "utilisateur_passkey_supprimee", u.Nom, nom, "", adresse, t); err != nil {
		return err
	}
	return tx.Commit()
}

// NoterPasskeyUtilisee garde le compteur et la date du dernier usage.
func NoterPasskeyUtilisee(ctx context.Context, db *sql.DB, id int64, compteur uint32, t time.Time) error {
	_, err := db.ExecContext(ctx, `UPDATE passkeys SET compteur = ?, utilise_le = ? WHERE id = ?`, compteur, t.Unix(), id)
	return err
}
