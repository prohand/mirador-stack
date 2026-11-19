// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"time"

	"mirador-stack/internal/server/alertes"
)

// Bloc « Dernières alertes » (demande du 19/11, SPEC § 72) : sur la page
// d'une machine et sur les fiches (sonde, application, conteneur), les
// 10 dernières alertes passées en alerte, avec leur retour à la normale.
// Une requête SQL sur la table des alertes (index sur l'id), rien de plus.

const nbDernieresAlertes = 10

type ligneDerniereAlerte struct {
	ID            int64
	Regle, Resume string
	Gravite       string
	Debut, Fin    time.Time
	EnCours       bool
	Duree         string
	PriseEnCharge string // « pris en charge par … »
}

type blocDernieresAlertes struct {
	Lignes   []ligneDerniereAlerte
	ToutVoir string // page Alertes filtrée sur l'élément
}

// dernieresAlertes : f dit quel élément (machine, sonde, application,
// conteneur) ; toutVoir : lien vers l'historique complet filtré.
func (c *Console) dernieresAlertes(ctx context.Context, f alertes.Filtre, toutVoir string, t time.Time) *blocDernieresAlertes {
	f.Activees, f.Limite = true, nbDernieresAlertes
	v := &blocDernieresAlertes{ToutVoir: toutVoir}
	hs, err := alertes.HistoriqueFiltre(ctx, c.db, f)
	if err != nil {
		return v
	}
	for _, a := range hs {
		l := ligneDerniereAlerte{ID: a.ID, Regle: a.Regle, Resume: a.Resume, Gravite: a.Gravite, Debut: a.ActiveLe, Fin: a.Fin}
		fin := a.Fin
		if a.Etat != alertes.Resolue || fin.IsZero() {
			l.EnCours, fin = true, t
		}
		if fin.After(l.Debut) {
			l.Duree = dureeLisible(fin.Sub(l.Debut))
		}
		if a.AcquitteePar != "" {
			l.PriseEnCharge = a.AcquitteePar
		}
		v.Lignes = append(v.Lignes, l)
	}
	return v
}
