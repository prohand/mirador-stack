// SPDX-License-Identifier: GPL-3.0-or-later

package dockerlogs

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"testing"
)

// BenchmarkLireMultiplexe : 1 000 trames d'une ligne (cas courant), comme
// dans lireFlux (horodatage coupé, message gardé).
func BenchmarkLireMultiplexe(b *testing.B) {
	var flux bytes.Buffer
	l := []byte("2026-11-27T10:00:00.123456789Z 172.18.0.5 - - [27/Nov/2026:10:00:00 +0000] \"GET /api/v1/health HTTP/1.1\" 200 17 \"-\" \"kube-probe/1.29\"\n")
	for i := 0; i < 1000; i++ {
		var e [8]byte
		e[0] = 1
		binary.BigEndian.PutUint32(e[4:], uint32(len(l)))
		flux.Write(e[:])
		flux.Write(l)
	}
	data := flux.Bytes()
	var garde string
	b.ReportAllocs()
	for b.Loop() {
		_ = lireMultiplexe(bufio.NewReaderSize(bytes.NewReader(data), 16<<10), func(x []byte, _ string) {
			_, garde, _ = couperHorodatage(x)
		})
	}
	_ = garde
}
