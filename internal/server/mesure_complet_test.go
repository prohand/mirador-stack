// SPDX-License-Identifier: GPL-3.0-or-later

package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"mirador-stack/internal/server/auth"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"mirador-stack/internal/agent/logs"
	"mirador-stack/internal/agent/metrics"
	"mirador-stack/internal/agent/remotewrite"
	"mirador-stack/internal/protocole"
	"mirador-stack/internal/server/config"
	"mirador-stack/internal/server/ingestion"
	"mirador-stack/internal/server/secrets"
	"mirador-stack/internal/server/stockage"
)

// TestMesureComplete (SPEC § 18.7, « test de charge complet ») : le serveur
// reçoit la charge visée (50 agents × ~640 séries / 30 s + logs du parc)
// PENDANT qu'un navigateur se sert de l'interface (vue d'ensemble rafraîchie,
// tableaux et leurs courbes, aide aux requêtes PromQL). Réglages mémoire de
// la production : GOMEMLIMIT 200 Mio, GOGC 50. Objectif : RSS < 256 Mio.
//
//	MESURE_COMPLETE=1 DUREE=5m go test ./internal/server -run TestMesureComplete -v -timeout 30m
func TestMesureComplete(t *testing.T) {
	if os.Getenv("MESURE_COMPLETE") == "" {
		t.Skip("MESURE_COMPLETE=1 pour lancer la mesure")
	}
	duree, _ := time.ParseDuration(os.Getenv("DUREE"))
	if duree == 0 {
		duree = 5 * time.Minute
	}
	debug.SetMemoryLimit(200 << 20)
	debug.SetGCPercent(50)

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
	ctx, now := context.Background(), time.Now()

	// 50 machines : 6 h d'historique « réel » (noms de mirador-agent) pour
	// que les tableaux aient du travail, puis la charge en direct.
	const agents = 50
	for i := 1; i <= agents; i++ {
		nom := fmt.Sprintf("machine%02d", i)
		e := ingestion.Empreinte(fmt.Sprintf("token-de-mesure-%04d-abcdefgh", i))
		_, _ = s.db.Exec(`INSERT INTO machines(id, nom, statut, cree_le, vu_le, site, env) VALUES (?, ?, 'approuvee', 1, ?, 'maison', 'vm')`, i, nom, now.Unix())
		_, _ = s.db.Exec(`INSERT INTO tokens(machine_id, empreinte, cree_le) VALUES (?, ?, 1)`, i, e[:])
		var lot []stockage.Serie
		mk := func(n string, f func(k int) float64, etq ...string) {
			sr := stockage.Serie{Labels: []stockage.Label{{Nom: "__name__", Valeur: n}, {Nom: "instance", Valeur: nom}, {Nom: "job", Valeur: "node"},
				{Nom: "site", Valeur: "maison"}, {Nom: "env", Valeur: "vm"}}}
			for j := 0; j+1 < len(etq); j += 2 {
				sr.Remplacer(etq[j], etq[j+1])
			}
			for k := 0; k < 720; k++ {
				sr.Points = append(sr.Points, stockage.Point{T: now.Add(time.Duration(k-720) * 30 * time.Second).UnixMilli(), V: f(k)})
			}
			lot = append(lot, sr)
		}
		mk("node_uname_info", func(int) float64 { return 1 })
		for c := 0; c < 4; c++ {
			for _, mode := range []string{"idle", "user", "system", "iowait"} {
				mk("node_cpu_seconds_total", func(k int) float64 { return float64(k) * 7 }, "cpu", fmt.Sprint(c), "mode", mode)
			}
		}
		for _, n := range []string{"node_memory_MemAvailable_bytes", "node_memory_MemTotal_bytes", "node_load5", "node_boot_time_seconds",
			"node_time_seconds", "node_hwmon_temp_celsius"} {
			mk(n, func(k int) float64 { return float64(1<<20 + k) })
		}
		for _, fs := range []string{"/", "/var"} {
			mk("node_filesystem_avail_bytes", func(int) float64 { return 10 << 30 }, "mountpoint", fs, "fstype", "ext4")
			mk("node_filesystem_size_bytes", func(int) float64 { return 40 << 30 }, "mountpoint", fs, "fstype", "ext4")
		}
		mk("node_network_receive_bytes_total", func(k int) float64 { return float64(k) * 1e6 }, "device", "eth0")
		if err := s.met.Ecrire(ctx, lot); err != nil {
			t.Fatal(err)
		}
	}
	_ = auth.CreerUtilisateur(ctx, s.db, "admin", auth.RoleAdmin, "une longue phrase de passe", "test", "", now)
	secret, _ := auth.NouveauSecretTOTP()
	ch, _ := secrets.Chiffrer(s.cle, "console.totp", secret)
	_ = auth.DefinirTOTP(ctx, s.db, "admin", ch, 0, "test", "", now)
	runtime.GC()
	// Pic remis à zéro : on mesure le fonctionnement, pas le chargement de
	// l'historique de test (écrit d'un coup, ce qui n'arrive pas en vrai).
	_ = os.WriteFile("/proc/self/clear_refs", []byte("5"), 0)
	picDepart := picRSS()

	// Charge des agents (goroutine) : 600 séries « node_metrique » + logs.
	fin := make(chan struct{})
	var attente sync.WaitGroup
	attente.Add(1)
	go func() {
		defer attente.Done()
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
		}
		var enc remotewrite.Encoder
		lots := make([]*metrics.Batch, agents+1)
		for i := 1; i <= agents; i++ {
			lots[i] = metrics.NewBatch(600, []metrics.Label{metrics.L("instance", "x"), metrics.L("job", "node")})
		}
		for cycle := 1; ; cycle++ {
			t1 := time.Now()
			for i := 1; i <= agents; i++ {
				b := lots[i]
				b.Reset(time.Now().UnixMilli())
				for k := 0; k < 600; k++ {
					b.Add(fmt.Sprintf("node_metrique_%02d", k%60), float64(cycle*k%977), metrics.L("cpu", strconv.Itoa(k/60)))
				}
				envoyer(i, protocole.CheminMetriques, "snappy", enc.Encode(nil, b))
			}
			for i := 1; i <= 5; i++ {
				lot := logs.NewLot("x", "journal", "", "", 1<<20)
				for k := 0; k < 96; k++ {
					lot.Ajouter(time.Now(), fmt.Sprintf("session opened for user u%d by (uid=0) cycle %d", k, cycle), logs.Champs{{"unit", "ssh.service"}})
				}
				corps, _ := lot.Vider()
				envoyer(i, protocole.CheminLogs, "gzip", corps)
			}
			select {
			case <-fin:
				return
			case <-time.After(time.Until(t1.Add(30 * time.Second))):
			}
		}
	}()

	// Navigateur : connexion, tableau installé, puis un « utilisateur »
	// qui ouvre une page toutes les 2 s environ et tape des requêtes.
	n := &navigateur{t: t, base: "http://" + cfg.EcouteInterface, c: &http.Client{Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	n.faire(http.MethodPost, "/connexion", url.Values{"nom": {"admin"}, "mot_de_passe": {"une longue phrase de passe"}})
	_, corps, _ := n.faire(http.MethodGet, "/connexion/code", nil)
	code, _ := auth.CodeTOTP(secret, time.Now())
	n.faire(http.MethodPost, "/connexion/code", url.Values{"csrf": {reCSRFe2e.FindStringSubmatch(corps)[1]}, "code": {code}})
	_, corps, _ = n.faire(http.MethodGet, "/tableaux", nil)
	csrf := reCSRFe2e.FindStringSubmatch(corps)[1]
	n.faire(http.MethodPost, "/tableaux", url.Values{"csrf": {csrf}, "action": {"installer"}, "nom": {"serveurs-linux"}})
	_, corps, _ = n.faire(http.MethodGet, "/tableaux/serveurs-linux?periode=6h", nil)
	courbes := regexp.MustCompile(`data-courbe="([^"]+)"`).FindAllStringSubmatch(corps, -1)

	requetes := []string{
		`100 * (1 - avg by (instance) (rate(node_cpu_seconds_total{mode="idle"}[5m])))`,
		`sum by (instance) (rate(node_network_receive_bytes_total[5m]))`,
		`max(node_hwmon_temp_celsius)`, `count by (__name__) ({__name__=~"node_metrique_0.*"})`,
	}
	var rssMax, tasMax int64
	var lignes []string
	debut := time.Now()
	var ru0 syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &ru0)
	pages := 0
	for tour := 0; time.Since(debut) < duree; tour++ {
		var chemins []string
		switch tour % 6 {
		case 0:
			chemins = []string{"/", "/fragments/accueil"}
		case 1:
			chemins = []string{"/tableaux/serveurs-linux?periode=6h"}
			for _, c := range courbes {
				chemins = append(chemins, strings.ReplaceAll(c[1], "&amp;", "&"))
			}
		case 2:
			chemins = []string{"/tableaux/serveurs-linux/panneau?id=1.0", "/tableaux/serveurs-linux/etiquettes?metrique=node_cpu_seconds_total"}
		case 3:
			chemins = []string{"/machines", "/machines/machine07", "/logs?periode=24h"}
		case 4:
			chemins = []string{"/tableaux/serveurs-linux?periode=7d"}
		case 5:
			chemins = []string{"/alertes", "/reglages/conservation"}
		}
		for _, c := range chemins {
			if code, _, _ := n.faire(http.MethodGet, c, nil); code != 200 {
				t.Errorf("%s : %d", c, code)
			}
			pages++
		}
		// Frappe d'une requête : vérification comme le ferait l'éditeur.
		n.faire(http.MethodPost, "/tableaux/serveurs-linux/verifier", url.Values{"csrf": {csrf}, "expr": {requetes[tour%len(requetes)]}})
		pages++
		if r := rssMesure(); r > rssMax {
			rssMax = r
		}
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		if int64(ms.HeapInuse) > tasMax {
			tasMax = int64(ms.HeapInuse)
		}
		if tour%30 == 0 {
			lignes = append(lignes, fmt.Sprintf("%5.0f s : RSS %d Mo, tas %d Mo", time.Since(debut).Seconds(), rssMesure()>>20, ms.HeapInuse>>20))
		}
		time.Sleep(2 * time.Second)
	}
	close(fin)
	attente.Wait()
	var ru1 syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &ru1)
	cpu := time.Duration(ru1.Utime.Nano()+ru1.Stime.Nano()-ru0.Utime.Nano()-ru0.Stime.Nano()) * time.Nanosecond
	mur := time.Since(debut)
	pic := picRSS()
	t.Logf("\n%s", strings.Join(lignes, "\n"))
	t.Logf("durée %v, %d requêtes de l'interface ; CPU %.1f %% d'un cœur (serveur + charge + navigateur)", mur.Round(time.Second), pages,
		100*cpu.Seconds()/mur.Seconds())
	t.Logf("RSS au départ %d Mo ; RSS max relevé %d Mo, pic du processus (VmHWM) %d Mo, tas Go max %d Mo (objectif < 256 Mo)",
		picDepart>>20, rssMax>>20, pic>>20, tasMax>>20)
	if pic > 256<<20 {
		t.Errorf("pic mémoire %d Mo au-dessus de 256 Mo", pic>>20)
	}
}

// picRSS : pic de mémoire résidente du processus (VmHWM).
func picRSS() int64 {
	b, _ := os.ReadFile("/proc/self/status")
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "VmHWM:") {
			var k int64
			fmt.Sscanf(strings.TrimSpace(strings.TrimPrefix(l, "VmHWM:")), "%d", &k)
			return k << 10
		}
	}
	return 0
}
