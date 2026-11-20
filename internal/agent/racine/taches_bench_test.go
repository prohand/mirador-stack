// SPDX-License-Identifier: GPL-3.0-or-later

package racine

import (
	"fmt"
	"strings"
	"testing"
)

// Coût de la lecture du journal des tâches (256 Kio, le plus lu à chaque
// passage de l'assistant, toutes les 15 min).
func BenchmarkAnalyserTaches(b *testing.B) {
	var sb strings.Builder
	for i := 0; sb.Len() < finLueTaches; i++ {
		fmt.Fprintf(&sb, "UPID:pve1:%08X:0012C3D4:%08X:vzdump:%d:root@pam: %08X OK\n", i, 0x65000000+i*60, 100+i%20, 0x65000000+i*60+30)
		fmt.Fprintf(&sb, "UPID:pve1:%08X:0012C3D4:%08X:qmstart:%d:root@pam: %08X OK\n", i, 0x65000000+i*60, 100+i%20, 0x65000000+i*60+1)
	}
	donnees := []byte(sb.String())
	b.SetBytes(int64(len(donnees)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if len(analyserTaches(donnees, "pve")) == 0 {
			b.Fatal("rien lu")
		}
	}
}
