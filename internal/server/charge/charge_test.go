// SPDX-License-Identifier: GPL-3.0-or-later

package charge

import (
	"runtime"
	"testing"
	"time"
)

func TestCompteurs(t *testing.T) {
	avant := Cumul(Logs)
	debut := time.Now().Add(-30 * time.Millisecond)
	Noter(Logs, debut)
	if d := Cumul(Logs) - avant; d < 30*time.Millisecond || d > time.Second {
		t.Errorf("logs : %s", d)
	}
	Noter(Partie(99), debut) // hors limites : ignoré
	// Du calcul et des allocations : le temps CPU et celui du ramasse-miettes avancent.
	cpu0 := CPUProcessus()
	x := 0
	for i := 0; i < 20_000_000; i++ {
		x += i % 7
	}
	for i := 0; i < 2000; i++ {
		_ = make([]byte, 64<<10)
	}
	runtime.GC()
	if CPUProcessus() <= cpu0 || x == 0 {
		t.Error("temps CPU du processus immobile")
	}
	if CPUGC() <= 0 {
		t.Error("temps du ramasse-miettes nul")
	}
	if len(Noms) != int(nbParties) || Noms[Interface] != "interface" {
		t.Error("noms des parties")
	}
}
