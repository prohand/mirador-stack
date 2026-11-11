// SPDX-License-Identifier: GPL-3.0-or-later

// Banc d'essai du stockage des logs (étape 3, SPEC.md § 7.2) : comparaison
// chiffrée de deux moteurs, sur le même jeu de logs simulé.
//
//	(a) segments compressés zstd + petit index par bloc (machine, niveau,
//	    source, intervalle de temps) ;
//	(b) SQLite FTS5 (modernc.org/sqlite, pur Go).
//
// Profil simulé d'après les mesures du 07/11 : ~1,4 million de lignes par jour
// pour 25 machines, 60 % Docker, ~190 octets par ligne.
//
//	go test -run TestBancLogs -v -timeout 60m        (LIGNES=1400000 par défaut)
package stockage

import (
	"bytes"
	"database/sql"
	"encoding/binary"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	_ "modernc.org/sqlite"
)

// ligne : une ligne de log simulée.
type ligne struct {
	ts                     int64 // ms
	host, job, unit, level string
	msg                    string
}

var (
	chemins  = []string{"/", "/api/v1/items", "/login", "/static/app.js", "/health", "/api/v1/users/42", "/favicon.ico"}
	services = []string{"nginx.service", "ssh.service", "cron.service", "docker.service", "systemd-timesyncd.service", "unattended-upgrades.service"}
	niveaux  = []string{"info", "info", "info", "notice", "warning", "err"}
)

// generer produit n lignes réparties sur 24 h, pour 25 machines.
func generer(n int) []ligne {
	r := rand.New(rand.NewPCG(1, 2))
	debut := time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC).UnixMilli()
	pas := float64(24*3600*1000) / float64(n)
	out := make([]ligne, n)
	for i := range out {
		h := fmt.Sprintf("machine-%02d", r.IntN(25))
		l := ligne{ts: debut + int64(float64(i)*pas), host: h}
		ip := fmt.Sprintf("10.0.%d.%d", r.IntN(4), r.IntN(254)+1)
		switch x := r.IntN(100); {
		case x < 60: // conteneurs : accès web, erreurs applicatives
			l.job, l.unit, l.level = "docker", fmt.Sprintf("app-%d", r.IntN(8)), ""
			if r.IntN(50) == 0 {
				l.msg = fmt.Sprintf("ERROR [worker-%d] échec de la requête id=%x : connection refused", r.IntN(16), r.Uint64())
			} else {
				l.msg = fmt.Sprintf(`%s - - [08/Nov/2026:%02d:%02d:%02d +0100] "GET %s HTTP/1.1" %d %d "-" "Mozilla/5.0 (X11; Linux x86_64)"`,
					ip, r.IntN(24), r.IntN(60), r.IntN(60), chemins[r.IntN(len(chemins))], []int{200, 200, 200, 304, 404, 500}[r.IntN(6)], r.IntN(50000))
			}
		case x < 70: // pare-feu du noyau
			l.job, l.unit, l.level = "journal", "", "warning"
			l.msg = fmt.Sprintf("BLOQUE: IN=eth0 OUT= MAC=00:16:3e:aa:bb:cc SRC=%d.%d.%d.%d DST=10.0.0.5 LEN=60 PROTO=TCP SPT=%d DPT=%d",
				r.IntN(223)+1, r.IntN(256), r.IntN(256), r.IntN(256), 1024+r.IntN(60000), []int{22, 23, 80, 443, 3389, 8080}[r.IntN(6)])
		case x < 80: // ssh
			l.job, l.unit, l.level = "journal", "ssh.service", "info"
			if r.IntN(3) == 0 {
				l.level = "notice"
				l.msg = fmt.Sprintf("Failed password for invalid user admin from %s port %d ssh2", ip, 1024+r.IntN(60000))
			} else {
				l.msg = fmt.Sprintf("Accepted publickey for kevin from %s port %d ssh2: ED25519 SHA256:%x", ip, 1024+r.IntN(60000), r.Uint64())
			}
		default: // systemd et autres
			l.job, l.unit = "journal", services[r.IntN(len(services))]
			l.level = niveaux[r.IntN(len(niveaux))]
			l.msg = fmt.Sprintf("%s: %s (pid %d) duration=%dms", l.unit, []string{"Started", "Finished", "Deactivated successfully", "Failed with result 'exit-code'"}[r.IntN(4)], r.IntN(99999), r.IntN(5000))
		}
		out[i] = l
	}
	return out
}

