// SPDX-License-Identifier: GPL-3.0-or-later

package distant

import (
	"slices"
	"strings"
	"testing"

	"mirador-stack/internal/agent/logs"
)

// TestPlancherMasquage (revue du 28/11) : « "masquage": [] » envoyé par le
// serveur ne coupe pas le masquage des secrets ; seul le réglage local
// MIRADOR_MASQUAGE_PLANCHER=non le permet.
func TestPlancherMasquage(t *testing.T) {
	d, err := Decoder(strings.NewReader(`{"masquage":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	b := base()
	a, err := Appliquer(b, d)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range logs.ModelesPlancher {
		if !slices.Contains(a.Masquage, n) {
			t.Errorf("modèle %q retiré par le serveur : %v", n, a.Masquage)
		}
	}
	if slices.Contains(a.Masquage, "cartes") {
		t.Errorf("« cartes » (hors plancher) devrait suivre le serveur : %v", a.Masquage)
	}
	// Le masqueur construit avec cette liste masque bien un mot de passe.
	m, err := logs.NewMasqueur(a.Masquage, a.MasquagePerso)
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Masquer("password=S3cret"); got != "password=***" {
		t.Errorf("mot de passe non masqué : %q", got)
	}
	// La configuration de départ n'est pas modifiée.
	if !slices.Equal(b.Masquage, base().Masquage) {
		t.Errorf("configuration locale modifiée : %v", b.Masquage)
	}

	// Veto local : MIRADOR_MASQUAGE_PLANCHER=non → le serveur décide.
	b.MasquagePlancher = false
	a, err = Appliquer(b, d)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Masquage) != 0 {
		t.Errorf("plancher coupé localement : liste du serveur attendue, obtenu %v", a.Masquage)
	}
}
