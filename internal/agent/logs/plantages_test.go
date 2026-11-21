// SPDX-License-Identifier: GPL-3.0-or-later

package logs

import (
	"strings"
	"testing"
	"time"

	"mirador-stack/internal/agent/metrics"
)

// Relecture du 21/11 : le compteur de plantages d'un conteneur sans ligne
// de plantage depuis 24 h (supprimé) n'est plus envoyé.
func TestPlantagesOublies(t *testing.T) {
	lot := NewLot("h", "journal", "", "", 512<<10)
	docker := NewLot("h", "docker", "", "", 512<<10)
	tr := NewTraitement(Options{Debit: 200, Rafale: 500}, lot, docker)
	maintenant := time.Now()
	tr.horloge = func() time.Time { return maintenant }
	tr.TraiterDocker(maintenant, "panic: nil map", "job-1234", "stderr")
	compter := func() int {
		b := metrics.NewBatch(16, nil)
		b.Reset(maintenant.UnixMilli())
		tr.Metriques(b)
		n := 0
		for _, s := range b.Samples {
			if strings.Contains(s.Name, "crash_lines") {
				n++
			}
		}
		return n
	}
	if compter() != 1 {
		t.Fatal("plantage non compté")
	}
	maintenant = maintenant.Add(25 * time.Hour)
	if n := compter(); n != 0 {
		t.Errorf("compteur d'un conteneur disparu encore envoyé (%d série)", n)
	}
}
