// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"regexp"
	"strconv"
	"strings"
)

// Portées lisibles (SPEC § 75) : « machine:2 » est l'identifiant gardé en
// base (stable si la machine change de nom) ; à l'écran, on montre son nom.
// « defaut » → « Par défaut », « groupe:lan » → « Groupe lan ».

var (
	reMachineID = regexp.MustCompile(`\bmachine:(\d+)\b`)
	reGroupe    = regexp.MustCompile(`\bgroupe:([A-Za-z0-9._-]+)`)
)

// nomsMachinesParID : « machine:ID » → nom de la machine.
func (c *Console) nomsMachinesParID(ctx context.Context) map[string]string {
	noms := map[string]string{}
	rows, err := c.db.QueryContext(ctx, `SELECT id, nom FROM machines`)
	if err != nil {
		return noms
	}
	defer func() { _ = rows.Close() }() // lecture seule
	for rows.Next() {
		var id int64
		var nom string
		if rows.Scan(&id, &nom) == nil {
			noms["machine:"+strconv.FormatInt(id, 10)] = nom
		}
	}
	return noms
}

// porteeLisible : titre d'une portée de configuration.
func porteeLisible(p string, noms map[string]string) string {
	switch {
	case p == "defaut" || p == "":
		return "Par défaut"
	case strings.HasPrefix(p, "groupe:"):
		return "Groupe " + strings.TrimPrefix(p, "groupe:")
	case strings.HasPrefix(p, "machine:"):
		if n, ok := noms[p]; ok {
			return "Machine " + n
		}
		return "Machine n° " + strings.TrimPrefix(p, "machine:")
	}
	return p
}

// remplacerPortees : dans un texte, « machine:2 » devient le nom de la
// machine (inchangé si elle n'existe plus).
func remplacerPortees(texte string, noms map[string]string) string {
	if strings.Contains(texte, "groupe:") {
		texte = reGroupe.ReplaceAllString(texte, "groupe $1")
	}
	if !strings.Contains(texte, "machine:") {
		return texte
	}
	return reMachineID.ReplaceAllStringFunc(texte, func(m string) string {
		if n, ok := noms[m]; ok {
			return n
		}
		return m
	})
}

// quiLisible : auteur d'une action (« web:admin » → « admin », « demo » →
// « démo », « mirador » inchangé).
func quiLisible(q string) string {
	switch {
	case strings.HasPrefix(q, "web:"):
		return strings.TrimPrefix(q, "web:")
	case q == "demo":
		return "démo"
	}
	return q
}
