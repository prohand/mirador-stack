// SPDX-License-Identifier: GPL-3.0-or-later

package flotte

import "testing"

// Revue du 27/11 (corrigé) : renommer une machine ne met pas à jour les dépendances (parent ou
// enfant « machine:NOM ») : la dépendance ne s'applique plus.
func TestRevueRenommerDependances(t *testing.T) {
	b := nouveauBanc(t)
	for _, n := range []string{"routeur", "pc1"} {
		if _, err := TokenManuel(ctx, b.db, n, "admin", t0); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := b.db.Exec(`INSERT INTO dependances(enfant, parent, commentaire, cree_le) VALUES ('machine:pc1', 'routeur', '', 1)`); err != nil {
		t.Fatal(err)
	}
	if err := Renommer(ctx, b.db, "routeur", "routeur-bureau", "admin", t0); err != nil {
		t.Fatal(err)
	}
	if err := Renommer(ctx, b.db, "pc1", "pc-compta", "admin", t0); err != nil {
		t.Fatal(err)
	}
	var enfant, parent string
	_ = b.db.QueryRow(`SELECT enfant, parent FROM dependances`).Scan(&enfant, &parent)
	if parent != "routeur-bureau" || enfant != "machine:pc-compta" {
		t.Errorf("dépendance restée sur les anciens noms : enfant=%s parent=%s", enfant, parent)
	}
}
