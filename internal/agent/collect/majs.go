// SPDX-License-Identifier: GPL-3.0-or-later

package collect

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"mirador-stack/internal/agent/metrics"
)

// MisesAJour produit :
//   - apt_upgrades_pending : paquets à mettre à jour ;
//   - apt_upgrades_security_pending : dont mises à jour de sécurité ;
//   - node_reboot_required : 1 si /run/reboot-required existe ;
//   - apt_upgrade_package{package, from, to, security} : un paquet à mettre
//     à jour (valeur 1), 200 au plus (SPEC § 42.3 : liste montrée sur la
//     page de la machine) ;
//   - apt_package_installed{package, from, to} : paquets installés depuis
//     48 h, d'après /var/log/dpkg.log (valeur : heure, dpkg.go, SPEC § 47) ;
//   - apt_upgrades_calcule_le : heure (Unix) du dernier calcul réussi ;
//   - mirador_agent_mises_a_jour_echec{raison} : le dernier calcul a échoué
//     (la page de la machine le dit au lieu d'attendre en silence, SPEC § 127).
//
// Méthode (SPEC.md § 6.2) : « apt-get --simulate dist-upgrade », chemin et
// arguments figés dans le code, sans droits root (la simulation n'en demande
// pas), dans une goroutine à part pour ne pas retarder la collecte. Les
// listes de paquets ne sont PAS rafraîchies par l'agent : c'est le rôle du
// timer apt-daily de Debian/Ubuntu (ou d'un « apt update »). Le calcul est
// refait (SPEC § 127) :
//   - 2 min après qu'apt a de nouvelles listes (après un apt update : date
//     du dossier des listes et du cache d'apt, relue à chaque passage) ;
//   - 2 min après une installation (dpkg.log) ;
//   - 15 min après un échec ;
//   - sinon toutes les 6 heures.
//
// La dernière valeur est renvoyée à chaque passage (60 s) pour que la série
// ne disparaisse pas entre deux mesures.
type MisesAJour struct {
	commande           string // /usr/bin/apt-get (figé, modifiable seulement par les tests)
	fichierRedemarrage string
	listesApt          []string // dates surveillées (dossier des listes, cache)
	periode            time.Duration
	delai              time.Duration
	lancer             func(ctx context.Context) ([]byte, error)
	maintenant         func() time.Time

	mu       sync.Mutex
	enCours  bool
	derniere time.Time
	ok       bool // au moins une mesure réussie
	total    int
	securite int
	paquets  []paquetAJour
	erreur   error
	calcule  time.Time // dernier calcul réussi
	listes   time.Time // date des listes d'apt vue au passage précédent

	dpkg journalDpkg // paquets installés (SPEC § 47)
}

// paquetAJour : une ligne « Inst » de la simulation.
type paquetAJour struct {
	nom, avant, apres string
	securite          bool
}

// maxPaquetsListes : au-delà, seuls les premiers sont envoyés (le nombre
// total reste juste) : quelques centaines de séries au pire.
const maxPaquetsListes = 200

// cheminAptGet : seul programme lancé par ce collecteur.
const cheminAptGet = "/usr/bin/apt-get"

// NewMisesAJour crée le collecteur ; il ne fait rien si apt-get est absent.
func NewMisesAJour() *MisesAJour {
	m := &MisesAJour{
		commande:           cheminAptGet,
		fichierRedemarrage: "/run/reboot-required",
		listesApt:          []string{"/var/lib/apt/lists", "/var/cache/apt/pkgcache.bin"},
		periode:            6 * time.Hour,
		delai:              2 * time.Minute,
		maintenant:         time.Now,
		dpkg:               journalDpkg{chemin: cheminDpkgLog},
	}
	m.lancer = m.lancerAptGet
	return m
}

func (m *MisesAJour) Nom() string { return "mises_a_jour" }
func (m *MisesAJour) Lent() bool  { return true }

