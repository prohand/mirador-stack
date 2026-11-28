// SPDX-License-Identifier: GPL-3.0-or-later

package config

import (
	"slices"
	"strings"
	"testing"

	"mirador-stack/internal/agent/logs"
)

// TestPlancherMasquageLocal (revue du 28/11) : les modèles de secrets sont
// ajoutés même si MIRADOR_MASQUAGE ne les cite pas ; seul
// MIRADOR_MASQUAGE_PLANCHER=non les rend facultatifs.
func TestPlancherMasquageLocal(t *testing.T) {
	a, err := Lire(strings.NewReader(fichierValide + "MIRADOR_MASQUAGE=cartes\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !a.MasquagePlancher || !slices.Contains(a.Masquage, "cartes") {
		t.Fatalf("lecture : %v (plancher %v)", a.Masquage, a.MasquagePlancher)
	}
	for _, n := range logs.ModelesPlancher {
		if !slices.Contains(a.Masquage, n) {
			t.Errorf("modèle %q absent : %v", n, a.Masquage)
		}
	}
	// Pas de doublon si la liste les contient déjà.
	a, err = Lire(strings.NewReader(fichierValide))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(a.Masquage, Defaut().Masquage) {
		t.Errorf("liste par défaut changée : %v", a.Masquage)
	}

	a, err = Lire(strings.NewReader(fichierValide + "MIRADOR_MASQUAGE=cartes\nMIRADOR_MASQUAGE_PLANCHER=non\n"))
	if err != nil {
		t.Fatal(err)
	}
	if a.MasquagePlancher || !slices.Equal(a.Masquage, []string{"cartes"}) {
		t.Errorf("veto local ignoré : %v (plancher %v)", a.Masquage, a.MasquagePlancher)
	}
}
