// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"sort"
	"strconv"
	"time"

	"mirador-stack/internal/server/taches"
)

// Tâches planifiées, suite (SPEC § 95, demande du 20/11 « fais tout ») :
//   - frise des 10 dernières exécutions (vert / rouge, durée au survol) ;
//   - tâches suivies regroupées par machine, filtre « en problème » ;

const caseFrise = 10 // exécutions montrées dans la frise

// caseFriseTache : une exécution de la frise.
type caseFriseTache struct {
	Classe string // ok, er, at (anormale)
	Titre  string // « 20/11 02:14 · OK · 12 min »
}

// friseTache : exécutions de la plus ancienne à la plus récente.
func friseTache(hist []taches.Execution) []caseFriseTache {
	n := min(caseFrise, len(hist))
	out := make([]caseFriseTache, 0, n)
	loc := fuseau.Load()
	for i := n - 1; i >= 0; i-- {
		e := hist[i]
		c := caseFriseTache{Classe: "ok", Titre: e.Fin.In(loc).Format("02/01 15:04") + " · OK"}
		if e.Code != 0 {
			c.Classe, c.Titre = "er", e.Fin.In(loc).Format("02/01 15:04")+" · échec (code "+strconv.Itoa(e.Code)+")"
		} else if h := taches.DureeHabituelle(hist[i:], e.Fin); h > 0 && e.Duree() > 3*h && e.Duree()-h >= 5*time.Minute {
			c.Classe = "at"
		}
		if d := e.Duree(); d > 0 {
			c.Titre += " · " + taches.DureeTexte(d)
		}
		if e.Octets > 0 {
			c.Titre += " · " + taches.TailleTexte(e.Octets)
		}
		out = append(out, c)
	}
	return out
}

// groupeTaches : tâches suivies d'une machine.
type groupeTaches struct {
	Machine    string // "" : signal curl sans machine
	Lignes     []ligneTache
	NbProbleme int
}

// grouperTaches : par machine (machines en problème d'abord, puis par nom) ;
// dans un groupe, les tâches en problème d'abord.
func grouperTaches(ls []ligneTache, seulementProblemes bool) []groupeTaches {
	idx := map[string]int{}
	var out []groupeTaches
	for _, l := range ls {
		if seulementProblemes && !l.Etat.Probleme {
			continue
		}
		i, ok := idx[l.T.Machine]
		if !ok {
			i = len(out)
			idx[l.T.Machine] = i
			out = append(out, groupeTaches{Machine: l.T.Machine})
		}
		out[i].Lignes = append(out[i].Lignes, l)
		if l.Etat.Probleme {
			out[i].NbProbleme++
		}
	}
	for _, g := range out {
		sort.SliceStable(g.Lignes, func(i, j int) bool { return g.Lignes[i].Etat.Probleme && !g.Lignes[j].Etat.Probleme })
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if (a.NbProbleme > 0) != (b.NbProbleme > 0) {
			return a.NbProbleme > 0
		}
		if (a.Machine == "") != (b.Machine == "") {
			return b.Machine == "" // sans machine à la fin
		}
		return a.Machine < b.Machine
	})
	return out
}
