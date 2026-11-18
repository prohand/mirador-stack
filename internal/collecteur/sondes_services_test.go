// SPDX-License-Identifier: GPL-3.0-or-later

package collecteur

import (
	"strings"
	"testing"
	"time"

	essais "mirador-stack/internal/sondes"
)

// Lot d'une sonde de service (SPEC § 48) : job, étiquettes, raison de l'échec.
func TestLotSonde(t *testing.T) {
	s := essais.Sonde{Nom: "HA", Type: essais.HTTP, Cible: "https://ha.lan:8123/", Depuis: "lan"}
	b := LotSonde(s, essais.Resultat{OK: false, Duree: time.Second, Code: 502, Erreur: "code 502"}, time.Unix(1_800_000_000, 0))
	var sb strings.Builder
	for _, l := range b.Common {
		sb.WriteString(l.Name + `="` + l.Value + `" `)
	}
	for _, x := range b.Samples {
		sb.WriteString(x.Name + " ")
		for _, l := range x.Labels[:x.N] {
			sb.WriteString(l.Name + `="` + l.Value + `" `)
		}
	}
	texte := sb.String()
	for _, attendu := range []string{`job="sondes"`, `sonde="HA"`, `type="http"`, `cible="https://ha.lan:8123/"`,
		"mirador_sonde_succes", "mirador_sonde_code_http", `erreur="code 502"`} {
		if !strings.Contains(texte, attendu) {
			t.Errorf("%q absent :\n%s", attendu, texte)
		}
	}
	if strings.Contains(texte, "instance=") {
		t.Error("instance posée par le serveur (nom du collecteur), pas par le lot")
	}
}
