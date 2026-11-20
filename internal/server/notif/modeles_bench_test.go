// SPDX-License-Identifier: GPL-3.0-or-later

package notif

import (
	"testing"
	"time"
)

// Coût d'un message avec modèle (SPEC § 82).
func BenchmarkTexteModele(b *testing.B) {
	m := MessageExemple(ModeleAlerte, time.Now())
	o := Mise{Fuseau: time.UTC, Lien: "https://m.fr", HTML: true, Canal: NomTelegram,
		Modeles: map[string]Modele{ModeleAlerte: ModeleDepart(ModeleAlerte)}}
	for i := 0; i < b.N; i++ {
		_ = Texte(m, o)
	}
}
