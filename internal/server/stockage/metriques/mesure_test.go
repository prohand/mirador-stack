// SPDX-License-Identifier: GPL-3.0-or-later

package metriques

import (
	"os"
	"runtime"
	"strconv"
	"testing"
	"time"

	"mirador-stack/internal/server/stockage"
)

// TestMesureRattrapage : après une coupure, des agents renvoient 20 h de
// points en retard alors que d'autres envoient déjà « maintenant ».
// MESURE=1 go test ./internal/server/stockage/metriques -run TestMesureRattrapage -v
func TestMesureRattrapage(t *testing.T) {
	if os.Getenv("MESURE") == "" {
		t.Skip("MESURE=1 pour lancer la mesure")
	}
	h := nouvelleHorloge(t0.Add(20 * time.Hour))
	m, err := Ouvrir(Options{Dossier: t.TempDir(), Maintenant: h.maintenant, Retard: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Fermer()
	const series = 9000 // 10 agents en rattrapage
	// Un agent à jour : la tête de la base est à « maintenant ».
	_ = m.Ecrire(ctx, []stockage.Serie{serie("a_jour", "x", pt(20*time.Hour, 1))})
	var ms runtime.MemStats
	debut := time.Now()
	for c := 0; c < 20*120; c++ { // 20 h de lots à 30 s, du plus ancien au plus récent
		lot := make([]stockage.Serie, 0, series)
		for s := 0; s < series; s++ {
			lot = append(lot, serie("node_m_"+strconv.Itoa(s%90), "agent"+strconv.Itoa(s/900)+"-"+strconv.Itoa(s%10), pt(time.Duration(c)*30*time.Second, float64(c))))
		}
		if err := m.Ecrire(ctx, lot); err != nil {
			t.Fatal(err)
		}
		if c%480 == 479 {
			runtime.GC()
			runtime.ReadMemStats(&ms)
			t.Logf("%2d h rattrapées : tas %.0f Mo", (c+1)/120, float64(ms.HeapInuse)/1e6)
		}
	}
	t.Logf("RÉSULTAT : %d points acceptés, %d refusés, %v", m.Points.Load(), m.Rejetes.Load(), time.Since(debut).Round(time.Second))
}
