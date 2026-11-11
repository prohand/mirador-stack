// SPDX-License-Identifier: GPL-3.0-or-later

// Banc d'essai DuckDB pour les logs : même jeu (generer) et mêmes requêtes
// que TestBancLogs (moteur (a) segments zstd, (b) SQLite FTS5).
//
//	go test -run TestBancDuckLogs -v -timeout 60m   (LIGNES=1400000 MEMOIRE=100MB LOT=1000)
package stockage

import (
	"context"
	"database/sql/driver"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	duckdb "github.com/duckdb/duckdb-go/v2"
)

func TestBancDuckLogs(t *testing.T) {
	n := envInt("LIGNES", 1_400_000)
	lot := envInt("LOT", 1000) // lignes par envoi d'agent (un Flush)
	memoire := envTexte("MEMOIRE", "100MB")
	lignes := generer(n)
	var brut int64
	for i := range lignes {
		brut += int64(len(lignes[i].msg))
	}
	runtime.GC()
	anon0 := champStatus("RssAnon:")
	t.Logf("jeu simulé : %d lignes, %.0f Mo de texte, RSS de départ %.0f Mo (propre %.0f Mo, dont le jeu en mémoire)",
		n, float64(brut)/(1<<20), float64(rssKo())/1024, float64(anon0)/1024)
	dir := t.TempDir()
	ctx := context.Background()
	chemin := filepath.Join(dir, "logs.duckdb")
	db := ouvrirDuck(t, chemin, memoire)
	defer db.Close()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `CREATE TABLE logs (ts BIGINT, host VARCHAR, job VARCHAR, unit VARCHAR, level VARCHAR, msg VARCHAR)`); err != nil {
		t.Fatal(err)
	}

	m, err := mesurer(func() error {
		return conn.Raw(func(dc any) error {
			ap, err := duckdb.NewAppender(dc.(driver.Conn), "", "main", "logs")
			if err != nil {
				return err
			}
			for i := range lignes {
				l := &lignes[i]
				if err := ap.AppendRow(l.ts, l.host, l.job, l.unit, l.level, l.msg); err != nil {
					return err
				}
				if i%lot == lot-1 {
					if err := ap.Flush(); err != nil {
						return err
					}
				}
			}
			if err := ap.Close(); err != nil {
				return err
			}
			_, err = dc.(driver.ExecerContext).ExecContext(ctx, "CHECKPOINT", nil)
			return err
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(chemin)
	t.Logf("RÉSULTAT (c) DuckDB écriture : %s, CPU %.1f s (%.2f µs/ligne), écrit %.0f Mo, taille %.1f Mo, pic RSS %.0f Mo, RSS propre %.0f Mo",
		m.duree.Truncate(time.Millisecond), m.cpu, m.cpu*1e6/float64(n), float64(m.ecrit)/(1<<20), float64(info.Size())/(1<<20),
		float64(m.rssPic)/1024, float64(champStatus("RssAnon:"))/1024)

	fin := lignes[len(lignes)-1].ts
	debutJour := lignes[0].ts
	reqs := []struct {
		nom                 string
		tmin                int64
		host, niveau, texte string
	}{
		{"erreurs d'une machine, dernière heure", fin - 3600_000, "machine-07", "err", ""},
		{"texte « Failed password », 24 h", debutJour, "", "", "Failed password"},
		{"texte « connection refused » d'une machine, 24 h", debutJour, "machine-03", "", "connection refused"},
		{"volume total, 24 h", debutJour, "", "", ""},
	}
	for _, q := range reqs {
		// Résultat attendu, calculé en Go sur le jeu.
		attendu := 0
		for i := range lignes {
			l := &lignes[i]
			if l.ts >= q.tmin && l.ts <= fin && (q.host == "" || l.host == q.host) &&
				(q.niveau == "" || l.level == q.niveau) && (q.texte == "" || strings.Contains(l.msg, q.texte)) {
				attendu++
			}
		}
		cond := []string{"ts >= ?", "ts <= ?"}
		args := []any{q.tmin, fin}
		if q.host != "" {
			cond, args = append(cond, "host = ?"), append(args, q.host)
		}
		if q.niveau != "" {
			cond, args = append(cond, "level = ?"), append(args, q.niveau)
		}
		if q.texte != "" {
			cond, args = append(cond, "contains(msg, ?)"), append(args, q.texte)
		}
		var nb int
		d := time.Now()
		if err := conn.QueryRowContext(ctx, "SELECT count(*) FROM logs WHERE "+strings.Join(cond, " AND "), args...).Scan(&nb); err != nil {
			t.Fatal(err)
		}
		duree := time.Since(d)
		// Les 500 lignes les plus récentes correspondantes (ce qu'affiche l'interface).
		d = time.Now()
		rows, err := conn.QueryContext(ctx, "SELECT ts, host, msg FROM logs WHERE "+strings.Join(cond, " AND ")+" ORDER BY ts DESC LIMIT 500", args...)
		if err != nil {
			t.Fatal(err)
		}
		k := 0
		for rows.Next() {
			k++
		}
		rows.Close()
		t.Logf("RÉSULTAT (c) « %s » : %d lignes (attendu %d) : comptage %v, 500 plus récentes %v",
			q.nom, nb, attendu, duree.Round(time.Microsecond), time.Since(d).Round(time.Microsecond))
		if nb != attendu {
			t.Errorf("résultat faux : %d au lieu de %d", nb, attendu)
		}
	}
	runtime.GC()
	t.Logf("RÉSULTAT (c) mémoire propre ajoutée par DuckDB après requêtes : %.0f Mo (RSS propre %.0f Mo, dont le jeu en mémoire)",
		float64(champStatus("RssAnon:")-anon0)/1024, float64(champStatus("RssAnon:"))/1024)

	// Purge : DELETE de 12 h (la place n'est pas rendue au disque) ; avec une
	// base par jour, la purge serait la suppression du fichier.
	moitie := debutJour + (fin-debutJour)/2
	mp, err := mesurer(func() error {
		if _, err := conn.ExecContext(ctx, `DELETE FROM logs WHERE ts < ?`, moitie); err != nil {
			return err
		}
		_, err := conn.ExecContext(ctx, `CHECKPOINT`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	apres, _ := os.Stat(chemin)
	t.Logf("RÉSULTAT (c) DELETE de 12 h : %s, écrit %.0f Mo, fichier %.1f → %.1f Mo",
		mp.duree.Truncate(time.Millisecond), float64(mp.ecrit)/(1<<20), float64(info.Size())/(1<<20), float64(apres.Size())/(1<<20))
}
