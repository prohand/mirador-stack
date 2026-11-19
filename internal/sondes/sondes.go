// SPDX-License-Identifier: GPL-3.0-or-later

// Package sondes : essais de services (SPEC § 48, idée reprise de New Relic
// « synthetics », idée seulement) : HTTP(S), port TCP, ping, DNS. Faits par
// le collecteur (services du LAN) ou par le serveur (vue de l'extérieur),
// avec le même code. Un essai ne fait que lire : requête GET, ouverture de
// connexion, écho ICMP, question DNS.
package sondes

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Types.
const (
	HTTP = "http"
	TCP  = "tcp"
	Ping = "ping"
	DNS  = "dns"
)

// Bornes.
const (
	MaxSondes        = 50
	IntervalleMin    = 30 * time.Second
	IntervalleMax    = time.Hour
	IntervalleDefaut = time.Minute
	Delai            = 10 * time.Second // délai d'un essai
	LenteDefaut      = time.Second
	tailleMaxCorps   = 1 << 20 // corps lu au plus pour chercher le contenu attendu
	tailleMaxContenu = 200
	tailleMaxCible   = 500
	redirectionsMax  = 3
	ServeurLocal     = "serveur" // « Faite depuis » : le serveur mirador
)

// Sonde : réglage d'un essai.
type Sonde struct {
	Nom        string `json:"nom"`                   // nom affiché, unique
	Type       string `json:"type"`                  // http, tcp, ping, dns
	Cible      string `json:"cible"`                 // URL, hôte:port, hôte, serveur DNS (hôte[:port])
	Depuis     string `json:"depuis"`                // « serveur » ou nom du collecteur
	Intervalle string `json:"intervalle,omitempty"`  // « 60s » par défaut
	Lente      string `json:"lente,omitempty"`       // au-delà : « lente » (1s par défaut)
	Code       int    `json:"code,omitempty"`        // HTTP : code attendu (0 = 200 à 399)
	Contenu    string `json:"contenu,omitempty"`     // HTTP : texte attendu dans la page
	NonVerifie bool   `json:"non_verifie,omitempty"` // HTTP : certificat non vérifié (LAN fait maison)
	Question   string `json:"question,omitempty"`    // DNS : nom demandé
	// Machine sondée (SPEC § 60) : rangement seulement (page de la
	// machine, incidents). Jamais envoyé au collecteur.
	Machine string `json:"machine,omitempty"`
}

// Le deux-points est permis pour nommer un ping d'adresse IPv6 (« nas ping
// 2001:db8::1 », SPEC § 62).
var reNom = regexp.MustCompile(`^[\pL\pN][\pL\pN _.:()'-]{0,47}$`)

// Durees : intervalle et seuil « lente » effectifs.
func (s *Sonde) Durees() (intervalle, lente time.Duration) {
	intervalle, lente = IntervalleDefaut, LenteDefaut
	if d, err := time.ParseDuration(s.Intervalle); err == nil && d > 0 {
		intervalle = d
	}
	if d, err := time.ParseDuration(s.Lente); err == nil && d > 0 {
		lente = d
	}
	return
}

// Valider : contrôle d'un réglage (nom, type, cible, bornes).
func (s *Sonde) Valider() error {
	var errs []error
	if !reNom.MatchString(s.Nom) {
		errs = append(errs, fmt.Errorf("nom %q invalide (48 caractères au plus : lettres, chiffres, espace, . : _ - ( ) ')", s.Nom))
	}
	if s.Depuis == "" {
		errs = append(errs, errors.New("« faite depuis » manquant"))
	}
	if len(s.Cible) == 0 || len(s.Cible) > tailleMaxCible || strings.ContainsAny(s.Cible, " \t\r\n") {
		errs = append(errs, errors.New("cible vide, trop longue ou avec des espaces"))
	}
	switch s.Type {
	case HTTP:
		if !strings.HasPrefix(s.Cible, "http://") && !strings.HasPrefix(s.Cible, "https://") {
			errs = append(errs, errors.New("HTTP : adresse en http:// ou https:// attendue"))
		}
		if s.Code != 0 && (s.Code < 100 || s.Code > 599) {
			errs = append(errs, errors.New("code attendu entre 100 et 599"))
		}
	case TCP:
		if _, p, err := net.SplitHostPort(s.Cible); err != nil || p == "" {
			errs = append(errs, errors.New("TCP : hôte:port attendu"))
		}
	case Ping:
		if strings.Contains(s.Cible, "/") || strings.Contains(s.Cible, ":") && net.ParseIP(s.Cible) == nil {
			errs = append(errs, errors.New("ping : hôte ou adresse IP attendu"))
		}
	case DNS:
		if s.Question == "" || len(s.Question) > 253 || strings.ContainsAny(s.Question, " /") {
			errs = append(errs, errors.New("DNS : nom à demander attendu"))
		}
	default:
		errs = append(errs, fmt.Errorf("type %q inconnu (http, tcp, ping, dns)", s.Type))
	}
	if len(s.Machine) > 64 || strings.ContainsAny(s.Machine, "\x00\r\n") {
		errs = append(errs, errors.New("nom de machine invalide"))
	}
	if len(s.Contenu) > tailleMaxContenu {
		errs = append(errs, errors.New("contenu attendu trop long (200 caractères au plus)"))
	}
	for _, d := range []struct{ nom, v string }{{"intervalle", s.Intervalle}, {"lente", s.Lente}} {
		if d.v == "" {
			continue
		}
		if _, err := time.ParseDuration(d.v); err != nil {
			errs = append(errs, fmt.Errorf("%s %q illisible (ex. 60s, 2m)", d.nom, d.v))
		}
	}
	if iv, _ := s.Durees(); iv < IntervalleMin || iv > IntervalleMax {
		errs = append(errs, fmt.Errorf("intervalle hors bornes (%s à %s)", IntervalleMin, IntervalleMax))
	}
	if _, l := s.Durees(); l > Delai {
		errs = append(errs, fmt.Errorf("seuil « lente » au plus %s (délai d'un essai)", Delai))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("sonde %s : %w", s.Nom, err)
	}
	return nil
}

