// SPDX-License-Identifier: GPL-3.0-or-later

package ingestion

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// Machine : identité tirée du token. Son nom est imposé aux données reçues.
type Machine struct {
	ID   int64
	Nom  string
	Type string // agent, collecteur
	// Site et Env : étiquettes choisies dans l'interface ; imposées aux
	// données d'un agent (vides = celles envoyées par l'agent sont gardées).
	Site, Env string
	Groupe    string // groupe de configuration, imposé aussi en étiquette « groupe »
}

// ErrToken : token absent, inconnu, révoqué, ou machine non approuvée.
var ErrToken = errors.New("token absent ou invalide")

// ErrIP : token valable, mais utilisé depuis une adresse non autorisée pour
// cette machine (HTTP 403 : l'agent garde ses lots et réessaie, rien n'est
// perdu le temps de corriger la liste dans l'interface).
var ErrIP = errors.New("adresse non autorisée pour ce token")

// Durées du cache des tokens. Un token révoqué cesse de marcher au plus tard
// après dureeCache (ou tout de suite avec Oublier).
const (
	dureeCache        = 60 * time.Second
	dureeCacheRefus   = 10 * time.Second // court : une machine tout juste approuvée attend peu
	tailleMaxCache    = 4096
	longueurMinToken  = 20
	longueurMaxToken  = 256
	intervalleVuLe    = 60 * time.Second
	prefixeAutorisant = "Bearer "
)

// Auth vérifie les tokens des machines. La base ne contient que leur empreinte
// SHA-256 : un vol de la base ne donne aucun token utilisable.
//
// Le token est aléatoire (256 bits) : un simple SHA-256 suffit (pas besoin
// d'un hachage lent comme pour un mot de passe), et la recherche par
// empreinte ne révèle rien d'utile sur le token.
type Auth struct {
	db *sql.DB

	mu    sync.Mutex
	cache map[[32]byte]entreeCache
	vus   map[int64]vu // machine → dernière requête, écrit par lots
}

type vu struct {
	t  int64 // secondes Unix
	ip netip.Addr
}

type entreeCache struct {
	m      Machine
	ips    []netip.Prefix // vide : toute adresse
	ok     bool           // false : token refusé (cache négatif)
	expire int64          // nanosecondes Unix
}

// NouvelleAuth prépare la vérification des tokens.
func NouvelleAuth(db *sql.DB) *Auth {
	return &Auth{db: db, cache: make(map[[32]byte]entreeCache), vus: make(map[int64]vu)}
}

// Empreinte : ce qui est gardé en base à la place du token.
func Empreinte(token string) [32]byte { return sha256.Sum256([]byte(token)) }

// extraireToken lit « Authorization: Bearer xxx ». Longueur bornée.
func extraireToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if len(h) <= len(prefixeAutorisant) || !strings.EqualFold(h[:len(prefixeAutorisant)], prefixeAutorisant) {
		return "", false
	}
	t := strings.TrimSpace(h[len(prefixeAutorisant):])
	if len(t) < longueurMinToken || len(t) > longueurMaxToken {
		return "", false
	}
	return t, true
}

// Verifier renvoie la machine du token. ErrToken si refusé, ErrIP si
// l'adresse n'est pas dans la liste de la machine ; une autre erreur si la
// base ne répond pas (le client réessaiera).
func (a *Auth) Verifier(ctx context.Context, token string, ip netip.Addr, maintenant time.Time) (Machine, error) {
	e := Empreinte(token)
	a.mu.Lock()
	c, trouve := a.cache[e]
	a.mu.Unlock()
	if !trouve || maintenant.UnixNano() >= c.expire {
		var err error
		if c, err = a.chercher(ctx, e, maintenant); err != nil {
			return Machine{}, err
		}
	}
	if !c.ok {
		return Machine{}, ErrToken
	}
	if !ipAutorisee(c.ips, ip) {
		return c.m, ErrIP
	}
	a.mu.Lock()
	a.vus[c.m.ID] = vu{t: maintenant.Unix(), ip: ip}
	a.mu.Unlock()
	return c.m, nil
}

// ipAutorisee : liste vide = toute adresse ; sinon l'adresse doit être dans
// un des préfixes (une adresse inconnue est refusée).
func ipAutorisee(ips []netip.Prefix, ip netip.Addr) bool {
	if len(ips) == 0 {
		return true
	}
	for _, p := range ips {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// LireIPs lit une liste « adresse ou préfixe » séparée par des virgules
// (ex. « 203.0.113.7, 198.51.100.0/24, 2001:db8:1:2::/64 »). Sert aussi à
// vérifier la saisie dans l'interface.
func LireIPs(s string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, champ := range strings.Split(s, ",") {
		champ = strings.TrimSpace(champ)
		if champ == "" {
			continue
		}
		if strings.Contains(champ, "/") {
			p, err := netip.ParsePrefix(champ)
			if err != nil {
				return nil, err
			}
			out = append(out, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(champ)
		if err != nil {
			return nil, err
		}
		a = a.Unmap()
		out = append(out, netip.PrefixFrom(a, a.BitLen()))
	}
	return out, nil
}

// chercher interroge la base et met le résultat en cache.
func (a *Auth) chercher(ctx context.Context, e [32]byte, maintenant time.Time) (entreeCache, error) {
	var c entreeCache
	var ips string
	err := a.db.QueryRowContext(ctx, `SELECT m.id, m.nom, m.type, m.site, m.env, m.groupe, m.ips_autorisees FROM tokens t
		JOIN machines m ON m.id = t.machine_id
		WHERE t.empreinte = ? AND t.revoque_le = 0 AND m.statut = 'approuvee'`, e[:]).Scan(&c.m.ID, &c.m.Nom, &c.m.Type, &c.m.Site, &c.m.Env, &c.m.Groupe, &ips)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		c.expire = maintenant.Add(dureeCacheRefus).UnixNano()
	case err != nil:
		return c, err
	default:
		c.ok = true
		c.expire = maintenant.Add(dureeCache).UnixNano()
		if c.ips, err = LireIPs(ips); err != nil {
			// Liste illisible : on refuse tout plutôt que tout accepter.
			c.ips = []netip.Prefix{{}}
		}
	}
	a.mu.Lock()
	// Cache borné : s'il est plein (ex. avalanche de faux tokens), on le vide.
	// Les vrais tokens sont relus en base au passage suivant.
	if len(a.cache) >= tailleMaxCache {
		clear(a.cache)
	}
	a.cache[e] = c
	a.mu.Unlock()
	return c, nil
}

// Oublier vide le cache (à appeler après une révocation ou un refus de
// machine, pour un effet immédiat).
func (a *Auth) Oublier() {
	a.mu.Lock()
	clear(a.cache)
	a.mu.Unlock()
}

// EcrireVus enregistre en base l'heure et l'adresse de dernière réception de
// chaque machine, en une seule transaction (une écriture par minute au lieu d'une
// par requête).
func (a *Auth) EcrireVus(ctx context.Context) error {
	a.mu.Lock()
	vus := a.vus
	a.vus = make(map[int64]vu, len(vus))
	a.mu.Unlock()
	if len(vus) == 0 {
		return nil
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // sans effet après Commit
	for id, v := range vus {
		ip := ""
		if v.ip.IsValid() {
			ip = v.ip.String()
		}
		if _, err := tx.ExecContext(ctx, `UPDATE machines SET vu_le = ?, derniere_ip = ? WHERE id = ? AND vu_le <= ?`,
			v.t, ip, id, v.t); err != nil {
			return err
		}
	}
	return tx.Commit()
}
