// SPDX-License-Identifier: GPL-3.0-or-later

package journal

import (
	"bufio"
	"encoding/json"
	"strings"
	"testing"
)

// Ligne de plus de 256 Kio : message coupé au milieu, pas perdu (revue du 27/11).
func TestLigneTropLongueCoupee(t *testing.T) {
	long := strings.Repeat(`ab"c\é`, 60<<10) // ~420 Kio une fois en JSON
	m, _ := json.Marshal(map[string]string{"__CURSOR": "s=1", "__REALTIME_TIMESTAMP": "1700000000000000",
		"MESSAGE": "DEBUT" + long + "FIN", "PRIORITY": "3", "_SYSTEMD_UNIT": "app.service"})
	// Ordre de journalctl : MESSAGE avant les autres champs demandés.
	ligne := strings.Replace(string(m), `"PRIORITY":"3",`, "", 1)
	ligne = strings.Replace(ligne, `"_SYSTEMD_UNIT":"app.service"`, `"PRIORITY":"3","_SYSTEMD_UNIT":"app.service"`, 1)
	r := bufio.NewReaderSize(strings.NewReader(ligne+"\n"+`{"__CURSOR":"s=2","__REALTIME_TIMESTAMP":"1700000000000001","MESSAGE":"suivante"}`+"\n"), 64<<10)
	for _, attendu := range []string{"DEBUT", "suivante"} {
		l, err := lireLigne(r)
		if err != nil {
			t.Fatal(err)
		}
		var e Entree
		if !analyser(l, &e) {
			t.Fatalf("ligne %q illisible (%d octets)", attendu, len(l))
		}
		if !strings.HasPrefix(e.Message, attendu) {
			t.Fatalf("message %.20q, attendu %q", e.Message, attendu)
		}
		if attendu == "DEBUT" && (e.Unite != "app.service" || e.Priorite != 3 || !strings.Contains(e.Message, "…")) {
			t.Errorf("ligne coupée : unité %q, priorité %d, marque de coupure %v", e.Unite, e.Priorite, strings.Contains(e.Message, "…"))
		}
	}
}

// Message en octets (tableau) trop long : coupé aussi.
func TestLigneTropLongueOctets(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"__CURSOR":"s=1","__REALTIME_TIMESTAMP":"1700000000000000","MESSAGE":[27`)
	for b.Len() < 400<<10 {
		b.WriteString(",65")
	}
	b.WriteString(`],"PRIORITY":"4"}`)
	l, err := lireLigne(bufio.NewReader(strings.NewReader(b.String())))
	if err != nil && err.Error() != "EOF" {
		t.Fatal(err)
	}
	var e Entree
	if !analyser(l, &e) || e.Priorite != 4 || !strings.HasPrefix(e.Message, "\x1bAAA") {
		t.Fatalf("octets : lisible=%v priorité %d", analyser(l, &e), e.Priorite)
	}
}
