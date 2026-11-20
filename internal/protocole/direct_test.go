// SPDX-License-Identifier: GPL-3.0-or-later

package protocole

import (
	"math"
	"strings"
	"testing"
)

func TestValiderDirect(t *testing.T) {
	d := Direct{CPU: 40, Coeurs: 8, Processus: []ProcessusDirect{{PID: 1, Nom: "a\x1b[31mb" + strings.Repeat("é", 80), CPU: 120}}}
	if err := d.Valider(); err != nil {
		t.Fatal(err)
	}
	if n := d.Processus[0].Nom; strings.ContainsRune(n, 0x1b) || len([]rune(n)) != 64 {
		t.Errorf("nom non nettoyé : %q", n)
	}
	for _, x := range []Direct{{CPU: 101}, {CPU: math.NaN()}, {NetRecu: -1}, {MemTotale: math.Inf(1)},
		{Processus: make([]ProcessusDirect, DirectMaxProcessus+1)}, {Processus: []ProcessusDirect{{PID: 0}}}} {
		if x.Valider() == nil {
			t.Errorf("accepté : %+v", x)
		}
	}
}
