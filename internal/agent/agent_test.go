// SPDX-License-Identifier: GPL-3.0-or-later

package agent

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/klauspost/compress/gzip"
	"github.com/klauspost/compress/snappy"

	"mirador-stack/internal/agent/config"
	"mirador-stack/internal/agent/journal"
)

func journalMuet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func cfgTest(url string) config.Agent {
	c := config.Defaut()
	c.Nom, c.Site, c.Env = "test-mirador", "bureau", "vm"
	c.URLMetriques = url
	c.Intervalle = 10 * time.Second
	c.IntervalleLent = 20 * time.Second
	// Pas de systemd ni d'apt-get dans les tests de l'agent : ils ont leurs
	// propres tests avec des sources simulées.
	c.Systemd, c.MisesAJour = false, false
	return c
}

// TestBoutEnBout : l'agent collecte le vrai /proc de la machine de test et
// envoie à un faux serveur remote write, avec les bons en-têtes.
func TestBoutEnBout(t *testing.T) {
	var mu sync.Mutex
	var recus [][]byte
	var entetes http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		corps, _ := io.ReadAll(r.Body)
		mu.Lock()
		recus = append(recus, corps)
		entetes = r.Header.Clone()
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	a := Nouveau(cfgTest(srv.URL), "jeton-de-test-0123456789", journalMuet())
	ctx, annuler := context.WithCancel(context.Background())
	defer annuler()
	go a.Envoyer(ctx)
	a.Cycle(time.Now(), true)
	a.Cycle(time.Now(), false)

	fin := time.Now().Add(3 * time.Second)
	for {
		mu.Lock()
		n := len(recus)
		mu.Unlock()
		if n == 2 {
			break
		}
		if time.Now().After(fin) {
			t.Fatalf("%d lots reçus sur 2", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, k := range []string{"Content-Encoding", "Content-Type", "X-Prometheus-Remote-Write-Version"} {
		if entetes.Get(k) == "" {
			t.Errorf("en-tête %s absent", k)
		}
	}
	brut, err := snappy.Decode(nil, recus[0])
	if err != nil || len(brut) < 1000 {
		t.Fatalf("lot illisible ou trop petit : %d octets, %v", len(brut), err)
	}
}

// BenchmarkCycle mesure un cycle complet sur la machine de test : lecture de
// /proc et /sys, remplissage du lot, encodage et compression.
func BenchmarkCycle(b *testing.B) {
	a := Nouveau(cfgTest("http://127.0.0.1:1/"), "x", journalMuet())
	var dst []byte
	// Premier cycle : dimensionne les tampons et remplit les interneurs.
	a.Remplir(time.Now(), true)
	dst = a.enc.Encode(dst, a.lot)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		a.Remplir(time.Now(), true)
		dst = a.enc.Encode(dst, a.lot)
	}
	b.ReportMetric(float64(a.lot.Len()), "points/op")
}

// TestPasDeFuiteMemoire enchaîne 3 000 cycles complets (collecte, encodage,
// envoi vers un faux serveur) et vérifie que la mémoire réellement utilisée
// par les données (après ramassage) ne grossit pas. 3 000 cycles = 25 h à 30 s.
func TestPasDeFuiteMemoire(t *testing.T) {
	if testing.Short() {
		t.Skip("test long")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	cfg := cfgTest(srv.URL)
	// /sys vide : ce test vise les fuites de l'agent, pas le matériel. Sur
	// certaines machines de CI, un capteur de /sys/class/hwmon répond très
	// lentement, et 3 300 cycles dépassaient les 10 min.
	cfg.RacineSys = t.TempDir()
	a := Nouveau(cfg, "jeton-de-test-0123456789", journalMuet())
	ctx, annuler := context.WithCancel(context.Background())
	defer annuler()
	go a.Envoyer(ctx)

	tasUtilise := func() uint64 {
		// Attend que la file soit vide pour mesurer un état comparable.
		for i := 0; i < 200; i++ {
			if lots, _, _ := a.File().Etat(); lots == 0 {
				break
			}
			time.Sleep(time.Millisecond)
		}
		runtime.GC()
		runtime.GC()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		return m.HeapAlloc
	}
	for i := 0; i < 300; i++ { // mise en régime
		a.Cycle(time.Now(), i%2 == 0)
	}
	debut := tasUtilise()
	for i := 0; i < 3000; i++ {
		a.Cycle(time.Now(), i%2 == 0)
		if i%50 == 0 {
			time.Sleep(time.Millisecond) // laisse l'expéditeur suivre
		}
	}
	fin := tasUtilise()
	t.Logf("tas utilisé : %d Ko après mise en régime, %d Ko après 3 000 cycles", debut/1024, fin/1024)
	if fin > debut+256*1024 {
		t.Errorf("la mémoire grossit : %d Ko → %d Ko", debut/1024, fin/1024)
	}
}

// TestChaineLogs : faux journalctl → traitement → lot gzip → faux VictoriaLogs.
// Vérifie le format jsonline, les paramètres d'URL et le masquage.
func TestChaineLogs(t *testing.T) {
	dir := t.TempDir()
	donnees := filepath.Join(dir, "sortie.json")
	lignes := `{"__CURSOR":"s=a;i=1","__REALTIME_TIMESTAMP":"1762596000000000","MESSAGE":"connexion password=hunter2 refusée","PRIORITY":"4","_SYSTEMD_UNIT":"app.service","SYSLOG_IDENTIFIER":"app","_TRANSPORT":"stdout"}
{"__CURSOR":"s=a;i=2","__REALTIME_TIMESTAMP":"1762596001000000","MESSAGE":"pam_unix(cron:session): session opened for user root","PRIORITY":"6","SYSLOG_IDENTIFIER":"CRON","_TRANSPORT":"syslog"}
`
	if err := os.WriteFile(donnees, []byte(lignes), 0o600); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "journalctl")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ncat "+donnees+"\nexec sleep 30\n"), 0o700); err != nil { // #nosec G306 -- script de test
		t.Fatal(err)
	}

	var mu sync.Mutex
	var corps []byte
	var requete *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gz, err := gzip.NewReader(r.Body)
		if err == nil {
			b, _ := io.ReadAll(gz)
			mu.Lock()
			corps, requete = b, r
			mu.Unlock()
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := cfgTest("http://127.0.0.1:1/")
	cfg.URLLogs = srv.URL + "/insert/jsonline"
	cfg.FichierCurseur = filepath.Join(dir, "journal.cursor")
	a := Nouveau(cfg, "jeton-de-test-0123456789", journalMuet())
	a.logs.lecteur = journal.NewLecteur(journal.Options{FichierCurseur: cfg.FichierCurseur, Commande: script, Journal: journalMuet()})

	ctx, annuler := context.WithCancel(context.Background())
	termine := make(chan struct{})
	go func() { a.TournerLogs(ctx, ctx); close(termine) }()
	// À la fin, attendre l'agent : il écrit encore sa position de lecture
	// dans le dossier temporaire, qui serait supprimé sous ses pieds.
	defer func() { annuler(); <-termine }()
	fin := time.Now().Add(10 * time.Second)
	for {
		mu.Lock()
		n := len(corps)
		mu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(fin) {
			annuler()
			t.Fatal("aucun lot de logs reçu")
		}
		time.Sleep(20 * time.Millisecond)
	}
	annuler()
	mu.Lock()
	defer mu.Unlock()
	texte := string(corps)
	if strings.Contains(texte, "hunter2") || !strings.Contains(texte, "password=***") {
		t.Errorf("masquage absent : %s", texte)
	}
	if strings.Contains(texte, "session opened") {
		t.Errorf("le bruit systemd aurait dû être jeté : %s", texte)
	}
	for _, champ := range []string{`"host":"test-mirador"`, `"job":"journal"`, `"unit":"app.service"`, `"level":"warning"`, `"ident":"app"`} {
		if !strings.Contains(texte, champ) {
			t.Errorf("champ %s absent : %s", champ, texte)
		}
	}
	q := requete.URL.Query()
	if q.Get("_stream_fields") != "host,job,site,env,unit,level,ident,container,stream" || q.Get("_msg_field") != "_msg" {
		t.Errorf("paramètres VictoriaLogs incorrects : %s", requete.URL.RawQuery)
	}
	if requete.Header.Get("Content-Encoding") != "gzip" || requete.Header.Get("Authorization") == "" {
		t.Errorf("en-têtes incorrects : %v", requete.Header)
	}
}

// Métriques désactivées (« logs seulement ») : seules celles de l'agent partent.
func TestLogsSeulement(t *testing.T) {
	cfg := cfgTest("http://127.0.0.1:1/")
	cfg.Metriques = false
	a := Nouveau(cfg, "x", journalMuet())
	a.Remplir(time.Now(), true)
	if a.lot.Len() == 0 || a.lot.Len() > 40 {
		t.Fatalf("%d points", a.lot.Len())
	}
	for _, s := range a.lot.Samples {
		if !strings.HasPrefix(s.Name, "mirador_") {
			t.Errorf("métrique de la machine envoyée : %s", s.Name)
		}
	}
	cfg.Metriques = true
	b := Nouveau(cfg, "x", journalMuet())
	b.Remplir(time.Now(), true)
	if b.lot.Len() <= a.lot.Len() {
		t.Error("métriques de la machine absentes avec Metriques = oui")
	}
}
