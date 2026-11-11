// SPDX-License-Identifier: GPL-3.0-or-later

package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"mirador-stack/internal/agent/logs"
	"mirador-stack/internal/agent/metrics"
	"mirador-stack/internal/agent/remotewrite"
	"mirador-stack/internal/protocole"
)

// TestChargeServeur : test de charge de l'étape 3 (partie 7). Le vrai binaire
// du serveur tourne dans son propre processus, avec GOMEMLIMIT=200MiB comme
// dans le conteneur ; 50 agents simulés lui parlent en HTTP comme de vrais
// agents (métriques toutes les 30 s, logs, battements, configuration). La
// RAM, le CPU et les écritures du serveur sont lus dans /proc/<pid>.
//
// Avant la mesure, RATTRAPAGE (défaut 2h30) de données passées est envoyé
// d'un coup : la première compaction de la TSDB (pic de mémoire) a ainsi lieu
// pendant la mesure.
//
//	CHARGE=1 DUREE=2h AGENTS=50 SORTIE=/chemin/mesures.csv go test ./internal/server -run TestChargeServeur -v -timeout 3h
func TestChargeServeur(t *testing.T) {
	if os.Getenv("CHARGE") == "" {
		t.Skip("CHARGE=1 pour lancer le test de charge")
	}
	duree, _ := time.ParseDuration(envOu("DUREE", "10m"))
	rattrapage, _ := time.ParseDuration(envOu("RATTRAPAGE", "2h30m"))
	agents, _ := strconv.Atoi(envOu("AGENTS", "50"))
	dir := t.TempDir()

	// Binaire du serveur, compilé comme pour la production.
	binaire := filepath.Join(dir, "mirador-server")
	compil := exec.Command("go", "build", "-trimpath", "-o", binaire, "mirador-stack/cmd/mirador-server") // #nosec G204 -- test
	compil.Env = append(os.Environ(), "CGO_ENABLED=0")
	if sortie, err := compil.CombinedOutput(); err != nil {
		t.Fatalf("compilation : %v\n%s", err, sortie)
	}
	cle := filepath.Join(dir, "cle")
	_ = os.WriteFile(cle, []byte("AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=\n"), 0o400)
	agentsAdr, ifaceAdr := portLibre(t), portLibre(t)
	conf := filepath.Join(dir, "server.conf")
	_ = os.WriteFile(conf, []byte(fmt.Sprintf("MIRADOR_ECOUTE_AGENTS=%s\nMIRADOR_ECOUTE_INTERFACE=%s\nMIRADOR_DONNEES=%s\nMIRADOR_FICHIER_CLE=%s\n",
		agentsAdr, ifaceAdr, filepath.Join(dir, "donnees"), cle)), 0o600)

	ctx, annuler := context.WithCancel(context.Background())
	defer annuler()
	srv := exec.CommandContext(ctx, binaire, "-config", conf) // #nosec G204 -- test
	srv.Env = []string{"GOMEMLIMIT=200MiB", "PATH=/usr/bin:/bin"}
	if g := os.Getenv("GOGC_SERVEUR"); g != "" { // essai de réglage du ramasse-miettes
		srv.Env = append(srv.Env, "GOGC="+g)
	}
	journalSrv, _ := os.Create(filepath.Join(dir, "serveur.log"))
	srv.Stdout, srv.Stderr = journalSrv, journalSrv
	srv.Cancel = func() error { return srv.Process.Signal(syscall.SIGTERM) }
	srv.WaitDelay = 30 * time.Second
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	pid := srv.Process.Pid
	client := &http.Client{Timeout: 30 * time.Second}
	for i := 0; ; i++ {
		if r, err := client.Get("http://" + agentsAdr + "/sante"); err == nil {
			r.Body.Close()
			break
		}
		if i > 100 {
			t.Fatal("le serveur ne démarre pas")
		}
		time.Sleep(100 * time.Millisecond)
	}
	// Tokens par la commande du serveur (comme en production).
	tokens := make([]string, agents)
	for i := range tokens {
		sortie, err := exec.Command(binaire, "-config", conf, "token", fmt.Sprintf("agent-%02d", i)).Output() // #nosec G204 -- test
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range strings.Split(string(sortie), "\n") {
			if strings.HasPrefix(l, "mirador_") {
				tokens[i] = strings.TrimSpace(l)
			}
		}
	}

	// Étape 4 : les règles actuelles (68) en plus des règles mirador, comme
	// en production après l'import (ALERTES=0 pour mesurer sans).
	if envOu("ALERTES", "1") == "1" {
		for _, f := range []string{"system.yml", "equipements.yml"} {
			sortie, err := exec.Command(binaire, "-config", conf, "regles-importer", // #nosec G204 -- test
				filepath.Join("alertes", "testdata", "regles", f)).CombinedOutput()
			if err != nil {
				t.Fatalf("import %s : %v\n%s", f, err, sortie)
			}
		}
	}

	sim := make([]*agentSimule, agents)
	for i := range sim {
		sim[i] = nouvelAgentSimule(i, tokens[i], agentsAdr, client)
	}
	// Rattrapage : données passées, envoyées comme une file d'attente qui se
	// vide après une coupure (chaque agent en parallèle, en respectant les
	// limites de débit du serveur : 429 → attendre Retry-After, comme l'agent).
	debutRattrapage := time.Now()
	cycles := int(rattrapage / (30 * time.Second))
	origine := time.Now().Add(-rattrapage)
	var wgR sync.WaitGroup
	erreursR := make(chan error, agents)
	for _, a := range sim {
		wgR.Add(1)
		go func() {
			defer wgR.Done()
			for c := 0; c < cycles; c++ {
				if err := a.metriques(origine.Add(time.Duration(c) * 30 * time.Second)); err != nil {
					erreursR <- err
					return
				}
			}
		}()
	}
	wgR.Wait()
	close(erreursR)
	for err := range erreursR {
		t.Fatal(err)
	}
	t.Logf("rattrapage de %s (%d cycles × %d agents) envoyé en %s", rattrapage, cycles, agents, time.Since(debutRattrapage).Round(time.Second))

	// Mesure en temps réel.
	var sortie *os.File
	if chemin := os.Getenv("SORTIE"); chemin != "" {
		sortie, _ = os.Create(chemin) // #nosec G304 -- test
		defer sortie.Close()
		fmt.Fprintln(sortie, "minutes,rss_mo,propre_mo,cpu_pct,ecrit_mo,disque_mo,requetes,erreurs")
	}
	var requetes, erreurs atomic.Int64
	fin := time.Now().Add(duree)
	var wg sync.WaitGroup
	for _, a := range sim {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a.tourner(ctx, fin, &requetes, &erreurs)
		}()
	}
	p0 := lireProc(pid)
	debut := time.Now()
	var maxRSS, maxPropre, sommePropre float64
	n := 0
	prec := p0
	for time.Now().Before(fin) {
		time.Sleep(time.Minute)
		p := lireProc(pid)
		cpu := (p.cpu - prec.cpu) / p.t.Sub(prec.t).Seconds() * 100
		disque := float64(tailleDossier(filepath.Join(dir, "donnees"))) / (1 << 20)
		maxRSS, maxPropre = max(maxRSS, p.rss), max(maxPropre, p.propre)
		sommePropre += p.propre
		n++
		ligne := fmt.Sprintf("%.0f,%.1f,%.1f,%.2f,%.1f,%.1f,%d,%d", time.Since(debut).Minutes(), p.rss, p.propre, cpu,
			float64(p.ecrit-p0.ecrit)/(1<<20), disque, requetes.Load(), erreurs.Load())
		t.Log(ligne)
		if sortie != nil {
			fmt.Fprintln(sortie, ligne)
		}
		prec = p
	}
	wg.Wait()
	p := lireProc(pid)
	heures := p.t.Sub(p0.t).Hours()
	t.Logf("RÉSULTAT : %d agents, %s de mesure, %d requêtes, %d erreurs", agents, duree, requetes.Load(), erreurs.Load())
	t.Logf("RÉSULTAT : CPU moyen %.2f %% d'un cœur", (p.cpu-p0.cpu)/(heures*3600)*100)
	t.Logf("RÉSULTAT : RSS max %.1f Mo ; RAM propre max %.1f Mo, moyenne %.1f Mo", maxRSS, maxPropre, sommePropre/float64(max(n, 1)))
	t.Logf("RÉSULTAT : écrit %.0f Mo en %.1f h → ~%.0f Mo par jour (%.1f Ko/s)", float64(p.ecrit-p0.ecrit)/(1<<20), heures,
		float64(p.ecrit-p0.ecrit)/(1<<20)/heures*24, float64(p.ecrit-p0.ecrit)/1024/(heures*3600))
	t.Logf("RÉSULTAT : disque des données %.0f Mo", float64(tailleDossier(filepath.Join(dir, "donnees")))/(1<<20))
	if erreurs.Load() > 0 {
		t.Errorf("%d requêtes en erreur", erreurs.Load())
	}
	annuler()
	_ = srv.Wait()
	if b, _ := os.ReadFile(filepath.Join(dir, "serveur.log")); bytes.Contains(b, []byte(`"level":"ERROR"`)) {
		t.Errorf("erreurs dans le journal du serveur :\n%s", extraitErreurs(b))
	}
}

