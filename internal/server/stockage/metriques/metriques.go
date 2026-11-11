// SPDX-License-Identifier: GPL-3.0-or-later

// Package metriques : moteur de stockage des métriques de mirador-server
// (SPEC.md § 16.1 et § 16.6), bâti sur la base de séries temporelles de
// Prometheus (« TSDB »).
//
// Deux bases :
//   - brut/   : tous les points reçus, gardés RetentionBrut (7 j par défaut) ;
//   - agrege/ : un point par série et par fenêtre de 5 min, gardé
//     RetentionAgrege (30 j par défaut). Moyenne pour les jauges, dernière
//     valeur pour les compteurs (rate / increase restent justes).
//
// Les deux bases suppriment des blocs ENTIERS (2 h, puis plus gros après
// compaction) quand ils sortent de la rétention ou dépassent le plafond de
// taille : pas de réécriture, pas de VACUUM.
package metriques

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/alecthomas/units"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/config"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/tsdb"
	"github.com/prometheus/prometheus/tsdb/chunkenc"

	"mirador-stack/internal/server/stockage"
)

// HorsOrdreMax : points gardés en mémoire, non compressés, par série
// arrivée « en retard » (file d'attente d'un agent après une coupure) avant
// d'être écrits sur disque. Défaut de la bibliothèque : 32 (SPEC § 17.9).
var HorsOrdreMax int64 = 32

// Fenetre : pas des agrégats.
const Fenetre = 5 * time.Minute

// Reglages modifiables à chaud (depuis l'interface, étape 5).
type Reglages struct {
	RetentionBrut, RetentionAgrege time.Duration
	MaxOctetsBrut, MaxOctetsAgrege int64 // 0 = pas de plafond de taille
}

// ReglagesDefaut : SPEC § 7.2 (brut 7 j, agrégé 30 j).
func ReglagesDefaut() Reglages {
	return Reglages{RetentionBrut: 7 * 24 * time.Hour, RetentionAgrege: 30 * 24 * time.Hour}
}

// Options d'ouverture.
type Options struct {
	Dossier  string
	Reglages Reglages
	// Retard accepté pour un point (file d'attente d'un agent après une
	// coupure du serveur : 8 Mio ≈ 19 h de lots). Les agrégats attendent ce
	// délai avant de calculer une fenêtre : ils sont donc toujours complets.
	Retard     time.Duration
	Journal    *slog.Logger
	Maintenant func() time.Time

	rechargement time.Duration // tests : relecture des blocs plus fréquente (défaut 1 min)
}

// Moteur : les deux bases et la tâche d'agrégation.
type Moteur struct {
	opt          Options
	brut, agrege *tsdb.DB

	mu       sync.Mutex
	reglages Reglages
	muAgreg  sync.Mutex // une seule agrégation à la fois

	arret chan struct{}
	fin   sync.WaitGroup

	// Compteurs (métriques internes, partie 7).
	Points, Rejetes, Agreges atomic.Uint64
}

// Ouvrir ouvre (ou crée) les deux bases et lance l'agrégation.
func Ouvrir(opt Options) (*Moteur, error) {
	if opt.Reglages == (Reglages{}) {
		opt.Reglages = ReglagesDefaut()
	}
	if opt.Retard <= 0 {
		// Mesuré : rattraper 20 h de retard pour 9 000 séries garde ~25 Mo de tas.
		opt.Retard = 24 * time.Hour
	}
	if opt.Journal == nil {
		opt.Journal = slog.New(slog.DiscardHandler)
	}
	if opt.Maintenant == nil {
		opt.Maintenant = time.Now
	}
	m := &Moteur{opt: opt, reglages: opt.Reglages, arret: make(chan struct{})}
	var err error
	if m.brut, err = ouvrirBase(filepath.Join(opt.Dossier, "brut"), opt.Reglages.RetentionBrut,
		opt.Reglages.MaxOctetsBrut, opt.Retard, opt.rechargement, opt.Journal.With("base", "brut")); err != nil {
		return nil, err
	}
	if m.agrege, err = ouvrirBase(filepath.Join(opt.Dossier, "agrege"), opt.Reglages.RetentionAgrege,
		opt.Reglages.MaxOctetsAgrege, 0, opt.rechargement, opt.Journal.With("base", "agrege")); err != nil {
		return nil, errors.Join(err, m.brut.Close())
	}
	m.fin.Add(1)
	go m.boucleAgregation()
	return m, nil
}

