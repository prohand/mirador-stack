// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"

	"mirador-stack/internal/server/flotte"
	"mirador-stack/internal/server/ingestion"
	"mirador-stack/internal/server/web"
)

// Liste de blocage (demande du 27/11, maquette validée) : mirador PUBLIE la
// liste des adresses insistantes ; OPNsense (alias « URL Table ») et les VPS
// (script mirador-blocage, installé par l'utilisateur) viennent la lire.
// mirador ne modifie jamais un pare-feu (SPEC § 136). Désactivée par défaut ;
// l'adresse de lecture porte un jeton (144 bits, gardé haché, montré une
// seule fois). Jamais dans la liste : LAN, adresses des machines, adresses
// autorisées de l'interface, adresses SSH « connues », liste de l'utilisateur.

const (
	cleBlocage        = "flux.blocage"
	maxBlocage        = 2000
	gardeListeBlocage = 10 * time.Minute
)

// ReglageBlocage : réglage de la liste.
type ReglageBlocage struct {
	Actif       bool   `json:"actif"`
	MinPareFeux int    `json:"min_pare_feux"` // vue par N pare-feu ou plus
	MinBlocages int    `json:"min_blocages"`  // ou bloquée N fois ou plus sur la période
	Jours       int    `json:"jours"`         // période (1 à 7 : adresses gardées 7 jours)
	Exclus      string `json:"exclus"`        // adresses ou préfixes jamais bloqués (un par ligne)
	Jeton       string `json:"jeton"`         // empreinte SHA-256 (hex) ; vide : pas encore créé
}

func blocageDefaut() ReglageBlocage {
	return ReglageBlocage{MinPareFeux: 2, MinBlocages: 500, Jours: 7}
}

func (r ReglageBlocage) valider() error {
	var errs []error
	if r.MinPareFeux < 1 || r.MinPareFeux > 50 {
		errs = append(errs, errors.New("pare-feu : 1 à 50"))
	}
	if r.MinBlocages < 10 || r.MinBlocages > 10_000_000 {
		errs = append(errs, errors.New("blocages : 10 au moins"))
	}
	if r.Jours < 1 || r.Jours > 7 {
		errs = append(errs, errors.New("période : 1 à 7 jours (adresses gardées 7 jours)"))
	}
	if len(r.Exclus) > 4096 {
		errs = append(errs, errors.New("adresses jamais bloquées : 4 Kio au plus"))
	} else if _, err := ingestion.LireIPs(r.Exclus); err != nil {
		errs = append(errs, errors.New("adresses jamais bloquées : "+err.Error()))
	}
	return errors.Join(errs...)
}

func (c *Console) lireBlocage(ctx context.Context) ReglageBlocage {
	r := blocageDefaut()
	if ok, err := lireJSONReglage(ctx, c.db, cleBlocage, &r); ok && (err != nil || r.valider() != nil) {
		return blocageDefaut()
	}
	return r
}

func (c *Console) ecrireBlocage(ctx context.Context, r ReglageBlocage, qui, action, ip string, t time.Time) error {
	if err := r.valider(); err != nil {
		return err
	}
	brut, _ := json.Marshal(r)
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // sans effet après Commit
	if _, err := tx.ExecContext(ctx, `INSERT INTO reglages(cle, valeur, modifie_le, modifie_par) VALUES (?, ?, ?, ?)
		ON CONFLICT(cle) DO UPDATE SET valeur = excluded.valeur, modifie_le = excluded.modifie_le, modifie_par = excluded.modifie_par`,
		cleBlocage, string(brut), t.Unix(), qui); err != nil {
		return err
	}
	r.Jeton = "" // jamais dans l'audit
	audit, _ := json.Marshal(r)
	if err := Auditer(ctx, tx, qui, action, "", "", string(audit), ip, t); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	c.blocage.oublier()
	return nil
}

// nouveauJetonBlocage : jeton (montré une fois) et son empreinte.
func nouveauJetonBlocage() (jeton, empreinte string, err error) {
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	jeton = base64.RawURLEncoding.EncodeToString(b)
	e := sha256.Sum256([]byte(jeton))
	return jeton, hex.EncodeToString(e[:]), nil
}

