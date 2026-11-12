// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" // #nosec G505 -- TOTP (RFC 6238) : SHA-1 est l'algorithme que toutes les applications comprennent ; HMAC-SHA1 reste sûr
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"html/template"
	"net/url"
	"strings"
	"time"

	"rsc.io/qr"
)

// Double authentification par TOTP (RFC 6238) : codes à 6 chiffres, pas de
// 30 s, compatibles avec toutes les applications (Aegis, FreeOTP, Google
// Authenticator…). Écrit ici (~60 lignes) plutôt qu'une dépendance.
//
// WebAuthn (clé de sécurité) n'est pas fait : il demande une bibliothèque
// lourde et du JavaScript ; TOTP suffit pour le brief (« TOTP ou WebAuthn »).

const (
	pasTOTP     = 30
	chiffreTOTP = 6
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// NouveauSecretTOTP : 20 octets aléatoires (160 bits, RFC 4226), en base32.
func NouveauSecretTOTP() (string, error) {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return b32.EncodeToString(b), nil
}

// codeTOTP calcule le code d'un pas donné.
func codeTOTP(secret string, pas int64) (string, error) {
	cle, err := b32.DecodeString(strings.ToUpper(strings.TrimSpace(secret)))
	if err != nil {
		return "", err
	}
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(pas)) // #nosec G115 -- pas toujours positif (heure Unix / 30)
	h := hmac.New(sha1.New, cle)
	h.Write(msg[:])
	s := h.Sum(nil)
	o := s[len(s)-1] & 0x0f
	v := binary.BigEndian.Uint32(s[o:o+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", v%1_000_000), nil
}

// VerifierTOTP accepte le code du pas courant et des pas voisins (±30 s de
// décalage d'horloge). Renvoie le pas reconnu : il doit être plus grand que
// le dernier pas accepté pour ce compte (un code déjà utilisé est refusé,
// même dans sa fenêtre de validité).
func VerifierTOTP(secret, code string, t time.Time, dernier int64) (int64, bool) {
	code = strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	if len(code) != chiffreTOTP {
		return 0, false
	}
	courant := t.Unix() / pasTOTP
	for _, d := range []int64{0, -1, 1} {
		p := courant + d
		if p <= dernier {
			continue
		}
		attendu, err := codeTOTP(secret, p)
		if err == nil && hmac.Equal([]byte(attendu), []byte(code)) {
			return p, true
		}
	}
	return 0, false
}

// URITOTP : adresse otpauth:// lue par les applications (QR code).
func URITOTP(compte, secret string) string {
	v := url.Values{}
	v.Set("secret", secret)
	v.Set("issuer", "mirador")
	v.Set("algorithm", "SHA1")
	v.Set("digits", "6")
	v.Set("period", "30")
	return "otpauth://totp/" + url.PathEscape("mirador:"+compte) + "?" + v.Encode()
}

// ImageQR : QR code en PNG, prêt pour un <img src="data:…"> (permis par la
// CSP : img-src 'self' data:). Rien ne sort du serveur.
func ImageQR(texte string) (template.URL, error) {
	c, err := qr.Encode(texte, qr.M)
	if err != nil {
		return "", err
	}
	c.Scale = 6
	// template.URL : adresse sûre (fabriquée ici), html/template la garde telle quelle.
	return template.URL("data:image/png;base64," + base64.StdEncoding.EncodeToString(c.PNG())), nil // #nosec G203 -- contenu produit par le serveur
}

// CodeTOTP : code valable à l'instant t (tests de bout en bout).
func CodeTOTP(secret string, t time.Time) (string, error) { return codeTOTP(secret, t.Unix()/pasTOTP) }
