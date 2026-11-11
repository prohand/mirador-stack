// SPDX-License-Identifier: GPL-3.0-or-later

package journaux

import (
	"context"
	"errors"
	"hash/crc32"
	"io"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"mirador-stack/internal/server/stockage"
)

// Requete : recherche de logs. Les filtres se cumulent (ET).
type Requete struct {
	Debut, Fin int64             // millisecondes Unix, bornes comprises
	Champs     map[string]string // égalités exactes (host, job, unit, level…)
	// Variantes : autres valeurs acceptées pour un champ de Champs (ex.
	// anciens noms d'une machine renommée : son historique).
	Variantes map[string][]string
	Mots      string // mots entiers, tous présents, casse ignorée (rapide : filtre de Bloom)
	Contient  string // morceau de texte, casse ignorée (lit tous les blocs de la période)
	Limite    int    // lignes renvoyées (défaut 500)
}

// Resultat : lignes les plus récentes d'abord.
type Resultat struct {
	Lignes      []stockage.Ligne
	BlocsLus    int  // blocs décompressés
	BlocsSautes int  // blocs écartés par l'index ou le filtre de Bloom
	Tronque     bool // limite de lignes, de blocs ou de durée atteinte : il peut y en avoir d'autres
}

// LimitesRecherche : protection du serveur (brief : « limites de durée et de
// taille des recherches »).
type LimitesRecherche struct {
	MaxPeriode   time.Duration // écart Fin - Debut
	MaxLignes    int           // plafond de Requete.Limite
	MaxBlocs     int           // blocs décompressés par recherche
	Duree        time.Duration // temps de calcul
	Travailleurs int           // décompressions en parallèle
}

// LimitesRechercheDefaut : VPS de 2 cœurs ; une journée du parc ≈ 1 000 blocs.
func LimitesRechercheDefaut() LimitesRecherche {
	return LimitesRecherche{MaxPeriode: 31 * 24 * time.Hour, MaxLignes: 10_000, MaxBlocs: 20_000,
		Duree: 10 * time.Second, Travailleurs: 2}
}

// ErrRequete : requête refusée (période trop longue, bornes inversées).
var ErrRequete = errors.New("recherche refusée")

type candidat struct {
	chemin string
	b      bloc
}

// Chercher applique la requête. Les blocs sont lus du plus récent au plus
// ancien ; la recherche s'arrête dès que la limite de lignes est atteinte et
// qu'aucun bloc restant ne peut contenir de ligne plus récente.
func (m *Moteur) Chercher(ctx context.Context, r Requete, lim LimitesRecherche) (Resultat, error) {
	var res Resultat
	if r.Fin < r.Debut || time.Duration(r.Fin-r.Debut)*time.Millisecond > lim.MaxPeriode {
		return res, ErrRequete
	}
	if r.Limite <= 0 {
		r.Limite = 500
	}
	r.Limite = min(r.Limite, lim.MaxLignes)
	ctx, annuler := context.WithTimeout(ctx, lim.Duree)
	defer annuler()

	filtre := nouveauFiltre(r)
	candidats, enCours := m.candidats(r)
	// Le bloc pas encore écrit est cherché aussi (logs des 30 dernières secondes).
	for i := range enCours {
		if filtre.garde(&enCours[i]) {
			res.Lignes = append(res.Lignes, enCours[i])
		}
	}

	fds := map[string]*os.File{}
	defer func() {
		for _, f := range fds {
			_ = f.Close()
		}
	}()
	for _, c := range candidats {
		if _, ok := fds[c.chemin]; !ok {
			f, err := os.Open(c.chemin) // #nosec G304 -- chemin de l'index
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					continue // purgé entre-temps
				}
				return res, err
			}
			fds[c.chemin] = f
		}
	}

	pas := max(1, lim.Travailleurs) * 2
	for debut := 0; debut < len(candidats); debut += pas {
		if ctx.Err() != nil || res.BlocsLus >= lim.MaxBlocs {
			res.Tronque = true
			break
		}
		// Limite atteinte : les blocs restants (plus anciens) ne peuvent plus
		// donner de ligne plus récente que la dernière gardée.
		if len(res.Lignes) >= r.Limite {
			garderPlusRecentes(&res.Lignes, r.Limite)
			if candidats[debut].b.tmax < res.Lignes[len(res.Lignes)-1].T {
				res.Tronque = true
				break
			}
		}
		vague := candidats[debut:min(debut+pas, len(candidats))]
		resultats := make([][]stockage.Ligne, len(vague))
		lus := make([]bool, len(vague))
		erreurs := make([]error, len(vague))
		var wg sync.WaitGroup
		sem := make(chan struct{}, max(1, lim.Travailleurs))
		for i, c := range vague {
			f := fds[c.chemin]
			if f == nil {
				continue
			}
			wg.Add(1)
			sem <- struct{}{}
			go func() {
				defer func() { <-sem; wg.Done() }()
				resultats[i], lus[i], erreurs[i] = m.lireBloc(f, c.b, filtre)
			}()
		}
		wg.Wait()
		for i := range vague {
			switch {
			case errors.Is(erreurs[i], errBloc):
				m.BlocsAbimes.Add(1)
				m.opt.Journal.Warn("bloc de logs abîmé, ignoré", "fichier", vague[i].chemin, "position", vague[i].b.pos)
			case erreurs[i] != nil:
				return res, erreurs[i]
			}
			if lus[i] {
				res.BlocsLus++
			} else {
				res.BlocsSautes++
			}
			res.Lignes = append(res.Lignes, resultats[i]...)
		}
	}
	if len(res.Lignes) > r.Limite {
		res.Tronque = true
	}
	garderPlusRecentes(&res.Lignes, r.Limite)
	return res, nil
}