func ouvrirBase(dossier string, retention time.Duration, maxOctets int64, retard, rechargement time.Duration,
	journal *slog.Logger) (*tsdb.DB, error) {
	if err := os.MkdirAll(dossier, 0o750); err != nil {
		return nil, err
	}
	o := tsdb.DefaultOptions()
	o.RetentionDuration = retention.Milliseconds()
	o.MaxBytes = maxOctets
	o.OutOfOrderTimeWindow = retard.Milliseconds()
	o.OutOfOrderCapMax = HorsOrdreMax
	// Encodage XOR2 des points : −9 % de disque mesuré (§ 16.8), même CPU et
	// mêmes temps de lecture. Les blocs déjà écrits en XOR restent lisibles.
	o.FloatChunkEncoding = chunkenc.EncXOR2
	if rechargement > 0 {
		o.BlockReloadInterval = rechargement
	}
	// WAL gardé, non compressé (réponse du 09/11, § 16.2) : aucune perte à
	// l'arrêt brutal ; la compression zstd coûtait 5 fois plus de CPU pour
	// 30 % de gain.
	db, err := tsdb.Open(dossier, journal, nil, o, nil)
	if err != nil {
		return nil, fmt.Errorf("base %s : %w", dossier, err)
	}
	return db, nil
}

// Fermer arrête l'agrégation et ferme les bases (le WAL garantit qu'aucun
// point accepté n'est perdu).
func (m *Moteur) Fermer() error {
	close(m.arret)
	m.fin.Wait()
	return errors.Join(m.agrege.Close(), m.brut.Close())
}

// Regler change la rétention et les plafonds à chaud : les blocs en trop sont
// supprimés à la relecture suivante (au plus 1 min).
func (m *Moteur) Regler(r Reglages) error {
	if r.RetentionBrut <= 0 || r.RetentionAgrege <= 0 || r.MaxOctetsBrut < 0 || r.MaxOctetsAgrege < 0 {
		return errors.New("réglages de rétention invalides")
	}
	if err := appliquer(m.brut, r.RetentionBrut, r.MaxOctetsBrut, m.opt.Retard); err != nil {
		return err
	}
	if err := appliquer(m.agrege, r.RetentionAgrege, r.MaxOctetsAgrege, 0); err != nil {
		return err
	}
	m.mu.Lock()
	m.reglages = r
	m.mu.Unlock()
	return nil
}

// appliquer passe par ApplyConfig, seule porte à chaud de la bibliothèque.
// La fenêtre de retard doit être redonnée (sinon elle serait remise à 0).
func appliquer(db *tsdb.DB, retention time.Duration, maxOctets int64, retard time.Duration) error {
	return db.ApplyConfig(&config.Config{StorageConfig: config.StorageConfig{TSDBConfig: &config.TSDBConfig{
		OutOfOrderTimeWindow: retard.Milliseconds(),
		Retention: &config.TSDBRetentionConfig{
			Time: model.Duration(retention),
			Size: units.Base2Bytes(maxOctets),
		},
	}}})
}

// Reglages renvoie les réglages en cours.
func (m *Moteur) Reglages() Reglages {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.reglages
}

// Ecrire ajoute les points dans la base brute. Un point refusé par la base
// (trop ancien, déjà reçu, désordre hors fenêtre) est compté et ignoré : il ne
// fait pas échouer le lot. Seule une erreur de la base elle-même est renvoyée.
func (m *Moteur) Ecrire(ctx context.Context, series []stockage.Serie) error {
	app := m.brut.Appender(ctx)
	var b labels.ScratchBuilder
	var ok, rejetes uint64
	for i := range series {
		s := &series[i]
		b.Reset()
		for _, l := range s.Labels {
			b.Add(l.Nom, l.Valeur)
		}
		b.Sort()
		lset := b.Labels()
		var ref storage.SeriesRef
		for _, p := range s.Points {
			r, err := app.Append(ref, lset, p.T, p.V)
			switch {
			case err == nil:
				ref = r
				ok++
			case errors.Is(err, storage.ErrOutOfOrderSample), errors.Is(err, storage.ErrOutOfBounds),
				errors.Is(err, storage.ErrTooOldSample), errors.Is(err, storage.ErrDuplicateSampleForTimestamp):
				rejetes++
			default:
				return errors.Join(err, app.Rollback())
			}
		}
	}
	if err := app.Commit(); err != nil {
		return err
	}
	m.Points.Add(ok)
	m.Rejetes.Add(rejetes)
	return nil
}

// Interroger ouvre une lecture sur la bonne base pour [debut, fin] (ms) :
// brute si la période tient dans sa rétention, agrégée sinon. À fermer.
func (m *Moteur) Interroger(debut, fin int64) (q storage.Querier, agrege bool, err error) {
	limite := m.opt.Maintenant().Add(-m.Reglages().RetentionBrut).UnixMilli()
	if debut >= limite {
		q, err = m.brut.Querier(debut, fin)
		return q, false, err
	}
	q, err = m.agrege.Querier(debut, fin)
	return q, true, err
}

