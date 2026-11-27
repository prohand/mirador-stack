// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"regexp"
	"strings"
	"testing"
)

// Projection Equal Earth (SPEC § 137) : tous les points dans la zone, même
// nombre de points qu'avant, Paris à l'est de New York et au nord de Rio.
func TestProjectionCarte(t *testing.T) {
	reNombre := regexp.MustCompile(`-?\d+(\.\d+)?`)
	for i, p := range paysProjetes {
		avant, apres := reNombre.FindAllString(paysCarte[i].D, -1), reNombre.FindAllString(p.D, -1)
		if len(avant) != len(apres) || !strings.HasPrefix(p.D, "M") {
			t.Fatalf("%s : %d nombres avant, %d après", p.Code, len(avant), len(apres))
		}
	}
	for _, d := range []string{grilleCarte, bordCarte} {
		for _, n := range reNombre.FindAllString(d, -1) {
			if len(n) > 0 && (n[0] == '-') {
				t.Fatalf("coordonnée négative : %s", n)
			}
		}
	}
	x, y := projeterLonLat(0, 84)
	if y < margeHaut-1 || x < 0 || x > largeurCarte {
		t.Errorf("haut de la carte : %v %v", x, y)
	}
	if _, y := projeterLonLat(0, -58); y > hauteurCarte-margeBas+1 {
		t.Errorf("bas de la carte : %v", y)
	}
	fr, us, br := centresPays["FR"], centresPays["US"], centresPays["BR"]
	if fr[0] <= us[0] || fr[1] >= br[1] {
		t.Errorf("centres : FR %v, US %v, BR %v", fr, us, br)
	}
}
