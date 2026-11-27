// SPDX-License-Identifier: GPL-3.0-or-later

package server

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"mirador-stack/internal/server/auth"
	"mirador-stack/internal/server/config"
	"mirador-stack/internal/server/secrets"
	"mirador-stack/internal/server/stockage"
)

// TestMesureGrandParc (MESURE_GRAND=N) : temps des pages avec N machines.
func TestMesureGrandParc(t *testing.T) {
	nb, _ := strconv.Atoi(os.Getenv("MESURE_GRAND"))
	if nb == 0 {
		t.Skip("MESURE_GRAND non défini")
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
	const pts = 240
	t0 := time.Now()
	for m := 0; m < nb; m++ {
		nom := fmt.Sprintf("vm-%04d", m)
		site := fmt.Sprintf("site-%d", m%10)
		_, _ = s.db.Exec(`INSERT INTO machines(nom, statut, cree_le, vu_le) VALUES (?, 'approuvee', 1, ?)`, nom, now.Unix())
		_, _ = s.db.Exec(`UPDATE machines SET site=? WHERE nom=?`, site, nom)
		_, _ = s.db.Exec(`INSERT INTO tokens(machine_id, empreinte, cree_le) SELECT id, ?, 1 FROM machines WHERE nom=?`, "e"+nom, nom)
		_, _ = s.db.Exec(`INSERT INTO jetons_enrolement(empreinte, expire_le, machine_id, cree_le) SELECT ?, 1, id, 1 FROM machines WHERE nom=?`, "j"+nom, nom)
		var lot []stockage.Serie
		mk := func(n string, f func(k int) float64, etq ...string) {
			sr := stockage.Serie{Labels: []stockage.Label{{Nom: "__name__", Valeur: n}, {Nom: "instance", Valeur: nom}, {Nom: "job", Valeur: "integrations/unix"}}}
			for j := 0; j+1 < len(etq); j += 2 {
				sr.Remplacer(etq[j], etq[j+1])
			}
			for k := 0; k < pts; k++ {
				sr.Points = append(sr.Points, stockage.Point{T: now.Add(time.Duration(k-pts+1) * 30 * time.Second).UnixMilli(), V: f(k)})
			}
			lot = append(lot, sr)
		}
		mk("node_uname_info", func(int) float64 { return 1 }, "nodename", nom, "release", "6.1.0")
		for c := 0; c < 2; c++ {
			for _, mode := range []string{"idle", "user", "system", "iowait"} {
				mk("node_cpu_seconds_total", func(k int) float64 { return float64(k) * 7 }, "cpu", fmt.Sprint(c), "mode", mode)
			}
		}
		for _, n := range []string{"node_memory_MemAvailable_bytes", "node_memory_MemTotal_bytes", "node_load1", "node_load5", "node_load15", "node_boot_time_seconds", "node_time_seconds",
			"node_memory_SwapTotal_bytes", "node_memory_SwapFree_bytes"} {
			mk(n, func(k int) float64 { return float64(1<<30 + k) })
		}
		for _, fs := range []string{"/", "/var"} {
			mk("node_filesystem_avail_bytes", func(int) float64 { return 10 << 30 }, "mountpoint", fs, "fstype", "ext4", "device", "/dev/sda1")
			mk("node_filesystem_size_bytes", func(int) float64 { return 40 << 30 }, "mountpoint", fs, "fstype", "ext4", "device", "/dev/sda1")
			mk("node_filesystem_files_free", func(int) float64 { return 1e6 }, "mountpoint", fs, "fstype", "ext4", "device", "/dev/sda1")
			mk("node_filesystem_files", func(int) float64 { return 2e6 }, "mountpoint", fs, "fstype", "ext4", "device", "/dev/sda1")
		}
		mk("node_network_receive_bytes_total", func(k int) float64 { return float64(k) * 1e6 }, "device", "eth0")
		mk("node_network_transmit_bytes_total", func(k int) float64 { return float64(k) * 1e5 }, "device", "eth0")
		mk("up", func(int) float64 { return 1 })
		if err := s.met.Ecrire(ctx, lot); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("préparation %d machines : %v", nb, time.Since(t0).Round(time.Millisecond))
	_ = auth.CreerUtilisateur(ctx, s.db, "admin", auth.RoleAdmin, "une longue phrase de passe", "test", "", now)
	secret, _ := auth.NouveauSecretTOTP()
	ch, _ := secrets.Chiffrer(s.cle, "console.totp", secret)
	_ = auth.DefinirTOTP(ctx, s.db, "admin", ch, 0, "test", "", now)

	n := &navigateur{t: t, base: "http://" + cfg.EcouteInterface, c: &http.Client{Timeout: 120 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	n.faire(http.MethodPost, "/connexion", url.Values{"nom": {"admin"}, "mot_de_passe": {"une longue phrase de passe"}})
	_, corps, _ := n.faire(http.MethodGet, "/connexion/code", nil)
	code, _ := auth.CodeTOTP(secret, time.Now())
	n.faire(http.MethodPost, "/connexion/code", url.Values{"csrf": {reCSRFe2e.FindStringSubmatch(corps)[1]}, "code": {code}})
	// Laisse le moteur d'alertes faire un premier tour.
	time.Sleep(3 * time.Second)
	if a, _ := time.ParseDuration(os.Getenv("MESURE_ATTENTE")); a > 0 {
		// Tâches de fond seules (aucune page) : CPU du processus.
		var ru0, ru1 syscall.Rusage
		_ = syscall.Getrusage(syscall.RUSAGE_SELF, &ru0)
		time.Sleep(a)
		_ = syscall.Getrusage(syscall.RUSAGE_SELF, &ru1)
		cpu := time.Duration(ru1.Utime.Nano() + ru1.Stime.Nano() - ru0.Utime.Nano() - ru0.Stime.Nano())
		t.Logf("FOND : %v de CPU en %v = %.2f %% d'un cœur", cpu.Round(time.Millisecond), a, 100*cpu.Seconds()/a.Seconds())
	}
	pages := strings.Fields(envOu("MESURE_PAGES", "/ /fragments/accueil /machines /machines?vue=liste /machines/vm-0007 /conteneurs /sondes /sante-mirador /taches /applications /elements /mises-a-jour /tableaux /tableaux/serveurs-linux /alertes /fragments/alertes /regles /logs?periode=24h /flux-bloques /mur /disponibilite /coupures /evenements /incidents /comparaison /reglages /reglages/conservation"))
	tours, _ := strconv.Atoi(envOu("MESURE_TOURS", "5"))
	durees := map[string][]time.Duration{}
	premier := map[string]time.Duration{}
	octets := map[string]int{}
	for i := 0; i < tours; i++ {
		for _, p := range pages {
			x := time.Now()
			code, c, _ := n.faire(http.MethodGet, p, nil)
			d := time.Since(x)
			if code != 200 {
				t.Logf("%s : %d", p, code)
			}
			if i == 0 {
				premier[p] = d
			} else {
				durees[p] = append(durees[p], d)
			}
			octets[p] = len(c)
		}
	}
	var lignes []string
	for _, p := range pages {
		d := durees[p]
		sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
		med := time.Duration(0)
		if len(d) > 0 {
			med = d[len(d)/2]
		}
		lignes = append(lignes, fmt.Sprintf("| `%s` | %v | %v | %v | %d Ko |", p, premier[p].Round(100*time.Microsecond), med.Round(100*time.Microsecond), med.Round(100*time.Microsecond), octets[p]/1024))
	}
	t.Logf("\n| Page | 1re | min | médiane | taille |\n|---|---|---|---|---|\n%s", strings.Join(lignes, "\n"))
}
