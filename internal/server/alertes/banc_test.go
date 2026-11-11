// SPDX-License-Identifier: GPL-3.0-or-later

package alertes

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/tsdb"
)

// Banc du moteur d'alertes (SPEC.md § 17.4) : les 68 règles importées + les
// règles mirador, sur un parc simulé de 50 machines × ~570 séries (profil
// mesuré à l'étape 2), avec 2 h d'historique pour tout et les longues
// fenêtres réellement remplies (7 j de ssl_certs_found_total et de
// node_uname_info, 24 h des systèmes de fichiers et de SMART).
//
//	BANC_ALERTES=1 go test -run TestBancAlertes -v ./internal/server/alertes/
//	go test -run '^$' -bench EvaluerTout ./internal/server/alertes/

const pas = 30 * time.Second

type serieBanc struct {
	etq    labels.Labels
	duree  time.Duration // historique écrit
	valeur func(i int) float64
}

// parc : séries d'une machine.
func parc(machine string) []serieBanc {
	var out []serieBanc
	aj := func(d time.Duration, v func(int) float64, kv ...string) {
		out = append(out, serieBanc{etq: labels.FromStrings(append(kv, "instance", machine, "job", "node")...), duree: d, valeur: v})
	}
	h2, j1, j7 := 2*time.Hour, 24*time.Hour, 7*24*time.Hour
	compteur := func(pente float64) func(int) float64 { return func(i int) float64 { return float64(i) * pente } }
	fixe := func(x float64) func(int) float64 { return func(int) float64 { return x } }
	for c := 0; c < 4; c++ {
		for k, mode := range []string{"idle", "user", "system", "iowait", "irq", "softirq", "steal", "nice"} {
			aj(h2, compteur(float64(8-k)), "__name__", "node_cpu_seconds_total", "cpu", strconv.Itoa(c), "mode", mode)
		}
	}
	for _, n := range []string{"node_load1", "node_load5", "node_load15"} {
		aj(h2, fixe(0.5), "__name__", n)
	}
	for _, n := range []string{"MemAvailable", "MemTotal", "Buffers", "Cached", "SwapFree", "SwapTotal"} {
		aj(h2, fixe(2e9), "__name__", "node_memory_"+n+"_bytes")
	}
	for _, n := range []string{"node_vmstat_pswpin", "node_vmstat_pswpout", "node_vmstat_oom_kill"} {
		aj(h2, fixe(3), "__name__", n)
	}
	for _, mp := range []string{"/", "/boot", "/var", "/home", "/srv"} {
		aj(j1, func(i int) float64 { return 5e10 - float64(i)*1e3 }, "__name__", "node_filesystem_avail_bytes", "mountpoint", mp, "fstype", "ext4")
		aj(h2, fixe(1e11), "__name__", "node_filesystem_size_bytes", "mountpoint", mp, "fstype", "ext4")
		aj(h2, fixe(1e6), "__name__", "node_filesystem_files", "mountpoint", mp, "fstype", "ext4")
		aj(h2, fixe(9e5), "__name__", "node_filesystem_files_free", "mountpoint", mp, "fstype", "ext4")
	}
	for _, d := range []string{"sda", "sdb"} {
		for _, n := range []string{"io_time_seconds", "read_bytes", "written_bytes"} {
			aj(h2, compteur(0.1), "__name__", "node_disk_"+n+"_total", "device", d)
		}
		aj(h2, fixe(1), "__name__", "smartctl_device_smart_status", "device", d)
		aj(h2, fixe(35), "__name__", "smartctl_device_temperature", "device", d, "temperature_type", "current")
		aj(j1, fixe(0), "__name__", "smartctl_device_attribute", "device", d, "attribute_name", "Reallocated_Sector_Ct", "attribute_value_type", "raw")
	}
	for _, d := range []string{"eth0", "eth1", "docker0"} {
		for _, n := range []string{"receive_bytes", "transmit_bytes", "receive_errs", "transmit_errs", "receive_drop", "transmit_drop"} {
			aj(h2, compteur(1000), "__name__", "node_network_"+n+"_total", "device", d)
		}
	}
	aj(h2, fixe(45), "__name__", "node_hwmon_temp_celsius", "chip", "coretemp", "sensor", "temp1")
	aj(h2, fixe(46), "__name__", "node_hwmon_temp_celsius", "chip", "coretemp", "sensor", "temp2")
	aj(h2, func(i int) float64 { return 1.7e9 + float64(i)*30 }, "__name__", "node_time_seconds")
	aj(h2, fixe(1.6e9), "__name__", "node_boot_time_seconds")
	aj(j7, fixe(1), "__name__", "node_uname_info", "release", "6.8.0")
	for u := 0; u < 200; u++ {
		aj(h2, fixe(0), "__name__", "node_systemd_unit_state", "name", fmt.Sprintf("u%d.service", u), "state", "failed")
	}
	for u := 0; u < 100; u++ {
		aj(h2, fixe(1), "__name__", "node_systemd_unit_enabled_active", "name", fmt.Sprintf("u%d.service", u))
		aj(h2, fixe(2), "__name__", "node_systemd_service_restart_total", "name", fmt.Sprintf("u%d.service", u))
	}
	aj(h2, fixe(1), "__name__", "node_docker_up")
	for c := 0; c < 10; c++ {
		n := fmt.Sprintf("c%d", c)
		aj(h2, fixe(1), "__name__", "node_docker_container_running", "name", n, "restart_policy", "unless-stopped")
		aj(h2, fixe(0), "__name__", "node_docker_container_restart_count", "name", n)
		aj(h2, fixe(1), "__name__", "node_docker_container_healthy", "name", n)
		aj(h2, fixe(0), "__name__", "node_docker_container_oom_killed", "name", n)
		aj(h2, fixe(1e8), "__name__", "node_docker_container_memory_bytes", "name", n)
		aj(h2, fixe(1e9), "__name__", "node_docker_container_memory_limit_bytes", "name", n)
	}
	for k := 0; k < 3; k++ {
		aj(h2, fixe(2e9), "__name__", "ssl_cert_not_after_timestamp_seconds", "cn", fmt.Sprintf("site%d.example.org", k))
	}
	aj(j7, fixe(3), "__name__", "ssl_certs_found_total")
	aj(h2, fixe(0), "__name__", "ssl_certs_read_errors_total")
	aj(h2, fixe(1), "__name__", "node_firewall_input_policy_drop")
	return out
}

