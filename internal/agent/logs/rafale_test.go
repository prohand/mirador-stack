// SPDX-License-Identifier: GPL-3.0-or-later

package logs

import (
	"strings"
	"testing"
	"time"

	"mirador-stack/internal/agent/journal"
)

// Relecture du 21/11 : rafale de longues lignes (traces Java) dans la même
// seconde. Le lot signale qu'il est plein dès 0,5 Mio, et après Vider la
// mémoire agrandie par la rafale n'est plus gardée.
func TestLotRafale(t *testing.T) {
	lot := NewLot("h", "journal", "", "", 512<<10)
	tr := NewTraitement(Options{Debit: 200, Rafale: 500}, lot, nil)
	maintenant := time.Now()
	tr.horloge = func() time.Time { return maintenant }
	msg := strings.Repeat("a", 20<<10)
	for i := 0; i < 2000; i++ {
		tr.Traiter(&journal.Entree{Temps: maintenant, Message: msg, Priorite: 3})
	}
	select {
	case <-lot.SignalPlein():
	default:
		t.Error("lot plein non signalé")
	}
	if _, n := lot.Vider(); n == 0 {
		t.Fatal("lot vide")
	}
	lot.mu.Lock()
	defer lot.mu.Unlock()
	if lot.buf.Cap() > 1<<20 || lot.sortie.Cap() > 1<<20 {
		t.Errorf("mémoire gardée après Vider : buf %d Kio, sortie %d Kio", lot.buf.Cap()>>10, lot.sortie.Cap()>>10)
	}
}
