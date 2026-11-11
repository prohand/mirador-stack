// SPDX-License-Identifier: GPL-3.0-or-later

// Banc d'essai DuckDB pour les métriques (demande du 09/11 : remplacer la TSDB
// de Prometheus pour n'avoir que SQLite + DuckDB). Même profil que
// TestBancTSDB, mêmes requêtes types (voir requetesTSDB dans tsdb_test.go).
//
//	go test -run TestBancDuckMetriques -v -timeout 60m
//	   AGENTS=50 HEURES=6 MEMOIRE=100MB PARTITION=2h
//	   FLUSH=agent|cycle  (écriture à chaque requête d'agent, ou regroupée par cycle de 30 s)
//	   TRI=non|oui        (partition terminée réécrite triée par série : meilleure compression)
//
// Organisation testée (la seule qui permette de purger par fichiers entiers,
// exigence du brief) : un fichier DuckDB par tranche de temps (« partition »),
// table points(serie INTEGER, t BIGINT, v DOUBLE) ; les étiquettes sont dans
// une table « series » (un identifiant par série, gardé aussi en mémoire).
package stockage

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	duckdb "github.com/duckdb/duckdb-go/v2"
)

func envTexte(nom, defaut string) string {
	if v := os.Getenv(nom); v != "" {
		return v
	}
	return defaut
}

