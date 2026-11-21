// SPDX-License-Identifier: GPL-3.0-or-later

package changements

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"mirador-stack/internal/server/alertes"
	"mirador-stack/internal/server/base"
)

// Relecture du 21/11 : conteneurs jetables (nom aléatoire, une heure chacun) :
// leurs clés « ct| » sont oubliées (mémoire et table changements_etat).
func TestConteneursEphemeresOublies(t *testing.T) {
	ctx := context.Background()
	d := t.TempDir()
	db, err := base.Ouvrir(filepath.Join(d, "mirador.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := base.Migrer(ctx, db, filepath.Join(d, "copies")); err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 11, 18, 3, 0, 0, 0, time.UTC)
	l := fauxLecteur{eBoot: {ech(float64(t0.Add(-48*time.Hour).Unix()), "instance", "pve1")}}
	det := &Detecteur{DB: db, Lecteur: l}
	const heures = 60 * 24 // 60 jours, un conteneur jetable par heure
	for h := 0; h < heures; h++ {
		tt := t0.Add(time.Duration(h) * time.Hour)
		nom := fmt.Sprintf("job-run-%06d", h)
		l[eDebut] = []alertes.Echantillon{ech(float64(tt.Unix()), "instance", "pve1", "name", nom)}
		if err := det.Tour(ctx, tt); err != nil {
			t.Fatal(err)
		}
	}
	var lignes int
	_ = db.QueryRow(`SELECT COUNT(*) FROM changements_etat WHERE cle LIKE 'ct|%'`).Scan(&lignes)
	if len(det.etat) > 100 || lignes > 100 {
		t.Errorf("%d clés en mémoire, %d en base, pour des conteneurs disparus", len(det.etat), lignes)
	}
}
