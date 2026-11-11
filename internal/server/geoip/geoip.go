// SPDX-License-Identifier: GPL-3.0-or-later

// Package geoip : pays d'une adresse IP, avec la base gratuite « DB-IP
// country lite » (CC BY 4.0, mention « IP Geolocation by DB-IP » affichée).
//
// Automatique (demande du 10/11) :
//   - téléchargée par le serveur, chaque mois, depuis une adresse FIGÉE dans
//     le code (download.db-ip.com), gardée compressée sur disque (~4,6 Mo) ;
//   - chargée en mémoire seulement quand on en a besoin (page « Flux
//     bloqués »), puis libérée après 10 min sans usage : coût nul le reste
//     du temps (~8 Mo pendant l'usage, SPEC § 18.8).
package geoip

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// AdresseBase : modèle de l'adresse de téléchargement (année-mois).
const AdresseBase = "https://download.db-ip.com/free/dbip-country-lite-%s.csv.gz"

const (
	nomFichier     = "dbip-country-lite.csv.gz"
	tailleMax      = 64 << 20 // téléchargement borné
	dureeInutile   = 10 * time.Minute
	verifierToutes = 6 * time.Hour
	// reessai : après un échec, nouvel essai 30 min plus tard (pas 6 h).
	reessai = 30 * time.Minute
)

// Base : la base sur disque et, si elle sert, en mémoire.
type Base struct {
	Dossier string
	Journal *slog.Logger
	// Adresse : modèle d'adresse (tests) ; défaut AdresseBase.
	Adresse string
	Client  *http.Client

	mu      sync.Mutex
	table   *table
	vueLe   time.Time
	erreur  string
	majLe   time.Time // dernier essai (ou date fictive qui fixe le prochain)
	essaiLe time.Time // dernier essai réel (affiché)
	force   bool      // téléchargement demandé depuis l'interface
}

// table : début de chaque plage (triées) et pays (index dans codes).
type table struct {
	v4     []uint32
	p4     []uint8
	v6     []uint64 // 64 premiers bits (les plages DB-IP IPv6 sont au moins des /64)
	p6     []uint8
	codes  []string
	charge time.Time
}

func (b *Base) chemin() string { return filepath.Join(b.Dossier, nomFichier) }

// Etat : date de la base sur disque, dernière erreur, chargée ou non.
func (b *Base) Etat() (fichier time.Time, enMemoire bool, erreur string) {
	if st, err := os.Stat(b.chemin()); err == nil {
		fichier = st.ModTime()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return fichier, b.table != nil, b.erreur
}

// Essais : dernier essai de téléchargement et essai demandé en attente.
func (b *Base) Essais() (dernier time.Time, demande bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.essaiLe, b.force
}

// Supprimer : efface la base du disque et de la mémoire.
func (b *Base) Supprimer() {
	b.mu.Lock()
	b.table = nil
	b.mu.Unlock()
	err := os.Remove(b.chemin())
	if err == nil {
		b.Journal.Info("GéoIP : base supprimée (réglage décoché)")
	} else if !errors.Is(err, os.ErrNotExist) {
		b.Journal.Warn("GéoIP : suppression impossible", "erreur", err)
	}
}

// Demander : téléchargement au prochain tour (moins d'une minute), même si
// la base du mois est déjà là ou si le dernier essai est récent (bouton
// « Télécharger maintenant », case cochée dans l'interface).
func (b *Base) Demander() {
	b.mu.Lock()
	b.force = true
	b.mu.Unlock()
}

// Pays : code ISO à 2 lettres (” si inconnu ou base absente).
func (b *Base) Pays(ip netip.Addr) string {
	t := b.charger()
	if t == nil || !ip.IsValid() {
		return ""
	}
	ip = ip.Unmap()
	var i int
	var p []uint8
	if ip.Is4() {
		a := ip.As4()
		v := binary.BigEndian.Uint32(a[:])
		i = sort.Search(len(t.v4), func(k int) bool { return t.v4[k] > v }) - 1
		p = t.p4
	} else {
		a := ip.As16()
		v := binary.BigEndian.Uint64(a[:8])
		i = sort.Search(len(t.v6), func(k int) bool { return t.v6[k] > v }) - 1
		p = t.p6
	}
	if i < 0 {
		return ""
	}
	c := t.codes[p[i]]
	if c == "ZZ" {
		return ""
	}
	return c
}

func (b *Base) charger() *table {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.vueLe = time.Now()
	if b.table != nil {
		return b.table
	}
	t, err := lire(b.chemin())
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			b.erreur = err.Error()
		}
		return nil
	}
	b.table = t
	return t
}