// remplirParc écrit le parc dans la TSDB jusqu'à fin, dans l'ordre du temps
// (comme des agents en direct) ; renvoie le nombre de séries et de points.
func remplirParc(t testing.TB, db *tsdb.DB, machines int, fin time.Time) (series, points int) {
	t.Helper()
	ctx := context.Background()
	var tout []serieBanc
	var plusLongue time.Duration
	for m := 0; m < machines; m++ {
		for _, s := range parc(fmt.Sprintf("machine%02d", m)) {
			tout = append(tout, s)
			plusLongue = max(plusLongue, s.duree)
		}
	}
	n := int(plusLongue / pas)
	app := db.Appender(ctx)
	for i := 0; i <= n; i++ {
		age := time.Duration(n-i) * pas
		ts := fin.Add(-age).UnixMilli()
		for _, s := range tout {
			if s.duree < age {
				continue
			}
			if _, err := app.Append(0, s.etq, ts, s.valeur(i)); err != nil {
				t.Fatal(err)
			}
			points++
		}
		if i%120 == 0 {
			if err := app.Commit(); err != nil {
				t.Fatal(err)
			}
			app = db.Appender(ctx)
		}
	}
	if err := app.Commit(); err != nil {
		t.Fatal(err)
	}
	return len(tout), points
}

func reglesBanc(t testing.TB) []Regle {
	t.Helper()
	var tout []Regle
	for _, f := range []string{"system.yml", "equipements.yml"} {
		fh, err := os.Open(filepath.Join("testdata", "regles", f))
		if err != nil {
			t.Fatal(err)
		}
		rap, err := Importer(fh, f)
		fh.Close()
		if err != nil {
			t.Fatal(err)
		}
		tout = append(tout, rap.Regles...)
	}
	return append(tout, ReglesMirador()...)
}

func moteurBanc(t testing.TB, machines int) (*Moteur, time.Time, int, int) {
	t.Helper()
	ctx := context.Background()
	db := baseTest(t)
	o := tsdb.DefaultOptions()
	o.OutOfOrderTimeWindow = (24 * time.Hour).Milliseconds()
	donnees, err := tsdb.Open(t.TempDir(), nil, nil, o, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { donnees.Close() })
	fin := time.Now().Truncate(time.Minute)
	series, points := remplirParc(t, donnees, machines, fin)
	if _, err := Enregistrer(ctx, db, reglesBanc(t), "banc", fin); err != nil {
		t.Fatal(err)
	}
	m, err := Nouveau(ctx, Options{DB: db, Donnees: donnees})
	if err != nil {
		t.Fatal(err)
	}
	return m, fin, series, points
}

