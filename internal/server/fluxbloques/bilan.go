// SPDX-License-Identifier: GPL-3.0-or-later

package fluxbloques

import (
	"context"
	"net/netip"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Filtre : un seul critère à la fois (clic sur un bloc de « Mon réseau »).
type Filtre struct {
	PareFeu, Pays, Port string
}

// Vide : aucun filtre.
func (f Filtre) Vide() bool { return f == Filtre{} }

// Compte : un nom et son nombre de blocages.
type Compte struct {
	Nom      string
	N        int
	PareFeux int // adresses : nombre de pare-feu qui l'ont bloquée
}

// Paire : deux noms (pays et pare-feu, ou pare-feu et port).
type Paire struct{ A, B string }

// Barre : blocages d'une tranche de temps.
type Barre struct {
	Debut time.Time
	N     int
}

// Bilan : tout ce que montre la page pour une période.
type Bilan struct {
	Total                      int
	Avant                      int // même durée juste avant (si AvantConnu)
	AvantConnu                 bool
	NbIP                       int
	IPPlus                     bool // limite atteinte : « plus de NbIP »
	NbPays                     int
	Pays, Ports, PareFeux, IPs []Compte // du plus gros au plus petit
	PaysPareFeu, PareFeuPort   map[Paire]int
	Estime                     bool // répartition estimée (filtre pays ou port)
	SansAdresses               bool // filtre port : adresses non suivies par port
	Barres                     []Barre
	LargeurBarre               time.Duration
	Pic                        int // indice de la barre la plus haute si elle sort du lot, sinon -1
	PicIP, PicPort             string
}

type ligneBase struct {
	heure int64
	genre string
	a, b  string
	n     int
}

// lireHeures : lignes de la base entre debut (inclus) et fin (exclu).
func (m *Magasin) lireHeures(ctx context.Context, debut, fin int64, genre string) ([]ligneBase, error) {
	if m.opt.DB == nil {
		return nil, nil
	}
	q := `SELECT heure, genre, a, b, n FROM heures WHERE heure >= ? AND heure < ?`
	args := []any{debut, fin}
	if genre != "" {
		q += ` AND genre = ?`
		args = append(args, genre)
	}
	rows, err := m.opt.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ligneBase
	for rows.Next() {
		var l ligneBase
		if err := rows.Scan(&l.heure, &l.genre, &l.a, &l.b, &l.n); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// seauxMinutes : copies des seaux d'une minute qui couvrent [debut, fin).
func (m *Magasin) seauxMinutes(debut, fin time.Time) []*seau {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*seau
	for _, s := range m.minutes {
		if s != nil && s.debut >= debut.Unix()/60*60 && s.debut < fin.Unix() {
			out = append(out, s.copie())
		}
	}
	return out
}

// paysCache : pays d'une adresse (cache partagé avec la réception).
func (m *Magasin) paysCache(ip string) string {
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.paysDe(a)
}

// Bilan calcule ce que montre la page pour [debut, fin), en nbBarres tranches.
// Dernière heure : seaux d'une minute en mémoire. Au-delà : sommes faites
// par SQLite sur les résumés par heure (mémoire constante, même sur 30 j),
// après écriture de l'heure en cours.
func (m *Magasin) Bilan(ctx context.Context, debut, fin time.Time, f Filtre, nbBarres int) (Bilan, error) {
	b := Bilan{PaysPareFeu: map[Paire]int{}, PareFeuPort: map[Paire]int{}, Pic: -1}
	grain := time.Hour
	if !debut.Before(m.opt.Maintenant().Add(-time.Hour).Truncate(time.Minute)) {
		grain = time.Minute
	}
	// Tranches : au moins le grain des seaux.
	if nbBarres < 1 {
		nbBarres = 1
	}
	largeur := max(fin.Sub(debut)/time.Duration(nbBarres), grain)
	largeur = largeur.Truncate(grain)
	debutBarres := debut.Truncate(grain)
	nb := int((fin.Sub(debutBarres) + largeur - 1) / largeur)
	b.Barres = make([]Barre, nb)
	for i := range b.Barres {
		b.Barres[i].Debut = debutBarres.Add(time.Duration(i) * largeur)
	}
	b.LargeurBarre = largeur
	barre := func(t int64) int { return int(time.Unix(t, 0).Sub(debutBarres) / largeur) }

	pays, ports, pfs, ips := map[string]int{}, map[string]int{}, map[string]int{}, map[string]int{}
	nbPF := map[string]int{}
	if grain == time.Minute {
		seaux := m.seauxMinutes(debut, fin)
		ipPF := map[string]map[string]bool{}
		for _, s := range seaux {
			total := m.ajouterSeau(s, f, pays, ports, pfs, ips, ipPF, &b)
			b.Total += total
			if i := barre(s.debut); i >= 0 && i < nb {
				b.Barres[i].N += total
			}
			if s.ipsPerdues > 0 {
				b.IPPlus = true
			}
		}
		for ip, set := range ipPF {
			nbPF[ip] = len(set)
		}
		b.NbIP = len(ips)
		m.chercherPic(&b, seaux, f, debutBarres, largeur)
	} else if m.opt.DB != nil {
		// Cache d'une minute : 7 j et 30 j coûtent jusqu'à une seconde de calcul.
		cle := cleCache{debut.Unix() / 60, fin.Unix() / 60, f, nbBarres}
		if r, ok := m.cache.lire(cle, m.opt.Maintenant()); ok {
			return r, nil
		}
		m.Ecrire(ctx, true)
		if err := m.bilanBase(ctx, &b, debut, fin, f, pays, ports, pfs, ips, nbPF, barre); err != nil {
			return b, err
		}
		defer func() { m.cache.garder(cle, b, m.opt.Maintenant()) }()
	}
	if f.Pays != "" || f.Port != "" {
		b.Estime = true
		b.SansAdresses = f.Port != ""
		repartir(&b, pays, pfs, ports, f)
	}
	b.Pays, b.Ports, b.PareFeux = trier(pays), trier(ports), trier(pfs)
	b.IPs = trier(ips)
	if len(b.IPs) > maxIPBilan {
		b.IPs = b.IPs[:maxIPBilan]
	}
	for i := range b.IPs {
		b.IPs[i].PareFeux = nbPF[b.IPs[i].Nom]
	}
	for p := range pays {
		if p != "" && p != Autres {
			b.NbPays++
		}
	}
	// Période d'avant (totaux seulement, sans filtre ou par pare-feu).
	if grain == time.Hour && f.Pays == "" && f.Port == "" && m.opt.DB != nil {
		d := fin.Sub(debut)
		debutAvant := debut.Add(-d).Unix() / 3600 * 3600
		// Comparaison seulement si la base couvre toute la période d'avant
		// (sinon « +2 500 % » juste après l'installation).
		var premiere int64
		_ = m.opt.DB.QueryRowContext(ctx, `SELECT coalesce(min(heure), 0) FROM heures WHERE genre = 't'`).Scan(&premiere)
		lignes, err := m.lireHeures(ctx, debutAvant, debut.Unix()/3600*3600, gTotal)
		if err == nil && len(lignes) > 0 && premiere > 0 && premiere <= debutAvant+3600 {
			b.AvantConnu = true
			for _, l := range lignes {
				if f.PareFeu == "" || l.a == f.PareFeu {
					b.Avant += l.n
				}
			}
		}
	}
	return b, nil
}

// maxIPBilan : adresses rendues par Bilan (les plus actives).
const maxIPBilan = 50

// bilanBase : sommes par SQLite sur les résumés par heure.
func (m *Magasin) bilanBase(ctx context.Context, b *Bilan, debut, fin time.Time, f Filtre,
	pays, ports, pfs, ips, nbPF map[string]int, barre func(int64) int) error {
	h0, h1 := debut.Unix()/3600*3600, fin.Unix()
	type somme struct {
		a, b string
		n    int
	}
	// sommes : « SELECT x, y, sum(n) … GROUP BY x, y » ; colonnes absentes : ''.
	sommes := func(q string, args ...any) ([]somme, error) {
		rows, err := m.opt.DB.QueryContext(ctx, q, append([]any{h0, h1}, args...)...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []somme
		for rows.Next() {
			var x somme
			if err := rows.Scan(&x.a, &x.b, &x.n); err != nil {
				return nil, err
			}
			out = append(out, x)
		}
		return out, rows.Err()
	}
	plage := ` FROM heures WHERE heure >= ? AND heure < ? AND genre = `
	var parHeure, liensPays, liensPorts []somme
	var err error
	switch {
	case f.Port != "":
		if parHeure, err = sommes(`SELECT CAST(heure AS TEXT), '', sum(n)`+plage+`'x' AND b = ? GROUP BY heure`, f.Port); err != nil {
			return err
		}
		x, err := sommes(`SELECT a, '', sum(n)`+plage+`'x' AND b = ? GROUP BY a`, f.Port)
		if err != nil {
			return err
		}
		for _, l := range x {
			pays[l.a] += l.n
		}
		if liensPorts, err = sommes(`SELECT a, b, sum(n)`+plage+`'o' AND b = ? GROUP BY a, b`, f.Port); err != nil {
			return err
		}
	case f.Pays != "":
		if parHeure, err = sommes(`SELECT CAST(heure AS TEXT), '', sum(n)`+plage+`'p' AND b = ? GROUP BY heure`, f.Pays); err != nil {
			return err
		}
		if liensPays, err = sommes(`SELECT a, b, sum(n)`+plage+`'p' AND b = ? GROUP BY a, b`, f.Pays); err != nil {
			return err
		}
		x, err := sommes(`SELECT b, '', sum(n)`+plage+`'x' AND a = ? GROUP BY b`, f.Pays)
		if err != nil {
			return err
		}
		for _, l := range x {
			ports[l.a] += l.n
		}
	default:
		filtrePF, args := "", []any{}
		if f.PareFeu != "" {
			filtrePF, args = " AND a = ?", []any{f.PareFeu}
		}
		if parHeure, err = sommes(`SELECT CAST(heure AS TEXT), '', sum(n)`+plage+`'t'`+filtrePF+` GROUP BY heure`, args...); err != nil {
			return err
		}
		if liensPays, err = sommes(`SELECT a, b, sum(n)`+plage+`'p'`+filtrePF+` GROUP BY a, b`, args...); err != nil {
			return err
		}
		if liensPorts, err = sommes(`SELECT a, b, sum(n)`+plage+`'o'`+filtrePF+` GROUP BY a, b`, args...); err != nil {
			return err
		}
	}
	for _, l := range parHeure {
		t, _ := strconv.ParseInt(l.a, 10, 64)
		b.Total += l.n
		if i := barre(t); i >= 0 && i < len(b.Barres) {
			b.Barres[i].N += l.n
		}
	}
	for _, l := range liensPays {
		pays[l.b] += l.n
		pfs[l.a] += l.n
		b.PaysPareFeu[Paire{l.b, l.a}] += l.n
	}
	for _, l := range liensPorts {
		ports[l.b] += l.n
		if f.Port != "" {
			pfs[l.a] += l.n
		}
		b.PareFeuPort[Paire{l.a, l.b}] += l.n
	}
	// Adresses (pas suivies par port).
	if f.Port == "" {
		filtre, args := "", []any{}
		if f.PareFeu != "" {
			filtre, args = " AND b = ?", []any{f.PareFeu}
		}
		// #nosec G202 -- morceaux constants ; valeurs passées en paramètres
		requete := `SELECT a, count(DISTINCT b), sum(n)` + plage + `'i'` + filtre + ` GROUP BY a ORDER BY 3 DESC`
		if f.Pays == "" {
			requete += ` LIMIT ` + strconv.Itoa(maxIPBilan)
			if err := m.opt.DB.QueryRowContext(ctx, `SELECT count(DISTINCT a)`+plage+`'i'`+filtre, append([]any{h0, h1}, args...)...).Scan(&b.NbIP); err != nil {
				return err
			}
		}
		rows, err := m.opt.DB.QueryContext(ctx, requete, append([]any{h0, h1}, args...)...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var ip string
			var npf, n int
			if err := rows.Scan(&ip, &npf, &n); err != nil {
				_ = rows.Close()
				return err
			}
			// Filtre pays : adresses lues une à une, gardées si du bon pays
			// (le tri par nombre est déjà fait : les premières suffisent).
			if f.Pays != "" {
				if m.paysCache(ip) != f.Pays {
					continue
				}
				b.NbIP++
				if len(ips) >= maxIPBilan {
					continue
				}
			}
			ips[ip], nbPF[ip] = n, npf
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		var tronque int
		_ = m.opt.DB.QueryRowContext(ctx, `SELECT count(*)`+plage+`'z'`, h0, h1).Scan(&tronque)
		b.IPPlus = tronque > 0
	}
	// Pic : la tranche la plus haute, son adresse et son port.
	b.Pic = pic(b.Barres)
	if b.Pic >= 0 {
		p0 := b.Barres[b.Pic].Debut.Unix()
		p1 := p0 + int64(b.LargeurBarre/time.Second)
		_ = m.opt.DB.QueryRowContext(ctx, `SELECT a FROM heures WHERE heure >= ? AND heure < ? AND genre = 'i' GROUP BY a ORDER BY sum(n) DESC LIMIT 1`, p0, p1).Scan(&b.PicIP)
		_ = m.opt.DB.QueryRowContext(ctx, `SELECT b FROM heures WHERE heure >= ? AND heure < ? AND genre = 'o' AND b != ? GROUP BY b ORDER BY sum(n) DESC LIMIT 1`, p0, p1, Autres).Scan(&b.PicPort)
	}
	return nil
}

// ajouterSeau : contributions d'un seau selon le filtre ; renvoie son total.
func (m *Magasin) ajouterSeau(s *seau, f Filtre, pays, ports, pfs, ips map[string]int, ipPF map[string]map[string]bool, b *Bilan) int {
	total := 0
	noterIP := func(ip, pf string, n int) {
		ips[ip] += n
		if ipPF[ip] == nil {
			ipPF[ip] = map[string]bool{}
		}
		ipPF[ip][pf] = true
	}
	switch {
	case f.Port != "":
		for k, n := range s.xport {
			if k.b == f.Port {
				pays[k.a] += n
				total += n
			}
		}
		for k, n := range s.ports {
			if k.b == f.Port {
				pfs[k.a] += n
				ports[k.b] += n
				b.PareFeuPort[Paire{k.a, k.b}] += n
			}
		}
	case f.Pays != "":
		for k, n := range s.pays {
			if k.b == f.Pays {
				pays[k.b] += n
				pfs[k.a] += n
				b.PaysPareFeu[Paire{k.b, k.a}] += n
				total += n
			}
		}
		for k, n := range s.xport {
			if k.a == f.Pays {
				ports[k.b] += n
			}
		}
		for k, n := range s.ips {
			if m.paysCache(k.a) == f.Pays {
				noterIP(k.a, k.b, n)
			}
		}
	default:
		for pf, n := range s.total {
			if f.PareFeu == "" || pf == f.PareFeu {
				total += n
			}
		}
		for k, n := range s.pays {
			if f.PareFeu == "" || k.a == f.PareFeu {
				pays[k.b] += n
				pfs[k.a] += n
				b.PaysPareFeu[Paire{k.b, k.a}] += n
			}
		}
		for k, n := range s.ports {
			if f.PareFeu == "" || k.a == f.PareFeu {
				ports[k.b] += n
				b.PareFeuPort[Paire{k.a, k.b}] += n
			}
		}
		for k, n := range s.ips {
			if f.PareFeu == "" || k.b == f.PareFeu {
				noterIP(k.a, k.b, n)
			}
		}
	}
	return total
}

// repartir : avec un filtre pays (ou port), la base ne sait pas quel
// pare-feu a vu quel port (ou quel pays) : répartition au prorata.
func repartir(b *Bilan, pays, pfs, ports map[string]int, f Filtre) {
	totalPF := 0
	for _, n := range pfs {
		totalPF += n
	}
	if totalPF == 0 {
		return
	}
	if f.Pays != "" {
		for pf, npf := range pfs {
			for port, n := range ports {
				if v := n * npf / totalPF; v > 0 {
					b.PareFeuPort[Paire{pf, port}] += v
				}
			}
		}
		return
	}
	for p, np := range pays {
		for pf, npf := range pfs {
			if v := np * npf / totalPF; v > 0 {
				b.PaysPareFeu[Paire{p, pf}] += v
			}
		}
	}
}

func trier(m map[string]int) []Compte {
	out := make([]Compte, 0, len(m))
	for k, n := range m {
		out = append(out, Compte{Nom: k, N: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].N != out[j].N {
			return out[i].N > out[j].N
		}
		return out[i].Nom < out[j].Nom
	})
	return out
}

// pic : indice de la barre la plus haute si elle dépasse nettement les
// autres (au moins 2 fois la médiane et 20 blocages), sinon -1.
func pic(barres []Barre) int {
	if len(barres) < 4 {
		return -1
	}
	vals := make([]int, len(barres))
	haut := 0
	for i, x := range barres {
		vals[i] = x.N
		if x.N > barres[haut].N {
			haut = i
		}
	}
	sort.Ints(vals)
	med := vals[len(vals)/2]
	if barres[haut].N < 20 || barres[haut].N < 2*max(med, 1) {
		return -1
	}
	return haut
}

// chercherPic (dernière heure) : pic, avec l'adresse et le port qui y dominent.
func (m *Magasin) chercherPic(b *Bilan, seaux []*seau, f Filtre, debut time.Time, largeur time.Duration) {
	haut := pic(b.Barres)
	if haut < 0 {
		return
	}
	b.Pic = haut
	pays, ports, pfs, ips := map[string]int{}, map[string]int{}, map[string]int{}, map[string]int{}
	ipPF := map[string]map[string]bool{}
	tmp := Bilan{PaysPareFeu: map[Paire]int{}, PareFeuPort: map[Paire]int{}}
	for _, s := range seaux {
		if int(time.Unix(s.debut, 0).Sub(debut)/largeur) == haut {
			m.ajouterSeau(s, f, pays, ports, pfs, ips, ipPF, &tmp)
		}
	}
	if t := trier(ips); len(t) > 0 {
		b.PicIP = t[0].Nom
	}
	if t := trier(ports); len(t) > 0 {
		b.PicPort = t[0].Nom
	}
}

// --- Cache des bilans sur plusieurs heures -------------------------------------

type cleCache struct {
	debut, fin int64 // minutes
	f          Filtre
	barres     int
}

type entreeCache struct {
	b      Bilan
	expire time.Time
}

type cacheBilans struct {
	mu sync.Mutex
	m  map[cleCache]entreeCache
}

const maxCacheBilans = 20

func (c *cacheBilans) lire(k cleCache, t time.Time) (Bilan, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[k]
	if !ok || t.After(e.expire) {
		return Bilan{}, false
	}
	return e.b, true
}

func (c *cacheBilans) garder(k cleCache, b Bilan, t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil || len(c.m) >= maxCacheBilans {
		c.m = map[cleCache]entreeCache{}
	}
	c.m[k] = entreeCache{b, t.Add(time.Minute)}
}
