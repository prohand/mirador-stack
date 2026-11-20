// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"sort"
	"strconv"
	"strings"
	"time"

	"mirador-stack/internal/server/alertes"
	"mirador-stack/internal/server/taches"
)

// Tâches planifiées, suite (SPEC § 95, demande du 20/11 « fais tout ») :
//   - frise des 10 dernières exécutions (vert / rouge, durée au survol) ;
//   - tâches suivies regroupées par machine, filtre « en problème » ;
//   - carte « Sauvegardes de la nuit » de la vue d'ensemble.

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

// --- Carte « Sauvegardes de la nuit » (vue d'ensemble) ------------------------------

// estSauvegarde : tâche de sauvegarde (Proxmox, ou nom qui le dit).
func estSauvegarde(source, nom, libelle string) bool {
	switch {
	case source == "pve" && strings.HasPrefix(nom, "vzdump"):
		return true
	case source == "pbs":
		typ, _, _ := strings.Cut(nom, " ")
		return typ == "backup" || typ == "syncjob"
	}
	n := strings.ToLower(nom + " " + libelle)
	return strings.Contains(n, "sauvegard") || strings.Contains(n, "backup")
}

// ligneSauvegardeNuit : une sauvegarde des dernières 24 h.
type ligneSauvegardeNuit struct {
	Nom, Machine, Cle string
	Fin               time.Time
	OK                bool
	Texte             string // « 12 VM · 48 Go »
}

type sauvegardesNuit struct {
	Total, Reussies  int
	VMs, VMsEchec    int
	Octets           float64
	Echecs, Reussite []ligneSauvegardeNuit // échecs d'abord (tous), réussies (5 au plus)
	Muettes          int                   // suivies, sans exécution depuis 24 h (en retard)
}

// sauvegardesDeLaNuit : dernière exécution de chaque sauvegarde finie dans
// les 24 dernières heures (nil : aucune sauvegarde connue).
func (c *Console) sauvegardesDeLaNuit(q *req) *sauvegardesNuit {
	ctx := q.r.Context()
	det := taches.Detectees(func(expr string) ([]alertes.Echantillon, error) { return c.instant(ctx, expr, q.t) })
	ts, _ := taches.Lister(ctx, c.db)
	ts = taches.AvecReleves(ts, det)
	type vue struct {
		nom, machine, cle string
		r                 taches.Releve
		suivie            bool
		retard            bool
	}
	var vues []vue
	suivies := map[string]bool{}
	for _, t := range ts {
		suivies[t.Cle] = true
		if estSauvegarde(t.Source, t.Nom, t.Libelle) {
			e := taches.Calculer(t, t.Releve, q.t)
			vues = append(vues, vue{t.Affiche(), t.Machine, t.Cle, t.Releve, true, strings.HasPrefix(e.Texte, "En retard") || e.Texte == "Jamais reçue"})
		}
	}
	for k, d := range det {
		if !suivies[k] && estSauvegarde(d.Source, d.Nom, "") {
			vues = append(vues, vue{d.Nom, d.Machine, k, d.Releve, false, false})
		}
	}
	if len(vues) == 0 {
		return nil
	}
	s := &sauvegardesNuit{}
	for _, v := range vues {
		if v.r.Fin.IsZero() || q.t.Sub(v.r.Fin) > 24*time.Hour {
			if v.retard {
				s.Muettes++
			}
			continue
		}
		l := ligneSauvegardeNuit{Nom: v.nom, Machine: v.machine, Cle: v.cle, Fin: v.r.Fin, OK: v.r.Code == 0}
		var oct float64
		echecs := 0
		for _, x := range v.r.VMs {
			oct += x.Octets
			if !x.OK {
				echecs++
			}
		}
		s.VMs += len(v.r.VMs)
		s.VMsEchec += echecs
		s.Octets += oct
		var morceaux []string
		if n := len(v.r.VMs); n > 0 {
			t := strconv.Itoa(n) + " VM/CT"
			if echecs > 0 {
				t += " dont " + strconv.Itoa(echecs) + " en échec"
			}
			morceaux = append(morceaux, t)
		}
		if oct > 0 {
			morceaux = append(morceaux, taches.TailleTexte(oct))
		}
		l.Texte = strings.Join(morceaux, " · ")
		s.Total++
		if l.OK {
			s.Reussies++
			s.Reussite = append(s.Reussite, l)
		} else {
			s.Echecs = append(s.Echecs, l)
		}
	}
	sort.Slice(s.Reussite, func(i, j int) bool { return s.Reussite[i].Fin.After(s.Reussite[j].Fin) })
	if len(s.Reussite) > 5 {
		s.Reussite = s.Reussite[:5]
	}
	return s
}
