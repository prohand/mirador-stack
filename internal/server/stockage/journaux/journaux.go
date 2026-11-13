// SPDX-License-Identifier: GPL-3.0-or-later

// Package journaux : moteur de stockage des logs de mirador-server
// (SPEC.md § 16.1, option (a), et § 16.6).
//
//   - un fichier par heure de réception (AAAAMMJJHH.seg), fait de blocs
//     compressés en zstd (~256 Kio de texte chacun) ;
//   - en-tête de chaque bloc : période, valeurs des champs indexés (machine,
//     job, niveau, unité…), filtre de Bloom des mots ;
//   - en mémoire : seulement la position et la période de chaque bloc
//     (~24 octets) et un résumé par fichier ; le reste est lu sur disque
//     quand une recherche en a besoin ;
//   - rétention et plafond de taille : suppression de fichiers entiers.
//
// Perte possible à l'arrêt brutal : le bloc en cours (≤ 30 s de logs). Un
// arrêt normal l'écrit.
package journaux

import (
	"bufio"
	"context"
	"errors"
	"hash/crc32"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/klauspost/compress/zstd"

	"mirador-stack/internal/server/stockage"
)

// Reglages modifiables à chaud (interface, étape 5).
type Reglages struct {
	Retention time.Duration
	MaxOctets int64 // 0 = pas de plafond
}

// Options d'ouverture.
type Options struct {
	Dossier    string
	Reglages   Reglages
	TailleBloc int           // texte avant compression (défaut 256 Kio)
	Vidage     time.Duration // écriture du bloc en cours au plus tard après (défaut 30 s)
	Journal    *slog.Logger
	Maintenant func() time.Time
}

// Moteur : fichiers de segments et index en mémoire.
type Moteur struct {
	opt Options
	enc *zstd.Encoder
	dec *zstd.Decoder

	mu        sync.Mutex
	reglages  Reglages
	fichiers  []*fichier // triés par heure
	enCours   []stockage.Ligne
	tailleEnC int
	debutEnC  time.Time

	arret chan struct{}
	fin   sync.WaitGroup

	// Compteurs (métriques internes, partie 7).
	Lignes, Blocs, BlocsAbimes atomic.Uint64

	// vus : valeurs vues des champs proposés en liste dans la page Logs
	// (champ → valeur → dernière fois, ms). En mémoire seulement : la
	// liste se remplit de nouveau après un redémarrage, au fil des lignes.
	vus map[string]map[string]int64
}

// ChampsListes : champs dont les valeurs vues sont gardées pour les listes.
var ChampsListes = []string{"job", "level", "unit", "container"}

// maxValeursVues : au-delà, les valeurs les plus anciennes sont oubliées.
const maxValeursVues = 500

// noterValeurs (m.mu tenu).
func (m *Moteur) noterValeurs(l *stockage.Ligne) {
	if m.vus == nil {
		m.vus = map[string]map[string]int64{}
	}
	for _, ch := range ChampsListes {
		v, ok := l.Champs[ch]
		if !ok || v == "" || len(v) > 200 {
			continue
		}
		vals := m.vus[ch]
		if vals == nil {
			vals = map[string]int64{}
			m.vus[ch] = vals
		}
		if _, connu := vals[v]; !connu && len(vals) >= maxValeursVues {
			// Oublie la plus ancienne (rare : une valeur nouvelle au-delà de 500).
			var vieille string
			var t int64 = math.MaxInt64
			for k, tv := range vals {
				if tv < t {
					vieille, t = k, tv
				}
			}
			delete(vals, vieille)
		}
		vals[v] = l.T
	}
}

