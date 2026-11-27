// SPDX-License-Identifier: GPL-3.0-or-later

package logs

import "testing"

// Lignes typiques (journald / syslog) pour mesurer le masquage par défaut.
var lignesPerf = []string{
	"Accepted publickey for admin from 198.51.100.23 port 52344 ssh2: ED25519 SHA256:abc",
	"nginx: 10.0.0.1 - - [08/Nov/2026:10:00:00 +0100] \"GET /index.html HTTP/1.1\" 200 512 \"-\" \"Mozilla/5.0\"",
	"Started Session 12345 of User root.",
	"kernel: [UFW BLOCK] IN=eth0 OUT= MAC=52:54:00:12:34:56 SRC=203.0.113.5 DST=192.168.1.10 LEN=60 TOS=0x00 PREC=0x00 TTL=50 ID=54321 DF PROTO=TCP SPT=44321 DPT=22 WINDOW=64240",
	"2026-11-27 10:00:00.123 UTC [12345] LOG:  checkpoint complete: wrote 1234 buffers (7.5%); 0 WAL file(s) added",
	"CRON[23456]: (root) CMD (command -v debian-sa1 > /dev/null && debian-sa1 1 1)",
}

// BenchmarkMasquerDefaut : masquage par défaut sur 6 lignes courantes.
func BenchmarkMasquerDefaut(b *testing.B) {
	m, _ := NewMasqueur([]string{"mots_de_passe", "tokens", "cles_api", "autorisation", "cartes"}, nil)
	b.ReportAllocs()
	for b.Loop() {
		for _, l := range lignesPerf {
			_ = m.Masquer(l)
		}
	}
}
