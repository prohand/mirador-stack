// SPDX-License-Identifier: GPL-3.0-or-later

package dockerlogs

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	idWeb  = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	idTerm = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// trame construit une trame du format multiplexé de Docker.
func trame(flux byte, texte string) []byte {
	var b bytes.Buffer
	b.Write([]byte{flux, 0, 0, 0})
	_ = binary.Write(&b, binary.BigEndian, uint32(len(texte)))
	b.WriteString(texte)
	return b.Bytes()
}

// fauxDocker : API minimale. Le flux de « web » envoie 2 lignes puis se coupe ;
// à la reconnexion il renvoie la 2e ligne (déjà vue) et une 3e.
type fauxDocker struct {
	mu      sync.Mutex
	since   []string
	appels  int
	methode []string
}

func (f *fauxDocker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.methode = append(f.methode, r.Method)
	f.mu.Unlock()
	switch {
	case r.URL.Path == "/containers/json":
		_, _ = w.Write([]byte(`[{"Id":"` + idWeb + `","Names":["/web"]},{"Id":"` + idTerm + `","Names":["/console"]},{"Id":"court","Names":["/invalide"]}]`))
	case r.URL.Path == "/containers/"+idWeb+"/json":
		_, _ = w.Write([]byte(`{"Config":{"Tty":false}}`))
	case r.URL.Path == "/containers/"+idTerm+"/json":
		_, _ = w.Write([]byte(`{"Config":{"Tty":true}}`))
	case r.URL.Path == "/containers/"+idWeb+"/logs":
		f.mu.Lock()
		f.since = append(f.since, r.URL.Query().Get("since"))
		f.appels++
		n := f.appels
		f.mu.Unlock()
		if n == 1 {
			_, _ = w.Write(trame(1, "2099-01-01T10:00:00.000000001Z GET / 200\n"))
			_, _ = w.Write(trame(2, "2099-01-01T10:00:01.000000000Z panic: plus de mémoire\n"))
			return // coupure du flux
		}
		_, _ = w.Write(trame(2, "2099-01-01T10:00:01.000000000Z panic: plus de mémoire\n")) // doublon
		_, _ = w.Write(trame(1, "2099-01-01T10:00:02.000000000Z GET /suite 200\n"))
		w.(http.Flusher).Flush() // le vrai Docker envoie chaque ligne aussitôt
		<-r.Context().Done()
	case r.URL.Path == "/containers/"+idTerm+"/logs":
		_, _ = w.Write([]byte("2099-01-01T10:00:00.5Z invite > commande\r\n"))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	default:
		http.NotFound(w, r)
	}
}

type ligneRecue struct{ msg, conteneur, flux string }

func TestSuiveur(t *testing.T) {
	f := &fauxDocker{}
	srv := httptest.NewServer(f)
	defer srv.Close()

	var mu sync.Mutex
	var recues []ligneRecue
	s, err := NewSuiveur(srv.URL, func(_ time.Time, msg, c, flux string) {
		mu.Lock()
		recues = append(recues, ligneRecue{msg, c, flux})
		mu.Unlock()
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	s.periode = 50 * time.Millisecond
	s.maintenant = func() time.Time { return time.Date(2098, 1, 1, 0, 0, 0, 0, time.UTC) }

	ctx, annuler := context.WithCancel(context.Background())
	go s.Tourner(ctx)
	fin := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(recues)
		mu.Unlock()
		if n >= 4 {
			break
		}
		if time.Now().After(fin) {
			annuler()
			t.Fatalf("%d lignes reçues : %v", n, recues)
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond) // laisse passer un éventuel doublon
	annuler()

	mu.Lock()
	defer mu.Unlock()
	attendu := []ligneRecue{
		{"GET / 200", "web", "stdout"},
		{"panic: plus de mémoire", "web", "stderr"},
		{"GET /suite 200", "web", "stdout"},
		{"invite > commande", "console", "stdout"},
	}
	for _, a := range attendu {
		n := 0
		for _, r := range recues {
			if r == a {
				n++
			}
		}
		if n != 1 {
			t.Errorf("ligne %v reçue %d fois (attendu 1) — tout : %v", a, n, recues)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.since) < 2 || !strings.HasPrefix(f.since[1], "4070944801.") {
		t.Errorf("la reconnexion doit reprendre à la dernière ligne : since=%v", f.since)
	}
	for _, m := range f.methode {
		if m != http.MethodGet {
			t.Fatalf("méthode %s : seul GET est permis", m)
		}
	}
}

func TestLireMultiplexeTrameGeante(t *testing.T) {
	var b bytes.Buffer
	b.Write([]byte{1, 0, 0, 0})
	_ = binary.Write(&b, binary.BigEndian, uint32(tailleMaxTrame+1))
	b.Write(make([]byte, tailleMaxTrame+1))
	b.Write(trame(1, "2099-01-01T10:00:00Z après\n"))
	var lues []string
	err := lireMultiplexe(bufio.NewReader(&b), func(l []byte, _ string) { lues = append(lues, string(l)) })
	if err != nil || len(lues) != 1 || !strings.HasSuffix(lues[0], "après") {
		t.Errorf("trame géante mal gérée : %v %v", lues, err)
	}
}

func FuzzMultiplexe(f *testing.F) {
	f.Add(trame(1, "2099-01-01T10:00:00Z x\n"))
	f.Fuzz(func(t *testing.T, b []byte) {
		_ = lireMultiplexe(bufio.NewReader(bytes.NewReader(b)), func(l []byte, _ string) { couperHorodatage(l) })
	})
}
