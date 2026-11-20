// SPDX-License-Identifier: GPL-3.0-or-later

package console

import "testing"

// coupeURL : points de coupure après « / . - », et tout le reste échappé
// (retour du 20/11 : l'adresse d'une sonde se coupait au milieu d'un mot).
func TestCoupeURL(t *testing.T) {
	got := string(coupeURL(`ha.lan/a<b>?x=1&y`))
	want := `ha.<wbr>lan/<wbr>a&lt;b&gt;?<wbr>x=1&amp;<wbr>y`
	if got != want {
		t.Fatalf("coupeURL = %q, attendu %q", got, want)
	}
}