// --- (a) segments zstd + index -------------------------------------------------

// blocIndex : ce qu'on sait d'un bloc sans le décompresser.
type blocIndex struct {
	off, taille int64
	tmin, tmax  int64
	hosts       map[string]bool
	niveaux     map[string]bool
	jobs        map[string]bool
}

type magasinSegments struct {
	f      *os.File
	enc    *zstd.Encoder
	dec    *zstd.Decoder
	index  []blocIndex
	cour   bytes.Buffer
	courIx blocIndex
	off    int64
}

const tailleBloc = 256 << 10

func ouvrirSegments(chemin string) (*magasinSegments, error) {
	f, err := os.Create(chemin)
	if err != nil {
		return nil, err
	}
	enc, _ := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithEncoderConcurrency(1))
	dec, _ := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1))
	m := &magasinSegments{f: f, enc: enc, dec: dec}
	m.nouveauBloc()
	return m, nil
}

func (m *magasinSegments) nouveauBloc() {
	m.cour.Reset()
	m.courIx = blocIndex{tmin: 1<<62 - 1, hosts: map[string]bool{}, niveaux: map[string]bool{}, jobs: map[string]bool{}}
}

// ajouter : format d'une ligne dans un bloc :
// ts(varint) | host | job | unit | level | msg  (chaînes préfixées par leur longueur)
func (m *magasinSegments) ajouter(l *ligne) error {
	var tmp [binary.MaxVarintLen64]byte
	m.cour.Write(tmp[:binary.PutVarint(tmp[:], l.ts)])
	for _, s := range []string{l.host, l.job, l.unit, l.level, l.msg} {
		m.cour.Write(tmp[:binary.PutUvarint(tmp[:], uint64(len(s)))])
		m.cour.WriteString(s)
	}
	ix := &m.courIx
	ix.tmin, ix.tmax = min(ix.tmin, l.ts), max(ix.tmax, l.ts)
	ix.hosts[l.host], ix.niveaux[l.level], ix.jobs[l.job] = true, true, true
	if m.cour.Len() >= tailleBloc {
		return m.fermerBloc()
	}
	return nil
}

func (m *magasinSegments) fermerBloc() error {
	if m.cour.Len() == 0 {
		return nil
	}
	c := m.enc.EncodeAll(m.cour.Bytes(), nil)
	if _, err := m.f.Write(c); err != nil {
		return err
	}
	m.courIx.off, m.courIx.taille = m.off, int64(len(c))
	m.off += int64(len(c))
	m.index = append(m.index, m.courIx)
	m.nouveauBloc()
	return nil
}

// chercher parcourt les blocs dont l'index correspond, puis les lignes.
func (m *magasinSegments) chercher(tmin, tmax int64, host, niveau, texte string, compter func(*ligne)) (blocsLus int, err error) {
	var brut, plat []byte
	for _, ix := range m.index {
		if ix.tmax < tmin || ix.tmin > tmax || (host != "" && !ix.hosts[host]) || (niveau != "" && !ix.niveaux[niveau]) {
			continue
		}
		blocsLus++
		if cap(brut) < int(ix.taille) {
			brut = make([]byte, ix.taille)
		}
		brut = brut[:ix.taille]
		if _, err := m.f.ReadAt(brut, ix.off); err != nil {
			return blocsLus, err
		}
		if plat, err = m.dec.DecodeAll(brut, plat[:0]); err != nil {
			return blocsLus, err
		}
		for p := plat; len(p) > 0; {
			var l ligne
			ts, n := binary.Varint(p)
			p = p[n:]
			l.ts = ts
			champs := [5]*string{&l.host, &l.job, &l.unit, &l.level, &l.msg}
			for _, c := range champs {
				t, n := binary.Uvarint(p)
				p = p[n:]
				*c = string(p[:t])
				p = p[t:]
			}
			if l.ts < tmin || l.ts > tmax || (host != "" && l.host != host) || (niveau != "" && l.level != niveau) ||
				(texte != "" && !strings.Contains(l.msg, texte)) {
				continue
			}
			compter(&l)
		}
	}
	return blocsLus, nil
}