// ouvrirDuck ouvre une base DuckDB avec un plafond de mémoire et 2 fils (VPS
// de 2 cœurs).
func ouvrirDuck(t testing.TB, chemin, memoire string) *sql.DB {
	t.Helper()
	c, err := duckdb.NewConnector(chemin, func(e driver.ExecerContext) error {
		for _, q := range []string{
			"SET memory_limit='" + memoire + "'",
			"SET threads=2",
			"SET preserve_insertion_order=false",
		} {
			if _, err := e.ExecContext(context.Background(), q, nil); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(c)
	db.SetMaxOpenConns(1) // une seule connexion : celle qui porte les ATTACH
	return db
}

func TestBancDuckMetriques(t *testing.T) {
	agents := envInt("AGENTS", 50)
	heures := envInt("HEURES", 6)
	memoire := envTexte("MEMOIRE", "100MB")
	partition, _ := time.ParseDuration(envTexte("PARTITION", "2h"))
	parCycle := envTexte("FLUSH", "agent") == "cycle"
	trier := envTexte("TRI", "non") == "oui"
	dir := t.TempDir()
	ctx := context.Background()
	db := ouvrirDuck(t, filepath.Join(dir, "series.duckdb"), memoire)
	defer db.Close()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := conn.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s : %v", q, err)
		}
	}
	exec(`CREATE TABLE series (id INTEGER, nom VARCHAR, instance VARCHAR, etiquettes VARCHAR)`)

	var series []serie
	for a := 0; a < agents; a++ {
		series = append(series, profilAgent(a)...)
	}
	// Table des séries (une fois ; en production : à la première apparition).
	if err := conn.Raw(func(dc any) error {
		ap, err := duckdb.NewAppender(dc.(driver.Conn), "", "main", "series")
		if err != nil {
			return err
		}
		for i, s := range series {
			if err := ap.AppendRow(int32(i), s.lbls.Get("__name__"), s.lbls.Get("instance"), s.lbls.String()); err != nil {
				return err
			}
		}
		return ap.Close()
	}); err != nil {
		t.Fatal(err)
	}
	t.Logf("%d agents, %d séries, %d h simulées à 30 s, mémoire DuckDB plafonnée à %s, partition %s, écriture par cycle %v, tri %v",
		agents, len(series), heures, memoire, partition, parCycle, trier)

	debut := time.Now().Add(-time.Duration(heures) * time.Hour).Truncate(time.Hour)
	cycles := heures * 120
	var ap *duckdb.Appender
	partCourante := ""
	var parts []string
	// trierPartition réécrit une partition terminée, triée par série puis par
	// temps (comme la compaction de la TSDB) ; l'ancien fichier est supprimé.
	var dureeTri time.Duration
	trierPartition := func(nom string) {
		d := time.Now()
		exec(`ATTACH '` + filepath.Join(dir, nom+"t.duckdb") + `' AS ` + nom + `t`)
		exec(`CREATE TABLE ` + nom + `t.points AS SELECT * FROM ` + nom + `.points ORDER BY serie, t`)
		exec(`DETACH ` + nom)
		exec(`DETACH ` + nom + `t`)
		if err := os.Rename(filepath.Join(dir, nom+"t.duckdb"), filepath.Join(dir, nom+".duckdb")); err != nil {
			t.Fatal(err)
		}
		exec(`ATTACH '` + filepath.Join(dir, nom+".duckdb") + `' AS ` + nom)
		dureeTri += time.Since(d)
	}
	ouvrirPartition := func(ts int64) {
		nom := "p" + time.UnixMilli(ts).UTC().Truncate(partition).Format("20060102150405")
		if nom == partCourante {
			return
		}
		if ap != nil {
			if err := ap.Close(); err != nil {
				t.Fatal(err)
			}
			if trier {
				trierPartition(partCourante)
			}
		}
		exec(`ATTACH '` + filepath.Join(dir, nom+".duckdb") + `' AS ` + nom)
		exec(`CREATE TABLE ` + nom + `.points (serie INTEGER, t BIGINT, v DOUBLE)`)
		if err := conn.Raw(func(dc any) error {
			var err error
			ap, err = duckdb.NewAppender(dc.(driver.Conn), nom, "main", "points")
			return err
		}); err != nil {
			t.Fatal(err)
		}
		partCourante = nom
		parts = append(parts, nom)
	}

	ecrit0, cpu0, mur0 := ecritOctets(), cpuSecondes(), time.Now()
	var points int64
	for c := 0; c < cycles; c++ {
		ts := debut.Add(time.Duration(c) * 30 * time.Second).UnixMilli()
		ouvrirPartition(ts)
		// Une requête d'agent = 600 points = un Flush (comme le ferait le serveur).
		for a := 0; a < agents; a++ {
			for i := a * 600; i < (a+1)*600 && i < len(series); i++ {
				series[i].avancer()
				if err := ap.AppendRow(int32(i), ts, series[i].valeur); err != nil {
					t.Fatal(err)
				}
			}
			if !parCycle {
				if err := ap.Flush(); err != nil {
					t.Fatal(err)
				}
			}
		}
		if parCycle {
			if err := ap.Flush(); err != nil {
				t.Fatal(err)
			}
		}
		points += int64(len(series))
		if c%240 == 239 {
			runtime.GC()
			t.Logf("après %3d h : RSS %6.1f Mo (propre %6.1f Mo), disque %6.1f Mo",
				(c+1)/120, float64(rssKo())/1024, float64(champStatus("RssAnon:"))/1024, float64(tailleDossier(dir))/(1<<20))
		}
	}
	if err := ap.Close(); err != nil {
		t.Fatal(err)
	}
	if trier {
		trierPartition(partCourante)
	}
	exec(`CHECKPOINT`)
	for _, p := range parts {
		exec(`CHECKPOINT ` + p)
	}
	cpu := cpuSecondes() - cpu0
	ecrit := ecritOctets() - ecrit0
	duree := time.Since(mur0)
	disque := tailleDossier(dir)
	t.Logf("RÉSULTAT : %d points en %s (CPU %.1f s, %.2f µs par point, génération comprise)", points, duree.Truncate(time.Millisecond), cpu, cpu*1e6/float64(points))
	if trier {
		t.Logf("RÉSULTAT : dont tri des partitions : %v au total", dureeTri.Round(time.Millisecond))
	}
	t.Logf("RÉSULTAT : RSS %.1f Mo (propre %.1f Mo, fichiers projetés %.1f Mo)",
		float64(rssKo())/1024, float64(champStatus("RssAnon:"))/1024, float64(champStatus("RssFile:"))/1024)
	t.Logf("RÉSULTAT : disque %.1f Mo (%d partitions) → %.2f octets par point → ~%.0f Mo par jour pour %d séries",
		float64(disque)/(1<<20), len(parts), float64(disque)/float64(points), float64(disque)/float64(points)*float64(len(series))*2880/(1<<20), len(series))
	t.Logf("RÉSULTAT : écrit sur disque %.0f Mo pendant l'ingestion → ~%.0f Mo par jour en temps réel",
		float64(ecrit)/(1<<20), float64(ecrit)/(1<<20)*24/float64(heures))
	t.Logf("RÉSULTAT : CPU en temps réel estimé : %.3f %% d'un cœur pour %.0f points/s",
		cpu/float64(heures*3600)*100, float64(len(series))/30)

	// Vue sur toutes les partitions.
	var unions []string
	for _, p := range parts {
		unions = append(unions, "SELECT * FROM "+p+".points")
	}
	exec(`CREATE TEMP VIEW points AS ` + strings.Join(unions, " UNION ALL "))
	fin := debut.Add(time.Duration(cycles-1) * 30 * time.Second).UnixMilli()
	requetes := []struct{ nom, sql string }{
		{"Q1 tableau : 5 séries d'une machine, dernière heure",
			`SELECT count(*) FROM points p JOIN series s ON s.id = p.serie
			 WHERE s.instance = 'machine-07' AND s.nom = 'node_filesystem_avail_bytes' AND p.t >= ` + fmt.Sprint(fin-3600_000)},
		{"Q2 moyenne par 5 min, 250 séries, toute la période",
			`SELECT count(*) FROM (SELECT p.serie, p.t // 300000 AS b, avg(p.v) FROM points p JOIN series s ON s.id = p.serie
			 WHERE s.nom = 'node_filesystem_avail_bytes' GROUP BY 1, 2)`},
		{"Q3 dernière valeur, 10 000 séries (alerte)",
			`SELECT count(*) FROM (SELECT p.serie, arg_max(p.v, p.t) FROM points p JOIN series s ON s.id = p.serie
			 WHERE s.nom = 'node_systemd_unit_state' AND p.t >= ` + fmt.Sprint(fin-300_000) + ` GROUP BY 1)`},
		{"Q4 agrégats 5 min d'une partition (toutes séries)",
			`SELECT count(*) FROM (SELECT serie, t // 300000 AS b, avg(v), max(v), arg_max(v, t) FROM ` + parts[0] + `.points GROUP BY 1, 2)`},
	}
	for _, q := range requetes {
		var n int64
		d := time.Now()
		if err := conn.QueryRowContext(ctx, q.sql).Scan(&n); err != nil {
			t.Fatalf("%s : %v", q.nom, err)
		}
		t.Logf("RÉSULTAT : %-52s : %8d lignes en %v", q.nom, n, time.Since(d).Round(time.Microsecond))
	}
	t.Logf("RÉSULTAT : RSS après requêtes %.1f Mo (propre %.1f Mo)", float64(rssKo())/1024, float64(champStatus("RssAnon:"))/1024)

	// Purge : partition entière (DETACH + suppression du fichier).
	d := time.Now()
	exec(`DROP VIEW points`)
	exec(`DETACH ` + parts[0])
	if err := os.Remove(filepath.Join(dir, parts[0]+".duckdb")); err != nil {
		t.Fatal(err)
	}
	t.Logf("RÉSULTAT : purge d'une partition (fichier entier) en %v", time.Since(d).Round(time.Microsecond))

	// Pour comparaison : DELETE dans une partition ne rend pas la place.
	p := parts[1]
	avant, _ := os.Stat(filepath.Join(dir, p+".duckdb"))
	d = time.Now()
	exec(`DELETE FROM ` + p + `.points WHERE t < (SELECT min(t) + (max(t) - min(t)) / 2 FROM ` + p + `.points)`)
	exec(`CHECKPOINT ` + p)
	apres, _ := os.Stat(filepath.Join(dir, p+".duckdb"))
	t.Logf("RÉSULTAT : DELETE de la moitié d'une partition : %v, fichier %.1f → %.1f Mo",
		time.Since(d).Round(time.Millisecond), float64(avant.Size())/(1<<20), float64(apres.Size())/(1<<20))
}
