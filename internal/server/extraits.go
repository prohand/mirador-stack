// SPDX-License-Identifier: GPL-3.0-or-later

package server

import (
	"context"
	"time"

	"mirador-stack/internal/server/notif"
	"mirador-stack/internal/server/stockage/journaux"
)

// Extraits de logs joints aux messages d'alerte (SPEC.md § 41) : recherche
// dans le stockage des logs, avec des limites plus serrées que la page Logs
// (un message ne doit jamais attendre longtemps).
var limitesExtraits = journaux.LimitesRecherche{MaxPeriode: 25 * time.Hour, MaxLignes: notif.ExtraitLignes,
	MaxBlocs: 200, Duree: 2 * time.Second, Travailleurs: 1}

// chercheurExtraits : lignes les plus récentes d'abord, « 15:04:05 message »
// (heure locale du serveur : TZ du conteneur).
func chercheurExtraits(jx *journaux.Moteur) notif.ChercheurLogs {
	return func(ctx context.Context, r notif.RequeteExtrait, n int) ([]string, error) {
		res, err := jx.Chercher(ctx, journaux.Requete{Debut: r.Debut.UnixMilli(), Fin: r.Fin.UnixMilli(),
			Champs: r.Champs, Mots: r.Mots, Limite: n}, limitesExtraits)
		if err != nil {
			return nil, err
		}
		out := make([]string, 0, len(res.Lignes))
		for _, l := range res.Lignes {
			out = append(out, time.UnixMilli(l.T).Format("15:04:05")+" "+l.Msg)
		}
		return out, nil
	}
}
