// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"strconv"
	"sync"
	"time"
)

// Liste des tableaux en cartes (maquette validée le 26/11) : pour chaque
// tableau, ses trois premiers chiffres (choix du 26/11, à la place d'une
// petite courbe étirée qui grossissait le moindre écart), son état (chiffres
// en attention ou en erreur) et ce qu'il couvre (le filtre qui a le plus de
// valeurs). Calculé au plus une fois par 5 min et par tableau.

const dureeResumeTableau = 5 * time.Minute

// resumeTableau : ce qu'une carte de la liste affiche.
type resumeTableau struct {
	Chiffres    []chiffreCarte // trois premiers chiffres du tableau
	Etat        string         // « 2 points à regarder », « tout va bien », vide
	ClasseEtat  string         // attention, erreur, ok
	Couvre      string         // « Machine : 26 », vide
	AvecDonnees bool           // au moins une valeur d'un filtre, ou un chiffre
	expire      time.Time
}

// chiffreCarte : un chiffre clé sur la carte d'un tableau.
type chiffreCarte struct {
	Titre, Texte, Couleur string
}

// chiffresCarte : nombre de chiffres montrés sur une carte.
const chiffresCarte = 3

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
				if len(r.Chiffres) < chiffresCarte && v.Texte != "" {
					titre := p.Titre
					if len(vp.Chiffres) > 1 && v.Legende != "" {
						titre = v.Legende
					}
					r.Chiffres = append(r.Chiffres, chiffreCarte{Titre: titre, Texte: v.Texte, Couleur: v.Couleur})
					r.AvecDonnees = true
				}
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
	return r
}
