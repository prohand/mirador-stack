// SPDX-License-Identifier: GPL-3.0-or-later

package flotte

import (
	"testing"
	"time"

	"mirador-stack/internal/protocole"
)

// Revue du 27/11 (corrigé) : machine approuvée après l'expiration du jeton (déjà utilisé) : le
// token n'est jamais remis, l'agent reçoit « jeton invalide ».
func TestRevueApprobationApresExpiration(t *testing.T) {
	b := nouveauBanc(t)
	// Jeton créé 23 h 59 avant t0, valable 24 h : demande faite à t0 - 1 min.
	jeton, err := CreerJeton(ctx, b.db, 24*time.Hour, "admin", t0.Add(-24*time.Hour+2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if rep := b.enroler(t, jeton, idA, "pc-tard", "198.51.100.4"); rep.Statut != protocole.EnAttente {
		t.Fatalf("1re demande : %+v", rep)
	}
	// L'admin approuve le lendemain ; l'horloge du banc est fixe : on
	// simule l'expiration en reculant expire_le (même effet).
	if _, err := b.db.Exec(`UPDATE jetons_enrolement SET expire_le = ?`, t0.Add(-time.Minute).Unix()); err != nil {
		t.Fatal(err)
	}
	if err := Approuver(ctx, b.db, "pc-tard", true, "admin", t0); err != nil {
		t.Fatal(err)
	}
	rep := b.enroler(t, jeton, idA, "pc-tard", "198.51.100.4")
	if rep.Statut != protocole.Approuve {
		t.Fatalf("machine approuvée mais token refusé : %+v", rep)
	}
}
