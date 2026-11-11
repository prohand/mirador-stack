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
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"mirador-stack/internal/agent/logs"
	"mirador-stack/internal/agent/metrics"
	"mirador-stack/internal/agent/remotewrite"
	"mirador-stack/internal/protocole"
	"mirador-stack/internal/server/config"
	"mirador-stack/internal/server/ingestion"
	"mirador-stack/internal/server/sauvegarde"
)

// TestMesureCharge : serveur complet (ingestion + stockage) sous la charge
// visée (50 agents × 600 séries / 30 s + logs du parc). Lancée à la main :
// MESURE=1 DUREE=3m go test ./internal/server -run TestMesureCharge -v
// debug2 : RSS en Ko.
func debug2() int64 {
	b, _ := os.ReadFile("/proc/self/status")
	for _, l := range strings.Split(string(b), "\n") {
		if f := strings.Fields(l); len(f) >= 2 && f[0] == "VmRSS:" {
			v, _ := strconv.ParseInt(f[1], 10, 64)
			return v
		}
	}
	return 0
}

func TestMesureCharge(t *testing.T) {
	if os.Getenv("MESURE") == "" {
		t.Skip("MESURE=1 pour lancer la mesure")
	}
	duree, _ := time.ParseDuration(os.Getenv("DUREE"))
	if duree == 0 {
		duree = 3 * time.Minute
	}
	debug := func() (rss, anon int64) {
		b, _ := os.ReadFile("/proc/self/status")
		for _, l := range strings.Split(string(b), "\n") {
			f := strings.Fields(l)
			if len(f) >= 2 && f[0] == "VmRSS:" {
				rss, _ = strconv.ParseInt(f[1], 10, 64)
			}
			if len(f) >= 2 && f[0] == "RssAnon:" {
				anon, _ = strconv.ParseInt(f[1], 10, 64)
			}
		}
		return
	}
	dir := t.TempDir()
	cle := filepath.Join(dir, "cle")
	_ = os.WriteFile(cle, []byte("AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=\n"), 0o400)
	cfg := config.Defaut()
	cfg.EcouteAgents, cfg.EcouteInterface = portLibre(t), portLibre(t)
	cfg.Donnees, cfg.FichierCle = filepath.Join(dir, "donnees"), cle
	s, err := Demarrer(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Arreter()
	const agents = 50
	for i := 1; i <= agents; i++ {
		e := ingestion.Empreinte(fmt.Sprintf("token-de-mesure-%04d-abcdefgh", i))
		_, _ = s.db.Exec(`INSERT INTO machines(id, nom, statut, cree_le) VALUES (?, ?, 'approuvee', 1)`, i, fmt.Sprintf("machine%02d", i))
		_, _ = s.db.Exec(`INSERT INTO tokens(machine_id, empreinte, cree_le) VALUES (?, ?, 1)`, i, e[:])
	}
	client := &http.Client{Timeout: 10 * time.Second}
	envoyer := func(i int, chemin, enc string, corps []byte) {
		req, _ := http.NewRequest(http.MethodPost, "http://"+cfg.EcouteAgents+chemin, bytes.NewReader(corps))
		req.Header.Set("Authorization", fmt.Sprintf("Bearer token-de-mesure-%04d-abcdefgh", i))
		req.Header.Set("Content-Encoding", enc)
		req.Header.Set(protocole.EnteteHeure, strconv.FormatInt(time.Now().UnixMilli(), 10))
		resp, err := client.Do(req)
		if err != nil {
			t.Error(err)
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Errorf("%s : HTTP %d", chemin, resp.StatusCode)
		}
	}
	var ru0 syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &ru0)
	debut := time.Now()
	var enc remotewrite.Encoder
	lots := make([]*metrics.Batch, agents+1)
	for i := 1; i <= agents; i++ {
		lots[i] = metrics.NewBatch(600, []metrics.Label{metrics.L("instance", "x"), metrics.L("job", "integrations/unix")})
	}
	cycle := 0
	for time.Since(debut) < duree {
		cycle++
		t1 := time.Now()
		for i := 1; i <= agents; i++ {
			b := lots[i]
			b.Reset(time.Now().UnixMilli())
			for k := 0; k < 600; k++ {
				b.Add(fmt.Sprintf("node_metrique_%02d", k%60), float64(cycle*k%977), metrics.L("cpu", strconv.Itoa(k/60)))
			}
			envoyer(i, protocole.CheminMetriques, "snappy", enc.Encode(nil, b))
		}
		// Logs : 16 lignes/s en tout, envoyées par 5 machines toutes les 30 s.
		for i := 1; i <= 5; i++ {
			lot := logs.NewLot("x", "journal", "", "", 1<<20)
			for k := 0; k < 96; k++ {
				lot.Ajouter(time.Now(), fmt.Sprintf("session opened for user u%d by (uid=0) cycle %d", k, cycle), logs.Champs{{"unit", "ssh.service"}})
			}
			corps, _ := lot.Vider()
			envoyer(i, protocole.CheminLogs, "gzip", corps)
		}
		rss, anon := debug()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		t.Logf("cycle %2d (%5.1f s) : RSS %5.1f Mo, propre %5.1f Mo, tas %5.1f Mo, envoi du cycle %v",
			cycle, time.Since(debut).Seconds(), float64(rss)/1024, float64(anon)/1024, float64(m.HeapInuse)/1e6, time.Since(t1).Round(time.Millisecond))
		time.Sleep(time.Until(t1.Add(30 * time.Second)))
	}
	var ru syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	cpu := time.Duration(ru.Utime.Nano()+ru.Stime.Nano()-ru0.Utime.Nano()-ru0.Stime.Nano()) * time.Nanosecond
	// Coût d'une sauvegarde (configuration seule, puis avec métriques et logs).
	for _, complete := range []bool{false, true} {
		id, err := sauvegarde.Demander(context.Background(), s.db, "commande", complete, complete, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		var r0 syscall.Rusage
		_ = syscall.Getrusage(syscall.RUSAGE_SELF, &r0)
		d := time.Now()
		if err := s.planif.Tour(context.Background()); err != nil {
			t.Fatal(err)
		}
		var r1 syscall.Rusage
		_ = syscall.Getrusage(syscall.RUSAGE_SELF, &r1)
		e, _ := sauvegarde.Etat(context.Background(), s.db, id)
		t.Logf("RÉSULTAT : sauvegarde (métriques et logs : %v) : %s %s, %s en %v, CPU %.1f s, RSS ensuite %.1f Mo",
			complete, e.Statut, e.Erreur, sauvegarde.FormatTaille(e.Taille), time.Since(d).Round(time.Millisecond),
			float64(r1.Utime.Nano()+r1.Stime.Nano()-r0.Utime.Nano()-r0.Stime.Nano())/1e9, float64(debug2())/1024)
	}
	brut, agr := s.met.Taille()
	t.Logf("RÉSULTAT : %d séries, CPU %.2f %% d'un cœur (générateur compris), points stockés %d, disque métriques %.1f Mo (+ agrégats %.1f Mo), logs %.2f Mo, écritures disque %d Ko",
		agents*600, cpu.Seconds()/time.Since(debut).Seconds()*100, s.met.Points.Load(), float64(brut)/1e6, float64(agr)/1e6, float64(s.jx.Taille())/1e6, ru.Oublock*512/1024)
}
