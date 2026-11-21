// SPDX-License-Identifier: GPL-3.0-or-later

package collecteur

import "testing"

func TestDeclencheurs(t *testing.T) {
	var vide *Declencheurs
	if vide.Canal("x") != nil {
		t.Fatal("Declencheurs nil : canal attendu nil")
	}
	d := &Declencheurs{}
	ch := d.Canal("Box")
	d.Declencher([]string{"Inconnue", "Box", "Box"}) // inconnue ignorée, doublon sans blocage
	select {
	case <-ch:
	default:
		t.Fatal("sonde connue non déclenchée")
	}
	select {
	case <-ch:
		t.Fatal("un seul signal attendu")
	default:
	}
}
