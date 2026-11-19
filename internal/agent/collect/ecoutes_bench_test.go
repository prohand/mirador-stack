// SPDX-License-Identifier: GPL-3.0-or-later

package collect

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Coût de la relecture (toutes les 5 min) : 40 ports en écoute et 2 000
// connexions établies (serveur chargé), comme sur une machine réelle.
func BenchmarkLirePortsEcoute(b *testing.B) {
	dir := b.TempDir()
	os.MkdirAll(filepath.Join(dir, "net"), 0o755)
	var sb strings.Builder
	sb.WriteString("  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n")
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&sb, "%4d: 00000000:%04X 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 %d 1 0000000000000000 100 0 0 10 0\n", i, 1000+i, i)
	}
	for i := 0; i < 2000; i++ {
		fmt.Fprintf(&sb, "%4d: 0501A8C0:01BB 0A01A8C0:%04X 01 00000000:00000000 00:00000000 00000000     0        0 %d 1 0000000000000000 100 0 0 10 0\n", i, 30000+i, i)
	}
	os.WriteFile(filepath.Join(dir, "net", "tcp"), []byte(sb.String()), 0o644)
	b.ReportAllocs()
	for b.Loop() {
		if _, err := lirePortsEcoute(dir); err != nil {
			b.Fatal(err)
		}
	}
}
