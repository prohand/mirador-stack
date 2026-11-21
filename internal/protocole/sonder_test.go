// SPDX-License-Identifier: GPL-3.0-or-later

package protocole

import (
	"slices"
	"strings"
	"testing"
)

func TestSonder(t *testing.T) {
	noms := []string{"Box (fibre)", "Accès L'Été", "a,b:c"}
	n, lus := LireSonder(FormerSonder(42, noms))
	if n != 42 || !slices.Equal(lus, noms) {
		t.Fatalf("aller-retour : %d %q", n, lus)
	}
	if FormerSonder(42, nil) != "" || FormerSonder(0, noms) != "" {
		t.Error("rien à demander : en-tête attendu vide")
	}
	for _, mauvais := range []string{"", "abc", "x:nom", "-1:nom", "0:nom"} {
		if n, lus := LireSonder(mauvais); n != 0 || lus != nil {
			t.Errorf("%q accepté : %d %q", mauvais, n, lus)
		}
	}
	if _, lus := LireSonder("1:" + strings.Repeat("a,", 500)); len(lus) > MaxSonder {
		t.Errorf("%d noms lus", len(lus))
	}
}
