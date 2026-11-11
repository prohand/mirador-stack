// SPDX-License-Identifier: GPL-3.0-or-later

package console

import "math"

// Carte du monde des flux bloqués (demande du 11/11, SPEC § 18.20) : fond
// Natural Earth (domaine public) embarqué (carte_monde.go, ~35 Ko une fois
// compressé), dessiné côté serveur en SVG dans la page : chaque pays reçoit
// une classe de couleur (niveau 0 à 5, échelle logarithmique) et une
// infobulle. Aucune ressource externe, aucun script, CSP inchangée.

type paysSVG struct{ Code, Nom, D string }

// paysVue : un pays de la carte avec son nombre de flux.
type paysVue struct {
	Code, Nom, D string
	N            int
	Niveau       int // 0 = aucun flux, 1 à 5 = de peu à beaucoup
}

// nomsPays : nom français d'un code ISO (palmarès), quand la carte le connaît.
var nomsPays = func() map[string]string {
	m := make(map[string]string, len(paysCarte))
	for _, p := range paysCarte {
		m[p.Code] = p.Nom
	}
	return m
}()

// carteFlux : pays de la carte avec leur niveau (log du nombre de flux
// rapporté au pays le plus touché).
func carteFlux(parPays map[string]int) []paysVue {
	max := 0
	for _, n := range parPays {
		if n > max {
			max = n
		}
	}
	out := make([]paysVue, 0, len(paysCarte))
	for _, p := range paysCarte {
		v := paysVue{Code: p.Code, Nom: p.Nom, D: p.D, N: parPays[p.Code]}
		if v.N > 0 && max > 0 {
			v.Niveau = 1 + int(4*math.Log1p(float64(v.N))/math.Log1p(float64(max)))
			if v.Niveau > 5 {
				v.Niveau = 5
			}
		}
		out = append(out, v)
	}
	return out
}
