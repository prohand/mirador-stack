// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"time"

	"mirador-stack/internal/server/alertes"
	"mirador-stack/internal/server/modeles"
)

// Règles importées couvertes par un modèle (SPEC § 27, choix du 15/11 : « les
// règles sont liées aux modèles ») : une règle importée de vmalert qu'un
// modèle remplace ne sert plus si toutes les machines où ce modèle est
// détecté le portent. La page Règles les liste et propose de les retirer.

// regleCouverte : une règle importée remplacée par un ou plusieurs modèles.
type regleCouverte struct {
	Nom      string
	Modeles  []string // titres des modèles qui la remplacent
	Porteurs int      // machines qui portent un de ces modèles
	// Manque : machines ou sources où le modèle est détecté sans être
	// porté (la règle importée y sert encore).
	Manque   []string
	Couverte bool
}

// reglesCouvertes : règles importées remplacées par un modèle, et si elles
// servent encore quelque part.
func (c *Console) reglesCouvertes(ctx context.Context, t time.Time) ([]regleCouverte, error) {
	rs, err := alertes.ListerRegles(ctx, c.db)
	if err != nil {
		return nil, err
	}
	rempl := modeles.Remplacees()
	// Aucune règle importée remplacée par un modèle : rien à calculer (la
	// photo des modèles coûte 1 à 2 requêtes par machine).
	aVoir := false
	for _, r := range rs {
		if len(rempl[r.Nom]) > 0 && strings.HasPrefix(r.Origine, "vmalert:") {
			aVoir = true
			break
		}
	}
	if !aVoir {
		return nil, nil
	}
	// Une seule photo (avant : une deuxième pour les noms des machines).
	porteurs, avant, err := c.photoModeles(ctx)
	if err != nil {
		return nil, err
	}
	ms := make(map[string]int64, len(porteurs))
	for _, m := range porteurs {
		ms[m.Nom] = m.ID
	}
	det := c.detectes(ctx, t)
	var out []regleCouverte
	for _, r := range rs {
		noms := rempl[r.Nom]
		if len(noms) == 0 || !strings.HasPrefix(r.Origine, "vmalert:") {
			continue
		}
		rc := regleCouverte{Nom: r.Nom}
		porte := func(instance string) bool {
			id, ok := ms[instance]
			if !ok {
				return false
			}
			for _, n := range noms {
				if avant[id][n] != nil {
					return true
				}
			}
			return false
		}
		manque := map[string]bool{}
		for _, n := range noms {
			if m := modeles.Trouver(n); m != nil {
				rc.Modeles = append(rc.Modeles, m.Titre)
			}
			for instance := range det[n] {
				if !porte(instance) {
					manque[instance] = true
				}
			}
		}
		for _, e := range avant {
			for _, n := range noms {
				if e[n] != nil {
					rc.Porteurs++
					break
				}
			}
		}
		for i := range manque {
			rc.Manque = append(rc.Manque, i)
		}
		sort.Strings(rc.Manque)
		rc.Couverte = rc.Porteurs > 0 && len(rc.Manque) == 0
		out = append(out, rc)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Nom < out[j].Nom })
	return out, nil
}

// retirerCouvertes : supprime les règles importées entièrement couvertes
// (audit règle par règle ; l'export YAML permet de revenir en arrière).
func (c *Console) retirerCouvertes(q *req) (string, error) {
	ctx := q.r.Context()
	rc, err := c.reglesCouvertes(ctx, q.t)
	if err != nil {
		return "", err
	}
	n := 0
	for _, r := range rc {
		if !r.Couverte {
			continue
		}
		if err := alertes.Supprimer(ctx, c.db, r.Nom, q.qui(), q.t); err != nil {
			return "", err
		}
		n++
	}
	return strconv.Itoa(n) + " règle(s) importée(s) retirée(s) : les modèles les remplacent", nil
}
