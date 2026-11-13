// SPDX-License-Identifier: GPL-3.0-or-later

// Package agent assemble les collecteurs, l'encodeur et l'envoi.
//
// Étape 2 (SPEC.md § 13) : l'agent envoie directement vers VictoriaMetrics
// (remote write) derrière le nginx actuel, en parallèle d'Alloy.
package agent

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/metrics"
	"syscall"
	"time"

	"mirador-stack/internal/agent/collect"
	"mirador-stack/internal/agent/config"
	"mirador-stack/internal/agent/envoi"
	mm "mirador-stack/internal/agent/metrics"
	"mirador-stack/internal/agent/remotewrite"
	"mirador-stack/internal/version"
)

// Agent tient l'état d'un agent en marche.
type Agent struct {
	cfg         config.Agent
	journal     *slog.Logger
	collecteurs []collect.Collecteur
	lot         *mm.Batch
	enc         remotewrite.Encoder
	file        *envoi.File
	exp         *envoi.Expediteur
	logs        *chaineLogs // nil si les logs sont désactivés

	// Statistiques par collecteur (métriques internes).
	duree   map[string]time.Duration
	erreurs map[string]uint64

	cycle   uint64
	interne interne
}

// Nouveau prépare l'agent (rien ne tourne encore).
func Nouveau(cfg config.Agent, token string, journal *slog.Logger) *Agent {
	common := []mm.Label{
		mm.L("instance", cfg.Nom),
		mm.L("job", "node"),
	}
	// Site et env facultatifs avec mirador-server : il les impose d'après la
	// fiche de la machine (choisis dans l'interface).
	if cfg.Site != "" {
		common = append(common, mm.L("site", cfg.Site))
	}
	if cfg.Env != "" {
		common = append(common, mm.L("env", cfg.Env))
	}
	a := &Agent{
		cfg:         cfg,
		journal:     journal,
		collecteurs: Collecteurs(cfg, journal),
		lot:         mm.NewBatch(1024, common),
		file:        envoi.NewFile(cfg.TamponMetriques),
		duree:       make(map[string]time.Duration),
		erreurs:     make(map[string]uint64),
	}
	a.interne.init(cfg.RacineProc)
	a.logs = nouvelleChaineLogs(cfg, token, journal)
	a.exp = envoi.NewExpediteur(a.file, envoi.Options{
		URL:   cfg.URLMetriques,
		Token: token,
		Entetes: cfg.EntetesMachine(map[string]string{
			"Content-Type":                      "application/x-protobuf",
			"Content-Encoding":                  "snappy",
			"X-Prometheus-Remote-Write-Version": "0.1.0",
		}),
		Journal: journal,
	})
	return a
}

