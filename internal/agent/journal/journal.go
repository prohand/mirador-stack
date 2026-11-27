// SPDX-License-Identifier: GPL-3.0-or-later

// Package journal lit le journal systemd en flux (SPEC.md § 6.2).
//
// La lecture native en Go demanderait libsystemd (cgo, plus de binaire
// statique). On lance donc UN SEUL « journalctl --follow » permanent, avec un
// chemin et des options figés, et on lit sa sortie JSON ligne par ligne. S'il
// s'arrête, il est relancé avec un délai croissant (jamais de fork en boucle).
//
// Reprise après redémarrage : la position (« curseur ») est gardée en mémoire
// et écrite sur disque au plus toutes les 5 minutes, et à l'arrêt.
package journal

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Entree : une ligne du journal, réduite aux champs utiles.
type Entree struct {
	Temps     time.Time
	Message   string
	Priorite  int    // 0 (emerg) à 7 (debug)
	Unite     string // _SYSTEMD_UNIT
	Ident     string // SYSLOG_IDENTIFIER
	Transport string // _TRANSPORT (kernel, syslog, journal, stdout…)
	Curseur   string
}

// Niveaux syslog, dans l'ordre des priorités (même nommage qu'Alloy).
var niveaux = [8]string{"emerg", "alert", "crit", "err", "warning", "notice", "info", "debug"}

// Niveau renvoie le nom du niveau (étiquette « level »).
func (e *Entree) Niveau() string {
	if e.Priorite >= 0 && e.Priorite < len(niveaux) {
		return niveaux[e.Priorite]
	}
	return ""
}

// Options du lecteur.
type Options struct {
	FichierCurseur string        // ex. /var/lib/mirador-agent/journal.cursor
	AgeMax         time.Duration // reprise au plus 12 h en arrière (comme Alloy)
	Journal        *slog.Logger
	// Commande : remplacée seulement dans les tests. En production :
	// /usr/bin/journalctl avec les options de argumentsJournalctl.
	Commande string
}

// cheminJournalctl : seul programme lancé par ce paquet.
const cheminJournalctl = "/usr/bin/journalctl"

// Lecteur lance journalctl et transmet les entrées à une fonction.
type Lecteur struct {
	opt Options

	mu           sync.Mutex
	curseur      string // dernière position lue
	curseurEcrit string
	ecritLe      time.Time

	// Compteurs pour les métriques internes (lus par une autre goroutine).
	Lignes     atomic.Uint64
	Illisibles atomic.Uint64
	Relances   atomic.Uint64
	Anciennes  atomic.Uint64 // lignes de plus de AgeMax à la reprise : ignorées
}

// NewLecteur prépare le lecteur et relit le curseur enregistré.
func NewLecteur(opt Options) *Lecteur {
	if opt.Commande == "" {
		opt.Commande = cheminJournalctl
	}
	if opt.AgeMax == 0 {
		opt.AgeMax = 12 * time.Hour
	}
	if opt.Journal == nil {
		opt.Journal = slog.Default()
	}
	l := &Lecteur{opt: opt}
	if brut, err := os.ReadFile(opt.FichierCurseur); err == nil { // #nosec G304 -- chemin de la configuration
		if c := string(bytes.TrimSpace(brut)); curseurValide(c) {
			l.curseur, l.curseurEcrit = c, c
		}
	}
	return l
}

// Un curseur journald ressemble à « s=…;i=…;b=…;m=…;t=…;x=… ».
var reCurseur = regexp.MustCompile(`^[A-Za-z0-9=;_-]{1,512}$`)

func curseurValide(c string) bool { return reCurseur.MatchString(c) }