// --- (b) SQLite FTS5 --------------------------------------------------------------

func ouvrirFTS(chemin string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", chemin+"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	for _, q := range []string{
		`CREATE TABLE logs(id INTEGER PRIMARY KEY, ts INTEGER, host TEXT, job TEXT, unit TEXT, level TEXT, msg TEXT)`,
		`CREATE INDEX logs_host_ts ON logs(host, ts)`,
		`CREATE INDEX logs_ts ON logs(ts)`,
		`CREATE VIRTUAL TABLE logs_fts USING fts5(msg, content='logs', content_rowid='id')`,
	} {
		if _, err := db.Exec(q); err != nil {
			return nil, err
		}
	}
	return db, nil
}

func insererFTS(db *sql.DB, lignes []ligne) error {
	const lot = 5000
	for i := 0; i < len(lignes); i += lot {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		st, _ := tx.Prepare(`INSERT INTO logs(ts, host, job, unit, level, msg) VALUES (?,?,?,?,?,?)`)
		ft, _ := tx.Prepare(`INSERT INTO logs_fts(rowid, msg) VALUES (last_insert_rowid(), ?)`)
		for _, l := range lignes[i:min(i+lot, len(lignes))] {
			if _, err := st.Exec(l.ts, l.host, l.job, l.unit, l.level, l.msg); err != nil {
				return err
			}
			if _, err := ft.Exec(l.msg); err != nil {
				return err
			}
		}
		st.Close()
		ft.Close()
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// --- mesures ------------------------------------------------------------------------

func ecritOctets() int64 {
	b, _ := os.ReadFile("/proc/self/io")
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "write_bytes:") {
			v, _ := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(l, "write_bytes:")), 10, 64)
			return v
		}
	}
	return 0
}

type mesure struct {
	cpu    float64
	duree  time.Duration
	ecrit  int64
	rssPic int64
}

func mesurer(f func() error) (mesure, error) {
	runtime.GC()
	c0, t0, w0 := cpuSecondes(), time.Now(), ecritOctets()
	arret := make(chan struct{})
	pic := make(chan int64)
	go func() {
		var p int64
		for {
			if r := rssKo(); r > p {
				p = r
			}
			select {
			case <-arret:
				pic <- p
				return
			case <-time.After(50 * time.Millisecond):
			}
		}
	}()
	err := f()
	close(arret)
	return mesure{cpu: cpuSecondes() - c0, duree: time.Since(t0), ecrit: ecritOctets() - w0, rssPic: <-pic}, err
}

