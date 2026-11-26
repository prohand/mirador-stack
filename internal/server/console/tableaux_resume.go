// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Liste des tableaux en cartes (maquette validée le 26/11) : pour chaque
// tableau, une petite courbe sur 24 h (premier graphique), son état (chiffres
// en attention ou en erreur) et ce qu'il couvre (valeurs de son premier
// filtre). Calculé au plus une fois par 5 min et par tableau.

const dureeResumeTableau = 5 * time.Minute

// resumeTableau : ce qu'une carte de la liste affiche.
type resumeTableau struct {
	Apercu      string // points de la petite courbe (300 × 40), vide sans données
	Etat        string // « 2 points à regarder », « tout va bien », vide
	ClasseEtat  string // attention, erreur, ok
	Couvre      string // « Machine : 26 », vide
	AvecDonnees bool   // au moins une valeur du premier filtre (ou une courbe)
	expire      time.Time
}

type cacheResumes struct {
	mu   sync.Mutex
	tout map[string]resumeTableau
}

// valsTous : chaque filtre du tableau sur « tous » (vide), comme une page
// ouverte sans choix : $instance… sont remplacés par « .* ».
func valsTous(t *Tableau) map[string]string {
	vals := map[string]string{}
	for _, v := range t.Variables {
		vals[v.Nom] = ""
	}
	return vals
}

// resumeDe : résumé d'un tableau, depuis le cache si possible.
func (c *Console) resumeDe(ctx context.Context, t *Tableau, maintenant time.Time) resumeTableau {
	c.resumesTab.mu.Lock()
	if r, ok := c.resumesTab.tout[t.Nom]; ok && maintenant.Before(r.expire) {
		c.resumesTab.mu.Unlock()
		return r
	}
	c.resumesTab.mu.Unlock()
	r := c.calculerResume(ctx, t, maintenant)
	r.expire = maintenant.Add(dureeResumeTableau)
	c.resumesTab.mu.Lock()
	if c.resumesTab.tout == nil || len(c.resumesTab.tout) > 200 {
		c.resumesTab.tout = map[string]resumeTableau{}
	}
	c.resumesTab.tout[t.Nom] = r
	c.resumesTab.mu.Unlock()
	return r
}

func (c *Console) calculerResume(ctx context.Context, t *Tableau, maintenant time.Time) resumeTableau {
	var r resumeTableau
	if c.lecteur == nil {
		return r
	}
	vals := valsTous(t)
	// Ce qu'il couvre : le filtre qui a le plus de valeurs sur 1 jour
	// (« Machine : 26 » plutôt que « Site : 1 »).
	meilleur := 0
	for _, v := range t.Variables {
		if expr, err := requeteVariable(v.Requete, vals); err == nil {
			if ech, err := c.instant(ctx, expr, maintenant); err == nil && len(ech) > 0 && len(ech) >= meilleur {
				meilleur = len(ech)
				r.Couvre = v.Titre + " : " + strconv.Itoa(len(ech))
				r.AvecDonnees = true
			}
		}
	}
	// État : chiffres en attention ou en erreur (comme le sommaire du tableau).
	pas, d := pasPour(24*time.Hour), 24*time.Hour
	nbChiffres, pb, grave := 0, 0, false
	for ri := range t.Rangees {
		for pi := range t.Rangees[ri].Panneaux {
			p := &t.Rangees[ri].Panneaux[pi]
			if p.Type != PanneauChiffre {
				continue
			}
			vp := vuePanneau{P: p}
			if err := c.remplirPanneau(ctx, &vp, vals, pas, d, maintenant); err != nil {
				continue
			}
			for _, v := range vp.Chiffres {
				nbChiffres++
				switch v.Couleur {
				case "erreur":
					pb++
					grave = true
				case "attention":
					pb++
				}
			}
		}
	}
	switch {
	case pb > 0:
		r.Etat, r.ClasseEtat = accorderTexte(strconv.Itoa(pb)+" point(s) à regarder"), "attention"
		if grave {
			r.ClasseEtat = "erreur"
		}
	case nbChiffres > 0:
		r.Etat, r.ClasseEtat = "tout va bien", "ok"
	}
	// Petite courbe : premier graphique, moyenne de ses courbes, 24 h.
	r.Apercu = c.apercuTableau(ctx, t, maintenant)
	if r.Apercu != "" {
		r.AvecDonnees = true
	}
	return r
}

// apercuTableau : points « x,y » de la moyenne des courbes de la première
// requête du premier graphique, sur 24 h (48 points).
func (c *Console) apercuTableau(ctx context.Context, t *Tableau, maintenant time.Time) string {
	var expr string
	for ri := range t.Rangees {
		for pi := range t.Rangees[ri].Panneaux {
			p := &t.Rangees[ri].Panneaux[pi]
			if p.Type == PanneauCourbe && len(p.Requetes) > 0 && expr == "" {
				expr = substituer(p.Requetes[0].Expr, valsTous(t), 30*time.Minute, 24*time.Hour)
			}
		}
	}
	if expr == "" {
		return ""
	}
	fin := maintenant.Truncate(30 * time.Minute)
	debut := fin.Add(-24 * time.Hour)
	courbes, _, err := c.lecteur.Plage(ctx, expr, debut, fin, 30*time.Minute)
	if err != nil || len(courbes) == 0 {
		return ""
	}
	somme, nb := map[int64]float64{}, map[int64]int{}
	for _, cb := range courbes {
		for i, ts := range cb.T {
			if v := cb.V[i]; !math.IsNaN(v) && !math.IsInf(v, 0) {
				somme[ts] += v
				nb[ts]++
			}
		}
	}
	var xs []int64
	for ts := debut.UnixMilli(); ts <= fin.UnixMilli(); ts += (30 * time.Minute).Milliseconds() {
		if nb[ts] > 0 {
			xs = append(xs, ts)
		}
	}
	if len(xs) < 2 {
		return ""
	}
	mn, mx := math.Inf(1), math.Inf(-1)
	for _, ts := range xs {
		v := somme[ts] / float64(nb[ts])
		mn, mx = math.Min(mn, v), math.Max(mx, v)
	}
	if mx == mn {
		mx = mn + 1
	}
	var b strings.Builder
	duree := float64(fin.UnixMilli() - debut.UnixMilli())
	for i, ts := range xs {
		v := somme[ts] / float64(nb[ts])
		x := float64(ts-debut.UnixMilli()) / duree * 300
		y := 36 - (v-mn)/(mx-mn)*32
		if i > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "%.1f,%.1f", x, y)
	}
	return b.String()
}