func TestBancAlertes(t *testing.T) {
	if os.Getenv("BANC_ALERTES") == "" {
		t.Skip("banc long : BANC_ALERTES=1")
	}
	ctx := context.Background()
	t0 := time.Now()
	m, fin, series, points := moteurBanc(t, 50)
	t.Logf("parc : 50 machines, %d séries, %d points écrits en %s", series, points, time.Since(t0).Round(time.Millisecond))

	var avant, apres runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&avant)
	const tours = 5
	// Pic de mémoire pendant l'évaluation (relevé toutes les 2 ms, GC forcé
	// avant chaque tour) : coût propre du moteur, hors données de la TSDB.
	var pic uint64
	stop := make(chan struct{})
	finRelevé := make(chan struct{})
	go func() {
		defer close(finRelevé)
		var ms runtime.MemStats
		for {
			select {
			case <-stop:
				return
			case <-time.After(2 * time.Millisecond):
				runtime.ReadMemStats(&ms)
				pic = max(pic, ms.HeapAlloc)
			}
		}
	}()
	debut := time.Now()
	cpu0 := cpuProcessus()
	for i := 0; i < tours; i++ {
		m.EvaluerTout(ctx, fin)
	}
	duree := time.Since(debut) / tours
	cpu := (cpuProcessus() - cpu0) / tours
	close(stop)
	<-finRelevé
	runtime.ReadMemStats(&apres)
	t.Logf("pic de tas pendant l'évaluation : +%.1f Mo au-dessus du tas de départ (%.1f Mo, surtout la tête de la TSDB)",
		float64(pic-avant.HeapAlloc)/1e6, float64(avant.HeapAlloc)/1e6)
	for _, s := range m.Sante() {
		if s.Erreur != "" {
			t.Errorf("%s : %s", s.Nom, s.Erreur)
		}
	}
	sante := m.Sante()
	sort.Slice(sante, func(i, j int) bool { return sante[i].Duree > sante[j].Duree })
	t.Logf("évaluation de %d règles : %s par tour (CPU %s), %.1f Mo alloués par tour, %d allocations",
		len(sante), duree.Round(time.Microsecond), cpu.Round(time.Microsecond),
		float64(apres.TotalAlloc-avant.TotalAlloc)/tours/1e6, (apres.Mallocs-avant.Mallocs)/tours)
	for _, s := range sante[:8] {
		t.Logf("  %-32s %8s  %d résultat(s)", s.Nom, s.Duree.Round(time.Microsecond), s.Resultats)
	}
	// Coût réel : chaque règle à son intervalle (30 s ou 1 min).
	var parMinute time.Duration
	for _, s := range sante {
		for _, r := range m.regles {
			if r.def.Nom == s.Nom {
				parMinute += s.Duree * time.Duration(time.Minute/time.Duration(r.def.Intervalle))
			}
		}
	}
	t.Logf("coût en régime : %s de calcul par minute = %.2f %% d'un cœur", parMinute.Round(time.Microsecond),
		100*parMinute.Seconds()/60)
}

// cpuProcessus : temps CPU (utilisateur + système) du processus.
func cpuProcessus() time.Duration {
	b, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return 0
	}
	champs := []byte{}
	n := 0
	var ut, st int64
	for i := len(b) - 1; i >= 0; i-- { // après la parenthèse fermante du nom
		if b[i] == ')' {
			champs = b[i+2:]
			break
		}
	}
	debut := 0
	for i := 0; i <= len(champs); i++ {
		if i == len(champs) || champs[i] == ' ' {
			n++
			v, _ := strconv.ParseInt(string(champs[debut:i]), 10, 64)
			switch n {
			case 12:
				ut = v
			case 13:
				st = v
			}
			debut = i + 1
		}
	}
	return time.Duration(ut+st) * 10 * time.Millisecond // 100 tops par seconde
}

func BenchmarkEvaluerTout(b *testing.B) {
	m, fin, _, _ := moteurBanc(b, 10)
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		m.EvaluerTout(ctx, fin)
	}
}
