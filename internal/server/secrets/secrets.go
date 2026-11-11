// SPDX-License-Identifier: GPL-3.0-or-later

// Package secrets chiffre les secrets gardés dans la base SQLite (clé SSH de
// sauvegarde, secret S3, plus tard tokens des canaux de notification) avec la
// clé des secrets du serveur (fichier séparé, jamais dans la base ni dans les
// sauvegardes).
//
// Format : "chiffre:v1:" + base64(nonce | AES-256-GCM(secret)). L'usage du
// secret (ex. "sauvegarde.sftp.cle_ssh") est lié au chiffré : un secret copié
// dans un autre champ ne se déchiffre pas.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
)

const prefixe = "chiffre:v1:"

// ErrSecret : clé fausse ou valeur modifiée.
var ErrSecret = errors.New("secret illisible : clé des secrets différente ou valeur modifiée")

func aead(cle []byte) (cipher.AEAD, error) {
	if len(cle) != 32 {
		return nil, errors.New("clé des secrets : 32 octets attendus")
	}
	k, err := hkdf.Key(sha256.New, cle, nil, "mirador-secrets-v1", 32)
	if err != nil {
		return nil, err
	}
	b, err := aes.NewCipher(k)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(b)
}

// EstChiffre : la valeur est déjà au format chiffré.
func EstChiffre(v string) bool { return strings.HasPrefix(v, prefixe) }

// Chiffrer renvoie la valeur à garder en base.
func Chiffrer(cle []byte, usage, clair string) (string, error) {
	a, err := aead(cle)
	if err != nil {
		return "", err
	}
	n := make([]byte, a.NonceSize())
	if _, err := rand.Read(n); err != nil {
		return "", err
	}
	return prefixe + base64.StdEncoding.EncodeToString(a.Seal(n, n, []byte(clair), []byte(usage))), nil
}

// Dechiffrer relit une valeur gardée en base.
func Dechiffrer(cle []byte, usage, valeur string) (string, error) {
	if !EstChiffre(valeur) {
		return "", errors.New("secret non chiffré")
	}
	a, err := aead(cle)
	if err != nil {
		return "", err
	}
	brut, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(valeur, prefixe))
	if err != nil || len(brut) < a.NonceSize()+a.Overhead() {
		return "", ErrSecret
	}
	clair, err := a.Open(nil, brut[:a.NonceSize()], brut[a.NonceSize():], []byte(usage))
	if err != nil {
		return "", ErrSecret
	}
	return string(clair), nil
}
