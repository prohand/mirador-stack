// SPDX-License-Identifier: GPL-3.0-or-later

// Package fluxbloques : flux entrants bloqués par les pare-feu (SPEC § 135).
//
// Sources : lignes iptables du noyau envoyées par les agents (gardées aussi
// dans les logs) et comptages filterlog d'OPNsense envoyés par le collecteur
// toutes les 10 s (comptés ici seulement, jamais rangés dans les logs).
//
// Rangement :
//   - en mémoire, 60 seaux d'une minute (vues « Direct » et « 1 h », fil des
//     derniers blocages, « par minute en ce moment ») et le seau de l'heure
//     en cours ;
//   - dans une petite base SQLite à part (flux.db, hors sauvegardes), un
//     résumé par heure : totaux par pare-feu, pays × pare-feu, pare-feu ×
//     port, pays × port (30 jours), adresses les plus actives (7 jours).
//
// Mémoire bornée : chaque seau garde un nombre limité de clés ; au-delà,
// les blocages comptent dans « autres » (ou sont seulement totalisés pour
// les adresses : la case « adresses » affiche alors « plus de N »).
package fluxbloques

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/netip"
	"sort"
	"strconv"
	"sync"
	"time"

	"mirador-stack/internal/protocole"
	"mirador-stack/internal/server/stockage"
)

// Limites.
const (
	maxRecents = 200 // fil des derniers blocages

	// Clés par seau d'une minute et par seau d'une heure.
	maxIPMinute, maxAutresMinute = 300, 200
	maxIPHeure, maxAutresHeure   = 10_000, 2000

	// Gardé en base pour chaque heure (les plus gros).
	garderIPHeure, garderPortsHeure, garderPaysPortsHeure = 1000, 150, 150

	dureePays     = 30 * 24 * time.Hour // totaux, pays, ports
	dureeIP       = 7 * 24 * time.Hour  // adresses
	ecritureHeure = 5 * time.Minute     // heure en cours réécrite toutes les 5 min

	maxCachePays = 5000
)

// Autres : nom de la clé qui regroupe ce qui dépasse les limites.
const Autres = "autres"

// Evenement : un blocage (ou N identiques) pour le fil en direct.
type Evenement struct {
	ID                              int64  `json:"id"`
	T                               int64  `json:"t"` // millisecondes
	PareFeu, Interface, Regle, Pays string `json:"-"`
	IP, Port                        string `json:"-"`
	N                               int    `json:"n"`
}

// Options du magasin.
type Options struct {
	DB         *sql.DB                 // base à part (flux.db) ; nil : mémoire seulement
	Pays       func(netip.Addr) string // GéoIP ; nil : pays inconnu
	Maintenant func() time.Time
	Journal    *slog.Logger
}

// Magasin : compteurs des flux bloqués.
type Magasin struct {
	opt Options

	mu      sync.Mutex
	minutes [60]*seau
	heure   *seau
	finies  []*seau     // heures terminées pas encore écrites
	recents []Evenement // anneau, du plus ancien au plus récent
	dernier int64       // dernier ID donné
	signal  chan struct{}
	pays    map[netip.Addr]string
	ecritLe time.Time // dernière écriture de l'heure en cours
	recus   uint64    // blocages comptés depuis le démarrage
	cache   cacheBilans
}