func envOu(nom, defaut string) string {
	if v := os.Getenv(nom); v != "" {
		return v
	}
	return defaut
}

func extraitErreurs(b []byte) string {
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if strings.Contains(l, `"level":"ERROR"`) && len(out) < 10 {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

// --- Mesures du processus ------------------------------------------------------------

type mesureProc struct {
	t           time.Time
	rss, propre float64 // Mo
	cpu         float64 // secondes
	ecrit       int64   // octets
}

func lireProc(pid int) mesureProc {
	m := mesureProc{t: time.Now()}
	if b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid)); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			f := strings.Fields(l)
			if len(f) < 2 {
				continue
			}
			v, _ := strconv.ParseFloat(f[1], 64)
			switch f[0] {
			case "VmRSS:":
				m.rss = v / 1024
			case "RssAnon:":
				m.propre = v / 1024
			}
		}
	}
	if b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
		// Champs 14 et 15 (utime, stime), après le nom entre parenthèses.
		s := string(b)
		f := strings.Fields(s[strings.LastIndexByte(s, ')')+2:])
		u, _ := strconv.ParseFloat(f[11], 64)
		st, _ := strconv.ParseFloat(f[12], 64)
		m.cpu = (u + st) / 100 // ticks de 10 ms
	}
	if f, err := os.Open(fmt.Sprintf("/proc/%d/io", pid)); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if v, ok := strings.CutPrefix(sc.Text(), "write_bytes: "); ok {
				m.ecrit, _ = strconv.ParseInt(v, 10, 64)
			}
		}
		_ = f.Close()
	}
	return m
}

