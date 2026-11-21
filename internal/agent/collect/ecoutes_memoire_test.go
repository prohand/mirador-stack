// SPDX-License-Identifier: GPL-3.0-or-later

package collect

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Relecture du 21/11 : /proc/net/tcp d'un serveur très chargé (100 000
// connexions établies) lu ligne à ligne : la mémoire allouée ne suit plus
// la taille du fichier.
func TestEcoutesGrosFichier(t *testing.T) {
	proc := t.TempDir()
	if err := os.MkdirAll(filepath.Join(proc, "net"), 0o755); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	b.WriteString("  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n")
	b.WriteString("   0: 00000000:0016 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1 1 0000000000000000 100 0 0 10 0\n")
	for i := 0; i < 100000; i++ {
		fmt.Fprintf(&b, "%6d: 0101A8C0:%04X 0201A8C0:01BB 01 00000000:00000000 00:00000000 00000000  1000        0 %d 1 0000000000000000 20 4 30 10 -1\n", i+1, 10000+i%50000, i)
	}
	if err := os.WriteFile(filepath.Join(proc, "net", "tcp"), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	var avant, apres runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&avant)
	ports, err := lirePortsEcoute(proc)
	runtime.ReadMemStats(&apres)
	if err != nil || len(ports) != 1 {
		t.Fatalf("ports : %v %v", ports, err)
	}
	if alloue := apres.TotalAlloc - avant.TotalAlloc; alloue > 2<<20 {
		t.Errorf("%d Kio alloués pour un fichier de %d Kio", alloue>>10, b.Len()>>10)
	}
}
