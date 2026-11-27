// SPDX-License-Identifier: GPL-3.0-or-later

package syslog

import (
	"bufio"
	"bytes"
	"testing"
)

const lignePerfFilterlog = "77,,,02f4bab031b57d1e30553ce08e0ec131,igb0,match,block,in,4,0x0,,64,12345,0,DF,6,tcp,60,45.33.32.156,192.168.1.10,51234,22,0,S,123456789,,64240,,mss;sackOK;TS;nop;wscale"

func BenchmarkLireFilterlog(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		if _, ok := LireFilterlog(lignePerfFilterlog); !ok {
			b.Fatal("non lu")
		}
	}
}

func BenchmarkAnalyserFilterlog(b *testing.B) {
	trame := []byte("<134>1 2026-11-27T10:00:00.123456+01:00 opnsense.lan filterlog 12345 - [meta sequenceId=\"1\"] " + lignePerfFilterlog)
	b.ReportAllocs()
	for b.Loop() {
		m, _ := Analyser(trame)
		_, _ = LireFilterlog(m.Texte)
	}
}

func BenchmarkLireTrame(b *testing.B) {
	un := []byte("280 <134>1 2026-11-27T10:00:00.123456+01:00 opnsense.lan filterlog 12345 - [meta sequenceId=\"1\"] " + lignePerfFilterlog)
	un = append([]byte{}, un...)
	var flux bytes.Buffer
	for i := 0; i < 1000; i++ {
		flux.WriteString("<134>Nov 27 10:00:00 filterlog[123]: " + lignePerfFilterlog + "\n")
	}
	data := flux.Bytes()
	b.ReportAllocs()
	for b.Loop() {
		r := bufio.NewReaderSize(bytes.NewReader(data), 16<<10)
		var t []byte
		for {
			tr, err := LireTrameDans(r, t)
			if err != nil {
				break
			}
			t = tr[:0]
		}
	}
}
