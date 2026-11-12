// SPDX-License-Identifier: GPL-3.0-or-later

package server

import (
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
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"mirador-stack/internal/server/config"
	"mirador-stack/internal/server/secrets"
	"mirador-stack/internal/server/stockage"
)

// TestMesureInterface (MESURE_INTERFACE=1) : coût de l'interface avec 50
// machines (~35 séries chacune, 6 h de données à 30 s) : temps de réponse
// par page, CPU, mémoire, et coût d'une connexion (Argon2id).
func TestMesureInterface(t *testing.T) {
	if os.Getenv("MESURE_INTERFACE") == "" {
		t.Skip("MESURE_INTERFACE non défini")
	}
	dir := t.TempDir()
	cleF := filepath.Join(dir, "cle")
	_ = os.WriteFile(cleF, []byte("AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=\n"), 0o400)
	cfg := config.Defaut()
	cfg.EcouteAgents, cfg.EcouteInterface = portLibre(t), portLibre(t)
	cfg.Donnees, cfg.FichierCle = filepath.Join(dir, "donnees"), cleF
	s, err := Demarrer(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Arreter()
	ctx, now := context.Background(), time.Now()
	for m := 0; m < 50; m++ {
		nom := fmt.Sprintf("vm-%02d", m)
		_, _ = s.db.Exec(`INSERT INTO machines(nom, statut, cree_le, vu_le) VALUES (?, 'approuvee', 1, ?)`, nom, now.Unix())
		var lot []stockage.Serie
		mk := func(n string, f func(k int) float64, etq ...string) {
			sr := stockage.Serie{Labels: []stockage.Label{{Nom: "__name__", Valeur: n}, {Nom: "instance", Valeur: nom}, {Nom: "job", Valeur: "integrations/unix"}}}
			for j := 0; j+1 < len(etq); j += 2 {
				sr.Remplacer(etq[j], etq[j+1])
			}
			for k := 0; k < 720; k++ {
				sr.Points = append(sr.Points, stockage.Point{T: now.Add(time.Duration(k-719) * 30 * time.Second).UnixMilli(), V: f(k)})
			}
			lot = append(lot, sr)
		}
		mk("node_uname_info", func(int) float64 { return 1 })
		for c := 0; c < 4; c++ {
			for _, mode := range []string{"idle", "user", "system", "iowait"} {
				mk("node_cpu_seconds_total", func(k int) float64 { return float64(k) * 7 }, "cpu", fmt.Sprint(c), "mode", mode)
			}
		}
		for _, n := range []string{"node_memory_MemAvailable_bytes", "node_memory_MemTotal_bytes", "node_load5", "node_boot_time_seconds", "node_time_seconds",
			"node_memory_SwapTotal_bytes", "node_memory_SwapFree_bytes"} {
			mk(n, func(k int) float64 { return float64(1<<30 + k) })
		}
		for _, fs := range []string{"/", "/var", "/boot"} {
			mk("node_filesystem_avail_bytes", func(int) float64 { return 10 << 30 }, "mountpoint", fs, "fstype", "ext4")
			mk("node_filesystem_size_bytes", func(int) float64 { return 40 << 30 }, "mountpoint", fs, "fstype", "ext4")
		}
		for _, d := range []string{"eth0", "eth1"} {
			mk("node_network_receive_bytes_total", func(k int) float64 { return float64(k) * 1e6 }, "device", d)
			mk("node_network_transmit_bytes_total", func(k int) float64 { return float64(k) * 1e5 }, "device", d)
		}
		if err := s.met.Ecrire(ctx, lot); err != nil {
			t.Fatal(err)
		}
	}
	_ = auth.CreerUtilisateur(ctx, s.db, "admin", auth.RoleAdmin, "une longue phrase de passe", "test", "", now)
	secret, _ := auth.NouveauSecretTOTP()
	ch, _ := secrets.Chiffrer(s.cle, "console.totp", secret)
	_ = auth.DefinirTOTP(ctx, s.db, "admin", ch, 0, "test", "", now)
	runtime.GC()
	rssAvant := rssMesure()

	n := &navigateur{t: t, base: "http://" + cfg.EcouteInterface, c: &http.Client{Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	t0 := time.Now()
	n.faire(http.MethodPost, "/connexion", url.Values{"nom": {"admin"}, "mot_de_passe": {"une longue phrase de passe"}})
	tConnexion := time.Since(t0)
	_, corps, _ := n.faire(http.MethodGet, "/connexion/code", nil)
	code, _ := auth.CodeTOTP(secret, time.Now())
	n.faire(http.MethodPost, "/connexion/code", url.Values{"csrf": {reCSRFe2e.FindStringSubmatch(corps)[1]}, "code": {code}})

	_, corps, _ = n.faire(http.MethodGet, "/tableaux/serveurs-linux?periode=6h", nil)
	courbes := regexp.MustCompile(`data-courbe="([^"]+)"`).FindAllStringSubmatch(corps, -1)
	pages := []string{"/", "/machines", "/machines/vm-07", "/tableaux/serveurs-linux", "/tableaux/serveurs-linux?periode=7d", "/alertes", "/regles", "/logs?periode=24h"}
	for _, c := range courbes[:3] {
		pages = append(pages, strings.ReplaceAll(c[1], "&amp;", "&"))
	}
	var ru0, ru1 syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &ru0)
	debut := time.Now()
	durees := map[string][]time.Duration{}
	octets := map[string]int{}
	var tasMax uint64
	const tours = 20
	for i := 0; i < tours; i++ {
		for _, p := range pages {
			x := time.Now()
			code, c, _ := n.faire(http.MethodGet, p, nil)
			if code != 200 {
				t.Fatalf("%s : %d", p, code)
			}
			durees[p] = append(durees[p], time.Since(x))
			octets[p] = len(c)
		}
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		if ms.HeapInuse > tasMax {
			tasMax = ms.HeapInuse
		}
		time.Sleep(25 * time.Millisecond) // le cache de 20 s joue comme en vrai sur une partie des tours
	}
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &ru1)
	cpu := time.Duration(ru1.Utime.Nano()+ru1.Stime.Nano()-ru0.Utime.Nano()-ru0.Stime.Nano()) * time.Nanosecond
	mur := time.Since(debut)
	var lignes []string
	for _, p := range pages {
		d := durees[p]
		sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
		nomP := p
		if len(nomP) > 60 {
			nomP = nomP[:60] + "…"
		}
		lignes = append(lignes, fmt.Sprintf("| `%s` | %v | %v | %v | %d Ko |", nomP, d[0].Round(time.Millisecond), d[len(d)/2].Round(time.Millisecond),
			d[len(d)-1].Round(time.Millisecond), octets[p]/1024))
	}
	t.Logf("\n| Page | min | médiane | max | taille |\n|---|---|---|---|---|\n%s", strings.Join(lignes, "\n"))
	t.Logf("connexion (Argon2id) : %v ; %d requêtes en %v, CPU %v (%.1f %% d'un cœur pendant l'essai)", tConnexion.Round(time.Millisecond),
		tours*len(pages), mur.Round(time.Millisecond), cpu.Round(time.Millisecond), 100*cpu.Seconds()/mur.Seconds())
	t.Logf("RSS du processus (serveur + test) : %d Mo avant, %d Mo après ; tas Go max %d Mo", rssAvant>>20, rssMesure()>>20, tasMax>>20)
}

func rssMesure() int64 {
	b, _ := os.ReadFile("/proc/self/status")
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "VmRSS:") {
			var k int64
			fmt.Sscanf(strings.TrimSpace(strings.TrimPrefix(l, "VmRSS:")), "%d", &k)
			return k << 10
		}
	}
	return 0
}
