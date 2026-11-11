// SPDX-License-Identifier: GPL-3.0-or-later

package logs

import (
	"regexp"
	"strings"
	"sync"
	"time"

	"mirador-stack/internal/agent/journal"
	"mirador-stack/internal/agent/metrics"
)

// Options du traitement.
type Options struct {
	Bruit  string  // expression des lignes jetées
	Debit  float64 // lignes par seconde autorisées (200)
	Rafale float64 // rafale autorisée (500)
	Masque *Masqueur
}

// Traitement filtre, masque et compte les entrées, puis les ajoute au lot.
// Il est appelé par la goroutine de lecture ; les compteurs sont lus par la
// goroutine de collecte des métriques (d'où le verrou).
type Traitement struct {
	opt       Options
	bruit     *regexp.Regexp
	lot       *Lot // journal
	lotDocker *Lot // conteneurs (nil si pas de Docker)

	mu       sync.Mutex
	jetons   float64
	derniere time.Time

	// Compteurs (alertes de logs calculées par l'agent, SPEC.md § 4.6 bis).
	parNiveau     [8]uint64
	jetes         map[string]uint64 // motif → lignes jetées
	echecsSSH     uint64
	erreursDisque uint64
	oom           map[string]uint64 // processus tué → nombre
	plantages     map[string]uint64 // conteneur → lignes panic/FATAL/segfault
	lignesDocker  uint64
}

// NewTraitement crée le traitement ; lot reçoit les lignes gardées du journal,
// lotDocker celles des conteneurs (peut être nil).
func NewTraitement(opt Options, lot, lotDocker *Lot) *Traitement {
	t := &Traitement{
		lotDocker: lotDocker,
		plantages: map[string]uint64{},
		opt:       opt,
		lot:       lot,
		jetons:    opt.Rafale,
		jetes:     map[string]uint64{},
		oom:       map[string]uint64{},
	}
	if opt.Bruit != "" {
		t.bruit = regexp.MustCompile(opt.Bruit) // validée par la configuration
	}
	return t
}

// Motifs des compteurs (figés dans le code, pas d'expression libre).
var (
	reEchecSSH     = regexp.MustCompile(`Failed password|Invalid user|authentication failure`)
	reErreurDisque = regexp.MustCompile(`I/O error|medium error|EXT4-fs error|XFS .*error|Buffer I/O error`)
	reOOM          = regexp.MustCompile(`Out of memory: Killed process \d+ \(([^)]{1,64})\)|oom-kill:.*task=([^,]{1,64}),`)
)

// maxProcessusOOM : au-delà, les nouveaux noms sont comptés sous « autre »
// (protection contre l'explosion du nombre de séries).
const maxProcessusOOM = 50

// Traiter : appelé pour chaque entrée lue dans le journal.
func (t *Traitement) Traiter(e *journal.Entree) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if e.Priorite >= 0 && e.Priorite < len(t.parNiveau) {
		t.parNiveau[e.Priorite]++
	}
	t.compter(e)

	if t.bruit != nil && t.bruit.MatchString(e.Message) {
		t.jetes["bruit"]++
		return
	}
	if !t.jeton(e.Temps) {
		t.jetes["debit"]++
		return
	}
	msg := e.Message
	if t.opt.Masque != nil {
		msg = t.opt.Masque.Masquer(msg)
	}
	t.lot.Ajouter(e.Temps, msg, Champs{{"unit", e.Unite}, {"level", e.Niveau()}, {"ident", e.Ident}})
}

// reCrashConteneur : signes d'un programme qui plante (règle ConteneurEnBoucle).
var reCrashConteneur = regexp.MustCompile(`panic:|FATAL|Segmentation fault`)

// maxConteneursCompteurs : au-delà, les nouveaux conteneurs sont comptés sous « autre ».
const maxConteneursCompteurs = 200

// TraiterDocker : appelé pour chaque ligne d'un conteneur. Même filtrage,
// même limite de débit (partagée avec le journal, comme Alloy) et même
// masquage que le journal.
func (t *Traitement) TraiterDocker(temps time.Time, msg, conteneur, flux string) {
	if t.lotDocker == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lignesDocker++
	if reCrashConteneur.MatchString(msg) {
		nom := conteneur
		if _, connu := t.plantages[nom]; !connu && len(t.plantages) >= maxConteneursCompteurs {
			nom = "autre"
		}
		t.plantages[nom]++
	}
	if t.bruit != nil && t.bruit.MatchString(msg) {
		t.jetes["bruit"]++
		return
	}
	if !t.jeton(temps) {
		t.jetes["debit"]++
		return
	}
	if t.opt.Masque != nil {
		msg = t.opt.Masque.Masquer(msg)
	}
	t.lotDocker.Ajouter(temps, msg, Champs{{"container", conteneur}, {"stream", flux}})
}

// compter met à jour les compteurs qui servent aux alertes. Ils sont
// calculés AVANT le filtrage : une ligne jetée pour le débit compte quand même.
func (t *Traitement) compter(e *journal.Entree) {
	switch {
	case e.Ident == "sshd" || strings.HasPrefix(e.Unite, "ssh"):
		if reEchecSSH.MatchString(e.Message) {
			t.echecsSSH++
		}
	case e.Transport == "kernel" || e.Ident == "kernel":
		if reErreurDisque.MatchString(e.Message) {
			t.erreursDisque++
		}
		if m := reOOM.FindStringSubmatch(e.Message); m != nil {
			nom := m[1]
			if nom == "" {
				nom = m[2]
			}
			if _, connu := t.oom[nom]; !connu && len(t.oom) >= maxProcessusOOM {
				nom = "autre"
			}
			t.oom[nom]++
		}
	}
}

// jeton applique la limite de débit (seau à jetons) : Debit lignes par
// seconde, avec une rafale possible de Rafale lignes.
func (t *Traitement) jeton(maintenant time.Time) bool {
	if t.opt.Debit <= 0 {
		return true
	}
	if !t.derniere.IsZero() {
		ecoule := maintenant.Sub(t.derniere).Seconds()
		if ecoule > 0 {
			t.jetons = min(t.opt.Rafale, t.jetons+ecoule*t.opt.Debit)
		}
	}
	if maintenant.After(t.derniere) {
		t.derniere = maintenant
	}
	if t.jetons < 1 {
		return false
	}
	t.jetons--
	return true
}

// Metriques ajoute les compteurs au lot de métriques (appelé à chaque cycle).
func (t *Traitement) Metriques(b *metrics.Batch) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for p, n := range t.parNiveau {
		if n > 0 {
			niv := (&journal.Entree{Priorite: p}).Niveau()
			b.Add("mirador_logs_lines_total", float64(n), metrics.L("level", niv))
		}
	}
	for motif, n := range t.jetes {
		b.Add("mirador_logs_dropped_total", float64(n), metrics.L("reason", motif))
	}
	if t.lotDocker != nil {
		b.Add("mirador_logs_docker_lines_total", float64(t.lignesDocker))
	}
	for nom, n := range t.plantages {
		b.Add("mirador_logs_container_crash_lines_total", float64(n), metrics.L("container", nom))
	}
	b.Add("mirador_logs_ssh_auth_failures_total", float64(t.echecsSSH))
	b.Add("mirador_logs_kernel_disk_errors_total", float64(t.erreursDisque))
	for nom, n := range t.oom {
		b.Add("mirador_logs_oom_kills_total", float64(n), metrics.L("process", nom))
	}
	if t.opt.Masque != nil {
		b.Add("mirador_logs_masked_total", float64(t.opt.Masque.Masquees))
	}
}
