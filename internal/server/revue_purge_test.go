// SPDX-License-Identifier: GPL-3.0-or-later

package server

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"mirador-stack/internal/server/base"
	"mirador-stack/internal/server/notif"
)

// Revue du 27/11 (corrigé) : silences et maintenances sont écrits en millisecondes (notif.ms),
// purgerTables compare en secondes : jamais purgés.
func TestRevuePurgeUnitesSilences(t *testing.T) {
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
	vieux := m.Add(-300 * 24 * time.Hour)
	if _, err := notif.AjouterSilence(ctx, db, `{instance="pc1"}`, vieux, time.Hour, "x", "", vieux); err != nil {
		t.Fatal(err)
	}
	if _, err := notif.AjouterMaintenance(ctx, db, "machine:pc1", vieux, vieux.Add(time.Hour), nil, "x", "", vieux); err != nil {
		t.Fatal(err)
	}
	if _, err := purgerTables(ctx, db, m, 180*24*time.Hour, 30*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	var s, mt int
	_ = db.QueryRow(`SELECT count(*) FROM silences`).Scan(&s)
	_ = db.QueryRow(`SELECT count(*) FROM maintenances`).Scan(&mt)
	if s != 0 || mt != 0 {
		t.Errorf("silence et maintenance de 300 j toujours là : silences=%d maintenances=%d", s, mt)
	}
}
