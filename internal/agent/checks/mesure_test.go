// SPDX-License-Identifier: GPL-3.0-or-later

package checks

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"mirador-stack/internal/agent/config"
)

// BenchmarkPassage : coût d'un passage de 40 checks (10 http, 10 tcp, 10
// processus sur le vrai /proc, 10 fichiers), comme toutes les 60 s.
//
//	go test ./internal/agent/checks/ -bench Passage -benchmem -run '^$'
func BenchmarkPassage(b *testing.B) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }))
	defer srv.Close()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	d := b.TempDir()
	var cs []config.Check
	for i := 0; i < 10; i++ {
		n := strconv.Itoa(i)
		f := filepath.Join(d, "s"+n+".tar")
		_ = os.WriteFile(f, []byte("x"), 0o600)
		cs = append(cs,
			config.Check{Type: "http", Nom: "h" + n, Cible: srv.URL + "/" + n},
			config.Check{Type: "tcp", Nom: "t" + n, Cible: l.Addr().String()},
			config.Check{Type: "processus", Nom: "p" + n, Cible: "init" + n},
			config.Check{Type: "fichier", Nom: "f" + n, Cible: f})
	}
	m := Nouveau(cs, "/proc")
	lot := make([]int, len(cs))
	for i := range lot {
		lot[i] = i
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.passer(context.Background(), lot)
	}
}