// Resultat : un essai.
type Resultat struct {
	OK     bool
	Duree  time.Duration
	Code   int    // HTTP
	Erreur string // pourquoi l'essai a échoué (court, pour la page et le message)
}

// Options : limites de l'exécutant (le serveur refuse les adresses locales).
type Options struct {
	// Refuser : adresse interdite (vrai = refusée), vérifiée à chaque
	// connexion, après résolution du nom. nil : tout est permis (collecteur).
	Refuser func(ip net.IP) bool
}

// Essayer : un essai, Delai au plus.
func Essayer(ctx context.Context, s Sonde, o Options) Resultat {
	ctx, fin := context.WithTimeout(ctx, Delai)
	defer fin()
	debut := time.Now()
	var r Resultat
	var err error
	switch s.Type {
	case HTTP:
		r, err = essaiHTTP(ctx, s, o)
	case TCP:
		err = essaiTCP(ctx, s.Cible, o)
	case Ping:
		err = essaiPing(ctx, s.Cible, o)
	case DNS:
		err = essaiDNS(ctx, s, o)
	default:
		err = errors.New("type inconnu")
	}
	r.Duree = time.Since(debut)
	if err != nil {
		r.OK, r.Erreur = false, court(err)
		return r
	}
	r.OK = true
	return r
}

// court : message d'erreur lisible, en français, borné (page Sondes,
// étiquette « erreur »). Les erreurs courantes sont traduites ; les autres
// gardent leur texte, sans la partie « Get "url": » répétée.
func court(err error) string {
	var ne net.Error
	var de *net.DNSError
	var ue *url.Error
	var ce *tls.CertificateVerificationError
	switch {
	case errors.As(err, &de) && de.IsNotFound:
		return "nom inconnu (DNS) : " + de.Name
	case errors.As(err, &de):
		return "DNS injoignable pour " + de.Name
	case errors.As(err, &ne) && ne.Timeout() || errors.Is(err, context.DeadlineExceeded):
		return "pas de réponse en " + Delai.String()
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connexion refusée (port fermé)"
	case errors.Is(err, syscall.EHOSTUNREACH) || errors.Is(err, syscall.ENETUNREACH):
		return "hôte injoignable (réseau)"
	case errors.Is(err, syscall.ECONNRESET):
		return "connexion coupée par le service"
	case errors.As(err, &ce):
		return "certificat refusé : " + raccourcir(ce.Err.Error(), 100)
	case errors.As(err, &ue):
		err = ue.Err
	}
	return raccourcir(err.Error(), 160)
}

func raccourcir(m string, n int) string {
	if len(m) <= n {
		return m
	}
	return m[:n] + "…"
}

// dialer : connexion avec contrôle de l'adresse (après résolution).
func dialer(o Options) *net.Dialer {
	d := &net.Dialer{Timeout: Delai}
	if o.Refuser != nil {
		d.Control = func(_, adresse string, _ syscall.RawConn) error {
			hote, _, err := net.SplitHostPort(adresse)
			if err != nil {
				return err
			}
			if ip := net.ParseIP(hote); ip == nil || o.Refuser(ip) {
				return fmt.Errorf("adresse %s refusée depuis le serveur", hote)
			}
			return nil
		}
	}
	return d
}