// Collecteurs renvoie la liste des collecteurs selon la configuration.
func Collecteurs(cfg config.Agent, journal *slog.Logger) []collect.Collecteur {
	liste := []collect.Collecteur{
		collect.NewCPU(cfg.RacineProc),
		collect.NewCharge(cfg.RacineProc),
		collect.NewMemoire(cfg.RacineProc),
		collect.NewVMStat(cfg.RacineProc),
		collect.NewPression(cfg.RacineProc),
		collect.NewDisques(cfg.RacineProc, cfg.ExclureDisques, cfg.IgnorerDisques...),
		collect.NewReseau(cfg.RacineProc, cfg.ExclureInterfaces, cfg.IgnorerInterfaces...),
		collect.NewSystemesFichiers(cfg.RacineProc, cfg.ExclureMontages, cfg.ExclureTypesFS, cfg.ExclureTypesFSDistant, cfg.IgnorerMontages...),
		collect.NewUname(),
		// Ventilateurs, ZFS, RAID logiciel : envoyés s'ils sont activés,
		// sinon seulement signalés à l'interface (SPEC.md § 19).
		collect.NewMateriel(cfg.RacineSys, cfg.RacineProc, cfg.Ventilateurs, cfg.ZFS, cfg.Mdraid),
	}
	if cfg.Temperatures {
		liste = append(liste, collect.NewTemperatures(cfg.RacineSys))
	}
	// Résultats de l'assistant root (pare-feu, SMART, certificats) : rien
	// n'est envoyé tant que l'assistant n'a pas écrit son fichier.
	r := collect.NewRacine(cfg.FichierRoot)
	r.SansPareFeu, r.SansSmart, r.SansCertificats = !cfg.EnvoyerPareFeu, !cfg.EnvoyerSmart, !cfg.EnvoyerCertificats
	// Nœud Proxmox : consommation des invités lue dans les cgroups (SPEC.md § 23.7).
	if collect.ProxmoxPresent(cfg.RacineSys) {
		liste = append(liste, collect.NewProxmox(cfg.RacineSys))
		r.Proxmox = true
	}
	r.IgnorerCertificats(cfg.CertificatsIgnorer)
	liste = append(liste, r)
	if cfg.Systemd {
		s := collect.NewSystemd(cfg.SystemdIgnorer)
		if cfg.SystemdValidation {
			s.Valider(cfg.SystemdSurveiller)
		}
		liste = append(liste, s)
	}
	if cfg.MisesAJour {
		liste = append(liste, collect.NewMisesAJour())
	}
	if cfg.Docker != "" && cfg.DockerMetriques {
		if d, err := collect.NewDocker(cfg.Docker); err == nil {
			liste = append(liste, d)
		} else {
			journal.Error("collecteur Docker désactivé", "erreur", err)
		}
	}
	return liste
}

// Envoyer vide la file vers le serveur jusqu'à l'arrêt de ctx. Il tourne à
// part de la collecte : à l'arrêt, on peut finir d'envoyer après la dernière
// collecte.
func (a *Agent) Envoyer(ctx context.Context) { a.exp.Tourner(ctx) }

// TournerLogs lit le journal et envoie les logs (rien si désactivés).
// La lecture s'arrête avec ctxLecture, l'envoi avec ctxEnvoi.
func (a *Agent) TournerLogs(ctxLecture, ctxEnvoi context.Context) {
	if a.logs != nil {
		a.logs.tourner(ctxLecture, ctxEnvoi)
	}
}

