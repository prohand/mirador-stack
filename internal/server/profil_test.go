// SPDX-License-Identifier: GPL-3.0-or-later

package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"runtime/pprof"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mirador-stack/internal/server/alertes"
	srvconfig "mirador-stack/internal/server/config"
	"mirador-stack/internal/server/flotte"
	"mirador-stack/internal/server/stockage/metriques"
)

// TestProfilAlertes : d'où vient la RAM ajoutée par le moteur d'alertes
// (SPEC § 17.9) ? Même charge que TestChargeServeur, mais dans ce processus,
// pour prendre deux profils du tas : avec les règles, puis sans.
//
//	PROFIL=1 PROFILS=/chemin go test ./internal/server -run TestProfilAlertes -v -timeout 60m
//	go tool pprof -top -inuse_space -diff_base sans.pprof avec.pprof
func TestProfilAlertes(t *testing.T) {
	if os.Getenv("PROFIL") == "" {
		t.Skip("PROFIL=1 pour lancer le profil")
	}
	sortie := envOu("PROFILS", t.TempDir())
	rattrapage, _ := time.ParseDuration(envOu("RATTRAPAGE", "2h30m"))
	phase, _ := time.ParseDuration(envOu("PHASE", "5m"))
	debug.SetMemoryLimit(200 << 20)            // comme GOMEMLIMIT dans le conteneur
	if v := os.Getenv("HORS_ORDRE"); v != "" { // essai du réglage de la TSDB (SPEC § 17.9)
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		defer func(avant int64) { metriques.HorsOrdreMax = avant }(metriques.HorsOrdreMax)
		metriques.HorsOrdreMax = n
	}
	ctx := context.Background()
	dir := t.TempDir()
	cle := filepath.Join(dir, "cle")
	_ = os.WriteFile(cle, []byte("AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=\n"), 0o400)
	cfg := srvconfig.Defaut()
	cfg.EcouteAgents, cfg.EcouteInterface = portLibre(t), portLibre(t)
	cfg.Donnees, cfg.FichierCle = filepath.Join(dir, "d"), cle
	s, err := Demarrer(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Arreter()
	for _, f := range []string{"system.yml", "equipements.yml"} {
		var so, se bytes.Buffer
		if Commande(ctx, cfg, []string{"regles-importer", filepath.Join("alertes", "testdata", "regles", f)}, &so, &se) != 0 {
			t.Fatal(se.String())
		}
	}
	if err := s.alertes.Recharger(ctx); err != nil {
		t.Fatal(err)
	}
	const agents = 50
	client := &http.Client{Timeout: 30 * time.Second}
	sim := make([]*agentSimule, agents)
	for i := range sim {
		tok, err := flotte.TokenManuel(ctx, s.db, fmt.Sprintf("agent-%02d", i), "test", time.Now())
		if err != nil {
			t.Fatal(err)
		}
		sim[i] = nouvelAgentSimule(i, tok, cfg.EcouteAgents, client)
	}
	// Rattrapage (comme le test de charge).
	cycles := int(rattrapage / (30 * time.Second))
	origine := time.Now().Add(-rattrapage)
	var wg sync.WaitGroup
	for _, a := range sim {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for c := 0; c < cycles; c++ {
				if err := a.metriques(origine.Add(time.Duration(c) * 30 * time.Second)); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()

	// Agents en direct pendant les deux phases.
	var requetes, erreurs atomic.Int64
	fin := time.Now().Add(2 * phase)
	for _, a := range sim {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a.tourner(ctx, fin, &requetes, &erreurs)
		}()
	}
	profil := func(nom string) {
		runtime.GC()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		_, anon := lireStatus()
		f, err := os.Create(filepath.Join(sortie, nom)) // #nosec G304 -- test
		if err != nil {
			t.Fatal(err)
		}
		if err := pprof.Lookup("heap").WriteTo(f, 0); err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
		t.Logf("RÉSULTAT %s : tas utilisé %.1f Mo, RAM propre %.1f Mo, %d règles chargées, écrit %.0f Mo depuis le début", nom,
			float64(m.HeapInuse)/1e6, float64(anon)/1024, len(s.alertes.Sante()), float64(ecritProcessus())/(1<<20))
	}
	time.Sleep(phase)
	profil("avec.pprof")
	// Sans règles : toutes supprimées, moteur rechargé.
	rs, err := alertes.ListerRegles(ctx, s.db)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rs {
		if err := alertes.Supprimer(ctx, s.db, r.Nom, "test", time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.alertes.Recharger(ctx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(phase)
	profil("sans.pprof")
	wg.Wait()
	t.Logf("RÉSULTAT : %d requêtes, %d erreurs ; profils dans %s", requetes.Load(), erreurs.Load(), sortie)
}

// lireStatus : RSS et RSS anonyme (Ko) du processus.
func lireStatus() (rss, anon int64) {
	b, _ := os.ReadFile("/proc/self/status")
	for _, l := range bytes.Split(b, []byte("\n")) {
		var v int64
		if _, err := fmt.Sscanf(string(l), "VmRSS: %d kB", &v); err == nil {
			rss = v
		}
		if _, err := fmt.Sscanf(string(l), "RssAnon: %d kB", &v); err == nil {
			anon = v
		}
	}
	return rss, anon
}

// ecritProcessus : octets écrits sur disque par le processus (/proc/self/io).
func ecritProcessus() int64 {
	b, _ := os.ReadFile("/proc/self/io")
	var v int64
	for _, l := range bytes.Split(b, []byte("\n")) {
		if _, err := fmt.Sscanf(string(l), "write_bytes: %d", &v); err == nil {
			return v
		}
	}
	return 0
}