// --- Agent simulé --------------------------------------------------------------------

// agentSimule : ~600 séries au profil réaliste (états systemd, compteurs,
// jauges), logs, battement, configuration.
type agentSimule struct {
	nom, token, adr string
	client          *http.Client
	lot             *metrics.Batch
	valeurs         []float64
	enc             remotewrite.Encoder
	r               *rand.Rand
	etag            string
}

func nouvelAgentSimule(i int, token, adr string, client *http.Client) *agentSimule {
	return &agentSimule{nom: fmt.Sprintf("agent-%02d", i), token: token, adr: adr, client: client,
		lot:     metrics.NewBatch(600, []metrics.Label{metrics.L("instance", "x"), metrics.L("job", "integrations/unix")}),
		valeurs: make([]float64, 600), r: rand.New(rand.NewPCG(uint64(i), 42))} // #nosec G404 -- test
}

func (a *agentSimule) requete(methode, chemin, encodage string, corps []byte, entetes ...string) (*http.Response, error) {
	req, _ := http.NewRequest(methode, "http://"+a.adr+chemin, bytes.NewReader(corps))
	req.Header.Set("Authorization", "Bearer "+a.token)
	req.Header.Set(protocole.EnteteHeure, strconv.FormatInt(time.Now().UnixMilli(), 10))
	req.Header.Set(protocole.EnteteVersion, "1.0.0")
	if encodage != "" {
		req.Header.Set("Content-Encoding", encodage)
	}
	for i := 0; i+1 < len(entetes); i += 2 {
		req.Header.Set(entetes[i], entetes[i+1])
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp, nil
}

func (a *agentSimule) metriques(quand time.Time) error {
	a.lot.Reset(quand.UnixMilli())
	k := 0
	ajoute := func(nom string, v float64, l ...metrics.Label) {
		a.lot.Add(nom, v, l...)
		k++
	}
	for u := 0; u < 200; u++ {
		ajoute("node_systemd_unit_state", 0, metrics.L("name", "service-"+strconv.Itoa(u)+".service"), metrics.L("state", "failed"))
	}
	for u := 0; u < 150; u++ {
		if a.r.IntN(5000) == 0 {
			a.valeurs[k]++
		}
		ajoute("node_systemd_service_restart_total", a.valeurs[k], metrics.L("name", "service-"+strconv.Itoa(u)+".service"))
	}
	for c := 0; c < 4; c++ {
		for _, m := range []string{"user", "nice", "system", "idle", "iowait", "irq", "softirq", "steal"} {
			a.valeurs[k] += float64(a.r.IntN(300)) / 100
			ajoute("node_cpu_seconds_total", a.valeurs[k], metrics.L("cpu", strconv.Itoa(c)), metrics.L("mode", m))
		}
	}
	for k < 600 {
		if a.r.IntN(3) == 0 {
			a.valeurs[k] = float64(int64((a.valeurs[k]+(a.r.Float64()-0.5)*20)*100)) / 100
		}
		ajoute("divers_"+strconv.Itoa(k), a.valeurs[k])
	}
	corps := a.enc.Encode(nil, a.lot)
	for essai := 0; essai < 100; essai++ {
		resp, err := a.requete(http.MethodPost, protocole.CheminMetriques, "snappy", corps)
		if err != nil {
			return err
		}
		switch resp.StatusCode {
		case http.StatusNoContent:
			return nil
		case http.StatusTooManyRequests, http.StatusServiceUnavailable:
			attente, _ := strconv.Atoi(resp.Header.Get("Retry-After"))
			time.Sleep(time.Duration(max(attente, 1)) * time.Second)
		default:
			return fmt.Errorf("métriques %s : HTTP %d", a.nom, resp.StatusCode)
		}
	}
	return fmt.Errorf("métriques %s : toujours limité", a.nom)
}

// tourner : rythme d'un vrai agent, décalé au hasard.
func (a *agentSimule) tourner(ctx context.Context, fin time.Time, requetes, erreurs *atomic.Int64) {
	verifier := func(resp *http.Response, err error, attendus ...int) {
		requetes.Add(1)
		if err != nil {
			erreurs.Add(1)
			return
		}
		for _, c := range attendus {
			if resp.StatusCode == c {
				return
			}
		}
		erreurs.Add(1)
	}
	time.Sleep(time.Duration(a.r.IntN(30000)) * time.Millisecond)
	lotLogs := logs.NewLot("x", "journal", "", "", 1<<20)
	messages := []string{"Accepted publickey for root from 192.0.2.%d port %d ssh2", "pam_unix(cron:session): session closed for user root",
		"GET /api/v1/query?query=up 200 %dms", "kernel: [UFW BLOCK] IN=eth0 SRC=203.0.113.%d DST=198.51.100.1 DPT=%d",
		"systemd[1]: Started session %d of user root.", "nginx: upstream timed out (110: Connection timed out) %d %d"}
	cycle := 0
	for time.Now().Before(fin) && ctx.Err() == nil {
		debut := time.Now()
		requetes.Add(1)
		if err := a.metriques(debut); err != nil {
			erreurs.Add(1)
		}
		for i := 0; i < 8+a.r.IntN(5); i++ { // ~10 lignes / 30 s / agent ≈ 1,4 M lignes / jour pour 50
			m := messages[a.r.IntN(len(messages))]
			if strings.Count(m, "%d") == 2 {
				m = fmt.Sprintf(m, a.r.IntN(255), a.r.IntN(65535))
			} else if strings.Contains(m, "%d") {
				m = fmt.Sprintf(m, a.r.IntN(5000))
			}
			lotLogs.Ajouter(time.Now(), m, logs.Champs{{"unit", "ssh.service"}, {"level", "info"}})
		}
		corps, _ := lotLogs.Vider()
		resp, err := a.requete(http.MethodPost, protocole.CheminLogs, "gzip", corps)
		verifier(resp, err, http.StatusNoContent)
		if cycle%2 == 0 {
			b, _ := json.Marshal(protocole.Battement{Version: "1.0.0", Architecture: "amd64", Demarrage: time.Now().Add(-time.Hour)})
			resp, err := a.requete(http.MethodPost, protocole.CheminBattement, "", b)
			verifier(resp, err, http.StatusNoContent)
		}
		if cycle%10 == 0 {
			resp, err := a.requete(http.MethodGet, protocole.CheminConfig, "", nil, "If-None-Match", a.etag)
			verifier(resp, err, http.StatusOK, http.StatusNotModified)
			if resp != nil {
				a.etag = resp.Header.Get("ETag")
			}
		}
		cycle++
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Until(debut.Add(30 * time.Second))):
		}
	}
}

func tailleDossier(d string) int64 {
	var n int64
	_ = filepath.WalkDir(d, func(_ string, e os.DirEntry, err error) error {
		if err == nil && !e.IsDir() {
			if i, err := e.Info(); err == nil {
				n += i.Size()
			}
		}
		return nil
	})
	return n
}