// arguments : options figées de journalctl.
//   - -o json : une entrée JSON par ligne ;
//   - --output-fields : seulement les champs utiles (moins de travail) ;
//   - -p 0..6 : le niveau debug est jeté par journalctl lui-même (filtrage à la source) ;
//   - --after-cursor : reprise après la dernière ligne envoyée. SEUL : les
//     systemd récents refusent --after-cursor avec --since (vu le 11/11 sur
//     Debian 13) ; la limite de AgeMax est appliquée par l'agent (lancer) ;
//   - sans curseur : on part de la fin (-n 0), pas de relecture du passé.
func (l *Lecteur) arguments() []string {
	// --all : sans lui, un champ de plus de 4 Kio arrive à null (traces Java
	// ou Python perdues, relecture du 21/11) ; la taille reste bornée ici.
	args := []string{"--follow", "--no-pager", "--quiet", "--all", "-o", "json",
		"--output-fields=MESSAGE,PRIORITY,_SYSTEMD_UNIT,SYSLOG_IDENTIFIER,_TRANSPORT",
		"-p", "0..6"}
	l.mu.Lock()
	c := l.curseur
	l.mu.Unlock()
	if c != "" {
		args = append(args, "--after-cursor="+c)
	} else {
		args = append(args, "-n", "0")
	}
	return args
}

// Tourner lit le journal jusqu'à l'arrêt du contexte, en relançant
// journalctl s'il s'arrête. Chaque entrée est passée à traiter.
func (l *Lecteur) Tourner(ctx context.Context, traiter func(*Entree)) {
	attente := time.Second
	// abandonne : le curseur vient d'être abandonné. Une seule fois de suite
	// (relecture du 21/11 : un journalctl qui donne une ligne puis échoue
	// redonnait un curseur, et la relance sans attente tournait en boucle,
	// ~450 lancements par seconde).
	abandonne := false
	for ctx.Err() == nil {
		debut := time.Now()
		err := l.lancer(ctx, traiter)
		if ctx.Err() != nil {
			break
		}
		l.Relances.Add(1)
		// Échec immédiat avec un curseur (curseur refusé, journal effacé…) :
		// on l'abandonne et on repart de la fin, plutôt que d'échouer sans fin.
		l.mu.Lock()
		abandon := err != nil && l.curseur != "" && time.Since(debut) < 10*time.Second && !abandonne
		if abandon {
			l.curseur = ""
		}
		l.mu.Unlock()
		if abandon {
			l.opt.Journal.Warn("journalctl refuse la reprise au curseur : reprise à la fin du journal", "erreur", err)
			abandonne = true
			continue
		}
		if time.Since(debut) > time.Minute {
			attente = time.Second // il a tourné normalement : on repart du début
			abandonne = false
		}
		l.opt.Journal.Warn("journalctl s'est arrêté, relance", "attente", attente.String(), "erreur", err)
		select {
		case <-ctx.Done():
		case <-time.After(attente):
		}
		attente = min(attente*2, 5*time.Minute)
	}
	l.EnregistrerCurseur(true)
}

// tailleMaxLigne : une ligne de journal plus longue est coupée (protection mémoire).
const tailleMaxLigne = 256 << 10

func (l *Lecteur) lancer(ctx context.Context, traiter func(*Entree)) error {
	args := l.arguments()
	// Reprise après un curseur : pas plus de AgeMax en arrière (voir arguments).
	reprise := strings.HasPrefix(args[len(args)-1], "--after-cursor=")
	cmd := exec.CommandContext(ctx, l.opt.Commande, args...) // #nosec G204 -- programme et options figés
	cmd.Env = []string{"LANG=C.UTF-8", "PATH=/usr/sbin:/usr/bin:/sbin:/bin", "SYSTEMD_COLORS=0"}
	// À l'arrêt : SIGTERM, puis on n'attend pas plus de 2 s la fermeture des
	// sorties (un processus bloqué ne doit pas bloquer l'arrêt de l'agent).
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 2 * time.Second
	var errBuf limite
	errBuf.max = 4096
	cmd.Stderr = &errBuf
	sortie, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	lire := bufio.NewReaderSize(sortie, 64<<10)
	var e Entree
	var tampon, queue []byte // ligne en cours, réutilisée (analyser copie ce qu'il garde)
	for {
		ligne, q, coupee, err := lireLigneDans(lire, tampon[:0], queue)
		tampon, queue = ligne, q
		if coupee {
			ligne = reparer(ligne, q) // message coupé au milieu au lieu d'être perdu
		}
		if cap(tampon) > 64<<10 {
			tampon = nil // agrandi par une très longue ligne : rendu après usage
		}
		if len(ligne) > 0 {
			l.Lignes.Add(1)
			if analyser(ligne, &e) {
				if reprise && time.Since(e.Temps) > l.opt.AgeMax {
					l.Anciennes.Add(1) // reprise après une longue coupure : pas au-delà de AgeMax
				} else {
					traiter(&e)
				}
				l.mu.Lock()
				l.curseur = e.Curseur
				l.mu.Unlock()
				l.EnregistrerCurseur(false)
			} else {
				l.Illisibles.Add(1)
			}
		}
		if err != nil {
			break
		}
	}
	errAttente := cmd.Wait()
	if errBuf.b.Len() > 0 {
		return errors.New(string(bytes.TrimSpace(errBuf.b.Bytes())))
	}
	return errAttente
}

