// SPDX-License-Identifier: GPL-3.0-or-later

// Banc d'essai de la bibliothèque Prometheus TSDB pour mirador-server
// (étape 3, SPEC.md § 7.2) : RAM, CPU et disque avec 50 agents.
//
// Profil simulé, d'après les mesures sur srv-test (09/11) : ~600 séries par
// agent (dont ~200 node_systemd_unit_state, ~150 restart_total), un point
// toutes les 30 s. Le temps est accéléré : chaque « cycle » ajoute un point à
// toutes les séries avec un horodatage avancé de 30 s.
//
// Lancement (quelques minutes) :
//
//	go test -run TestBancTSDB -v -timeout 30m
package stockage

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/config"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/tsdb"
	"github.com/prometheus/prometheus/tsdb/chunkenc"
	"github.com/prometheus/prometheus/util/compression"
)

// serie : étiquettes d'une série simulée et sa valeur courante.
type serie struct {
	lbls     labels.Labels
	valeur   float64
	compteur bool
}

// profilAgent crée les ~600 séries d'un agent, avec des noms réalistes.
func profilAgent(agent int) []serie {
	inst := fmt.Sprintf("machine-%02d", agent)
	var s []serie
	ajoute := func(compteur bool, nom string, kv ...string) {
		b := labels.NewScratchBuilder(len(kv)/2 + 5)
		b.Add("__name__", nom)
		b.Add("instance", inst)
		b.Add("job", "node")
		b.Add("site", "bureau")
		b.Add("env", "vm")
		for i := 0; i+1 < len(kv); i += 2 {
			b.Add(kv[i], kv[i+1])
		}
		b.Sort()
		s = append(s, serie{lbls: b.Labels(), valeur: rand.Float64() * 1000, compteur: compteur})
	}
	for u := 0; u < 200; u++ {
		ajoute(false, "node_systemd_unit_state", "name", "service-"+strconv.Itoa(u)+".service", "state", "failed")
	}
	for u := 0; u < 150; u++ {
		ajoute(true, "node_systemd_service_restart_total", "name", "service-"+strconv.Itoa(u)+".service")
	}
	for c := 0; c < 4; c++ {
		for _, m := range []string{"user", "nice", "system", "idle", "iowait", "irq", "softirq", "steal"} {
			ajoute(true, "node_cpu_seconds_total", "cpu", strconv.Itoa(c), "mode", m)
		}
	}
	for _, d := range []string{"sda", "sdb", "vda"} {
		for _, m := range []string{"node_disk_read_bytes_total", "node_disk_written_bytes_total", "node_disk_io_time_seconds_total"} {
			ajoute(true, m, "device", d)
		}
	}
	for _, mp := range []string{"/", "/boot", "/var", "/home", "/srv"} {
		for _, m := range []string{"node_filesystem_size_bytes", "node_filesystem_avail_bytes", "node_filesystem_files", "node_filesystem_files_free"} {
			ajoute(false, m, "device", "/dev/sda1", "fstype", "ext4", "mountpoint", mp)
		}
	}
	for _, i := range []string{"eth0", "eth1"} {
		for _, m := range []string{"receive_bytes", "transmit_bytes", "receive_errs", "transmit_errs", "receive_drop", "transmit_drop"} {
			ajoute(true, "node_network_"+m+"_total", "device", i)
		}
	}
	for c := 0; c < 20; c++ {
		for _, m := range []string{"running", "restart_count", "cpu_seconds_total", "memory_bytes"} {
			ajoute(m == "cpu_seconds_total", "node_docker_container_"+m, "name", "conteneur-"+strconv.Itoa(c), "project", "p")
		}
	}
	for len(s) < 600 { // métriques diverses (mémoire, charge, internes…)
		ajoute(false, "divers_"+strconv.Itoa(len(s)))
	}
	return s
}

// avancer fait évoluer la valeur comme le font les vraies métriques :
//   - compteurs : hausses par petits pas « ronds » (centièmes de seconde CPU,
//     octets par multiples de 512…) ;
//   - états systemd : 0 presque toujours ;
//   - jauges : constantes la plupart du temps, petits changements arrondis.
//
// Une simulation trop aléatoire gonflerait artificiellement le disque.
func (s *serie) avancer() {
	nom := s.lbls.Get("__name__")
	switch {
	case s.compteur && strings.Contains(nom, "restart"):
		if rand.IntN(5000) == 0 {
			s.valeur++
		}
	case s.compteur && strings.Contains(nom, "cpu_seconds"):
		s.valeur += float64(rand.IntN(300)) / 100
	case s.compteur:
		s.valeur += float64(rand.IntN(64) * 512)
	case strings.Contains(nom, "unit_state"):
		s.valeur = 0
	case strings.Contains(nom, "filesystem"):
		if rand.IntN(10) == 0 {
			s.valeur = float64(int64(s.valeur) - int64(rand.IntN(8))*4096)
		}
	default:
		if rand.IntN(3) == 0 {
			s.valeur = float64(int64((s.valeur+(rand.Float64()-0.5)*20)*100)) / 100
		}
	}
}

