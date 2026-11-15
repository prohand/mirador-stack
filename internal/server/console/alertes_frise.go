// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"net/url"
	"sort"
	"time"

	"mirador-stack/internal/server/alertes"
)

// Historique des alertes (SPEC § 28, suite) : frise sur 24 h ou 7 jours (une
// ligne par machine, une barre par alerte, couleur de la gravité) et règles
// les plus bruyantes de la période, avec un lien pour régler leur seuil.

// barreFrise : une alerte sur la frise (unités : 0 à 1000 = la période).
type barreFrise struct {
	ID     int64
	X, L   float64
	Classe string // gravité
	Titre  string // infobulle
}

// ligneFrise : une machine et ses alertes de la période.
type ligneFrise struct {
	Machine, Lien string
	Nb            int
	Barres        []barreFrise
}

// regleBruyante : une règle et ce qu'elle a fait sonner sur la période.
type regleBruyante struct {
	Regle, LienHistorique string
	LienRegler, Regler    string // page où régler (modèle ou règle)
	Fois, Machines        int
	Duree                 time.Duration
}

// donneesFrise : bloc « historique » de la page Alertes.
type donneesFrise struct {
	Periode     string    // 24h ou 7j
	Graduations []string  // libellés, à intervalles égaux depuis le début
	Grille      []float64 // abscisses des graduations (0 à 1000)
	Lignes      []ligneFrise
	Bruyantes   []regleBruyante
	Total       int
	Coupee      bool // plus de lignes que maxLignesFrise
}

const (
	maxLignesFrise   = 25
	maxBruyantes     = 10
	maxAlertesFrise  = 5000
	largeurMinBarre  = 3.0 // une alerte courte reste visible
	sansMachineFrise = "(sans machine)"
)

func periodeFrise(p string) string {
	if p == "7j" {
		return p
	}
	return "24h"
}

// frise : historique de la période qui finit maintenant.
func (c *Console) frise(ctx context.Context, periode string, t time.Time) donneesFrise {
	d := donneesFrise{Periode: periodeFrise(periode)}
	duree, pas, format := 24*time.Hour, 3*time.Hour, "15:04"
	if d.Periode == "7j" {
		duree, pas, format = 7*24*time.Hour, 24*time.Hour, "Mon 02"
	}
	loc := fuseau.Load()
	debut := t.Add(-duree)
	for g := debut; g.Before(t); g = g.Add(pas) {
		d.Graduations = append(d.Graduations, joursFr(g.In(loc).Format(format)))
		d.Grille = append(d.Grille, float64(g.Sub(debut))*1000/float64(duree))
	}
	as, err := alertes.HistoriquePeriode(ctx, c.db, debut, t, maxAlertesFrise)
	if err != nil {
		return d
	}
	d.Total = len(as)
	echelle := 1000 / float64(duree)
	parMachine := map[string]*ligneFrise{}
	parRegle := map[string]*regleBruyante{}
	machinesRegle := map[string]map[string]bool{}
	for _, a := range as {
		m := a.Machine()
		if m == "" {
			m = sansMachineFrise
		}
		l := parMachine[m]
		if l == nil {
			l = &ligneFrise{Machine: m}
			if m != sansMachineFrise {
				l.Lien = "/alertes?machine=" + url.QueryEscape(m)
			}
			parMachine[m] = l
		}
		de, a2 := a.ActiveLe, a.Fin
		if a2.IsZero() || a2.After(t) {
			a2 = t
		}
		if de.Before(debut) {
			de = debut
		}
		x := float64(de.Sub(debut)) * echelle
		w := float64(a2.Sub(de)) * echelle
		if w < largeurMinBarre {
			w = largeurMinBarre
		}
		if x+w > 1000 {
			x = 1000 - w
		}
		l.Nb++
		l.Barres = append(l.Barres, barreFrise{ID: a.ID, X: x, L: w, Classe: a.Gravite,
			Titre: a.Regle + " — " + a.ActiveLe.In(loc).Format("02/01 15:04") + ", " + dureeLisible(a2.Sub(a.ActiveLe))})
		r := parRegle[a.Regle]
		if r == nil {
			r = &regleBruyante{Regle: a.Regle, LienHistorique: "/alertes?regle=" + url.QueryEscape(a.Regle)}
			parRegle[a.Regle] = r
			machinesRegle[a.Regle] = map[string]bool{}
		}
		r.Fois++
		r.Duree += a2.Sub(de)
		machinesRegle[a.Regle][m] = true
	}
	for _, l := range parMachine {
		d.Lignes = append(d.Lignes, *l)
	}
	sort.Slice(d.Lignes, func(i, j int) bool {
		if d.Lignes[i].Nb != d.Lignes[j].Nb {
			return d.Lignes[i].Nb > d.Lignes[j].Nb
		}
		return d.Lignes[i].Machine < d.Lignes[j].Machine
	})
	if len(d.Lignes) > maxLignesFrise {
		d.Lignes, d.Coupee = d.Lignes[:maxLignesFrise], true
	}
	// Règles : où régler (modèle ou règle libre).
	regles := map[string]alertes.Regle{}
	if rs, err := alertes.ListerRegles(ctx, c.db); err == nil {
		for _, r := range rs {
			regles[r.Nom] = r
		}
	}
	for nom, r := range parRegle {
		r.Machines = len(machinesRegle[nom])
		if rg, ok := regles[nom]; ok {
			if m, lien := modeleDeRegle(rg); m != nil {
				r.LienRegler, r.Regler = lien, "seuils du modèle « "+m.Titre+" »"
			} else {
				r.LienRegler, r.Regler = "/regles/modifier/"+url.PathEscape(nom), "la règle"
			}
		}
		d.Bruyantes = append(d.Bruyantes, *r)
	}
	sort.Slice(d.Bruyantes, func(i, j int) bool {
		if d.Bruyantes[i].Fois != d.Bruyantes[j].Fois {
			return d.Bruyantes[i].Fois > d.Bruyantes[j].Fois
		}
		return d.Bruyantes[i].Duree > d.Bruyantes[j].Duree
	})
	if len(d.Bruyantes) > maxBruyantes {
		d.Bruyantes = d.Bruyantes[:maxBruyantes]
	}
	return d
}

// joursFr : noms de jours anglais de Go → français (« Mon 02 » → « lun 02 »).
func joursFr(s string) string {
	for en, fr := range map[string]string{"Mon": "lun", "Tue": "mar", "Wed": "mer", "Thu": "jeu", "Fri": "ven", "Sat": "sam", "Sun": "dim"} {
		if len(s) >= 3 && s[:3] == en {
			return fr + s[3:]
		}
	}
	return s
}
