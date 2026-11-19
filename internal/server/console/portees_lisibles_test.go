// SPDX-License-Identifier: GPL-3.0-or-later

package console

import "testing"

func TestPorteesLisibles(t *testing.T) {
	noms := map[string]string{"machine:2": "pve1"}
	for p, attendu := range map[string]string{"defaut": "Par défaut", "groupe:lan": "Groupe lan", "machine:2": "Machine pve1", "machine:9": "Machine n° 9"} {
		if got := porteeLisible(p, noms); got != attendu {
			t.Errorf("porteeLisible(%q) = %q, attendu %q", p, got, attendu)
		}
	}
	if got := remplacerPortees("Modèle appliqué (machine:2 physique), machine:22, groupe:lan", noms); got != "Modèle appliqué (pve1 physique), machine:22, groupe lan" {
		t.Errorf("remplacerPortees : %q", got)
	}
}
