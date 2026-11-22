// SPDX-License-Identifier: GPL-3.0-or-later

package collecteur

import (
	"testing"

	"mirador-stack/internal/agent/config"
)

// Retour du 22/11 : la tuile du collecteur n'avait pas les jauges des
// machines à agent. Il mesure aussi le CPU, la mémoire et les disques de
// sa machine (mêmes séries node_* que l'agent).
func TestCollecteursMachine(t *testing.T) {
	noms := map[string]bool{}
	for _, c := range Collecteurs(config.Agent{RacineProc: "/proc"}) {
		noms[c.Nom()] = true
	}
	for _, n := range []string{"collecteur", "adresses", "cpu", "memoire", "fs"} {
		if !noms[n] {
			t.Errorf("collecteur %q absent", n)
		}
	}
}
