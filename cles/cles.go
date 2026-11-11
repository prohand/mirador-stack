// SPDX-License-Identifier: GPL-3.0-or-later

// Package cles intègre au binaire les clés PUBLIQUES de signature des
// versions de mirador-agent (publiques.txt). Même résultat avec « make » et
// avec la construction de l'image Docker du serveur.
package cles

import (
	_ "embed"
	"strings"
)

//go:embed publiques.txt
var fichier string

// Publiques renvoie les clés (base64) séparées par des virgules, sans les
// lignes de commentaire.
func Publiques() string {
	var out []string
	for _, l := range strings.Split(fichier, "\n") {
		l = strings.TrimSpace(l)
		if l != "" && !strings.HasPrefix(l, "#") {
			out = append(out, l)
		}
	}
	return strings.Join(out, ",")
}