// Requetes : base brute, pour le moteur d'alertes (lecture seule, PromQL).
func (m *Moteur) Requetes() storage.Queryable { return m.brut }

// LimitesLecture bornent une lecture (protection mémoire).
type LimitesLecture struct {
	MaxSeries, MaxPoints int
}

// ErrTropDeDonnees : la lecture dépasse les limites ; affiner les filtres.
var ErrTropDeDonnees = errors.New("trop de données demandées : affinez les filtres ou la période")

// Lire renvoie les séries qui correspondent aux filtres sur [debut, fin] (ms).
// Sert aux tests et aux requêtes nommées (étapes 4 et 5).
func (m *Moteur) Lire(ctx context.Context, debut, fin int64, lim LimitesLecture, filtres ...*labels.Matcher) ([]stockage.Serie, error) {
	q, _, err := m.Interroger(debut, fin)
	if err != nil {
		return nil, err
	}
	defer q.Close()
	return lireTout(ctx, q, lim, filtres...)
}

func lireTout(ctx context.Context, q storage.Querier, lim LimitesLecture, filtres ...*labels.Matcher) ([]stockage.Serie, error) {
	ss := q.Select(ctx, true, nil, filtres...)
	var out []stockage.Serie
	points := 0
	var it chunkenc.Iterator
	for ss.Next() {
		if len(out) >= lim.MaxSeries {
			return nil, ErrTropDeDonnees
		}
		s := ss.At()
		var serie stockage.Serie
		s.Labels().Range(func(l labels.Label) {
			serie.Labels = append(serie.Labels, stockage.Label{Nom: l.Name, Valeur: l.Value})
		})
		it = s.Iterator(it)
		for it.Next() == chunkenc.ValFloat {
			if points++; points > lim.MaxPoints {
				return nil, ErrTropDeDonnees
			}
			t, v := it.At()
			serie.Points = append(serie.Points, stockage.Point{T: t, V: v})
		}
		if err := it.Err(); err != nil {
			return nil, err
		}
		out = append(out, serie)
	}
	return out, ss.Err()
}

// Instantane écrit une copie cohérente des deux bases dans dossier
// (brut/ et agrege/, tête comprise), plus la progression des agrégats.
// Sert à la sauvegarde ; l'écriture continue pendant ce temps.
func (m *Moteur) Instantane(dossier string) error {
	if err := m.brut.Snapshot(filepath.Join(dossier, "brut"), true); err != nil {
		return err
	}
	m.muAgreg.Lock() // progression cohérente avec la base agrégée
	defer m.muAgreg.Unlock()
	if err := m.agrege.Snapshot(filepath.Join(dossier, "agrege"), true); err != nil {
		return err
	}
	if p := m.lireProgression(); p > 0 {
		return os.WriteFile(filepath.Join(dossier, "agrege", "progression"), []byte(strconv.FormatInt(p, 10)+"\n"), 0o600)
	}
	return nil
}

// Taille renvoie l'espace disque occupé (octets) par chaque base.
func (m *Moteur) Taille() (brut, agrege int64) {
	return tailleDossier(filepath.Join(m.opt.Dossier, "brut")), tailleDossier(filepath.Join(m.opt.Dossier, "agrege"))
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

// --- Agrégation ----------------------------------------------------------------

// estCompteur : un compteur ne doit pas être moyenné (rate deviendrait faux) ;
// on garde sa dernière valeur. Reconnu à son nom (conventions Prometheus).
func estCompteur(nom string) bool {
	for _, s := range []string{"_total", "_count", "_sum", "_bucket"} {
		if strings.HasSuffix(nom, s) {
			return true
		}
	}
	return false
}

// fichierProgression : dernière fenêtre agrégée (ms), pour reprendre après un
// redémarrage sans trou ni doublon.
func (m *Moteur) fichierProgression() string {
	return filepath.Join(m.opt.Dossier, "agrege", "progression")
}

func (m *Moteur) lireProgression() int64 {
	b, err := os.ReadFile(m.fichierProgression())
	if err != nil {
		return 0
	}
	v, _ := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	return v
}

func (m *Moteur) ecrireProgression(t int64) error {
	tmp := m.fichierProgression() + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.FormatInt(t, 10)+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, m.fichierProgression())
}

func (m *Moteur) boucleAgregation() {
	defer m.fin.Done()
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		if err := m.Agreger(context.Background()); err != nil {
			m.opt.Journal.Error("agrégation", "erreur", err)
		}
		select {
		case <-m.arret:
			return
		case <-t.C:
		}
	}
}

