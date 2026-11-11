// SPDX-License-Identifier: GPL-3.0-or-later

package base

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func ouvrirTest(t *testing.T) (*sql.DB, string) {
	t.Helper()
	dir := t.TempDir()
	db, err := Ouvrir(filepath.Join(dir, "mirador.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db, dir
}

func TestMigrationInitiale(t *testing.T) {
	db, dir := ouvrirTest(t)
	ctx := context.Background()
	copie, err := Migrer(ctx, db, filepath.Join(dir, "sauvegardes"))
	if err != nil {
		t.Fatal(err)
	}
	if copie != "" {
		t.Error("pas de copie attendue pour une base vide")
	}
	v, _ := Version(ctx, db)
	migs, _ := lireMigrations()
	if v != len(migs) {
		t.Errorf("version %d, attendu %d", v, len(migs))
	}
	// Mode WAL et clés étrangères actifs.
	var mode string
	if err := db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil || mode != "wal" {
		t.Errorf("journal_mode = %q", mode)
	}
	var fk int
	if err := db.QueryRow(`PRAGMA foreign_keys`).Scan(&fk); err != nil || fk != 1 {
		t.Error("clés étrangères inactives")
	}
	info, _ := os.Stat(filepath.Join(dir, "mirador.db"))
	if info.Mode().Perm() != 0o600 {
		t.Errorf("droits de la base : %v", info.Mode().Perm())
	}
	// Relancer ne fait rien.
	if c, err := Migrer(ctx, db, filepath.Join(dir, "sauvegardes")); err != nil || c != "" {
		t.Errorf("seconde migration : %q %v", c, err)
	}
}

func TestCopieAvantMigration(t *testing.T) {
	db, dir := ouvrirTest(t)
	ctx := context.Background()
	migs, _ := lireMigrations()
	if len(migs) < 2 {
		t.Skip("une seule migration")
	}
	// Vraie base ancienne : migrée jusqu'à l'avant-dernière version.
	if _, err := migrerJusqua(ctx, db, dir, len(migs)-1); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO machines(nom, cree_le) VALUES ('srv-test-mirador', 1)`); err != nil {
		t.Fatal(err)
	}
	c, err := Migrer(ctx, db, filepath.Join(dir, "sauvegardes"))
	if err != nil || c == "" {
		t.Fatalf("copie avant migration absente : %q %v", c, err)
	}
	verifierCopie(t, c)
	if v, _ := Version(ctx, db); v != len(migs) {
		t.Errorf("version %d après migration, attendu %d", v, len(migs))
	}
}

func verifierCopie(t *testing.T, chemin string) {
	t.Helper()
	copie, err := sql.Open("sqlite", "file:"+chemin+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer copie.Close()
	var n int
	if err := copie.QueryRow(`SELECT count(*) FROM machines WHERE nom = 'srv-test-mirador'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("la copie ne contient pas les données : %d %v", n, err)
	}
	info, _ := os.Stat(chemin)
	if info.Mode().Perm() != 0o600 {
		t.Errorf("droits de la copie : %v", info.Mode().Perm())
	}
}

func TestBasePlusRecente(t *testing.T) {
	db, dir := ouvrirTest(t)
	ctx := context.Background()
	if _, err := Migrer(ctx, db, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO schema_version VALUES (999, 'futur.sql', 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := Migrer(ctx, db, dir); err == nil || !strings.Contains(err.Error(), "plus récente") {
		t.Errorf("une base plus récente que le binaire doit être refusée : %v", err)
	}
}

func TestCopierNomInvalide(t *testing.T) {
	db, dir := ouvrirTest(t)
	if _, err := Copier(context.Background(), db, dir, "x'; DROP TABLE machines; --"); err == nil {
		t.Error("nom dangereux accepté")
	}
}

func TestTokenUnique(t *testing.T) {
	db, dir := ouvrirTest(t)
	if _, err := Migrer(context.Background(), db, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO machines(id, nom, cree_le) VALUES (1, 'a', 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO tokens(machine_id, empreinte, cree_le) VALUES (1, x'01', 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO tokens(machine_id, empreinte, cree_le) VALUES (1, x'01', 1)`); err == nil {
		t.Error("deux tokens identiques acceptés")
	}
	// Suppression d'une machine : ses tokens partent avec (clé étrangère).
	if _, err := db.Exec(`DELETE FROM machines WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = db.QueryRow(`SELECT count(*) FROM tokens`).Scan(&n)
	if n != 0 {
		t.Error("tokens orphelins après suppression de la machine")
	}
}