func essaiHTTP(ctx context.Context, s Sonde, o Options) (Resultat, error) {
	tr := &http.Transport{DialContext: dialer(o).DialContext, Proxy: nil, DisableKeepAlives: true,
		TLSHandshakeTimeout: Delai, ResponseHeaderTimeout: Delai,
		// #nosec G402 -- choisi par l'administrateur pour un service du LAN
		// au certificat fait maison ; vérifié sinon.
		TLSClientConfig: &tls.Config{InsecureSkipVerify: s.NonVerifie, MinVersion: tls.VersionTLS12}}
	cl := &http.Client{Transport: tr, Timeout: Delai, CheckRedirect: func(_ *http.Request, via []*http.Request) error {
		if len(via) >= redirectionsMax {
			return errors.New("trop de redirections")
		}
		return nil
	}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.Cible, nil)
	if err != nil {
		return Resultat{}, err
	}
	req.Header.Set("User-Agent", "mirador-sonde")
	rep, err := cl.Do(req)
	if err != nil {
		return Resultat{}, err
	}
	defer rep.Body.Close()
	r := Resultat{Code: rep.StatusCode}
	if s.Code != 0 && rep.StatusCode != s.Code || s.Code == 0 && (rep.StatusCode < 200 || rep.StatusCode >= 400) {
		_, _ = io.Copy(io.Discard, io.LimitReader(rep.Body, tailleMaxCorps))
		return r, fmt.Errorf("code %d", rep.StatusCode)
	}
	if s.Contenu != "" {
		corps, err := io.ReadAll(io.LimitReader(rep.Body, tailleMaxCorps))
		if err != nil {
			return r, err
		}
		if !strings.Contains(string(corps), s.Contenu) {
			return r, errors.New("contenu attendu absent")
		}
	}
	return r, nil
}

func essaiTCP(ctx context.Context, cible string, o Options) error {
	c, err := dialer(o).DialContext(ctx, "tcp", cible)
	if err != nil {
		return err
	}
	return c.Close()
}

func essaiDNS(ctx context.Context, s Sonde, o Options) error {
	serveur := s.Cible
	if _, _, err := net.SplitHostPort(serveur); err != nil {
		serveur = net.JoinHostPort(serveur, "53")
	}
	res := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, reseau, _ string) (net.Conn, error) {
		return dialer(o).DialContext(ctx, reseau, serveur)
	}}
	adresses, err := res.LookupHost(ctx, s.Question)
	if err != nil {
		var de *net.DNSError
		if errors.As(err, &de) && de.IsNotFound {
			return errors.New("nom inconnu du serveur DNS")
		}
		return err
	}
	if len(adresses) == 0 {
		return errors.New("aucune adresse")
	}
	if s.Contenu != "" {
		for _, a := range adresses {
			if a == s.Contenu {
				return nil
			}
		}
		return errors.New("réponse " + strings.Join(adresses, ", ") + " sans " + s.Contenu)
	}
	return nil
}

// Etiquettes : sonde, type et cible (pour les métriques).
func Etiquettes(s Sonde) [][2]string {
	return [][2]string{{"sonde", s.Nom}, {"type", s.Type}, {"cible", s.Cible}}
}

// Valeurs : métriques d'un essai (nom → valeur).
//   - mirador_sonde_succes : 1 si l'essai a réussi ;
//   - mirador_sonde_duree_secondes : durée de l'essai ;
//   - mirador_sonde_lente : 1 si réussi mais plus lent que le seuil ;
//   - mirador_sonde_code_http : code HTTP reçu (HTTP seulement).
func Valeurs(s Sonde, r Resultat) map[string]float64 {
	_, lente := s.Durees()
	v := map[string]float64{
		"mirador_sonde_succes":         bool01(r.OK),
		"mirador_sonde_duree_secondes": r.Duree.Seconds(),
		"mirador_sonde_lente":          bool01(r.OK && r.Duree > lente),
	}
	if s.Type == HTTP && r.Code > 0 {
		v["mirador_sonde_code_http"] = float64(r.Code)
	}
	return v
}

func bool01(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// Texte : « OK · 41 ms », « Échec : code 502 » (page Sondes).
func (r Resultat) Texte() string {
	if !r.OK {
		return "Échec : " + r.Erreur
	}
	return "OK · " + strconv.FormatInt(r.Duree.Milliseconds(), 10) + " ms"
}

// Tourner : essais d'une sonde à son rythme jusqu'à l'arrêt de ctx ;
// chaque résultat est passé à rendre. Premier essai décalé (0 à 10 s,
// d'après le nom) pour étaler les sondes.
func Tourner(ctx context.Context, s Sonde, o Options, rendre func(Sonde, Resultat)) {
	intervalle, _ := s.Durees()
	var h uint32
	for i := 0; i < len(s.Nom); i++ { // octets : pas de conversion signée
		h = h*31 + uint32(s.Nom[i])
	}
	attente := time.Duration(h%10) * time.Second
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(attente):
		}
		rendre(s, Essayer(ctx, s, o))
		attente = intervalle
	}
}
