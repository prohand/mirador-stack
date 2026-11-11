// SPDX-License-Identifier: GPL-3.0-or-later

package collect

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"sync"
	"time"

	"mirador-stack/internal/agent/metrics"
)

// MisesAJour produit :
//   - apt_upgrades_pending : paquets à mettre à jour ;
//   - apt_upgrades_security_pending : dont mises à jour de sécurité ;
//   - node_reboot_required : 1 si /run/reboot-required existe.
//
// Méthode (SPEC.md § 6.2) : « apt-get --simulate dist-upgrade », chemin et
// arguments figés dans le code, sans droits root (la simulation n'en demande
// pas), toutes les 6 heures seulement, dans une goroutine à part pour ne pas
// retarder la collecte. Les listes de paquets ne sont PAS rafraîchies par
// l'agent : c'est le rôle du timer apt-daily de Debian/Ubuntu.
//
// La dernière valeur est renvoyée à chaque passage (60 s) pour que la série
// ne disparaisse pas entre deux mesures.
type MisesAJour struct {
	commande           string // /usr/bin/apt-get (figé, modifiable seulement par les tests)
	fichierRedemarrage string
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
	erreur   error
}

// cheminAptGet : seul programme lancé par ce collecteur.
const cheminAptGet = "/usr/bin/apt-get"

// NewMisesAJour crée le collecteur ; il ne fait rien si apt-get est absent.
func NewMisesAJour() *MisesAJour {
	m := &MisesAJour{
		commande:           cheminAptGet,
		fichierRedemarrage: "/run/reboot-required",
		periode:            6 * time.Hour,
		delai:              2 * time.Minute,
		maintenant:         time.Now,
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
	if !m.enCours && (m.derniere.IsZero() || m.maintenant().Sub(m.derniere) >= m.periode) {
		m.enCours = true
		m.derniere = m.maintenant()
		go m.mesurer()
	}
	if m.ok {
		b.Add("apt_upgrades_pending", float64(m.total))
		b.Add("apt_upgrades_security_pending", float64(m.securite))
	}
	return m.erreur
}

// mesurer lance la simulation puis range le résultat.
func (m *MisesAJour) mesurer() {
	ctx, annuler := context.WithTimeout(context.Background(), m.delai)
	defer annuler()
	sortie, err := m.lancer(ctx)
	total, securite := 0, 0
	if err == nil {
		total, securite = compterMisesAJour(sortie)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.enCours = false
	m.erreur = err
	if err == nil {
		m.ok, m.total, m.securite = true, total, securite
	}
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
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	if sortie.tronque {
		return nil, errors.New("sortie d'apt-get trop longue")
	}
	return sortie.buf.Bytes(), nil
}

// compterMisesAJour lit les lignes « Inst paquet [ancienne] (nouvelle origine [arch]) ».
// Une mise à jour est « de sécurité » si son origine contient « -security »
// (Debian : bookworm-security, Ubuntu : noble-security).
func compterMisesAJour(sortie []byte) (total, securite int) {
	sc := bufio.NewScanner(bytes.NewReader(sortie))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		ligne := sc.Bytes()
		if !bytes.HasPrefix(ligne, []byte("Inst ")) {
			continue
		}
		total++
		if i := bytes.IndexByte(ligne, '('); i >= 0 && bytes.Contains(ligne[i:], []byte("-security")) {
			securite++
		}
	}
	return total, securite
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
