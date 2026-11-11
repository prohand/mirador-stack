// SPDX-License-Identifier: GPL-3.0-or-later

package notif

import "testing"

// FuzzCible : un sélecteur ou une cible quelconque ne plante jamais ; un
// sélecteur accepté ne vise jamais tout (au moins un filtre précis).
func FuzzCible(f *testing.F) {
	for _, s := range []string{`{instance="pc1"}`, `machine:pve1`, `groupe:vms`, `{a=~".*"}`, `{}`, `{via="collecteur",x!="y"}`} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if ms, err := LireSelecteur(s); err == nil {
			tout := true
			for _, m := range ms {
				tout = tout && m.Matches("")
			}
			if tout {
				t.Fatalf("sélecteur %q accepté alors qu'il vise tout", s)
			}
		}
		_, _ = LireCible(s)
	})
}