// ValeursVues : valeurs vues d'un champ (ChampsListes), triées.
func (m *Moteur) ValeursVues(champ string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.vus[champ]))
	for v := range m.vus[champ] {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// fichier : un segment d'une heure.
type fichier struct {
	heure  int64 // début de l'heure (s Unix)
	chemin string
	taille int64
	blocs  []bloc
	tmin   int64
	tmax   int64
	f      *os.File // ouvert en écriture tant que c'est l'heure courante
}

// bloc : ce qui reste en mémoire d'un bloc.
type bloc struct {
	pos, tmin, tmax int64
}

// Ouvrir relit les segments existants (en-têtes seulement) et lance la tâche
// de fond (vidage, rotation, rétention).
func Ouvrir(opt Options) (*Moteur, error) {
	if opt.Reglages.Retention <= 0 {
		opt.Reglages.Retention = 30 * 24 * time.Hour
	}
	if opt.TailleBloc <= 0 {
		opt.TailleBloc = 256 << 10
	}
	if opt.Vidage <= 0 {
		opt.Vidage = 30 * time.Second
	}
	if opt.Journal == nil {
		opt.Journal = slog.New(slog.DiscardHandler)
	}
	if opt.Maintenant == nil {
		opt.Maintenant = time.Now
	}
	if err := os.MkdirAll(opt.Dossier, 0o750); err != nil {
		return nil, err
	}
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithEncoderConcurrency(1))
	if err != nil {
		return nil, err
	}
	dec, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(2), zstd.WithDecoderMaxMemory(maxBrut))
	if err != nil {
		return nil, err
	}
	m := &Moteur{opt: opt, enc: enc, dec: dec, reglages: opt.Reglages, arret: make(chan struct{})}
	if err := m.relire(); err != nil {
		return nil, err
	}
	m.fin.Add(1)
	go m.boucle()
	return m, nil
}

// relire charge l'index des segments présents. Un bloc abîmé en fin de
// fichier (arrêt brutal pendant l'écriture) est coupé.
func (m *Moteur) relire() error {
	entrees, err := os.ReadDir(m.opt.Dossier)
	if err != nil {
		return err
	}
	for _, e := range entrees {
		nom := e.Name()
		if e.IsDir() || !strings.HasSuffix(nom, ".seg") {
			continue
		}
		h, err := time.Parse("2006010215", strings.TrimSuffix(nom, ".seg"))
		if err != nil {
			continue
		}
		f := &fichier{heure: h.Unix(), chemin: filepath.Join(m.opt.Dossier, nom)}
		if err := m.indexer(f); err != nil {
			return err
		}
		if len(f.blocs) > 0 {
			m.fichiers = append(m.fichiers, f)
		} else if err := os.Remove(f.chemin); err != nil {
			return err
		}
	}
	sort.Slice(m.fichiers, func(i, j int) bool { return m.fichiers[i].heure < m.fichiers[j].heure })
	return nil
}

func (m *Moteur) indexer(f *fichier) error {
	fd, err := os.Open(f.chemin)
	if err != nil {
		return err
	}
	defer fd.Close()
	r := bufio.NewReaderSize(fd, 64<<10)
	var pos int64
	for {
		e, err := lireEntete(r)
		if errors.Is(err, io.EOF) {
			break
		}
		if err == nil {
			_, err = r.Discard(e.compresse)
			if err != nil {
				err = errBloc
			}
		}
		if err != nil {
			// Fin abîmée : on coupe le fichier au dernier bloc complet.
			m.BlocsAbimes.Add(1)
			m.opt.Journal.Warn("fin de segment abîmée, coupée", "fichier", f.chemin, "position", pos)
			if err := os.Truncate(f.chemin, pos); err != nil {
				return err
			}
			break
		}
		f.ajouterBloc(bloc{pos: pos, tmin: e.tmin, tmax: e.tmax})
		pos += int64(e.tailleTotale)
	}
	f.taille = pos
	return nil
}

func (f *fichier) ajouterBloc(b bloc) {
	if len(f.blocs) == 0 || b.tmin < f.tmin {
		f.tmin = b.tmin
	}
	if len(f.blocs) == 0 || b.tmax > f.tmax {
		f.tmax = b.tmax
	}
	f.blocs = append(f.blocs, b)
}

// Ecrire ajoute des lignes au bloc en cours ; il est écrit sur disque quand il
// atteint TailleBloc (ou après Vidage, par la tâche de fond).
func (m *Moteur) Ecrire(_ context.Context, lignes []stockage.Ligne) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range lignes {
		if len(m.enCours) == 0 {
			m.debutEnC = m.opt.Maintenant()
		}
		m.enCours = append(m.enCours, lignes[i])
		m.noterValeurs(&lignes[i])
		m.tailleEnC += len(lignes[i].Msg) + 40
		for k, v := range lignes[i].Champs {
			m.tailleEnC += len(k) + len(v)
		}
		if m.tailleEnC >= m.opt.TailleBloc {
			if err := m.viderVerrou(); err != nil {
				return err
			}
		}
	}
	m.Lignes.Add(uint64(len(lignes)))
	return nil
}

// viderVerrou écrit le bloc en cours (m.mu tenu).
func (m *Moteur) viderVerrou() error {
	if len(m.enCours) == 0 {
		return nil
	}
	lignes := m.enCours
	m.enCours, m.tailleEnC = nil, 0
	e := &entete{tmin: lignes[0].T, tmax: lignes[0].T, lignes: len(lignes), index: map[string][]string{}}
	valeurs := map[string]map[string]struct{}{}
	motsBloc := map[string]struct{}{}
	for i := range lignes {
		l := &lignes[i]
		e.tmin, e.tmax = min(e.tmin, l.T), max(e.tmax, l.T)
		for _, nom := range champsIndexes {
			v, ok := l.Champs[nom]
			if !ok {
				continue
			}
			ens := valeurs[nom]
			if ens == nil {
				ens = map[string]struct{}{}
				valeurs[nom] = ens
			}
			if len(ens) <= maxValeursIndex {
				ens[v] = struct{}{}
			}
		}
		if len(motsBloc) < 1<<16 {
			mots(l.Msg, func(w string) { motsBloc[w] = struct{}{} })
		}
	}
	for _, nom := range champsIndexes {
		ens, ok := valeurs[nom]
		switch {
		case !ok:
			e.index[nom] = []string{} // champ absent de tout le bloc
		case len(ens) > maxValeursIndex:
			e.index[nom] = nil // trop de valeurs : non listées
		default:
			vals := make([]string, 0, len(ens))
			for v := range ens {
				vals = append(vals, v)
			}
			sort.Strings(vals)
			e.index[nom] = vals
		}
	}
	if len(motsBloc) >= 1<<16 {
		e.bloomK, e.bloom = 0, nil // trop de mots : pas de filtre, le bloc sera lu
	} else {
		e.bloomK, e.bloom = nouveauBloom(motsBloc)
	}
	brut := encoderLignes(nil, lignes, e.tmin)
	donnees := m.enc.EncodeAll(brut, nil)
	e.brut, e.compresse = len(brut), len(donnees)
	e.crcDonnees = crc32.Checksum(donnees, crc)
	tete := encoderEntete(e)

	f, err := m.fichierCourant()
	if err != nil {
		return err
	}
	// Une seule écriture : en-tête et données ensemble.
	if _, err := f.f.Write(append(tete, donnees...)); err != nil {
		return err
	}
	f.ajouterBloc(bloc{pos: f.taille, tmin: e.tmin, tmax: e.tmax})
	f.taille += int64(len(tete) + len(donnees))
	m.Blocs.Add(1)
	return nil
}

// fichierCourant renvoie le segment de l'heure de réception, en le créant
// (et en fermant le précédent) si besoin.
func (m *Moteur) fichierCourant() (*fichier, error) {
	h := m.opt.Maintenant().UTC().Truncate(time.Hour)
	if n := len(m.fichiers); n > 0 && m.fichiers[n-1].heure == h.Unix() && m.fichiers[n-1].f != nil {
		return m.fichiers[n-1], nil
	}
	m.fermerCourant()
	chemin := filepath.Join(m.opt.Dossier, h.Format("2006010215")+".seg")
	fd, err := os.OpenFile(chemin, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) // #nosec G304 -- nom fabriqué ici
	if err != nil {
		return nil, err
	}
	if n := len(m.fichiers); n > 0 && m.fichiers[n-1].heure == h.Unix() {
		m.fichiers[n-1].f = fd // reprise après redémarrage dans la même heure
		return m.fichiers[n-1], nil
	}
	f := &fichier{heure: h.Unix(), chemin: chemin, f: fd}
	m.fichiers = append(m.fichiers, f)
	return f, nil
}

// fermerCourant : fsync puis fermeture du segment ouvert.
func (m *Moteur) fermerCourant() {
	for _, f := range m.fichiers {
		if f.f != nil {
			if err := errors.Join(f.f.Sync(), f.f.Close()); err != nil {
				m.opt.Journal.Error("fermeture d'un segment de logs", "fichier", f.chemin, "erreur", err)
			}
			f.f = nil
		}
	}
}

// Vider écrit le bloc en cours (arrêt, tests).
func (m *Moteur) Vider() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.viderVerrou()
}

// Fermer écrit le bloc en cours et ferme les fichiers.
func (m *Moteur) Fermer() error {
	close(m.arret)
	m.fin.Wait()
	m.mu.Lock()
	defer m.mu.Unlock()
	err := m.viderVerrou()
	m.fermerCourant()
	err = errors.Join(err, m.enc.Close())
	m.dec.Close()
	return err
}

// Regler change la rétention et le plafond à chaud (appliqués à la minute).
func (m *Moteur) Regler(r Reglages) error {
	if r.Retention <= 0 || r.MaxOctets < 0 {
		return errors.New("réglages de rétention des logs invalides")
	}
	m.mu.Lock()
	m.reglages = r
	m.mu.Unlock()
	return m.Purger()
}

// Segment : un fichier et la taille à copier (le fichier ne fait que
// grandir : copier jusqu'à cette taille donne un état cohérent).
type Segment struct {
	Nom, Chemin string
	Taille      int64
}

// Segments écrit le bloc en cours puis renvoie la liste figée des fichiers.
// Sert à la sauvegarde.
func (m *Moteur) Segments() ([]Segment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.viderVerrou(); err != nil {
		return nil, err
	}
	out := make([]Segment, 0, len(m.fichiers))
	for _, f := range m.fichiers {
		out = append(out, Segment{Nom: filepath.Base(f.chemin), Chemin: f.chemin, Taille: f.taille})
	}
	return out, nil
}

// Taille renvoie l'espace disque occupé (octets).
func (m *Moteur) Taille() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for _, f := range m.fichiers {
		n += f.taille
	}
	return n
}

func (m *Moteur) boucle() {
	defer m.fin.Done()
	t := time.NewTicker(time.Second)
	defer t.Stop()
	derniereP := time.Time{}
	for {
		select {
		case <-m.arret:
			return
		case <-t.C:
		}
		maintenant := m.opt.Maintenant()
		m.mu.Lock()
		var err error
		if len(m.enCours) > 0 && maintenant.Sub(m.debutEnC) >= m.opt.Vidage {
			err = m.viderVerrou()
		}
		// Changement d'heure : le segment précédent est fermé (fsync).
		if n := len(m.fichiers); n > 0 && m.fichiers[n-1].f != nil &&
			m.fichiers[n-1].heure != maintenant.UTC().Truncate(time.Hour).Unix() {
			m.fermerCourant()
		}
		m.mu.Unlock()
		if err != nil {
			m.opt.Journal.Error("écriture d'un bloc de logs", "erreur", err)
		}
		if maintenant.Sub(derniereP) >= time.Minute {
			derniereP = maintenant
			if err := m.Purger(); err != nil {
				m.opt.Journal.Error("purge des logs", "erreur", err)
			}
		}
	}
}

// Purger supprime les segments entiers hors rétention, puis les plus anciens
// tant que le plafond de taille est dépassé (jamais le segment en cours).
func (m *Moteur) Purger() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	limite := m.opt.Maintenant().Add(-m.reglages.Retention).UnixMilli()
	var total int64
	for _, f := range m.fichiers {
		total += f.taille
	}
	var errs []error
	garde := m.fichiers[:0]
	for i, f := range m.fichiers {
		dernier := i == len(m.fichiers)-1
		finHeure := (f.heure + 3600) * 1000
		tropVieux := finHeure <= limite && f.tmax < limite
		tropGros := m.reglages.MaxOctets > 0 && total > m.reglages.MaxOctets
		if !dernier && f.f == nil && (tropVieux || tropGros) {
			if err := os.Remove(f.chemin); err != nil && !errors.Is(err, os.ErrNotExist) {
				errs = append(errs, err)
				garde = append(garde, f)
				continue
			}
			total -= f.taille
			continue
		}
		garde = append(garde, f)
	}
	clear(m.fichiers[len(garde):])
	m.fichiers = garde
	return errors.Join(errs...)
}

// PlusAncien : heure du plus ancien fichier gardé (0 = aucun).
func (m *Moteur) PlusAncien() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	var t time.Time
	for _, f := range m.fichiers {
		h, err := time.ParseInLocation("2006010215", strings.TrimSuffix(filepath.Base(f.chemin), ".seg"), time.UTC)
		if err == nil && (t.IsZero() || h.Before(t)) {
			t = h
		}
	}
	return t
}
