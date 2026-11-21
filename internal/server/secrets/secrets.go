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
	"encoding/json"
	"errors"
	"fmt"
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

// Masque : texte mis à la place d'un secret dans ce qui est montré ou
// exporté « sans secret ». À l'import, il veut dire « garder la valeur
// actuelle ».
const Masque = "***"

// Masquer remplace par Masque tous les secrets chiffrés d'un document JSON
// déjà décodé (tables, listes, textes), à n'importe quelle profondeur. Le
// document est modifié sur place et renvoyé. Sert au journal d'audit et à
// l'export « sans secret » : un texte chiffré n'est pas lisible sans la clé,
// mais il n'a rien à faire là (audit du 16/11, SPEC § 34).
func Masquer(v any) any {
	switch x := v.(type) {
	case string:
		if EstChiffre(x) {
			return Masque
		}
	case map[string]any:
		for k, e := range x {
			x[k] = Masquer(e)
		}
	case []any:
		for i, e := range x {
			x[i] = Masquer(e)
		}
	}
	return v
}

// MasquerJSON : comme Masquer, pour un document JSON en texte. Un document
// vide ou illisible est rendu tel quel (rien à masquer).
func MasquerJSON(doc string) string {
	if !strings.Contains(doc, prefixe) {
		return doc
	}
	var v any
	if err := json.Unmarshal([]byte(doc), &v); err != nil {
		// Illisible mais avec un secret chiffré dedans : on ne garde rien.
		return `"(document illisible, non gardé)"`
	}
	b, err := json.Marshal(Masquer(v))
	if err != nil {
		return `"(document illisible, non gardé)"`
	}
	return string(b)
}

// Garder remet, dans un document JSON décodé, la valeur actuelle à la place
// de chaque Masque (import d'un export « sans secret »). Les listes de
// tables sont rapprochées par leur champ « machine » s'il existe, sinon par
// leur rang. Erreur si un Masque n'a aucune valeur actuelle à reprendre.
func Garder(nouveau, actuel any) (any, error) {
	switch x := nouveau.(type) {
	case string:
		if x != Masque {
			return x, nil
		}
		a, _ := actuel.(string)
		if !EstChiffre(a) {
			return nil, errors.New("secret « *** » sans valeur actuelle à garder : le saisir dans l'interface")
		}
		return a, nil
	case map[string]any:
		am, _ := actuel.(map[string]any)
		// Secret gardé (« *** ») d'un équipement : seulement pour la même
		// adresse et la même empreinte TLS (relecture du 21/11 : à l'import,
		// un mot de passe UniFi ou SNMP pouvait partir vers une autre
		// machine du LAN ; le formulaire refusait déjà ce cas).
		if contientMasque(x) {
			for _, k := range []string{"adresse", "empreinte"} {
				if fmt.Sprint(vide(x[k])) != fmt.Sprint(vide(am[k])) {
					return nil, fmt.Errorf("secret « *** » gardé alors que « %s » change : saisir le secret dans l'interface", k)
				}
			}
		}
		for k, e := range x {
			g, err := Garder(e, am[k])
			if err != nil {
				return nil, err
			}
			x[k] = g
		}
	case []any:
		al, _ := actuel.([]any)
		for i, e := range x {
			var a any
			if i < len(al) {
				a = al[i]
			}
			// Même « machine » : c'est le même équipement, où qu'il soit dans la liste.
			if em, ok := e.(map[string]any); ok && em["machine"] != nil {
				a = nil
				for _, c := range al {
					if cm, ok := c.(map[string]any); ok && cm["machine"] == em["machine"] {
						a = c
					}
				}
			}
			g, err := Garder(e, a)
			if err != nil {
				return nil, err
			}
			x[i] = g
		}
	}
	return nouveau, nil
}

// contientMasque : un champ direct de m vaut « *** ».
func contientMasque(m map[string]any) bool {
	for _, v := range m {
		if v == Masque {
			return true
		}
	}
	return false
}

// vide : nil et "" comptent pareil (champ absent).
func vide(v any) any {
	if v == nil {
		return ""
	}
	return v
}