// Agreger calcule toutes les fenêtres terminées depuis plus de Retard et pas
// encore agrégées. Appelée chaque minute (et directement dans les tests).
func (m *Moteur) Agreger(ctx context.Context) error {
	m.muAgreg.Lock()
	defer m.muAgreg.Unlock()
	pas := Fenetre.Milliseconds()
	derniere := (m.opt.Maintenant().Add(-m.opt.Retard).UnixMilli() / pas) * pas // fin de la dernière fenêtre complète
	debut := m.lireProgression()
	if debut == 0 {
		// Première fois : depuis le plus ancien point brut.
		min := m.brut.Head().MinTime()
		if bs := m.brut.Blocks(); len(bs) > 0 && bs[0].MinTime() < min {
			min = bs[0].MinTime()
		}
		if min == int64(^uint64(0)>>1) || min <= 0 { // base vide
			return m.ecrireProgression(derniere)
		}
		debut = (min / pas) * pas
	}
	// Pas plus loin en arrière que la rétention brute (données déjà parties).
	if bas := (m.opt.Maintenant().Add(-m.Reglages().RetentionBrut).UnixMilli() / pas) * pas; debut < bas {
		debut = bas
	}
	for t0 := debut; t0+pas <= derniere; t0 += pas {
		select {
		case <-m.arret:
			return nil
		default:
		}
		if err := m.agregerFenetre(ctx, t0, t0+pas); err != nil {
			return err
		}
		if err := m.ecrireProgression(t0 + pas); err != nil {
			return err
		}
	}
	return nil
}

// agregerFenetre : un point par série pour [t0, t1), horodaté t1 - 1 ms.
func (m *Moteur) agregerFenetre(ctx context.Context, t0, t1 int64) error {
	q, err := m.brut.Querier(t0, t1-1)
	if err != nil {
		return err
	}
	defer q.Close()
	ss := q.Select(ctx, false, nil, labels.MustNewMatcher(labels.MatchRegexp, labels.MetricName, ".+"))
	app := m.agrege.Appender(ctx)
	var it chunkenc.Iterator
	n := uint64(0)
	for ss.Next() {
		s := ss.At()
		compteur := estCompteur(s.Labels().Get(labels.MetricName))
		var somme, derniere float64
		nb := 0
		it = s.Iterator(it)
		for it.Next() == chunkenc.ValFloat {
			_, v := it.At()
			somme += v
			derniere = v
			nb++
		}
		if err := it.Err(); err != nil {
			return errors.Join(err, app.Rollback())
		}
		if nb == 0 {
			continue
		}
		v := somme / float64(nb)
		if compteur {
			v = derniere
		}
		if _, err := app.Append(0, s.Labels(), t1-1, v); err != nil &&
			!errors.Is(err, storage.ErrDuplicateSampleForTimestamp) && !errors.Is(err, storage.ErrOutOfOrderSample) &&
			!errors.Is(err, storage.ErrOutOfBounds) && !errors.Is(err, storage.ErrTooOldSample) {
			return errors.Join(err, app.Rollback())
		}
		n++
	}
	if err := ss.Err(); err != nil {
		return errors.Join(err, app.Rollback())
	}
	if err := app.Commit(); err != nil {
		return err
	}
	m.Agreges.Add(n)
	return nil
}

// Effacer supprime l'historique des séries qui correspondent aux filtres
// (ex. instance="vieille-machine"), dans la base brute et les agrégats.
// La TSDB marque les points supprimés (« tombstones ») ; la place est rendue
// à la compaction suivante (≤ 2 h), sans réécriture immédiate.
func (m *Moteur) Effacer(ctx context.Context, filtres ...*labels.Matcher) error {
	if len(filtres) == 0 {
		return errors.New("au moins un filtre")
	}
	return errors.Join(m.brut.Delete(ctx, math.MinInt64, math.MaxInt64, filtres...),
		m.agrege.Delete(ctx, math.MinInt64, math.MaxInt64, filtres...))
}

// Etat : nombre de séries en mémoire et plus ancienne donnée de chaque base
// (millisecondes ; 0 = vide). Sert à la page « Conservation des données ».
func (m *Moteur) Etat() (series uint64, brutMin, agregeMin int64) {
	plusAncien := func(db *tsdb.DB) int64 {
		if bs := db.Blocks(); len(bs) > 0 {
			return bs[0].Meta().MinTime
		}
		if t := db.Head().MinTime(); t != math.MaxInt64 {
			return t
		}
		return 0
	}
	return m.brut.Head().NumSeries(), plusAncien(m.brut), plusAncien(m.agrege)
}
