// SPDX-License-Identifier: GPL-3.0-or-later

package server

import (
	"context"
	"database/sql"
	"time"
)

// purgerTables : ménage par âge des petites tables qui n'en avaient pas
// (relecture du 21/11, SPEC § 116). Une fois par jour, avec la purge de
// l'historique des alertes.
//   - hist : conservation de l'historique (alertes, envois ; 180 j par défaut) ;
//   - donnees : la plus longue conservation des métriques et des logs.
//
// Renvoie le nombre de lignes retirées.
func purgerTables(ctx context.Context, db *sql.DB, t time.Time, hist, donnees time.Duration) (int64, error) {
	// Garde-fou : une durée nulle (réglage illisible) effacerait tout.
	if hist < 24*time.Hour || donnees < 24*time.Hour {
		return 0, nil
	}
	avant := t.Add(-hist).Unix()
	// Silences, maintenances et écarts vmalert sont écrits en millisecondes
	// (comparés en secondes, ils n'étaient jamais purgés).
	avantMs := t.Add(-hist).UnixMilli()
	requetes := []struct {
		sql  string
		args []any
	}{
		// Silences finis ou annulés.
		{`DELETE FROM silences WHERE fin < ? OR (annule_le > 0 AND annule_le < ?)`, []any{avantMs, avantMs}},
		// Maintenances ponctuelles finies (les récurrentes restent).
		{`DELETE FROM maintenances WHERE recurrence = '' AND fin > 0 AND fin < ?`, []any{avantMs}},
		// Jetons d'installation expirés et jamais liés à une machine.
		{`DELETE FROM jetons_enrolement WHERE expire_le < ? AND machine_id IS NULL`, []any{avant}},
		// Tokens d'agent révoqués.
		{`DELETE FROM tokens WHERE revoque_le > 0 AND revoque_le < ?`, []any{avant}},
		// Notes d'incident (l'incident, une alerte, part avec l'historique).
		{`DELETE FROM notes_incidents WHERE le < ?`, []any{avant}},
		// Écarts avec vmalert terminés (mode ombre).
		{`DELETE FROM ecarts_vmalert WHERE fin > 0 AND fin < ?`, []any{avantMs}},
		// Historique des sauvegardes (la dernière réussie reste toujours).
		{`DELETE FROM sauvegardes WHERE statut IN ('ok', 'echec') AND fin > 0 AND fin < ?
			AND id != (SELECT coalesce(max(id), 0) FROM sauvegardes WHERE statut = 'ok')`, []any{avant}},
		// Ancien nom d'une machine renommée : oublié quand plus aucune
		// donnée ne le porte (sinon chaque graphique le cherchait encore).
		{`DELETE FROM noms_anciens WHERE jusqu_a < ?`, []any{t.Add(-donnees).Unix()}},
	}
	var total int64
	for _, r := range requetes {
		res, err := db.ExecContext(ctx, r.sql, r.args...)
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += n
	}
	return total, nil
}
