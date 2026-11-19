// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"sort"
	"strconv"
	"strings"
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
	Nombre                int    // changements identiques le même jour, regroupés (×N)
	DetailCourt           string // détail coupé (le reste en dépliant)
}

type vueChangements struct {
	Lignes    []ligneChangement // regroupées, les plus récentes d'abord
	Recentes  []ligneChangement // montrées d'office
	Anciennes []ligneChangement // repliées (« N plus anciens »)
	Total     int               // changements avant regroupement
	Recent    bool              // au moins un changement depuis 24 h : bloc ouvert d'office
}

// Bloc compact (retour du 19/11 : « prend trop de place ») : 5 lignes
// montrées, le reste replié ; changements identiques du même jour
// regroupés ; détail coupé à 90 caractères.
const (
	changementsMontres = 5
	detailCourtMax     = 90
)

// regrouperChangements : lignes triées (récentes d'abord) ; même jour, même
// type, même détail → une ligne « ×N » (heure de la plus récente).
func regrouperChangements(ls []ligneChangement) []ligneChangement {
	loc := fuseau.Load()
	var out []ligneChangement
	index := map[string]int{}
	for _, l := range ls {
		cle := l.Le.In(loc).Format("2006-01-02") + "\x00" + l.Titre + "\x00" + l.Detail
		if i, ok := index[cle]; ok {
			out[i].Nombre++
			continue
		}
		l.Nombre = 1
		if r := []rune(l.Detail); len(r) > detailCourtMax {
			l.DetailCourt = strings.TrimSpace(string(r[:detailCourtMax])) + "…"
		}
		index[cle] = len(out)
		out = append(out, l)
	}
	return out
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
	v.Total = len(v.Lignes)
	for _, l := range v.Lignes {
		if t.Sub(l.Le) < 24*time.Hour {
			v.Recent = true
		}
	}
	v.Lignes = regrouperChangements(v.Lignes)
	v.Recentes = v.Lignes
	if len(v.Lignes) > changementsMontres {
		v.Recentes, v.Anciennes = v.Lignes[:changementsMontres], v.Lignes[changementsMontres:]
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
