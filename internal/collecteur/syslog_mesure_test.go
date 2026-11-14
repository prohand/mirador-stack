// SPDX-License-Identifier: GPL-3.0-or-later

package collecteur

import (
	"testing"
	"time"

	"mirador-stack/internal/agent/config"
	"mirador-stack/internal/agent/logs"
	"mirador-stack/internal/collecteur/syslog"
)

// BenchmarkLigneSyslog : coût d'une ligne reçue (lecture, masquage, mise en
// lot, compression par lots de 2 000), comme dans Syslog.Tourner.
//
//	go test ./internal/collecteur/ -bench LigneSyslog -benchmem -run '^$'
func BenchmarkLigneSyslog(b *testing.B) {
	masque, _ := logs.NewMasqueur(config.Defaut().Masquage, nil)
	lot := logs.NewLot("", "syslog", "", "", tailleLotSyslog)
	trame := []byte(`<165>1 2026-11-14T10:00:00.003Z nas.lan evntslog - ID47 [exemple@32473 iut="3"] Connexion de admin depuis 192.168.1.50 (session ouverte, durée 3 min)`)
	t := time.Now()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m, _ := syslog.Analyser(trame)
		lot.Ajouter(t, masque.Masquer(m.Texte), logs.Champs{{"host", "nas"}, {"ident", m.App}, {"level", m.Niveau}})
		if i%2000 == 1999 {
			lot.Vider()
		}
	}
}
