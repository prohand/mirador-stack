// SPDX-License-Identifier: GPL-3.0-or-later

package direct

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// iowait qui recule (documenté dans proc(5)) : le CPU doit rester ~90 %, pas 0.
func TestRevueIowaitRecule(t *testing.T) {
	d := t.TempDir()
	ecrire := func(l string) {
		_ = os.WriteFile(filepath.Join(d, "stat"), []byte(l), 0o644)
	}
	r := &Releveur{Proc: d, Sys: d, Passwd: "/nonexistent"}
	t0 := time.Unix(1000, 0)
	//           user nice system idle iowait irq softirq steal
	ecrire("cpu  1000 0 0 1000 500 0 0 0\ncpu0 1000 0 0 1000 500 0 0 0\n")
	r.Relever(t0)
	// +90 user, +10 idle, iowait -5 (recule)
	ecrire("cpu  1095 0 0 1000 495 0 0 0\ncpu0 1095 0 0 1000 495 0 0 0\n")
	got := r.Relever(t0.Add(time.Second))
	t.Logf("CPU = %.1f %%", got.CPU)
	if got.CPU < 50 {
		t.Fatalf("CPU à %.1f %% alors que la machine a travaillé ~95 %% du temps", got.CPU)
	}
}
