// SPDX-License-Identifier: GPL-3.0-or-later

package server

import "testing"

// Mémoire du processus (retour du 26/11) : RSS totale et mémoire propre
// lues dans /proc/self/status ; la mémoire propre n'excède pas la RSS.
func TestMemoireProcessus(t *testing.T) {
	total, anon := memoireProcessus()
	if total <= 0 || anon <= 0 || anon > total {
		t.Errorf("RSS %d, mémoire propre %d", total, anon)
	}
	// Deux lectures successives peuvent différer (la mémoire bouge) : rss()
	// doit seulement donner une valeur.
	if rss() <= 0 {
		t.Error("rss() nulle")
	}
}