// lire : CSV « début,fin,pays » compressé ; une plage manquante entre deux
// lignes devient « inconnu ».
func lire(chemin string) (*table, error) {
	f, err := os.Open(chemin) // #nosec G304 -- fichier du dossier des données
	if err != nil {
		return nil, err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	t := &table{codes: []string{"ZZ"}, charge: time.Now()}
	index := map[string]uint8{"ZZ": 0}
	code := func(c string) uint8 {
		if i, ok := index[c]; ok {
			return i
		}
		if len(t.codes) >= 255 {
			return 0
		}
		index[c] = uint8(len(t.codes)) // #nosec G115 -- borné à 255
		t.codes = append(t.codes, c)
		return index[c]
	}
	sc := bufio.NewScanner(io.LimitReader(zr, 256<<20))
	var fin4 uint32
	var fin6 uint64
	premier4, premier6 := true, true
	for sc.Scan() {
		champs := strings.Split(sc.Text(), ",")
		if len(champs) != 3 || len(champs[2]) != 2 {
			continue
		}
		d, e1 := netip.ParseAddr(champs[0])
		f2, e2 := netip.ParseAddr(champs[1])
		if e1 != nil || e2 != nil {
			continue
		}
		c := code(champs[2])
		if d.Is4() {
			a, z := d.As4(), f2.As4()
			v, w := binary.BigEndian.Uint32(a[:]), binary.BigEndian.Uint32(z[:])
			if !premier4 && v > fin4+1 {
				t.v4, t.p4 = append(t.v4, fin4+1), append(t.p4, 0)
			}
			t.v4, t.p4, fin4, premier4 = append(t.v4, v), append(t.p4, c), w, false
		} else {
			a, z := d.As16(), f2.As16()
			v, w := binary.BigEndian.Uint64(a[:8]), binary.BigEndian.Uint64(z[:8])
			if len(t.v6) > 0 && t.v6[len(t.v6)-1] == v {
				t.p6[len(t.p6)-1] = c // plage plus fine qu'un /64 : la dernière l'emporte
				fin6 = w
				continue
			}
			if !premier6 && v > fin6+1 {
				t.v6, t.p6 = append(t.v6, fin6+1), append(t.p6, 0)
			}
			t.v6, t.p6, fin6, premier6 = append(t.v6, v), append(t.p6, c), w, false
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	// Après la dernière plage : inconnu.
	if !premier4 && fin4 < ^uint32(0) {
		t.v4, t.p4 = append(t.v4, fin4+1), append(t.p4, 0)
	}
	if !premier6 && fin6 < ^uint64(0) {
		t.v6, t.p6 = append(t.v6, fin6+1), append(t.p6, 0)
	}
	if len(t.v4) < 1000 {
		return nil, errors.New("base GéoIP trop petite ou illisible")
	}
	return t, nil
}

// Boucle : mise à jour mensuelle et libération de la mémoire inutilisée.
func (b *Base) Boucle(ctx context.Context, actif func() bool) {
	tic := time.NewTicker(time.Minute)
	defer tic.Stop()
	for {
		b.Tour(ctx, time.Now(), actif())
		select {
		case <-ctx.Done():
			return
		case <-tic.C:
		}
	}
}

// Tour : libère la table inutilisée ; vérifie la base toutes les 6 h.
func (b *Base) Tour(ctx context.Context, t time.Time, actif bool) {
	b.mu.Lock()
	if b.majLe.IsZero() {
		// Premier essai 5 min après le démarrage (pas de pic au démarrage).
		b.majLe = t.Add(-verifierToutes + 5*time.Minute)
	}
	if b.table != nil && t.Sub(b.vueLe) > dureeInutile {
		b.table = nil // rendue au ramasse-miettes
	}
	force := b.force
	if !actif && !force {
		// Case décochée : base effacée du disque et de la mémoire (les pays ne
		// sont plus affichés). Recochée : retéléchargée dans la minute.
		b.table, b.erreur = nil, ""
		b.mu.Unlock()
		b.Supprimer()
		return
	}
	doit := force || (actif && t.Sub(b.majLe) > verifierToutes)
	if doit {
		b.majLe, b.essaiLe, b.force = t, t, false
	}
	b.mu.Unlock()
	if !doit {
		return
	}
	if force {
		b.Journal.Info("GéoIP : téléchargement demandé depuis l'interface")
	}
	if err := b.mettreAJour(ctx, t, force); err != nil {
		b.mu.Lock()
		b.erreur = err.Error()
		// Échec (réseau, pare-feu…) : on réessaie dans 30 min, pas dans 6 h.
		b.majLe = t.Add(-verifierToutes + reessai)
		b.mu.Unlock()
		b.Journal.Warn("GéoIP : mise à jour impossible", "erreur", err)
	}
}

// MettreAJour télécharge la base du mois si celle du disque est plus
// ancienne (DB-IP la publie en début de mois ; à défaut, le mois précédent).
func (b *Base) MettreAJour(ctx context.Context, t time.Time) error {
	return b.mettreAJour(ctx, t, false)
}

// mettreAJour : force = télécharger même si la base du mois est là.
func (b *Base) mettreAJour(ctx context.Context, t time.Time, force bool) error {
	if st, err := os.Stat(b.chemin()); err == nil && !force {
		m := st.ModTime().UTC()
		if m.Year() == t.UTC().Year() && m.Month() == t.UTC().Month() {
			return nil // déjà la base de ce mois
		}
	}
	modele := b.Adresse
	if modele == "" {
		modele = AdresseBase
	}
	client := b.Client
	if client == nil {
		client = &http.Client{Timeout: 3 * time.Minute,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	var dernier error
	for _, mois := range []time.Time{t.UTC(), t.UTC().AddDate(0, -1, 0)} {
		if err := b.telecharger(ctx, client, fmt.Sprintf(modele, mois.Format("2006-01"))); err != nil {
			dernier = err
			continue
		}
		b.mu.Lock()
		b.table, b.erreur = nil, "" // relue au prochain usage
		b.mu.Unlock()
		b.Journal.Info("GéoIP : base mise à jour", "mois", mois.Format("2006-01"))
		return nil
	}
	return dernier
}

func (b *Base) telecharger(ctx context.Context, client *http.Client, adresse string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, adresse, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "mirador-server")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("téléchargement : %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("téléchargement : réponse %d", resp.StatusCode)
	}
	if err := os.MkdirAll(b.Dossier, 0o750); err != nil {
		return err
	}
	tmp := b.chemin() + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600) // #nosec G304 -- dossier des données
	if err != nil {
		return err
	}
	n, err := io.Copy(f, io.LimitReader(resp.Body, tailleMax+1))
	if e := f.Close(); err == nil {
		err = e
	}
	if err == nil && n > tailleMax {
		err = errors.New("fichier trop gros")
	}
	if err == nil {
		// Vérifié AVANT de remplacer l'ancienne base : un fichier abîmé ne la casse pas.
		_, err = lire(tmp)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, b.chemin())
}
