// SPDX-License-Identifier: GPL-3.0-or-later

package motif

import (
	"regexp"
	"strings"
	"testing"
)

// Même réponse que l'ancienne expression à répétition bornée.
func TestMemeReponse(t *testing.T) {
	ancien := regexp.MustCompile(`^[\pL\pN][\pL\pN _.:()'-]{0,47}$`)
	nouveau := Doit(`^[\pL\pN][\pL\pN _.:()'-]*$`, 1, 48)
	for _, s := range []string{"", "a", "Box internet", "é" + strings.Repeat("x", 47), "é" + strings.Repeat("x", 48),
		" début", "fin ", "nom/avec/barre", "ça marche (2)", strings.Repeat("é", 48), strings.Repeat("é", 49), "\xff"} {
		if ancien.MatchString(s) != nouveau.MatchString(s) {
			t.Errorf("%q : ancien %v, nouveau %v", s, ancien.MatchString(s), nouveau.MatchString(s))
		}
	}
}
