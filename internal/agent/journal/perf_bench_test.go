// SPDX-License-Identifier: GPL-3.0-or-later

package journal

import (
	"bufio"
	"bytes"
	"testing"
)

const lignePerf = `{"__CURSOR":"s=8a6f3c2b1d0e4f5a9b8c7d6e5f4a3b2c;i=1a2b3c;b=0f1e2d3c4b5a69788796a5b4c3d2e1f0;m=12345678;t=5f6e7d8c9bab;x=1234567890abcdef","__REALTIME_TIMESTAMP":"1764237600123456","__MONOTONIC_TIMESTAMP":"12345678","_BOOT_ID":"0f1e2d3c4b5a69788796a5b4c3d2e1f0","MESSAGE":"Accepted publickey for admin from 198.51.100.23 port 52344 ssh2: ED25519 SHA256:abcdefghijklmnop","PRIORITY":"6","_SYSTEMD_UNIT":"ssh.service","SYSLOG_IDENTIFIER":"sshd","_TRANSPORT":"syslog"}`

// BenchmarkAnalyserJournal : une ligne JSON de journalctl.
func BenchmarkAnalyserJournal(b *testing.B) {
	var e Entree
	l := []byte(lignePerf)
	b.ReportAllocs()
	for b.Loop() {
		if !analyser(l, &e) {
			b.Fatal("illisible")
		}
	}
}

// BenchmarkLireLigneJournal : 1 000 lignes lues avec le tampon réutilisé.
func BenchmarkLireLigneJournal(b *testing.B) {
	var flux bytes.Buffer
	for i := 0; i < 1000; i++ {
		flux.WriteString(lignePerf + "\n")
	}
	data := flux.Bytes()
	b.ReportAllocs()
	for b.Loop() {
		r := bufio.NewReaderSize(bytes.NewReader(data), 64<<10)
		var t []byte
		for {
			l, _, _, err := lireLigneDans(r, t[:0], nil)
			if err != nil {
				break
			}
			t = l
		}
	}
}