// lireLigne lit une ligne entière ; au-delà de tailleMaxLigne, le milieu est
// jeté (voir lireLigneDans).
func lireLigne(r *bufio.Reader) ([]byte, error) {
	l, q, coupee, err := lireLigneDans(r, nil, nil)
	if coupee {
		l = reparer(l, q)
	}
	return l, err
}

// tailleQueue : fin d'une ligne trop longue gardée pour la réparer (les
// champs écrits après MESSAGE : priorité, unité…).
const tailleQueue = 8 << 10

// lireLigneDans : comme lireLigne, en écrivant dans ligne (tampon réutilisé :
// pas d'allocation par ligne une fois le tampon à la bonne taille). Ligne
// trop longue : coupee vaut true, ligne garde le début (tailleMaxLigne) et
// queue les derniers tailleQueue octets.
func lireLigneDans(r *bufio.Reader, ligne, queue []byte) (_, _ []byte, coupee bool, err error) {
	queue = queue[:0]
	for {
		var morceau []byte
		morceau, err = r.ReadSlice('\n')
		if !coupee && len(ligne)+len(morceau) <= tailleMaxLigne {
			ligne = append(ligne, morceau...)
		} else {
			if !coupee {
				// Début rempli jusqu'à la limite, le reste va dans la queue.
				n := tailleMaxLigne - len(ligne)
				ligne = append(ligne, morceau[:n]...)
				morceau = morceau[n:]
				coupee = true
			}
			queue = append(queue, morceau...)
			if len(queue) > tailleQueue {
				queue = append(queue[:0], queue[len(queue)-tailleQueue:]...)
			}
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if coupee {
			return ligne, bytes.TrimRight(queue, "\n"), true, err
		}
		return bytes.TrimRight(ligne, "\n"), queue, false, err
	}
}

// reparer : ligne JSON trop longue (revue du 27/11 : elle était perdue,
// comptée « illisible »). Le message est coupé au milieu : début + « … » +
// fin de la ligne (champs après MESSAGE). Si ce n'est pas MESSAGE qui est
// trop long, ou si le résultat n'est pas du JSON valide, la ligne reste
// illisible comme avant.
func reparer(debut, queue []byte) []byte {
	p := bytes.LastIndex(debut, []byte(`"MESSAGE":`))
	if p < 0 {
		return debut
	}
	v := p + len(`"MESSAGE":`)
	if v >= len(debut) {
		return debut
	}
	var out []byte
	switch debut[v] {
	case '"': // texte
		if finTexte(debut[v+1:]) >= 0 {
			return debut // MESSAGE fini avant la coupure : autre chose de trop long
		}
		d := debut
		// Pas de séquence d'échappement coupée en deux (« \ » ou « \u00 »).
		if i := bytes.LastIndexByte(d, '\\'); i > v && len(d)-i < 6 {
			n := 0
			for j := i; j > v && d[j] == '\\'; j-- {
				n++
			}
			if n%2 == 1 {
				d = d[:i]
			}
		}
		q := finTexte(queue)
		if q < 0 {
			return debut
		}
		out = append(append(append(out, d...), "…\""...), queue[q+1:]...)
	case '[': // octets (caractères non imprimables)
		if bytes.IndexByte(debut[v:], ']') >= 0 {
			return debut
		}
		i, q := bytes.LastIndexByte(debut, ','), bytes.IndexByte(queue, ']')
		if i <= v || q < 0 {
			return debut
		}
		out = append(append(append(out, debut[:i]...), ']'), queue[q+1:]...)
	default:
		return debut
	}
	if !json.Valid(out) {
		return debut
	}
	return out
}

// finTexte : position du premier « " » non échappé (fin d'une chaîne JSON),
// -1 s'il n'y en a pas.
func finTexte(b []byte) int {
	for i := 0; i < len(b); i++ {
		switch b[i] {
		case '\\':
			i++ // caractère échappé sauté
		case '"':
			return i
		}
	}
	return -1
}

// analyser lit une ligne JSON de journalctl dans e (réutilisée).
func analyser(ligne []byte, e *Entree) bool {
	var brut struct {
		Curseur   string          `json:"__CURSOR"`
		Temps     string          `json:"__REALTIME_TIMESTAMP"`
		Message   json.RawMessage `json:"MESSAGE"`
		Priorite  string          `json:"PRIORITY"`
		Unite     string          `json:"_SYSTEMD_UNIT"`
		Ident     string          `json:"SYSLOG_IDENTIFIER"`
		Transport string          `json:"_TRANSPORT"`
	}
	if err := json.Unmarshal(ligne, &brut); err != nil {
		return false
	}
	us, err := strconv.ParseInt(brut.Temps, 10, 64)
	if err != nil || brut.Curseur == "" {
		return false
	}
	*e = Entree{
		Temps:     time.UnixMicro(us),
		Message:   message(brut.Message),
		Priorite:  6,
		Unite:     brut.Unite,
		Ident:     brut.Ident,
		Transport: brut.Transport,
		Curseur:   brut.Curseur,
	}
	if p, err := strconv.Atoi(brut.Priorite); err == nil && p >= 0 && p <= 7 {
		e.Priorite = p
	}
	return true
}

// message : journalctl écrit MESSAGE en texte, ou en tableau d'octets quand
// il contient des caractères non imprimables (codes couleur, binaire).
func message(brut json.RawMessage) string {
	if len(brut) == 0 || string(brut) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(brut, &s) == nil {
		return s
	}
	var octets []byte
	var nombres []int
	if json.Unmarshal(brut, &nombres) == nil {
		octets = make([]byte, 0, len(nombres))
		for _, n := range nombres {
			octets = append(octets, byte(n)) // #nosec G115 -- octets 0-255 envoyés par journalctl
		}
	}
	return string(octets)
}

// periodeCurseur : la position est écrite sur disque au plus toutes les 5 min.
const periodeCurseur = 5 * time.Minute

// EnregistrerCurseur écrit la position si elle a changé et si 5 minutes sont
// passées (ou toujours, avec force, à l'arrêt). Écriture atomique.
func (l *Lecteur) EnregistrerCurseur(force bool) {
	l.mu.Lock()
	c := l.curseur
	if c == "" || c == l.curseurEcrit || (!force && time.Since(l.ecritLe) < periodeCurseur) {
		l.mu.Unlock()
		return
	}
	l.curseurEcrit, l.ecritLe = c, time.Now()
	l.mu.Unlock()

	tmp := l.opt.FichierCurseur + ".tmp"
	if err := os.WriteFile(tmp, []byte(c+"\n"), 0o600); err != nil {
		l.opt.Journal.Warn("curseur du journal non enregistré", "erreur", err)
		return
	}
	if err := os.Rename(tmp, l.opt.FichierCurseur); err != nil {
		l.opt.Journal.Warn("curseur du journal non enregistré", "erreur", err)
	}
}

// limite garde au plus max octets (sortie d'erreur de journalctl).
type limite struct {
	b   bytes.Buffer
	max int
}

func (l *limite) Write(p []byte) (int, error) {
	if reste := l.max - l.b.Len(); reste > 0 {
		if len(p) > reste {
			l.b.Write(p[:reste])
		} else {
			l.b.Write(p)
		}
	}
	return len(p), nil
}

var _ io.Writer = (*limite)(nil)