func TestBancLogs(t *testing.T) {
	n := envInt("LIGNES", 1_400_000)
	lignes := generer(n)
	var brut int64
	for i := range lignes {
		brut += int64(len(lignes[i].msg))
	}
	t.Logf("jeu simulé : %d lignes (1 jour, 25 machines), %.0f Mo de texte, RSS de départ %.0f Mo", n, float64(brut)/(1<<20), float64(rssKo())/1024)
	dir := t.TempDir()
	fin := lignes[len(lignes)-1].ts
	debutJour := lignes[0].ts

	// (a) segments zstd
	var seg *magasinSegments
	ma, err := mesurer(func() error {
		var err error
		if seg, err = ouvrirSegments(filepath.Join(dir, "segments.zst")); err != nil {
			return err
		}
		for i := range lignes {
			if err := seg.ajouter(&lignes[i]); err != nil {
				return err
			}
		}
		if err := seg.fermerBloc(); err != nil {
			return err
		}
		return seg.f.Sync()
	})
	if err != nil {
		t.Fatal(err)
	}
	infoA, _ := os.Stat(filepath.Join(dir, "segments.zst"))
	t.Logf("(a) écriture : %s, CPU %.1f s (%.2f µs/ligne), écrit %.0f Mo, taille %.1f Mo (%d blocs), pic RSS %.0f Mo",
		ma.duree.Truncate(time.Millisecond), ma.cpu, ma.cpu*1e6/float64(n), float64(ma.ecrit)/(1<<20), float64(infoA.Size())/(1<<20), len(seg.index), float64(ma.rssPic)/1024)

	// (b) SQLite FTS5
	var db *sql.DB
	mb, err := mesurer(func() error {
		var err error
		if db, err = ouvrirFTS(filepath.Join(dir, "logs.db")); err != nil {
			return err
		}
		if err := insererFTS(db, lignes); err != nil {
			return err
		}
		_, err = db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tailleB := tailleDossier(dir) - infoA.Size()
	t.Logf("(b) écriture : %s, CPU %.1f s (%.2f µs/ligne), écrit %.0f Mo, taille %.1f Mo, pic RSS %.0f Mo",
		mb.duree.Truncate(time.Millisecond), mb.cpu, mb.cpu*1e6/float64(n), float64(mb.ecrit)/(1<<20), float64(tailleB)/(1<<20), float64(mb.rssPic)/1024)

	// Requêtes (celles des dashboards : erreurs d'une machine, recherche de
	// texte, volume par machine).
	type requete struct {
		nom                 string
		tmin                int64
		host, niveau, texte string
		sqlTexte            string
	}
	reqs := []requete{
		{"erreurs d'une machine, dernière heure", fin - 3600_000, "machine-07", "err", "", ""},
		{"texte « Failed password », 24 h", debutJour, "", "", "Failed password", `"Failed password"`},
		{"texte « connection refused » d'une machine, 24 h", debutJour, "machine-03", "", "connection refused", `"connection refused"`},
		{"volume total, 24 h", debutJour, "", "", "", ""},
	}
	for _, q := range reqs {
		var na int
		var blocs int
		da, _ := mesurer(func() error {
			var err error
			blocs, err = seg.chercher(q.tmin, fin, q.host, q.niveau, q.texte, func(*ligne) { na++ })
			return err
		})
		var nb int
		db2, _ := mesurer(func() error {
			cond := []string{"l.ts >= ?", "l.ts <= ?"}
			args := []any{q.tmin, fin}
			from := "logs l"
			if q.host != "" {
				cond, args = append(cond, "l.host = ?"), append(args, q.host)
			}
			if q.niveau != "" {
				cond, args = append(cond, "l.level = ?"), append(args, q.niveau)
			}
			if q.sqlTexte != "" {
				from = "logs_fts f JOIN logs l ON l.id = f.rowid"
				cond, args = append(cond, "logs_fts MATCH ?"), append(args, q.sqlTexte)
			}
			return db.QueryRow("SELECT count(*) FROM "+from+" WHERE "+strings.Join(cond, " AND "), args...).Scan(&nb)
		})
		t.Logf("requête « %s » : (a) %d lignes en %s (%d blocs lus) | (b) %d lignes en %s",
			q.nom, na, da.duree.Truncate(time.Microsecond), blocs, nb, db2.duree.Truncate(time.Microsecond))
		if na != nb {
			t.Errorf("résultats différents : %d contre %d", na, nb)
		}
	}

	// Purge d'une période (rétention) : (a) supprimer des fichiers entiers ;
	// (b) DELETE ligne par ligne + mise à jour de l'index plein texte.
	moitie := debutJour + (fin-debutJour)/2
	dp, err := mesurer(func() error {
		_, err := db.Exec(`DELETE FROM logs_fts WHERE rowid IN (SELECT id FROM logs WHERE ts < ?)`, moitie)
		if err != nil {
			return err
		}
		_, err = db.Exec(`DELETE FROM logs WHERE ts < ?`, moitie)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("(b) purge de 12 h : %s, écrit %.0f Mo (en (a) : suppression de fichiers entiers, quasi instantanée)",
		dp.duree.Truncate(time.Millisecond), float64(dp.ecrit)/(1<<20))
}
