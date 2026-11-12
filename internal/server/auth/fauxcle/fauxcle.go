// SPDX-License-Identifier: GPL-3.0-or-later

// Package fauxcle : faux authentificateur WebAuthn, pour les tests
// seulement. Il fait ce que font un navigateur et une clé de sécurité :
// créer une clé, puis signer une demande de connexion.
package fauxcle

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"math/big"
)

// Cle : une passkey simulée.
type Cle struct {
	Algorithme  int // -7 ES256, -8 EdDSA, -257 RS256
	Identifiant []byte
	Compteur    uint32
	Domaine     string
	Origine     string
	// SansVerification : la clé confirme la présence (un toucher) mais ne
	// vérifie pas l'utilisateur (ni code PIN ni biométrie).
	SansVerification bool
	prive            crypto.Signer
}

// Nouvelle crée une clé pour un domaine et une origine.
func Nouvelle(algorithme int, domaine, origine string) *Cle {
	c := &Cle{Algorithme: algorithme, Domaine: domaine, Origine: origine, Identifiant: make([]byte, 20)}
	_, _ = rand.Read(c.Identifiant)
	switch algorithme {
	case -8:
		_, k, _ := ed25519.GenerateKey(rand.Reader)
		c.prive = k
	case -257:
		k, _ := rsa.GenerateKey(rand.Reader, 2048)
		c.prive = k
	default:
		k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		c.prive = k
	}
	return c
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// --- Écriture CBOR (ce qu'il faut, pas plus) -----------------------------------------

// grosBoutiste : les « octets » derniers octets de n, poids fort d'abord
// (aucune conversion d'entier : rien ne peut déborder).
func grosBoutiste(n uint64, octets int) []byte {
	return binary.BigEndian.AppendUint64(nil, n)[8-octets:]
}

func tete(majeur byte, n uint64) []byte {
	switch {
	case n < 24:
		return []byte{majeur<<5 | byte(n)}
	case n < 1<<8:
		return append([]byte{majeur<<5 | 24}, grosBoutiste(n, 1)...)
	case n < 1<<16:
		return append([]byte{majeur<<5 | 25}, grosBoutiste(n, 2)...)
	}
	return append([]byte{majeur<<5 | 26}, grosBoutiste(n, 4)...)
}

func entier(v int) []byte {
	if v < 0 {
		return tete(1, uint64(-1-v))
	}
	return tete(0, uint64(v))
}

func octets(b []byte) []byte { return append(tete(2, uint64(len(b))), b...) }
func texte(s string) []byte  { return append(tete(3, uint64(len(s))), s...) }

// table : paires clé, valeur déjà encodées.
func table(paires ...[]byte) []byte {
	out := tete(5, uint64(len(paires)/2))
	for _, p := range paires {
		out = append(out, p...)
	}
	return out
}

func (c *Cle) cose() []byte {
	switch k := c.prive.Public().(type) {
	case ed25519.PublicKey:
		return table(entier(1), entier(1), entier(3), entier(-8), entier(-1), entier(6), entier(-2), octets(k))
	case *rsa.PublicKey:
		e := big.NewInt(int64(k.E)).Bytes()
		return table(entier(1), entier(3), entier(3), entier(-257), entier(-1), octets(k.N.Bytes()), entier(-2), octets(e))
	case *ecdsa.PublicKey:
		x, y := make([]byte, 32), make([]byte, 32)
		k.X.FillBytes(x)
		k.Y.FillBytes(y)
		return table(entier(1), entier(2), entier(3), entier(-7), entier(-1), entier(1), entier(-2), octets(x), entier(-3), octets(y))
	}
	return nil
}

func (c *Cle) client(typ, defi string) []byte {
	b, _ := json.Marshal(map[string]any{"type": typ, "challenge": defi, "origin": c.Origine, "crossOrigin": false})
	return b
}

// entete : empreinte du domaine, drapeaux, compteur.
func (c *Cle) entete(drapeaux byte) []byte {
	h := sha256.Sum256([]byte(c.Domaine))
	return binary.BigEndian.AppendUint32(append(h[:], drapeaux), c.Compteur)
}

// Creer renvoie clientDataJSON et attestationObject (base64 « URL »), comme
// navigator.credentials.create.
func (c *Cle) Creer(defi string) (client, attestation string) {
	aut := c.entete(0x45) // présent, vérifié, clé jointe
	aut = append(aut, make([]byte, 16)...)
	aut = append(aut, grosBoutiste(uint64(len(c.Identifiant)), 2)...)
	aut = append(aut, c.Identifiant...)
	aut = append(aut, c.cose()...)
	objet := table(texte("fmt"), texte("none"), texte("attStmt"), table(), texte("authData"), octets(aut))
	return b64(c.client("webauthn.create", defi)), b64(objet)
}

// Signer renvoie clientDataJSON, authenticatorData et signature (base64
// « URL »), comme navigator.credentials.get. Le compteur avance d'un cran
// s'il est utilisé (non nul).
func (c *Cle) Signer(defi string) (client, authentificateur, signature string) {
	if c.Compteur > 0 {
		c.Compteur++
	}
	drapeaux := byte(0x05) // présent + vérifié
	if c.SansVerification {
		drapeaux = 0x01
	}
	aut := c.entete(drapeaux)
	cl := c.client("webauthn.get", defi)
	h := sha256.Sum256(cl)
	message := append(append([]byte{}, aut...), h[:]...)
	var sig []byte
	switch k := c.prive.(type) {
	case ed25519.PrivateKey:
		sig = ed25519.Sign(k, message)
	default:
		d := sha256.Sum256(message)
		sig, _ = k.Sign(rand.Reader, d[:], crypto.SHA256)
	}
	return b64(cl), b64(aut), b64(sig)
}
