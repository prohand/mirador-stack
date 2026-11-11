// SPDX-License-Identifier: GPL-3.0-or-later

package majagent

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
)

// Clés publiques posées À L'INSTALLATION (choix du 11/11, SPEC § 18.25) :
// le script d'installation écrit les clés dans un fichier qui appartient à
// root (/etc/mirador-agent/cles-publiques.txt, 0644), que l'agent (utilisateur
// sans droits) peut lire mais pas modifier. Le serveur ne peut pas les
// changer à distance : même s'il est piraté, aucune mise à jour non signée
// par la clé privée de l'administrateur n'est acceptée.
//
// Les clés intégrées au binaire (cles/publiques.txt, facultatif : pour qui
// compile lui-même) s'ajoutent à celles du fichier.

// ClesMax : la clé actuelle et la suivante (rotation).
const ClesMax = 2

// ErrDroitsCles : le fichier de clés pourrait être modifié par l'agent.
var ErrDroitsCles = errors.New("fichier de clés modifiable par un autre utilisateur que root : refusé")

// ValiderCles : clés Ed25519 en base64, séparées par des virgules, des
// espaces ou des retours à la ligne (lignes « # » ignorées). Renvoie la
// liste nettoyée.
func ValiderCles(brut string) ([]string, error) {
	var out []string
	for _, l := range strings.Split(brut, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		for _, c := range strings.FieldsFunc(l, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' }) {
			if _, err := cles(c); err != nil {
				return nil, fmt.Errorf("clé publique invalide (base64 de 32 octets attendu) : %q", c)
			}
			if !contient(out, c) {
				out = append(out, c)
			}
		}
	}
	if len(out) > ClesMax {
		return nil, fmt.Errorf("%d clés au plus (l'actuelle et la suivante)", ClesMax)
	}
	return out, nil
}

func contient(l []string, v string) bool {
	for _, x := range l {
		if x == v {
			return true
		}
	}
	return false
}

// ClesInstallees : clés intégrées + clés du fichier posé à l'installation,
// séparées par des virgules (format attendu par VerifierManifeste). Fichier
// absent : clés intégrées seules (vide = aucune mise à jour possible).
func ClesInstallees(chemin string) (string, error) {
	tout := []string{}
	if ClesPubliques != "" {
		tout = append(tout, strings.Split(ClesPubliques, ",")...)
	}
	if chemin != "" {
		st, err := os.Lstat(chemin)
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			return "", err
		default:
			if !st.Mode().IsRegular() {
				return "", fmt.Errorf("%s : fichier ordinaire attendu (pas un lien)", chemin)
			}
			if err := droitsSurs(st); err != nil {
				return "", fmt.Errorf("%s : %w", chemin, err)
			}
			f, err := os.Open(chemin) // #nosec G304 -- chemin de la configuration locale (root)
			if err != nil {
				return "", err
			}
			brut, err := io.ReadAll(io.LimitReader(f, 4096))
			_ = f.Close()
			if err != nil {
				return "", err
			}
			ls, err := ValiderCles(string(brut))
			if err != nil {
				return "", fmt.Errorf("%s : %w", chemin, err)
			}
			for _, c := range ls {
				if !contient(tout, c) {
					tout = append(tout, c)
				}
			}
		}
	}
	return strings.Join(tout, ","), nil
}

// EmpreinteCle : 16 premiers caractères hexadécimaux du SHA-256 de la clé
// (octets bruts), pour comparer d'un coup d'œil la clé de l'interface, celle
// de mirador-release et celle écrite sur la machine (le script affiche la
// même : « base64 -d | sha256sum | cut -c1-16 »).
func EmpreinteCle(b64 string) string {
	k, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil {
		return ""
	}
	h := sha256.Sum256(k)
	return hex.EncodeToString(h[:])[:16]
}