// Nouveau prépare le magasin (table créée au besoin).
func Nouveau(ctx context.Context, opt Options) (*Magasin, error) {
	if opt.Maintenant == nil {
		opt.Maintenant = time.Now
	}
	if opt.Journal == nil {
		opt.Journal = slog.New(slog.DiscardHandler)
	}
	if opt.DB != nil {
		if _, err := opt.DB.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS heures (
			heure INTEGER NOT NULL, genre TEXT NOT NULL, a TEXT NOT NULL, b TEXT NOT NULL, n INTEGER NOT NULL,
			PRIMARY KEY (heure, genre, a, b)) WITHOUT ROWID`); err != nil {
			return nil, err
		}
	}
	m := &Magasin{opt: opt, signal: make(chan struct{}), pays: map[netip.Addr]string{}}
	if opt.DB != nil {
		// Redémarrage en cours d'heure : ce qui a déjà été écrit pour
		// l'heure en cours est repris (sinon la prochaine écriture l'effacerait).
		h := opt.Maintenant().Unix() / 3600 * 3600
		s, err := m.lireHeures(ctx, h, h+3600, "")
		if err != nil {
			return nil, err
		}
		m.heure = nouveauSeau(h, maxIPHeure, maxAutresHeure)
		for _, l := range s {
			m.heure.reprendre(l)
		}
		m.ecritLe = opt.Maintenant()
	}
	return m, nil
}

// --- Seaux --------------------------------------------------------------------

// Genres des compteurs (aussi colonne « genre » de la base).
const (
	gTotal    = "t" // a = pare-feu
	gPays     = "p" // a = pare-feu, b = pays
	gPort     = "o" // a = pare-feu, b = port (« tcp/22 »)
	gIP       = "i" // a = adresse, b = pare-feu
	gPaysPort = "x" // a = pays, b = port
	gTronque  = "z" // a = « ip » : adresses perdues (limite)
)

type paire struct{ a, b string }

type seau struct {
	debut                   int64 // secondes
	total                   map[string]int
	pays, ports, ips, xport map[paire]int
	ipsPerdues              int
	maxIP, maxAutres        int
}

func nouveauSeau(debut int64, maxIP, maxAutres int) *seau {
	return &seau{debut: debut, total: map[string]int{}, pays: map[paire]int{}, ports: map[paire]int{},
		ips: map[paire]int{}, xport: map[paire]int{}, maxIP: maxIP, maxAutres: maxAutres}
}

// ajouterBorne : compte k, ou « autres » (même premier élément) au-delà de max.
func ajouterBorne(m map[paire]int, k paire, n, max int, autresA bool) {
	if _, ok := m[k]; !ok && len(m) >= max {
		if autresA {
			k.a = Autres
		} else {
			k.b = Autres
		}
	}
	m[k] += n
}

func (s *seau) ajouter(pf, pays, port, ip string, n int) {
	if len(s.total) < s.maxAutres || s.total[pf] > 0 {
		s.total[pf] += n
	} else {
		s.total[Autres] += n
	}
	ajouterBorne(s.pays, paire{pf, pays}, n, s.maxAutres, false)
	ajouterBorne(s.ports, paire{pf, port}, n, s.maxAutres, false)
	ajouterBorne(s.xport, paire{pays, port}, n, s.maxAutres, false)
	k := paire{ip, pf}
	if _, ok := s.ips[k]; !ok && len(s.ips) >= s.maxIP {
		s.ipsPerdues += n
		return
	}
	s.ips[k] += n
}

// reprendre : une ligne lue en base remise dans le seau.
func (s *seau) reprendre(l ligneBase) {
	switch l.genre {
	case gTotal:
		s.total[l.a] += l.n
	case gPays:
		s.pays[paire{l.a, l.b}] += l.n
	case gPort:
		s.ports[paire{l.a, l.b}] += l.n
	case gIP:
		s.ips[paire{l.a, l.b}] += l.n
	case gPaysPort:
		s.xport[paire{l.a, l.b}] += l.n
	case gTronque:
		s.ipsPerdues += l.n
	}
}

func (s *seau) copie() *seau {
	c := nouveauSeau(s.debut, s.maxIP, s.maxAutres)
	for k, v := range s.total {
		c.total[k] = v
	}
	for _, p := range [][2]map[paire]int{{s.pays, c.pays}, {s.ports, c.ports}, {s.ips, c.ips}, {s.xport, c.xport}} {
		for k, v := range p[0] {
			p[1][k] = v
		}
	}
	c.ipsPerdues = s.ipsPerdues
	return c
}

// --- Réception ----------------------------------------------------------------

// Recevoir compte un blocage vu par le pare-feu pf (nom de la machine ou de
// l'équipement) à l'heure t.
func (m *Magasin) Recevoir(pf string, f protocole.FluxBloque, t time.Time) {
	if pf == "" || f.N <= 0 {
		return
	}
	maintenant := m.opt.Maintenant()
	// Heure plausible seulement : une ligne en retard de plus d'une heure
	// (tampon d'un agent) ou venue du futur compte à l'heure de réception.
	if t.After(maintenant) || t.Before(maintenant.Add(-time.Hour)) {
		t = maintenant
	}
	port := f.Proto
	if f.Port > 0 {
		port += "/" + strconv.Itoa(f.Port)
	}
	if port == "" {
		port = "?"
	}
	ip := f.IP.String()
	m.mu.Lock()
	defer m.mu.Unlock()
	pays := m.paysDe(f.IP)
	m.recus += uint64(f.N) // #nosec G115 -- N ≥ 1
	min := t.Unix() / 60 * 60
	i := (min / 60) % 60
	if m.minutes[i] == nil || m.minutes[i].debut != min {
		m.minutes[i] = nouveauSeau(min, maxIPMinute, maxAutresMinute)
	}
	m.minutes[i].ajouter(pf, pays, port, ip, f.N)
	h := maintenant.Unix() / 3600 * 3600
	if m.heure == nil || m.heure.debut != h {
		// L'heure précédente, s'il y en a une, est écrite par la boucle
		// (tourner) : elle a pu manquer la bascule de quelques secondes.
		m.basculer(h)
	}
	m.heure.ajouter(pf, pays, port, ip, f.N)
	m.dernier++
	e := Evenement{ID: m.dernier, T: t.UnixMilli(), PareFeu: pf, Interface: f.Interface, Regle: f.Regle,
		Pays: pays, IP: ip, Port: port, N: f.N}
	if len(m.recents) >= maxRecents {
		copy(m.recents, m.recents[1:])
		m.recents = m.recents[:maxRecents-1]
	}
	m.recents = append(m.recents, e)
	close(m.signal)
	m.signal = make(chan struct{})
}

// paysDe : GéoIP avec un petit cache (sous m.mu).
func (m *Magasin) paysDe(ip netip.Addr) string {
	if m.opt.Pays == nil {
		return ""
	}
	if p, ok := m.pays[ip]; ok {
		return p
	}
	if len(m.pays) >= maxCachePays {
		m.pays = map[netip.Addr]string{}
	}
	p := m.opt.Pays(ip)
	m.pays[ip] = p
	return p
}

// basculer ouvre le seau de l'heure h ; l'ancien attend d'être écrit (sous m.mu).
func (m *Magasin) basculer(h int64) {
	if m.heure != nil && m.heure.debut < h {
		m.finies = append(m.finies, m.heure)
	}
	m.heure = nouveauSeau(h, maxIPHeure, maxAutresHeure)
}

// Lignes : branchement sur l'ingestion des logs. Les lignes iptables
// (entrées publiques) sont comptées et gardées ; les comptages d'un
// collecteur (ident IdentFlux) sont comptés et retirés. Une ligne de
// comptage envoyée par un agent est gardée comme un log ordinaire et
// comptée pour 1 (seul un collecteur peut annoncer N blocages).
func (m *Magasin) Lignes(collecteur bool, lignes []stockage.Ligne) []stockage.Ligne {
	garder := lignes[:0]
	for _, l := range lignes {
		comptage := collecteur && l.Champs["ident"] == protocole.IdentFlux
		if f, ok := protocole.LireFluxBloque(l.Msg, comptage); ok {
			m.Recevoir(l.Champs["host"], f, time.UnixMilli(l.T))
		}
		if !comptage {
			garder = append(garder, l)
		}
	}
	return garder
}

// Recus : blocages comptés depuis le démarrage.
func (m *Magasin) Recus() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.recus
}

// --- Fil en direct ----------------------------------------------------------

// Depuis : blocages reçus après l'ID apres (au plus n, les plus récents),
// et un canal fermé à la prochaine arrivée.
func (m *Magasin) Depuis(apres int64, n int) ([]Evenement, <-chan struct{}) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Evenement
	for i := len(m.recents) - 1; i >= 0 && len(out) < n; i-- {
		if m.recents[i].ID <= apres {
			break
		}
		out = append(out, m.recents[i])
	}
	// Du plus ancien au plus récent.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, m.signal
}

// DernierID : ID du dernier blocage reçu.
func (m *Magasin) DernierID() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.dernier
}

// ParMinute : blocages des n dernières minutes complètes (la plus ancienne
// d'abord) et de la minute en cours.
func (m *Magasin) ParMinute(n int) (minutes []int, enCours int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.opt.Maintenant().Unix() / 60 * 60
	minutes = make([]int, 0, n)
	for k := n; k >= 1; k-- {
		minutes = append(minutes, m.totalMinute(now-int64(k)*60))
	}
	return minutes, m.totalMinute(now)
}

func (m *Magasin) totalMinute(debut int64) int {
	s := m.minutes[(debut/60)%60]
	if s == nil || s.debut != debut {
		return 0
	}
	t := 0
	for _, v := range s.total {
		t += v
	}
	return t
}

// --- Écriture en base ---------------------------------------------------------

// Boucle : écrit les heures terminées, réécrit l'heure en cours toutes les
// 5 min, efface ce qui dépasse la durée de conservation (chaque heure).
// À l'arrêt, l'heure en cours est écrite une dernière fois.
func (m *Magasin) Boucle(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	nettoye := time.Time{}
	for {
		select {
		case <-ctx.Done():
			m.Ecrire(context.Background(), true)
			return
		case <-t.C:
			m.Ecrire(ctx, false)
			if time.Since(nettoye) >= time.Hour {
				nettoye = time.Now()
				m.Nettoyer(ctx)
			}
		}
	}
}

// Ecrire : heures terminées, et l'heure en cours si 5 min sont passées
// (ou si force).
func (m *Magasin) Ecrire(ctx context.Context, force bool) {
	if m.opt.DB == nil {
		return
	}
	maintenant := m.opt.Maintenant()
	m.mu.Lock()
	h := maintenant.Unix() / 3600 * 3600
	if m.heure != nil && m.heure.debut < h {
		m.basculer(h)
	}
	finies := m.finies
	m.finies = nil
	var enCours *seau
	if m.heure != nil && (force || maintenant.Sub(m.ecritLe) >= ecritureHeure) {
		enCours = m.heure.copie()
		m.ecritLe = maintenant
	}
	m.mu.Unlock()
	if enCours != nil {
		finies = append(finies, enCours)
	}
	for _, s := range finies {
		if err := m.ecrireHeure(ctx, s); err != nil {
			m.opt.Journal.Warn("flux bloqués : écriture de l'heure impossible", "heure", time.Unix(s.debut, 0), "erreur", err)
		}
	}
}

// garderPlusGros : les n plus gros de m (et le reste regroupé sous « autres »
// dans le même premier élément si regrouper).
func garderPlusGros(m map[paire]int, n int, regrouper bool) map[paire]int {
	if len(m) <= n {
		return m
	}
	cles := make([]paire, 0, len(m))
	for k := range m {
		cles = append(cles, k)
	}
	sort.Slice(cles, func(i, j int) bool { return m[cles[i]] > m[cles[j]] })
	out := make(map[paire]int, n+8)
	for i, k := range cles {
		if i < n {
			out[k] += m[k]
		} else if regrouper {
			out[paire{k.a, Autres}] += m[k]
		}
	}
	return out
}

func (m *Magasin) ecrireHeure(ctx context.Context, s *seau) error {
	tx, err := m.opt.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM heures WHERE heure = ?`, s.debut); err != nil {
		return err
	}
	st, err := tx.PrepareContext(ctx, `INSERT INTO heures (heure, genre, a, b, n) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (heure, genre, a, b) DO UPDATE SET n = n + excluded.n`)
	if err != nil {
		return err
	}
	defer st.Close()
	ins := func(g, a, b string, n int) error {
		_, err := st.ExecContext(ctx, s.debut, g, a, b, n)
		return err
	}
	for pf, n := range s.total {
		if err := ins(gTotal, pf, "", n); err != nil {
			return err
		}
	}
	ips := garderPlusGros(s.ips, garderIPHeure, false)
	perdues := s.ipsPerdues
	if len(ips) < len(s.ips) {
		perdues += len(s.ips) - len(ips) // adresses (et non blocages) laissées de côté
	}
	for _, g := range []struct {
		genre string
		m     map[paire]int
	}{{gPays, s.pays}, {gPort, garderPlusGros(s.ports, garderPortsHeure, true)},
		{gPaysPort, garderPlusGros(s.xport, garderPaysPortsHeure, true)}, {gIP, ips}} {
		for k, n := range g.m {
			if err := ins(g.genre, k.a, k.b, n); err != nil {
				return err
			}
		}
	}
	if perdues > 0 {
		if err := ins(gTronque, "ip", "", perdues); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Nettoyer efface ce qui dépasse la durée de conservation.
func (m *Magasin) Nettoyer(ctx context.Context) {
	if m.opt.DB == nil {
		return
	}
	now := m.opt.Maintenant()
	_, err1 := m.opt.DB.ExecContext(ctx, `DELETE FROM heures WHERE heure < ?`, now.Add(-dureePays).Unix())
	_, err2 := m.opt.DB.ExecContext(ctx, `DELETE FROM heures WHERE genre IN ('i', 'z') AND heure < ?`, now.Add(-dureeIP).Unix())
	if err := errors.Join(err1, err2); err != nil {
		m.opt.Journal.Warn("flux bloqués : nettoyage impossible", "erreur", err)
	}
}