// cacheBlocage : liste calculée gardée 10 min (OPNsense et chaque VPS la
// relisent souvent).
type cacheBlocage struct {
	mu     sync.Mutex
	expire time.Time
	liste  []string
	nb     int // adresses écartées (jamais bloquées)
}

func (k *cacheBlocage) oublier() {
	k.mu.Lock()
	k.expire = time.Time{}
	k.mu.Unlock()
}

// listeBlocage : adresses à bloquer maintenant, triées.
func (c *Console) listeBlocage(ctx context.Context, r ReglageBlocage, t time.Time) (liste []string, ecartees int) {
	c.blocage.mu.Lock()
	defer c.blocage.mu.Unlock()
	if t.Before(c.blocage.expire) {
		return c.blocage.liste, c.blocage.nb
	}
	liste, ecartees = c.calculerBlocage(ctx, r, t)
	c.blocage.liste, c.blocage.nb, c.blocage.expire = liste, ecartees, t.Add(gardeListeBlocage)
	return liste, ecartees
}

func (c *Console) calculerBlocage(ctx context.Context, r ReglageBlocage, t time.Time) ([]string, int) {
	if c.opt.Flux == nil || c.opt.Flux.Entrants == nil {
		return nil, 0
	}
	cs, err := c.opt.Flux.Entrants.Insistantes(ctx, t.Add(-time.Duration(r.Jours)*24*time.Hour), t, r.MinPareFeux, r.MinBlocages, maxBlocage+500)
	if err != nil {
		c.journal.Warn("liste de blocage", "erreur", err)
		return nil, 0
	}
	garde := c.jamaisBloquees(ctx, r, t)
	var out []string
	ecartees := 0
	for _, x := range cs {
		a, err := netip.ParseAddr(x.Nom)
		if err != nil {
			continue
		}
		a = a.Unmap()
		if garde(a) {
			ecartees++
			continue
		}
		if len(out) < maxBlocage {
			out = append(out, a.String())
		}
	}
	sort.Strings(out)
	return out, ecartees
}

// jamaisBloquees : LAN, adresses des machines (relevées par les agents),
// adresses autorisées de l'interface, adresses SSH connues, liste de
// l'utilisateur.
func (c *Console) jamaisBloquees(ctx context.Context, r ReglageBlocage, t time.Time) func(netip.Addr) bool {
	exactes := map[netip.Addr]bool{}
	var prefixes []netip.Prefix
	if p, err := ingestion.LireIPs(r.Exclus); err == nil {
		prefixes = append(prefixes, p...)
	}
	prefixes = append(prefixes, c.securite.Load().prefixes()...)
	if es, err := c.instant(ctx, `max by (adresse) (last_over_time(mirador_adresse_info[1h]))`, t); err == nil {
		for _, e := range es {
			if a, err := netip.ParseAddr(e.Etiquettes.Get("adresse")); err == nil {
				exactes[a.Unmap()] = true
			}
		}
	}
	// Adresses SSH connues (toutes, quelle que soit la machine).
	if cs, err := flotte.ListerSSHConnues(ctx, c.db); err == nil {
		for _, x := range cs {
			prefixes = append(prefixes, x.Prefixe)
		}
	}
	return func(a netip.Addr) bool {
		if flotte.AdresseLocale(a) || exactes[a] {
			return true
		}
		for _, p := range prefixes {
			if p.Contains(a) {
				return true
			}
		}
		return false
	}
}

