// SPDX-License-Identifier: GPL-3.0-or-later

package server

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"mirador-stack/internal/server/base"
)

// Relecture du 21/11 : les vieilles lignes partent, les récentes, les
// maintenances récurrentes et la dernière sauvegarde réussie restent.
func TestPurgerTables(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := base.Ouvrir(filepath.Join(dir, "mirador.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := base.Migrer(ctx, db, dir); err != nil {
		t.Fatal(err)
	}
	m := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	vieux, recent := m.Add(-200*24*time.Hour).Unix(), m.Add(-time.Hour).Unix()
	// Silences et maintenances : en millisecondes, comme notif.ms (revue du 27/11).
	vieuxMs, recentMs := m.Add(-200*24*time.Hour).UnixMilli(), m.Add(-time.Hour).UnixMilli()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO machines(id, nom, statut, cree_le) VALUES (1, 'pc1', 'approuvee', 1)`, nil},
		{`INSERT INTO silences(selecteur, debut, fin, auteur, cree_le) VALUES ('a', ?, ?, 'x', 1), ('b', ?, ?, 'x', 1)`, []any{vieuxMs, vieuxMs, recentMs, m.UnixMilli() + 3600_000}},
		{`INSERT INTO maintenances(cible, debut, fin, recurrence, auteur, cree_le) VALUES ('machine:pc1', ?, ?, '', 'x', 1), ('machine:pc1', 0, 0, '{"jours":[1]}', 'x', 1)`, []any{vieuxMs, vieuxMs}},
		{`INSERT INTO tokens(machine_id, empreinte, cree_le, revoque_le) VALUES (1, x'01', 1, ?), (1, x'02', 1, 0)`, []any{vieux}},
		{`INSERT INTO notes_incidents(incident, le, qui, texte) VALUES (1, ?, 'x', 'vieux'), (2, ?, 'x', 'récent')`, []any{vieux, recent}},
		{`INSERT INTO sauvegardes(demandee_le, fin, statut, origine) VALUES (1, ?, 'ok', 'planifiee'), (2, ?, 'echec', 'planifiee')`, []any{vieux - 10, vieux}},
		{`INSERT INTO noms_anciens(machine_id, nom, jusqu_a) VALUES (1, 'ancien-pc1', ?)`, []any{vieux}},
	} {
		if _, err := db.Exec(q.sql, q.args...); err != nil {
			t.Fatal(q.sql, err)
		}
	}
	n, err := purgerTables(ctx, db, m, 180*24*time.Hour, 30*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	compte := func(table string) int {
		var c int
		_ = db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&c) // #nosec G202 -- nom de table fixe du test
		return c
	}
	if n != 6 || compte("silences") != 1 || compte("maintenances") != 1 || compte("tokens") != 1 ||
		compte("notes_incidents") != 1 || compte("sauvegardes") != 1 || compte("noms_anciens") != 0 {
		t.Errorf("%d lignes retirées ; restent : silences %d, maintenances %d, tokens %d, notes %d, sauvegardes %d, noms %d", n,
			compte("silences"), compte("maintenances"), compte("tokens"), compte("notes_incidents"), compte("sauvegardes"), compte("noms_anciens"))
	}
	if n, _ := purgerTables(ctx, db, m, 0, 0); n != 0 {
		t.Error("durée nulle : rien ne doit partir")
	}
}