func rssKo() int64 { return champStatus("VmRSS:") }

// champStatus lit une ligne de /proc/self/status (en Ko).
func champStatus(cle string) int64 {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, cle) {
			v, _ := strconv.ParseInt(strings.Fields(l)[1], 10, 64)
			return v
		}
	}
	return 0
}

func tailleDossier(dir string) int64 {
	var t int64
	_ = filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			t += info.Size()
		}
		return nil
	})
	return t
}

func cpuSecondes() float64 {
	var ru syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	return float64(ru.Utime.Sec+ru.Stime.Sec) + float64(ru.Utime.Usec+ru.Stime.Usec)/1e6
}

// TestBancTSDB simule HEURES heures de données pour AGENTS agents.
func TestBancTSDB(t *testing.T) {
	agents := envInt("AGENTS", 50)
	heures := envInt("HEURES", 6)
	dir := t.TempDir()
	opts := tsdb.DefaultOptions()
	opts.RetentionDuration = int64(7 * 24 * time.Hour / time.Millisecond)
	// En production, la base relit ses blocs (et applique la rétention)
	// toutes les minutes ; accéléré ici pour le test.
	opts.BlockReloadInterval = 200 * time.Millisecond
	// Compression du WAL (zstd) : moins d'écritures disque, un peu plus de CPU.
	if c := os.Getenv("WAL"); c != "" {
		opts.WALCompression = compression.Type(c)
	}
	// Encodage des blocs de points : XOR (Gorilla, défaut) ou XOR2 (plus récent).
	if os.Getenv("ENCODAGE") == "xor2" {
		opts.FloatChunkEncoding = chunkenc.EncXOR2
	}
	t.Logf("encodage des points : %s", opts.FloatChunkEncoding)
	// Le WAL est gardé : c'est ce qui rend l'arrêt brutal sans perte.
	db, err := tsdb.Open(dir, nil, nil, opts, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var series []serie
	for a := 0; a < agents; a++ {
		series = append(series, profilAgent(a)...)
	}
	t.Logf("%d agents, %d séries actives, %d h simulées à 30 s", agents, len(series), heures)

	debut := time.Now().Add(-time.Duration(heures) * time.Hour).Truncate(time.Hour)
	cycles := heures * 120
	cpu0, mur0, ecrit0 := cpuSecondes(), time.Now(), ecritOctets()
	var points int64
	for c := 0; c < cycles; c++ {
		ts := debut.Add(time.Duration(c) * 30 * time.Second).UnixMilli()
		app := db.Appender(context.Background())
		for i := range series {
			series[i].avancer()
			if _, err := app.Append(0, series[i].lbls, ts, series[i].valeur); err != nil {
				t.Fatal(err)
			}
		}
		if err := app.Commit(); err != nil {
			t.Fatal(err)
		}
		points += int64(len(series))
		if c%240 == 239 { // toutes les 2 h simulées
			runtime.GC()
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			t.Logf("après %3d h : RSS %6.1f Mo, tas %6.1f Mo, disque %6.1f Mo",
				(c+1)/120, float64(rssKo())/1024, float64(m.HeapInuse)/(1<<20), float64(tailleDossier(dir))/(1<<20))
		}
	}
	cpu := cpuSecondes() - cpu0
	duree := time.Since(mur0)
	// Compaction finale (comme le ferait le serveur en tâche de fond).
	if err := db.Compact(context.Background()); err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	disque := tailleDossier(dir)
	t.Logf("RÉSULTAT : %d points en %s (CPU %.1f s, %.2f µs par point)", points, duree.Truncate(time.Millisecond), cpu, cpu*1e6/float64(points))
	t.Logf("RÉSULTAT : RSS %.1f Mo (propre %.1f Mo, fichiers projetés %.1f Mo), tas utilisé %.1f Mo",
		float64(rssKo())/1024, float64(champStatus("RssAnon:"))/1024, float64(champStatus("RssFile:"))/1024, float64(m.HeapInuse)/(1<<20))
	blocs := int64(0)
	var tBlocs int64
	for _, b := range db.Blocks() {
		blocs++
		tBlocs += tailleDossier(filepath.Join(dir, b.Meta().ULID.String()))
	}
	wal := tailleDossier(filepath.Join(dir, "wal"))
	head := tailleDossier(filepath.Join(dir, "chunks_head"))
	var pointsBlocs int64
	for _, b := range db.Blocks() {
		pointsBlocs += int64(b.Meta().Stats.NumSamples)
	}
	t.Logf("RÉSULTAT : disque %.1f Mo = blocs %.1f Mo (%d) + WAL %.1f Mo + chunks_head %.1f Mo",
		float64(disque)/(1<<20), float64(tBlocs)/(1<<20), blocs, float64(wal)/(1<<20), float64(head)/(1<<20))
	if pointsBlocs > 0 {
		t.Logf("RÉSULTAT : blocs compactés : %.2f octets par point → ~%.0f Mo par jour pour %d séries",
			float64(tBlocs)/float64(pointsBlocs), float64(tBlocs)/float64(pointsBlocs)*float64(len(series))*2880/(1<<20), len(series))
	}
	// En temps réel, 50 agents × 600 séries / 30 s = 1 000 points/s.
	t.Logf("RÉSULTAT : écrit sur disque %.0f Mo pendant l'ingestion et la compaction → ~%.0f Mo par jour en temps réel",
		float64(ecritOctets()-ecrit0)/(1<<20), float64(ecritOctets()-ecrit0)/(1<<20)*24/float64(heures))
	t.Logf("RÉSULTAT : CPU en temps réel estimé : %.3f %% d'un cœur pour %.0f points/s",
		cpu/float64(heures*3600)*100, float64(len(series))/30)

	requetesTSDB(t, db, debut.UnixMilli(), debut.Add(time.Duration(cycles-1)*30*time.Second).UnixMilli())

	// Changement de rétention à chaud (sans redémarrage) : 1 h seulement.
	conf := &config.Config{StorageConfig: config.StorageConfig{TSDBConfig: &config.TSDBConfig{
		Retention: &config.TSDBRetentionConfig{Time: model.Duration(time.Hour)},
	}}}
	avant := len(db.Blocks())
	if err := db.ApplyConfig(conf); err != nil {
		t.Fatalf("rétention à chaud : %v", err)
	}
	time.Sleep(time.Second) // laisse passer une relecture des blocs
	t.Logf("RÉSULTAT : rétention ramenée à chaud à 1 h → %d blocs au lieu de %d, disque %.1f Mo (blocs entiers supprimés)",
		len(db.Blocks()), avant, float64(tailleDossier(dir))/(1<<20))
	if len(db.Blocks()) >= avant {
		t.Error("la rétention à chaud n'a supprimé aucun bloc")
	}
}

func envInt(nom string, defaut int) int {
	if v, err := strconv.Atoi(os.Getenv(nom)); err == nil && v > 0 {
		return v
	}
	return defaut
}

// requetesTSDB : les 4 requêtes types, comme dans TestBancDuckMetriques (la
// TSDB n'a pas de SQL : les calculs sont faits en Go, comme le ferait mirador).
func requetesTSDB(t *testing.T, db *tsdb.DB, debut, fin int64) {
	ctx := context.Background()
	compter := func(mint, maxt int64, f func(serie int, ts int64, v float64), m ...*labels.Matcher) {
		q, err := db.Querier(mint, maxt)
		if err != nil {
			t.Fatal(err)
		}
		defer q.Close()
		ss := q.Select(ctx, false, nil, m...)
		var it chunkenc.Iterator
		n := 0
		for ss.Next() {
			it = ss.At().Iterator(it)
			for it.Next() == chunkenc.ValFloat {
				ts, v := it.At()
				f(n, ts, v)
			}
			n++
		}
		if err := ss.Err(); err != nil {
			t.Fatal(err)
		}
	}
	type cle struct {
		serie int
		b     int64
	}
	mesure := func(nom string, f func() int) {
		d := time.Now()
		n := f()
		t.Logf("RÉSULTAT : %-52s : %8d lignes en %v", nom, n, time.Since(d).Round(time.Microsecond))
	}
	nom := func(n string) *labels.Matcher { return labels.MustNewMatcher(labels.MatchEqual, "__name__", n) }
	mesure("Q1 tableau : 5 séries d'une machine, dernière heure", func() int {
		n := 0
		compter(fin-3600_000, fin, func(int, int64, float64) { n++ }, nom("node_filesystem_avail_bytes"),
			labels.MustNewMatcher(labels.MatchEqual, "instance", "machine-07"))
		return n
	})
	mesure("Q2 moyenne par 5 min, 250 séries, toute la période", func() int {
		somme := map[cle][2]float64{}
		compter(debut, fin, func(s int, ts int64, v float64) {
			k := cle{s, ts / 300000}
			x := somme[k]
			somme[k] = [2]float64{x[0] + v, x[1] + 1}
		}, nom("node_filesystem_avail_bytes"))
		return len(somme)
	})
	mesure("Q3 dernière valeur, 10 000 séries (alerte)", func() int {
		der := map[int]float64{}
		compter(fin-300_000, fin, func(s int, _ int64, v float64) { der[s] = v }, nom("node_systemd_unit_state"))
		return len(der)
	})
	mesure("Q4 agrégats 5 min d'une partition (toutes séries)", func() int {
		agr := map[cle][3]float64{}
		compter(debut, debut+2*3600_000-1, func(s int, ts int64, v float64) {
			k := cle{s, ts / 300000}
			x := agr[k]
			agr[k] = [3]float64{x[0] + v, max(x[1], v), v}
		}, labels.MustNewMatcher(labels.MatchRegexp, "__name__", ".+"))
		return len(agr)
	})
	t.Logf("RÉSULTAT : RSS après requêtes %.1f Mo (propre %.1f Mo)", float64(rssKo())/1024, float64(champStatus("RssAnon:"))/1024)
}