// Tourner collecte jusqu'à l'arrêt du contexte.
func (a *Agent) Tourner(ctx context.Context) {
	// Un collecteur « lent » tourne une fois tous les n cycles.
	n := uint64(a.cfg.IntervalleLent / a.cfg.Intervalle) // #nosec G115 -- durées validées positives (config.Valider)
	if n == 0 {
		n = 1
	}
	ticker := time.NewTicker(a.cfg.Intervalle)
	defer ticker.Stop()
	for {
		a.Cycle(time.Now(), a.cycle%n == 0)
		a.cycle++
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Cycle fait une collecte complète et met le lot dans la file d'envoi.
func (a *Agent) Cycle(maintenant time.Time, avecLents bool) {
	a.Remplir(maintenant, avecLents)
	donnees := a.enc.Encode(a.file.Prendre(), a.lot)
	if !a.file.Ajouter(donnees) {
		a.journal.Warn("lot trop gros pour le tampon, abandonné", "octets", len(donnees))
	}
}

// Remplir exécute les collecteurs et remplit le lot (sans l'envoyer).
func (a *Agent) Remplir(maintenant time.Time, avecLents bool) *mm.Batch {
	a.lot.Reset(maintenant.UnixMilli())
	// Métriques désactivées (« logs seulement ») : seules les métriques de
	// l'agent partent (il reste visible et supervisé).
	for _, c := range a.collecteurs {
		if !a.cfg.Metriques {
			break
		}
		if c.Lent() && !avecLents {
			continue
		}
		debut := time.Now()
		if err := c.Collecter(a.lot); err != nil {
			a.erreurs[c.Nom()]++
			a.journal.Debug("collecteur en erreur", "collecteur", c.Nom(), "erreur", err)
		}
		a.duree[c.Nom()] = time.Since(debut)
	}
	a.ajouterInternes()
	return a.lot
}

// --- Métriques internes de l'agent (supervision de la supervision) ------------

// interne lit la consommation de l'agent sans allocation.
type interne struct {
	statm  string
	lect   []byte
	echant []metrics.Sample
	page   float64
}

func (i *interne) init(proc string) {
	i.statm = filepath.Join(proc, "self", "statm")
	i.lect = make([]byte, 0, 256)
	i.echant = []metrics.Sample{
		{Name: "/memory/classes/heap/objects:bytes"},
		{Name: "/sched/goroutines:goroutines"},
	}
	i.page = float64(os.Getpagesize())
}

// rss lit la mémoire résidente dans /proc/self/statm (2e champ, en pages).
func (i *interne) rss() float64 {
	fd, err := syscall.Open(i.statm, syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return 0
	}
	defer syscall.Close(fd)
	buf := i.lect[:cap(i.lect)]
	n, err := syscall.Read(fd, buf)
	if err != nil || n <= 0 {
		return 0
	}
	buf = buf[:n]
	// Saute le premier champ (taille totale).
	j := 0
	for j < len(buf) && buf[j] != ' ' {
		j++
	}
	j++
	var pages float64
	for ; j < len(buf) && buf[j] >= '0' && buf[j] <= '9'; j++ {
		pages = pages*10 + float64(buf[j]-'0')
	}
	return pages * i.page
}

func (a *Agent) ajouterInternes() {
	b := a.lot
	b.Add("mirador_agent_info", 1, mm.L("version", version.Version))
	b.Add("mirador_agent_resident_memory_bytes", a.interne.rss())

	var ru syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &ru) == nil {
		cpu := float64(ru.Utime.Sec+ru.Stime.Sec) + float64(ru.Utime.Usec+ru.Stime.Usec)/1e6
		b.Add("mirador_agent_cpu_seconds_total", cpu)
		b.Add("mirador_agent_max_resident_memory_bytes", float64(ru.Maxrss)*1024)
	}
	metrics.Read(a.interne.echant)
	if v := a.interne.echant[0].Value; v.Kind() == metrics.KindUint64 {
		b.Add("mirador_agent_go_heap_bytes", float64(v.Uint64()))
	}
	if v := a.interne.echant[1].Value; v.Kind() == metrics.KindUint64 {
		b.Add("mirador_agent_goroutines", float64(v.Uint64()))
	}

	for _, c := range a.collecteurs {
		nom := mm.L("collector", c.Nom())
		b.Add("mirador_agent_collect_duration_seconds", a.duree[c.Nom()].Seconds(), nom)
		b.Add("mirador_agent_collect_errors_total", float64(a.erreurs[c.Nom()]), nom)
	}
	lots, octets, jetes := a.file.Etat()
	b.Add("mirador_agent_queue_batches", float64(lots))
	b.Add("mirador_agent_queue_bytes", float64(octets))
	b.Add("mirador_agent_queue_dropped_total", float64(jetes))
	b.Add("mirador_agent_send_total", float64(a.exp.Envoyes.Load()), mm.L("result", "ok"))
	b.Add("mirador_agent_send_total", float64(a.exp.Echecs.Load()), mm.L("result", "error"))
	b.Add("mirador_agent_send_total", float64(a.exp.Rejetes.Load()), mm.L("result", "rejected"))
	b.Add("mirador_agent_sent_bytes_total", float64(a.exp.OctetsEnvoy.Load()))
	if a.logs != nil {
		a.logs.metriques(b)
	}
	// Compte les points ajoutés jusqu'ici (+1 pour celui-ci).
	b.Add("mirador_agent_points", float64(b.Len()+1))
}

// File donne accès à la file (tests et arrêt propre).
func (a *Agent) File() *envoi.File { return a.file }

// Vider attend (au plus delai) que la file soit vide : utilisé à l'arrêt,
// pendant qu'Envoyer tourne encore.
func (a *Agent) Vider(delai time.Duration) {
	fin := time.Now().Add(delai)
	for time.Now().Before(fin) {
		lots, _, _ := a.file.Etat()
		if a.logs != nil {
			l, _, _ := a.logs.file.Etat()
			lots += l
		}
		if lots == 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}
