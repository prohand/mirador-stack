// SPDX-License-Identifier: GPL-3.0-or-later

package veille

import "testing"

// BenchmarkLireOOM : relecture de /proc/vmstat toutes les 2 s.
func BenchmarkLireOOM(b *testing.B) {
	l := nouveauLecteurOOM("/proc")
	b.ReportAllocs()
	for b.Loop() {
		if _, ok := l.lire(); !ok {
			b.Fatal("oom_kill absent")
		}
	}
}
