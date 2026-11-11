// SPDX-License-Identifier: GPL-3.0-or-later

package geoip

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// baseTest : 2 000 plages IPv4 de /24 (FR puis DE en alternance), un trou,
// et quelques plages IPv6.
func baseTest() []byte {
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	for i := 0; i < 2000; i++ {
		pays := "FR"
		if i%2 == 1 {
			pays = "DE"
		}
		if i == 1500 {
			continue // trou : adresses inconnues
		}
		fmt.Fprintf(zw, "10.%d.%d.0,10.%d.%d.255,%s\n", i/256, i%256, i/256, i%256, pays)
	}
	fmt.Fprintln(zw, "2001:db8::,2001:db8:0:ffff:ffff:ffff:ffff:ffff,JP")
	fmt.Fprintln(zw, "2001:db8:1::,2001:db8:1:ffff:ffff:ffff:ffff:ffff,US")
	_ = zw.Close()
	return b.Bytes()
}

func TestMiseAJourEtPays(t *testing.T) {
	demandes := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		demandes++
		if strings.Contains(r.URL.Path, "2026-11") {
			http.NotFound(w, r) // base du mois pas encore publiée : le mois précédent sert
			return
		}
		_, _ = w.Write(baseTest())
	}))
	defer srv.Close()
	b := &Base{Dossier: t.TempDir(), Journal: slog.New(slog.NewTextHandler(io.Discard, nil)), Adresse: srv.URL + "/%s.csv.gz"}
	if b.Pays(netip.MustParseAddr("10.0.0.1")) != "" {
		t.Error("pays sans base")
	}
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	if err := b.MettreAJour(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	for ip, att := range map[string]string{"10.0.0.7": "FR", "10.0.1.7": "DE", "10.5.220.9": "", "2001:db8::5": "JP",
		"2001:db8:1:2::9": "US", "::ffff:10.0.0.7": "FR", "192.0.2.1": ""} {
		if p := b.Pays(netip.MustParseAddr(ip)); p != att {
			t.Errorf("%s : %q, attendu %q", ip, p, att)
		}
	}
	// Une seconde demande le même mois ne télécharge rien.
	n := demandes
	_ = os.Chtimes(filepath.Join(b.Dossier, nomFichier), now, now)
	if err := b.MettreAJour(context.Background(), now); err != nil || demandes != n {
		t.Errorf("téléchargement inutile : %d → %d", n, demandes)
	}
	// Libérée après 10 min sans usage.
	b.Tour(context.Background(), time.Now().Add(11*time.Minute), true)
	if _, mem, _ := b.Etat(); mem {
		t.Error("table encore en mémoire")
	}
	// Case décochée : base effacée.
	b.Pays(netip.MustParseAddr("10.0.0.7"))
	b.Tour(context.Background(), time.Now(), false)
	if f, mem, _ := b.Etat(); !f.IsZero() || mem {
		t.Error("base gardée après avoir décoché la case")
	}
	if b.Pays(netip.MustParseAddr("10.0.0.7")) != "" {
		t.Error("pays encore donné")
	}
}

func TestFichierAbimeRefuse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("pas du gzip")) }))
	defer srv.Close()
	b := &Base{Dossier: t.TempDir(), Journal: slog.New(slog.NewTextHandler(io.Discard, nil)), Adresse: srv.URL + "/%s"}
	if err := b.MettreAJour(context.Background(), time.Now()); err == nil {
		t.Fatal("fichier abîmé accepté")
	}
	if _, err := os.Stat(filepath.Join(b.Dossier, nomFichier)); err == nil {
		t.Error("fichier abîmé gardé")
	}
}

// TestMesureVraieBase : GEOIP_FICHIER=chemin de la vraie base (mesure de la mémoire).
func TestMesureVraieBase(t *testing.T) {
	f := os.Getenv("GEOIP_FICHIER")
	if f == "" {
		t.Skip("GEOIP_FICHIER non défini")
	}
	runtime.GC()
	var m0, m1 runtime.MemStats
	runtime.ReadMemStats(&m0)
	t0 := time.Now()
	tb, err := lire(f)
	if err != nil {
		t.Fatal(err)
	}
	d := time.Since(t0)
	runtime.GC()
	runtime.ReadMemStats(&m1)
	b := &Base{table: tb, vueLe: time.Now()}
	t.Logf("%d plages IPv4, %d IPv6, %d pays ; chargement %v ; mémoire gardée %.1f Mo ; 8.8.8.8 → %s, 2a01:cb00:: → %s",
		len(tb.v4), len(tb.v6), len(tb.codes), d.Round(time.Millisecond), float64(m1.HeapInuse-m0.HeapInuse)/(1<<20),
		b.Pays(netip.MustParseAddr("8.8.8.8")), b.Pays(netip.MustParseAddr("2a01:cb00::1")))
	runtime.KeepAlive(tb)
}

func TestPremierEssaiDiffere(t *testing.T) {
	appels := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { appels++; http.NotFound(w, nil) }))
	defer srv.Close()
	b := &Base{Dossier: t.TempDir(), Journal: slog.New(slog.NewTextHandler(io.Discard, nil)), Adresse: srv.URL + "/%s"}
	t0 := time.Now()
	b.Tour(context.Background(), t0, true)
	b.Tour(context.Background(), t0.Add(4*time.Minute), true)
	if appels != 0 {
		t.Fatalf("téléchargement avant 5 min : %d", appels)
	}
	b.Tour(context.Background(), t0.Add(6*time.Minute), true)
	if appels == 0 {
		t.Error("pas d'essai après 5 min")
	}
}

func TestDemandeEtReessai(t *testing.T) {
	appels := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { appels++; http.NotFound(w, nil) }))
	defer srv.Close()
	b := &Base{Dossier: t.TempDir(), Journal: slog.New(slog.NewTextHandler(io.Discard, nil)), Adresse: srv.URL + "/%s"}
	t0 := time.Now()
	b.Tour(context.Background(), t0, false)
	b.Demander()
	b.Tour(context.Background(), t0.Add(time.Minute), false) // demandé : même case décochée
	if appels == 0 {
		t.Fatal("la demande n'a pas déclenché de téléchargement")
	}
	if _, _, e := b.Etat(); e == "" {
		t.Error("erreur non affichée")
	}
	n := appels
	b.Tour(context.Background(), t0.Add(20*time.Minute), true)
	if appels != n {
		t.Error("nouvel essai trop tôt après un échec")
	}
	b.Tour(context.Background(), t0.Add(32*time.Minute), true)
	if appels == n {
		t.Error("pas de nouvel essai 30 min après un échec")
	}
}
