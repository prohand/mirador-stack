// SPDX-License-Identifier: GPL-3.0-or-later

package flotte

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"mirador-stack/internal/server/base"
)

// Déploiement par vagues (SPEC § 141) : essai, site, tout le parc ; pause
// sur échec ; versions propres retirées à la fin.
func TestDeploiementParVagues(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := base.Ouvrir(filepath.Join(dir, "mirador.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := base.Migrer(ctx, db, dir); err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	for _, q := range []string{
		`INSERT INTO versions_binaires(produit, version, architecture, sha256, taille, signature, publiee_le) VALUES ('mirador-agent', '0.7.0', 'amd64', 'x', 1, 'x', 1)`,
		`INSERT INTO machines(id, nom, statut, site, version_agent, cree_le, vu_le, battement_le) VALUES (1, 'essai', 'approuvee', 'Paris', '0.6.0', 0, 0, ` + strconv.FormatInt(t0.Unix(), 10) + `)`,
		`INSERT INTO machines(id, nom, statut, site, version_agent, cree_le, vu_le, battement_le) VALUES (2, 'p2', 'approuvee', 'Paris', '0.6.0', 0, 0, ` + strconv.FormatInt(t0.Unix(), 10) + `)`,
		`INSERT INTO machines(id, nom, statut, site, version_agent, cree_le, vu_le, battement_le) VALUES (3, 'l1', 'approuvee', 'Lyon', '0.6.0', 0, 0, ` + strconv.FormatInt(t0.Unix(), 10) + `)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	cible := func(id int) string {
		var v string
		_ = db.QueryRow(`SELECT version_cible FROM machines WHERE id = ?`, id).Scan(&v)
		return v
	}
	maj := func(ids ...int) {
		for _, id := range ids {
			_, _ = db.Exec(`UPDATE machines SET version_agent = '0.7.0' WHERE id = ?`, id)
		}
	}
	if err := LancerDeploiement(ctx, db, "0.7.0", []Vague{{Genre: "machine", Valeur: "essai"}, {Genre: "site", Valeur: "Paris"}}, "admin", t0); err != nil {
		t.Fatal(err)
	}
	if cible(1) != "0.7.0" || cible(2) != "" {
		t.Fatalf("vague 1 : cibles %q %q", cible(1), cible(2))
	}
	maj(1)
	AvancerDeploiement(ctx, db, t0.Add(time.Minute))                          // saine
	if m, _ := AvancerDeploiement(ctx, db, t0.Add(20*time.Minute)); m != "" { // pas encore 30 min
		t.Fatalf("trop tôt : %q", m)
	}
	if m, _ := AvancerDeploiement(ctx, db, t0.Add(32*time.Minute)); !strings.Contains(m, "Site Paris") || cible(2) != "0.7.0" || cible(3) != "" {
		t.Fatalf("vague 2 : %q, cibles %q %q", m, cible(2), cible(3))
	}
	// Échec sur p2 : pause.
	_, _ = db.Exec(`UPDATE machines SET maj_echec = 'signature refusée' WHERE id = 2`)
	if m, _ := AvancerDeploiement(ctx, db, t0.Add(33*time.Minute)); !strings.Contains(m, "pause") {
		t.Fatalf("échec : %q", m)
	}
	d, _ := LireDeploiement(ctx, db)
	if !d.Pause {
		t.Fatal("pas en pause")
	}
	// Corrigé, repris : p2 à jour, 30 min après → tout le parc.
	_, _ = db.Exec(`UPDATE machines SET maj_echec = '' WHERE id = 2`)
	maj(2)
	if err := PauseDeploiement(ctx, db, false, "admin", t0.Add(40*time.Minute)); err != nil {
		t.Fatal(err)
	}
	AvancerDeploiement(ctx, db, t0.Add(41*time.Minute))
	if m, _ := AvancerDeploiement(ctx, db, t0.Add(72*time.Minute)); !strings.Contains(m, "Tout le parc") {
		t.Fatalf("dernière vague : %q", m)
	}
	if g, _ := CibleGlobale(ctx, db); g != "0.7.0" || cible(1) != "" || cible(2) != "" {
		t.Errorf("fin : parc %q, propres %q %q", g, cible(1), cible(2))
	}
	maj(3)
	AvancerDeploiement(ctx, db, t0.Add(73*time.Minute))
	if m, _ := AvancerDeploiement(ctx, db, t0.Add(104*time.Minute)); !strings.Contains(m, "terminé") {
		t.Errorf("fin : %q", m)
	}
}
