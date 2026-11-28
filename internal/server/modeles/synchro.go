// SPDX-License-Identifier: GPL-3.0-or-later

package modeles

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"mirador-stack/internal/server/alertes"
)

// Synchroniser : les règles des modèles sont gérées par les modèles seuls
// (SPEC § 27, choix du 15/11). À chaque démarrage, les règles des modèles
// livrés sont remises à jour (une amélioration d'une version arrive donc
// seule) ; le choix « active / désactivée » de chaque règle est gardé.
// Les règles d'un modèle livré qui n'existent plus, ou d'un modèle qui
// n'existe plus, sont retirées. Les modèles personnels ont leurs règles
// posées à l'enregistrement ; seule leur expression est remise à jour ici
// si la forme du seuil a changé (voir plus bas).
func Synchroniser(ctx context.Context, db *sql.DB, qui string, maintenant time.Time) (int, error) {
	existantes, err := alertes.ListerRegles(ctx, db)
	if err != nil {
		return 0, err
	}
	parNom := map[string]alertes.Regle{}
	for _, r := range existantes {
		parNom[r.Nom] = r
	}
	voulues := map[string]bool{}
	var aJour []alertes.Regle
	for _, m := range Fournis() {
		for _, rm := range m.Regles {
			r := m.RegleAlerte(rm)
			voulues[r.Nom] = true
			if avant, ok := parNom[r.Nom]; ok {
				if avant.Origine != r.Origine {
					continue // nom pris par une autre règle : la sienne est gardée
				}
				r.Active = avant.Active
			}
			aJour = append(aJour, r)
		}
	}
	// Modèles personnels : leurs règles sont posées à l'enregistrement, mais
	// la forme du seuil ($p) peut changer d'une version à l'autre (revue du
	// 28/11 : « max by (instance) » autour du seuil). Seule l'expression est
	// remise à jour, et seulement si elle a changé (rien d'autre n'est touché).
	for _, m := range Tous() {
		if !m.Perso {
			continue
		}
		for _, rm := range m.Regles {
			r := m.RegleAlerte(rm)
			if avant, ok := parNom[r.Nom]; ok && avant.Origine == r.Origine && avant.Expr != r.Expr {
				avant.Expr = r.Expr
				aJour = append(aJour, avant)
			}
		}
	}
	n, err := alertes.Enregistrer(ctx, db, aJour, qui, maintenant)
	if err != nil {
		return n, err
	}
	for _, r := range existantes {
		modele, ok := strings.CutPrefix(r.Origine, "modele:")
		if !ok || voulues[r.Nom] {
			continue
		}
		if m := Trouver(modele); m != nil && m.Perso {
			continue // modèle personnel : ses règles suivent son enregistrement
		}
		if err := alertes.Supprimer(ctx, db, r.Nom, qui, maintenant); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
