// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"sort"
	"strconv"
	"time"

	"mirador-stack/internal/server/changements"
)

// Bloc « Changements » de la page d'une machine (SPEC § 47) : paquets mis
// à jour, redémarrages, noyau, agent, conteneurs recréés (repérés par le
// serveur), et réglages faits dans l'interface pour la machine (audit).

type ligneChangement struct {
	Le                    time.Time
	Titre, Classe, Detail string
	Source                string
}

type vueChangements struct {
	Lignes []ligneChangement
	Recent bool // au moins un changement depuis 24 h : bloc ouvert d'office
}

// dureeChangements : période montrée.
const dureeChangements = 7 * 24 * time.Hour

// classeChangement : couleur de la pastille (même code que les repères).
func classeChangement(typ string) string {
	switch typ {
	case changements.Paquets:
		return "attention"
	case changements.Conteneur:
		return "info"
	case changements.Agent:
		return "ok"
	}
	return ""
}

func (c *Console) changementsMachine(ctx context.Context, instance string, t time.Time) *vueChangements {
	v := &vueChangements{}
	debut := t.Add(-dureeChangements)
	if cs, err := changements.Lister(ctx, c.db, instance, debut, 100); err == nil {
		for _, x := range cs {
			source := "agent"
			if x.Type == changements.Agent {
				source = "serveur"
			}
			v.Lignes = append(v.Lignes, ligneChangement{Le: x.Le, Titre: changements.Titre(x.Type), Classe: classeChangement(x.Type),
				Detail: x.Detail, Source: source})
		}
	}
	for _, e := range c.auditMachine(ctx, instance, debut, t, 50) {
		v.Lignes = append(v.Lignes, ligneChangement{Le: time.Unix(e.T, 0), Titre: "Réglage", Detail: e.Nom, Source: "audit mirador"})
	}
	sort.SliceStable(v.Lignes, func(i, j int) bool { return v.Lignes[i].Le.After(v.Lignes[j].Le) })
	for _, l := range v.Lignes {
		if t.Sub(l.Le) < 24*time.Hour {
			v.Recent = true
		}
	}
	return v
}

// auditMachine : changements faits dans l'interface pour cette machine
// (hors actions automatiques), du plus ancien au plus récent.
func (c *Console) auditMachine(ctx context.Context, instance string, debut, fin time.Time, max int) []evenementCourbe {
	var id int64
	if err := c.db.QueryRowContext(ctx, `SELECT id FROM machines WHERE nom = ?`, instance).Scan(&id); err != nil {
		return nil
	}
	porte := "machine:" + strconv.FormatInt(id, 10)
	rows, err := c.db.QueryContext(ctx, `SELECT le, action, objet FROM audit WHERE le >= ? AND le <= ? AND (objet = ? OR objet = ? OR objet LIKE ?)
		AND qui NOT IN ('mirador', 'auto') ORDER BY le LIMIT ?`, debut.Unix(), fin.Unix(), instance, porte, porte+" %", max)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []evenementCourbe
	for rows.Next() {
		var le int64
		var action, objet string
		if rows.Scan(&le, &action, &objet) == nil {
			out = append(out, evenementCourbe{T: le, Nom: phraseAudit(action, objet)})
		}
	}
	return out
}
