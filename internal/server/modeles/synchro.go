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
// n'existe plus, sont retirées. Les modèles personnels ont déjà leurs
// règles à jour (enregistrées avec le modèle).
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