func (m *MisesAJour) Collecter(b *metrics.Batch) error {
	_, err := os.Stat(m.fichierRedemarrage)
	b.Add("node_reboot_required", bool01(err == nil))

	if _, err := os.Stat(m.commande); err != nil {
		return nil // pas une Debian/Ubuntu : rien à mesurer
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	// Paquets installés : liste lue au fil de dpkg.log. Après une
	// installation, la liste « à mettre à jour » est recalculée (2 min
	// plus tard, le temps qu'apt finisse) au lieu d'attendre 6 h.
	if m.dpkg.lire(m.maintenant()) && !m.enCours {
		m.derniere = m.maintenant().Add(-m.periode + 2*time.Minute)
	}
	// Nouvelles listes d'apt (apt update) : nouveau calcul 2 min plus tard.
	if d := m.dateListes(); !d.IsZero() {
		if !m.listes.IsZero() && d.After(m.listes) && !m.enCours {
			m.derniere = m.maintenant().Add(-m.periode + 2*time.Minute)
		}
		m.listes = d
	}
	for _, p := range m.dpkg.liste {
		b.Add("apt_package_installed", float64(p.le.Unix()), metrics.L("package", p.nom),
			metrics.L("from", p.avant), metrics.L("to", p.apres))
	}
	if !m.enCours && (m.derniere.IsZero() || m.maintenant().Sub(m.derniere) >= m.periode) {
		m.enCours = true
		m.derniere = m.maintenant()
		go m.mesurer()
	}
	if m.erreur != nil {
		b.Add("mirador_agent_mises_a_jour_echec", 1, metrics.L("raison", raisonEchec(m.erreur)))
	}
	if m.ok {
		b.Add("apt_upgrades_calcule_le", float64(m.calcule.Unix()))
		b.Add("apt_upgrades_pending", float64(m.total))
		b.Add("apt_upgrades_security_pending", float64(m.securite))
		for _, p := range m.paquets {
			b.Add("apt_upgrade_package", 1, metrics.L("package", p.nom), metrics.L("from", p.avant),
				metrics.L("to", p.apres), metrics.L("security", bool01s(p.securite)))
		}
	}
	return m.erreur
}

// mesurer lance la simulation puis range le résultat.
func (m *MisesAJour) mesurer() {
	ctx, annuler := context.WithTimeout(context.Background(), m.delai)
	defer annuler()
	sortie, err := m.lancer(ctx)
	var paquets []paquetAJour
	total, securite := 0, 0
	if err == nil {
		paquets, total, securite = lireMisesAJour(sortie)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.enCours = false
	m.erreur = err
	if err == nil {
		m.ok, m.total, m.securite, m.paquets = true, total, securite, paquets
		m.calcule = m.maintenant()
	} else {
		// Échec : nouvel essai dans 15 min au lieu de 6 h.
		m.derniere = m.maintenant().Add(-m.periode + reessaiEchec)
	}
}

// reessaiEchec : délai avant un nouveau calcul après un échec.
const reessaiEchec = 15 * time.Minute

// dateListes : la plus récente des dates surveillées (zéro si aucune).
func (m *MisesAJour) dateListes() time.Time {
	var d time.Time
	for _, c := range m.listesApt {
		if fi, err := os.Stat(c); err == nil && fi.ModTime().After(d) {
			d = fi.ModTime()
		}
	}
	return d
}

// raisonEchec : motif court et sûr pour une étiquette (100 caractères,
// lettres, chiffres et ponctuation simple).
func raisonEchec(err error) string {
	var e *exec.ExitError
	texte := err.Error()
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		texte = "apt-get : pas de réponse en 2 min"
	case errors.As(err, &e) && e.ExitCode() == -1:
		texte = "apt-get arrêté (" + e.String() + ")"
	case errors.As(err, &e):
		texte = "apt-get : code de sortie " + strconv.Itoa(e.ExitCode())
	}
	var b strings.Builder
	for _, c := range texte {
		if b.Len() >= 100 {
			break
		}
		if c == '\n' || c == '\r' || c == '\t' {
			c = ' '
		}
		if c >= ' ' && c != '"' && c != '\\' {
			b.WriteRune(c)
		}
	}
	return b.String()
}

// tailleMaxSortie : au-delà, la sortie d'apt-get est tronquée (protection mémoire).
const tailleMaxSortie = 2 << 20

// lancerAptGet exécute la simulation avec un environnement minimal.
func (m *MisesAJour) lancerAptGet(ctx context.Context) ([]byte, error) {
	cmd := exec.CommandContext(ctx, m.commande, "--simulate", "--quiet", "dist-upgrade") // #nosec G204 -- chemin et arguments figés
	cmd.Env = []string{"LANG=C", "LC_ALL=C", "PATH=/usr/sbin:/usr/bin:/sbin:/bin"}
	var sortie limiteur
	sortie.max = tailleMaxSortie
	cmd.Stdout = &sortie
	// Un enfant qui garderait la sortie ouverte ne bloque plus la mesure
	// pour toujours (relecture du 21/11).
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	if sortie.tronque {
		return nil, errors.New("sortie d'apt-get trop longue")
	}
	return sortie.buf.Bytes(), nil
}

// compterMisesAJour : nombre de mises à jour, dont de sécurité.
func compterMisesAJour(sortie []byte) (total, securite int) {
	_, total, securite = lireMisesAJour(sortie)
	return total, securite
}

// lireMisesAJour lit les lignes « Inst paquet [ancienne] (nouvelle origine [arch]) ».
// Une mise à jour est « de sécurité » si son origine contient « -security »
// (Debian : bookworm-security, Ubuntu : noble-security). Un nouveau paquet
// (sans [ancienne]) a une version installée vide. Les champs sont bornés
// (nom 128, version 64 caractères) : ce sont des étiquettes de séries.
func lireMisesAJour(sortie []byte) (paquets []paquetAJour, total, securite int) {
	sc := bufio.NewScanner(bytes.NewReader(sortie))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		ligne := sc.Bytes()
		reste, ok := bytes.CutPrefix(ligne, []byte("Inst "))
		if !ok {
			continue
		}
		total++
		p := paquetAJour{}
		if i := bytes.IndexByte(ligne, '('); i >= 0 && bytes.Contains(ligne[i:], []byte("-security")) {
			securite++
			p.securite = true
		}
		if len(paquets) >= maxPaquetsListes {
			continue
		}
		nom, apres, _ := bytes.Cut(reste, []byte(" "))
		p.nom = borne(string(nom), 128)
		if v, ok := bytes.CutPrefix(apres, []byte("[")); ok {
			avant, suite, _ := bytes.Cut(v, []byte("] "))
			p.avant, apres = borne(string(avant), 64), suite
		}
		if v, ok := bytes.CutPrefix(apres, []byte("(")); ok {
			version, _, _ := bytes.Cut(v, []byte(" "))
			p.apres = borne(strings.TrimSuffix(string(version), ")"), 64)
		}
		if p.nom != "" {
			paquets = append(paquets, p)
		}
	}
	return paquets, total, securite
}

// borne : au plus n octets, sans couper un caractère.
func borne(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "")
}

func bool01s(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// limiteur garde au plus max octets.
type limiteur struct {
	buf     bytes.Buffer
	max     int
	tronque bool
}

func (l *limiteur) Write(p []byte) (int, error) {
	if l.buf.Len()+len(p) > l.max {
		l.tronque = true
		return len(p), nil
	}
	return l.buf.Write(p)
}
