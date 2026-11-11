// SPDX-License-Identifier: GPL-3.0-or-later

package ingestion

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/gzip"

	"mirador-stack/internal/agent/logs"
)

var reception = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

// Le décodeur relit exactement ce que l'agent produit (gzip compris).
func TestJsonlineAllerRetourAgent(t *testing.T) {
	lot := logs.NewLot("pc", "journal", "maison", "prod", 1<<20)
	t1 := time.Date(2026, 11, 9, 11, 59, 58, 123_000_000, time.UTC)
	lot.Ajouter(t1, "démarrage \"ok\"\n\tsuite", logs.Champs{{"unit", "ssh.service"}, {"level", "info"}})
	lot.Ajouter(t1, "deuxième", logs.Champs{})
	corps, n := lot.Vider()
	if n != 2 {
		t.Fatal(n)
	}
	lignes, ignorees, err := DecoderJsonline(bytes.NewReader(corps), true, LimitesLogsDefaut(), reception)
	if err != nil || ignorees != 0 || len(lignes) != 2 {
		t.Fatalf("lignes %d, ignorées %d, erreur %v", len(lignes), ignorees, err)
	}
	l := lignes[0]
	if l.T != t1.UnixMilli() || l.Msg != "démarrage \"ok\"\n\tsuite" {
		t.Errorf("ligne : %+v", l)
	}
	for k, v := range map[string]string{"host": "pc", "job": "journal", "site": "maison", "env": "prod", "unit": "ssh.service", "level": "info"} {
		if l.Champs[k] != v {
			t.Errorf("champ %s = %q, attendu %q", k, l.Champs[k], v)
		}
	}
}

func TestJsonlineLignesIgnorees(t *testing.T) {
	long := `{"_msg":"` + strings.Repeat("x", 200<<10) + `"}`
	corps := strings.Join([]string{
		`{"_msg":"bonne","n":12,"b":true,"objet":{"a":1},"tableau":[1],"_stream":"x","nom invalide":"x","vide":""}`,
		`pas du json`,
		`{"sans":"message"}`,
		long,
		``,
		`{"_msg":"deux objets"} {"_msg":"x"}`,
		`{"_msg":"heure illisible","_time":"hier"}`,
	}, "\n")
	lignes, ignorees, err := DecoderJsonline(strings.NewReader(corps), false, LimitesLogsDefaut(), reception)
	if err != nil {
		t.Fatal(err)
	}
	if len(lignes) != 2 || ignorees != 4 {
		t.Fatalf("lignes %d, ignorées %d", len(lignes), ignorees)
	}
	c := lignes[0].Champs
	if c["n"] != "12" || c["b"] != "true" || len(c) != 2 {
		t.Errorf("champs : %v", c)
	}
	if lignes[1].T != reception.UnixMilli() {
		t.Error("une heure illisible doit devenir l'heure de réception")
	}
}

func TestJsonlineMessageTronque(t *testing.T) {
	lim := LimitesLogsDefaut()
	lim.MaxMessage = 5
	lignes, _, err := DecoderJsonline(strings.NewReader(`{"_msg":"abcdé"}`), false, lim, reception)
	if err != nil || len(lignes) != 1 || lignes[0].Msg != "abcd" { // pas de demi-caractère
		t.Fatalf("%+v %v", lignes, err)
	}
}

func TestJsonlineBombeGzip(t *testing.T) {
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	ligne := []byte(`{"_msg":"` + strings.Repeat("a", 1000) + `"}` + "\n")
	for i := 0; i < 10_000; i++ { // 10 Mo décompressés, ~30 Ko compressés
		_, _ = gz.Write(ligne)
	}
	_ = gz.Close()
	_, _, err := DecoderJsonline(&b, true, LimitesLogsDefaut(), reception)
	if !errors.Is(err, ErrTropGros) {
		t.Fatalf("bombe gzip : %v", err)
	}
}

func TestJsonlineTropDeLignes(t *testing.T) {
	lim := LimitesLogsDefaut()
	lim.MaxLignes = 3
	corps := strings.Repeat(`{"_msg":"x"}`+"\n", 4)
	if _, _, err := DecoderJsonline(strings.NewReader(corps), false, lim, reception); !errors.Is(err, ErrTropGros) {
		t.Fatal(err)
	}
	if _, _, err := DecoderJsonline(strings.NewReader("pas du gzip"), true, lim, reception); !errors.Is(err, ErrFormat) {
		t.Fatal(err)
	}
}

// FuzzDecoderJsonline : jamais de panique, champs toujours conformes.
func FuzzDecoderJsonline(f *testing.F) {
	f.Add([]byte(`{"_msg":"a","_time":"2026-11-09T12:00:00Z","unit":"x"}` + "\n{}"))
	f.Add([]byte(`{"_msg":1}`))
	f.Fuzz(func(t *testing.T, corps []byte) {
		lim := LimitesLogsDefaut()
		lim.MaxLigne = 4096
		lignes, _, err := DecoderJsonline(bytes.NewReader(corps), false, lim, reception)
		if err != nil {
			return
		}
		for _, l := range lignes {
			if len(l.Champs) > lim.MaxChamps || len(l.Msg) > lim.MaxMessage {
				t.Fatalf("ligne hors limites : %+v", l)
			}
			for k, v := range l.Champs {
				if !NomChampValide(k) || strings.HasPrefix(k, "_") || v == "" || len(v) > lim.MaxValeurChamp {
					t.Fatalf("champ non conforme : %q=%q", k, v)
				}
			}
		}
	})
}

func BenchmarkDecoderJsonline(b *testing.B) {
	lot := logs.NewLot("pc", "docker", "maison", "prod", 1<<20)
	t0 := time.Now()
	for i := 0; i < 2000; i++ {
		lot.Ajouter(t0, "GET /api/v1/query?x=1 200 12ms user-agent=Mozilla/5.0 (X11; Linux x86_64) quelque chose", logs.Champs{{"container", "nginx"}, {"stream", "stdout"}})
	}
	corps, _ := lot.Vider()
	b.ReportAllocs()
	for b.Loop() {
		if _, _, err := DecoderJsonline(bytes.NewReader(corps), true, LimitesLogsDefaut(), reception); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/2000, "ns/ligne")
}