// servirBlocage : /blocage/<jeton>.txt (liste) et /blocage/installer.sh
// (script, public, sans secret) ; avant la liste d'adresses autorisées de
// l'interface (un VPS ou OPNsense n'y est pas forcément), sans session.
func (c *Console) servirBlocage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "méthode refusée", http.StatusMethodNotAllowed)
		return
	}
	t := c.maintenant()
	ip := web.AdresseClient(r).String()
	if !c.lecturesBlocage.permis(ip, t) {
		http.Error(w, "trop d'appels", http.StatusTooManyRequests)
		return
	}
	nom := strings.TrimPrefix(r.URL.Path, "/blocage/")
	if nom == "installer.sh" {
		s := statiques["/statique/mirador-blocage.sh"]
		if s == nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="mirador-blocage.sh"`)
		_, _ = w.Write(s.brut)
		return
	}
	jeton, ok := strings.CutSuffix(nom, ".txt")
	reg := c.lireBlocage(r.Context())
	e := sha256.Sum256([]byte(jeton))
	if !ok || !reg.Actif || reg.Jeton == "" || subtle.ConstantTimeCompare([]byte(hex.EncodeToString(e[:])), []byte(reg.Jeton)) != 1 {
		c.journal.Warn("liste de blocage : jeton inconnu ou liste désactivée", "ip", ip)
		http.NotFound(w, r)
		return
	}
	liste, _ := c.listeBlocage(r.Context(), reg, t)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	var b strings.Builder
	for _, a := range liste {
		b.WriteString(a + "\n")
	}
	_, _ = w.Write([]byte(b.String()))
}

// vueBlocage : carte « Liste de blocage » (Flux bloqués › Mon réseau).
type vueBlocage struct {
	R            ReglageBlocage
	Nb, Ecartees int
	Base         string // https://hôte de l'interface
}

func (c *Console) vueBlocageFlux(q *req, d *donneesFlux) {
	if d.Vue != "reseau" || d.Sortant {
		return
	}
	r := c.lireBlocage(q.r.Context())
	v := &vueBlocage{R: r, Base: "https://" + q.r.Host}
	if r.Actif {
		l, ec := c.listeBlocage(q.r.Context(), r, q.t)
		v.Nb, v.Ecartees = len(l), ec
	}
	d.Blocage = v
}

// donneesJetonBlocage : page montrée une seule fois après la création du jeton.
type donneesJetonBlocage struct {
	URL, Script string
}

// postBlocage : réglages (code 2FA pour activer ou créer le jeton : la
// liste devient lisible de l'extérieur).
func (c *Console) postBlocage(q *req) {
	ctx := q.r.Context()
	f := champs{q}
	vers := "/flux-bloques?vue=reseau#blocage"
	r := c.lireBlocage(ctx)
	switch f.texte("action") {
	case "reglages":
		n := r
		var errs []error
		n.Actif = f.coche("actif")
		n.MinPareFeux, n.MinBlocages, n.Jours = f.entier("min_pare_feux", &errs), f.entier("min_blocages", &errs), f.entier("jours", &errs)
		if len(errs) > 0 {
			c.retour(q, vers, errors.Join(errs...), "")
			return
		}
		n.Exclus = strings.TrimSpace(strings.ReplaceAll(q.r.PostFormValue("exclus"), "\r\n", "\n"))
		if n.Actif && !r.Actif {
			if err := c.verifierCode(q); err != nil {
				c.retour(q, vers, err, "")
				return
			}
		}
		err := c.ecrireBlocage(ctx, n, q.qui(), "blocage_reglages", q.ip.String(), q.t)
		ok := "liste de blocage désactivée"
		if n.Actif {
			ok = "liste de blocage enregistrée"
			if n.Jeton == "" {
				ok += " : créer l'adresse de lecture (bouton « Nouvelle adresse »)"
			}
		}
		c.retour(q, vers, err, ok)
	case "jeton":
		if err := c.verifierCode(q); err != nil {
			c.retour(q, vers, err, "")
			return
		}
		jeton, empreinte, err := nouveauJetonBlocage()
		if err == nil {
			r.Jeton = empreinte
			err = c.ecrireBlocage(ctx, r, q.qui(), "blocage_jeton", q.ip.String(), q.t)
		}
		if err != nil {
			c.retour(q, vers, err, "")
			return
		}
		base := "https://" + q.r.Host
		c.rendre(q, "blocage-jeton", "Liste de blocage", "tableaux", donneesJetonBlocage{URL: base + "/blocage/" + jeton + ".txt", Script: base + "/blocage/installer.sh"})
	default:
		c.retour(q, vers, errors.New("action inconnue"), "")
	}
}