// candidats : blocs (et lignes en attente) qui recoupent la période, du plus
// récent au plus ancien. Copie faite sous verrou : l'écriture continue pendant
// la recherche.
func (m *Moteur) candidats(r Requete) ([]candidat, []stockage.Ligne) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []candidat
	for _, f := range m.fichiers {
		if f.tmax < r.Debut || f.tmin > r.Fin {
			continue
		}
		for _, b := range f.blocs {
			if b.tmax >= r.Debut && b.tmin <= r.Fin {
				out = append(out, candidat{chemin: f.chemin, b: b})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].b.tmax > out[j].b.tmax })
	return out, slices.Clone(m.enCours)
}

// lireBloc lit l'en-tête, écarte le bloc si l'index ou le Bloom le permettent,
// sinon décompresse et filtre. lu = le bloc a été décompressé.
func (m *Moteur) lireBloc(f *os.File, b bloc, filtre *filtre) (lignes []stockage.Ligne, lu bool, err error) {
	e, err := lireEntete(io.NewSectionReader(f, b.pos, maxEntete+16))
	if err != nil {
		return nil, false, errBloc
	}
	if !filtre.blocPossible(e) {
		return nil, false, nil
	}
	donnees := make([]byte, e.compresse)
	if _, err := f.ReadAt(donnees, b.pos+int64(e.tailleEntete)); err != nil {
		return nil, false, errBloc
	}
	if crc32.Checksum(donnees, crc) != e.crcDonnees {
		return nil, false, errBloc
	}
	brut, err := m.dec.DecodeAll(donnees, make([]byte, 0, e.brut))
	if err != nil || len(brut) != e.brut {
		return nil, true, errBloc
	}
	err = decoderLignes(brut, e, func(l *stockage.Ligne) bool {
		if filtre.garde(l) {
			lignes = append(lignes, *l)
		}
		return true
	})
	if err != nil {
		return nil, true, errBloc
	}
	return lignes, true, nil
}

func garderPlusRecentes(l *[]stockage.Ligne, n int) {
	sort.SliceStable(*l, func(i, j int) bool { return (*l)[i].T > (*l)[j].T })
	if len(*l) > n {
		clear((*l)[n:])
		*l = (*l)[:n]
	}
}

// --- Filtre ------------------------------------------------------------------------

type filtre struct {
	r        Requete
	mots     []string
	contient string
}

func nouveauFiltre(r Requete) *filtre {
	f := &filtre{r: r, contient: strings.ToLower(r.Contient)}
	mots(r.Mots, func(w string) { f.mots = append(f.mots, w) })
	return f
}

// blocPossible : faux si l'en-tête prouve qu'aucune ligne ne correspond.
func (f *filtre) blocPossible(e *entete) bool {
	if e.tmax < f.r.Debut || e.tmin > f.r.Fin {
		return false
	}
	for nom, v := range f.r.Champs {
		vals, connu := e.index[nom]
		if connu && vals != nil && !slices.Contains(vals, v) &&
			!slices.ContainsFunc(f.r.Variantes[nom], func(x string) bool { return slices.Contains(vals, x) }) {
			return false // champ indexé, aucune valeur acceptée dans le bloc
		}
	}
	for _, w := range f.mots {
		if !peutContenir(e.bloomK, e.bloom, w) {
			return false
		}
	}
	return true
}

func (f *filtre) garde(l *stockage.Ligne) bool {
	if l.T < f.r.Debut || l.T > f.r.Fin {
		return false
	}
	for nom, v := range f.r.Champs {
		if x := l.Champs[nom]; x != v && !slices.Contains(f.r.Variantes[nom], x) {
			return false
		}
	}
	if len(f.mots) == 0 && f.contient == "" {
		return true
	}
	bas := strings.ToLower(l.Msg)
	for _, w := range f.mots {
		if !contientMot(bas, w) {
			return false
		}
	}
	return f.contient == "" || strings.Contains(bas, f.contient)
}

// contientMot : w apparaît dans s comme mot entier (pas au milieu d'un autre).
func contientMot(s, w string) bool {
	for i := 0; ; {
		j := strings.Index(s[i:], w)
		if j < 0 {
			return false
		}
		debut, fin := i+j, i+j+len(w)
		avant, _ := utf8.DecodeLastRuneInString(s[:debut])
		apres, _ := utf8.DecodeRuneInString(s[fin:])
		if (debut == 0 || !estLettre(avant)) && (fin == len(s) || !estLettre(apres)) {
			return true
		}
		i = debut + 1
	}
}

func estLettre(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }
