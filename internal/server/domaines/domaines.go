// SPDX-License-Identifier: GPL-3.0-or-later

// Package domaines suit la date d'expiration des noms de domaine (demande du
// 27/11, maquette validée). Les noms viennent des sondes (adresse d'un site,
// hôte d'un port, nom demandé au DNS) ; chaque sous-domaine est rangé sous
// son domaine principal (« cloud.example.org » → « example.org », liste
// publique des suffixes) : une ligne et une alerte par domaine.
//
// La date est lue par RDAP (le service des registres qui remplace whois) :
// l'annuaire officiel de l'IANA dit quel serveur interroger pour chaque
// extension (gardé 7 jours) ; chaque domaine est relu une fois par jour.
// Lecture seule, réponses bornées (1 Mio), https seulement.
package domaines

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/publicsuffix"

	essais "mirador-stack/internal/sondes"
)

const (
	// URLAnnuaire : liste IANA des serveurs RDAP par extension.
	URLAnnuaire      = "https://data.iana.org/rdap/dns.json"
	gardeAnnuaire    = 7 * 24 * time.Hour
	relireApres      = 24 * time.Hour
	relireErreur     = 6 * time.Hour
	tailleMaxReponse = 1 << 20
	maxDomaines      = 200
	// Metrique : date d'expiration (s) par domaine, écrite par le serveur.
	Metrique = "mirador_domaine_expiration_timestamp_seconds"
)

// Domaine : un domaine principal suivi.
type Domaine struct {
	Nom       string
	Sous      []string // sous-domaines des sondes (« cloud », « www »), triés
	Expire    time.Time
	VerifieLe time.Time
	Erreur    string
}

// Principal : « cloud.example.org » → « example.org » ; vide pour une
// adresse IP, un nom sans point ou un suffixe public seul.
func Principal(hote string) string {
	h := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(hote)), ".")
	if h == "" || !strings.Contains(h, ".") {
		return ""
	}
	if _, err := netip.ParseAddr(h); err == nil {
		return ""
	}
	d, err := publicsuffix.EffectiveTLDPlusOne(h)
	if err != nil {
		return ""
	}
	// Extension inconnue de la liste (« .lan », « .home », « .internal ») :
	// rien à demander à un registre.
	if _, icann := publicsuffix.PublicSuffix(h); !icann {
		return ""
	}
	return d
}

// hoteDe : nom d'hôte d'une sonde (adresse d'un site, hôte:port, hôte, nom
// demandé au DNS).
func hoteDe(s essais.Sonde) []string {
	var out []string
	switch s.Type {
	case essais.HTTP:
		if u, err := url.Parse(s.Cible); err == nil {
			out = append(out, u.Hostname())
		}
	case essais.DNS:
		out = append(out, s.Question)
	default:
		h := s.Cible
		if x, _, err := net.SplitHostPort(s.Cible); err == nil {
			h = x
		}
		out = append(out, h)
	}
	return out
}

// Regrouper : domaine principal → sous-domaines (« www », ou « @ » pour le
// domaine lui-même), d'après les sondes.
func Regrouper(ss []essais.Sonde) map[string][]string {
	out := map[string][]string{}
	vus := map[string]bool{}
	for _, s := range ss {
		for _, h := range hoteDe(s) {
			p := Principal(h)
			if p == "" {
				continue
			}
			h = strings.TrimSuffix(strings.ToLower(h), ".")
			sous := strings.TrimSuffix(strings.TrimSuffix(h, p), ".")
			if sous == "" {
				sous = "@"
			}
			if !vus[p+"|"+sous] {
				vus[p+"|"+sous] = true
				out[p] = append(out[p], sous)
			}
		}
	}
	for p := range out {
		sort.Strings(out[p])
	}
	return out
}

// Suivi : relecture des dates (une boucle de fond).
type Suivi struct {
	DB         *sql.DB
	Client     *http.Client
	Journal    *slog.Logger
	Maintenant func() time.Time
	Sondes     func(ctx context.Context) ([]essais.Sonde, error)
	Annuaire   string // URLAnnuaire par défaut (remplacé en test)

	mu         sync.Mutex
	serveurs   map[string]string // extension → adresse du serveur RDAP
	annuaireLe time.Time
}

// Boucle : un tour 2 min après le démarrage, puis toutes les heures.
func (s *Suivi) Boucle(ctx context.Context) {
	if s.Journal == nil {
		s.Journal = slog.New(slog.DiscardHandler)
	}
	attente := 2 * time.Minute
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(attente):
		}
		attente = time.Hour
		if err := s.Tour(ctx); err != nil {
			s.Journal.Warn("expiration des domaines", "erreur", err)
		}
	}
}

func (s *Suivi) maintenant() time.Time {
	if s.Maintenant != nil {
		return s.Maintenant()
	}
	return time.Now()
}

// Tour : liste à jour, et relecture des domaines dont la date a plus d'un
// jour (6 h après une erreur).
func (s *Suivi) Tour(ctx context.Context) error {
	ss, err := s.Sondes(ctx)
	if err != nil {
		return err
	}
	groupes := Regrouper(ss)
	t := s.maintenant()
	connus, err := Lister(ctx, s.DB)
	if err != nil {
		return err
	}
	parNom := map[string]Domaine{}
	for _, d := range connus {
		parNom[d.Nom] = d
		if _, garde := groupes[d.Nom]; !garde {
			if _, err := s.DB.ExecContext(ctx, `DELETE FROM domaines WHERE nom = ?`, d.Nom); err != nil {
				return err
			}
		}
	}
	noms := make([]string, 0, len(groupes))
	for n := range groupes {
		noms = append(noms, n)
	}
	sort.Strings(noms)
	if len(noms) > maxDomaines {
		noms = noms[:maxDomaines]
	}
	for _, n := range noms {
		d, connu := parNom[n]
		sous := strings.Join(groupes[n], ", ")
		delai := relireApres
		if d.Erreur != "" {
			delai = relireErreur
		}
		if connu && t.Sub(d.VerifieLe) < delai {
			if strings.Join(d.Sous, ", ") != sous {
				_, _ = s.DB.ExecContext(ctx, `UPDATE domaines SET sous = ? WHERE nom = ?`, sous, n)
			}
			continue
		}
		expire, errL := s.lire(ctx, n)
		msg := ""
		if errL != nil {
			msg = errL.Error()
			if connu && !d.Expire.IsZero() {
				expire = d.Expire // la dernière date lue reste valable
			}
		}
		var ex int64
		if !expire.IsZero() {
			ex = expire.Unix()
		}
		if _, err := s.DB.ExecContext(ctx, `INSERT INTO domaines(nom, sous, expire, verifie_le, erreur) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(nom) DO UPDATE SET sous = excluded.sous, expire = excluded.expire, verifie_le = excluded.verifie_le,
				erreur = excluded.erreur`, n, sous, ex, t.Unix(), msg); err != nil {
			return err
		}
	}
	return nil
}

// Lister : domaines suivis, triés par nom.
func Lister(ctx context.Context, db *sql.DB) ([]Domaine, error) {
	rows, err := db.QueryContext(ctx, `SELECT nom, sous, expire, verifie_le, erreur FROM domaines ORDER BY nom`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Domaine
	for rows.Next() {
		var d Domaine
		var sous string
		var ex, le int64
		if err := rows.Scan(&d.Nom, &sous, &ex, &le, &d.Erreur); err != nil {
			return nil, err
		}
		if sous != "" {
			d.Sous = strings.Split(sous, ", ")
		}
		if ex > 0 {
			d.Expire = time.Unix(ex, 0)
		}
		if le > 0 {
			d.VerifieLe = time.Unix(le, 0)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// lire : date d'expiration d'un domaine par RDAP.
func (s *Suivi) lire(ctx context.Context, domaine string) (time.Time, error) {
	base, err := s.serveur(ctx, domaine)
	if err != nil {
		return time.Time{}, err
	}
	var r struct {
		Events []struct {
			Action string `json:"eventAction"`
			Date   string `json:"eventDate"`
		} `json:"events"`
	}
	if err := s.lireJSON(ctx, strings.TrimSuffix(base, "/")+"/domain/"+url.PathEscape(domaine), &r); err != nil {
		return time.Time{}, err
	}
	for _, e := range r.Events {
		if e.Action == "expiration" {
			t, err := time.Parse(time.RFC3339, e.Date)
			if err != nil {
				return time.Time{}, errors.New("date d'expiration illisible")
			}
			return t, nil
		}
	}
	return time.Time{}, errors.New("date d'expiration non publiée par le registre")
}

// serveur : adresse du serveur RDAP de l'extension (annuaire IANA).
func (s *Suivi) serveur(ctx context.Context, domaine string) (string, error) {
	ext := domaine[strings.LastIndex(domaine, ".")+1:]
	s.mu.Lock()
	frais := s.serveurs != nil && s.maintenant().Sub(s.annuaireLe) < gardeAnnuaire
	s.mu.Unlock()
	if !frais {
		var a struct {
			Services [][][]string `json:"services"`
		}
		adresse := s.Annuaire
		if adresse == "" {
			adresse = URLAnnuaire
		}
		if err := s.lireJSON(ctx, adresse, &a); err != nil {
			return "", fmt.Errorf("annuaire RDAP : %w", err)
		}
		m := map[string]string{}
		for _, sv := range a.Services {
			if len(sv) != 2 {
				continue
			}
			for _, u := range sv[1] {
				if strings.HasPrefix(u, "https://") {
					for _, e := range sv[0] {
						m[strings.ToLower(e)] = u
					}
					break
				}
			}
		}
		s.mu.Lock()
		s.serveurs, s.annuaireLe = m, s.maintenant()
		s.mu.Unlock()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if u := s.serveurs[ext]; u != "" {
		return u, nil
	}
	return "", errors.New("pas de RDAP pour ." + ext)
}

func (s *Suivi) lireJSON(ctx context.Context, adresse string, v any) error {
	if !strings.HasPrefix(adresse, "https://") {
		return errors.New("adresse non https")
	}
	ctx, annuler := context.WithTimeout(ctx, 20*time.Second)
	defer annuler()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, adresse, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/rdap+json, application/json")
	client := s.Client
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(r *http.Request, via []*http.Request) error {
			if r.URL.Scheme != "https" || len(via) >= 3 {
				return errors.New("redirection refusée")
			}
			return nil
		}}
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return errors.New("domaine inconnu du registre")
	case resp.StatusCode/100 != 2:
		return fmt.Errorf("réponse %d", resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, tailleMaxReponse)).Decode(v)
}
