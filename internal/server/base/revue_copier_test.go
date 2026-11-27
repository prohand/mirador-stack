// SPDX-License-Identifier: GPL-3.0-or-later

package base

import (
	"context"
	"path/filepath"
	"testing"
)

// Revue du 27/11 (corrigé) : un dossier de données avec une apostrophe casse VACUUM INTO
// (copie avant migration, sauvegardes).
func TestRevueCopierApostrophe(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "l'équipe")
	db, err := Ouvrir(filepath.Join(dir, "mirador.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := Copier(ctx, db, filepath.Join(dir, "sauvegardes"), "copie.db"); err != nil {
		t.Fatalf("copie impossible : %v", err)
	}
}
