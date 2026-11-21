// SPDX-License-Identifier: GPL-3.0-or-later

// Package sondes : essais de services (SPEC § 48, idée reprise de New Relic
// « synthetics », idée seulement) : HTTP(S), port TCP, ping, DNS. Faits par
// le collecteur (services du LAN) ou par le serveur (vue de l'extérieur),
// avec le même code. Un essai ne fait que lire : requête GET, ouverture de
// connexion, écho ICMP, question DNS.
package sondes

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/net/dns/dnsmessage"
	"mirador-stack/internal/motif"
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
	// HTTP, réponse JSON (SPEC § 81, idée reprise de Checkmate) : champ à
	// lire (« status », « checks.db.status », « items.0.etat ») et valeur
	// attendue (vide : le champ doit seulement exister).
	ChampJSON  string `json:"champ_json,omitempty"`
	ValeurJSON string `json:"valeur_json,omitempty"`
	// Certificat : alerter N jours avant l'expiration du certificat du site
	// (0 = non ; HTTPS seulement).
	Certificat int    `json:"certificat,omitempty"`
	Question   string `json:"question,omitempty"` // DNS : nom demandé
	// Machine sondée (SPEC § 60) : rangement seulement (page de la
	// machine, incidents). Jamais envoyé au collecteur.
	Machine string `json:"machine,omitempty"`
}

// reChampJSON : chemin simple, vide permis.
var reChampJSON = regexp.MustCompile(`^([^.\s]+(\.[^.\s]+)*)?$`)

// Le deux-points est permis pour nommer un ping d'adresse IPv6 (« nas ping
// 2001:db8::1 », SPEC § 62).
var reNom = motif.Doit(`^[\pL\pN][\pL\pN _.:()'-]*$`, 1, 48)

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
	if s.Type != HTTP && (s.ChampJSON != "" || s.ValeurJSON != "" || s.Certificat != 0) {
		errs = append(errs, errors.New("champ JSON et certificat : sondes HTTP seulement"))
	}
	if s.ChampJSON == "" && s.ValeurJSON != "" {
		errs = append(errs, errors.New("valeur JSON attendue sans champ"))
	}
	if len(s.ChampJSON) > 100 || !reChampJSON.MatchString(s.ChampJSON) {
		errs = append(errs, errors.New("champ JSON : noms séparés par des points (ex. checks.db.status), 100 caractères au plus"))
	}
	if len(s.ValeurJSON) > tailleMaxContenu {
		errs = append(errs, errors.New("valeur JSON attendue trop longue (200 caractères au plus)"))
	}
	if s.Certificat < 0 || s.Certificat > 90 {
		errs = append(errs, errors.New("alerte certificat : 1 à 90 jours avant l'expiration"))
	}
	if s.Certificat > 0 && !strings.HasPrefix(strings.ToLower(s.Cible), "https://") {
		errs = append(errs, errors.New("alerte certificat : adresse en https:// attendue"))
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
	// FinCertificat : fin de validité du certificat du site (HTTPS).
	FinCertificat time.Time
}

// Options : limites de l'exécutant (le serveur refuse les adresses locales).
type Options struct {
	// Refuser : adresse interdite (vrai = refusée), vérifiée à chaque
	// connexion, après résolution du nom. nil : tout est permis (collecteur).
	Refuser func(ip net.IP) bool
	// Maintenant : un signal reçu fait l'essai tout de suite, sans attendre
	// le rythme (bouton « Vérifier maintenant », SPEC § 108). nil : aucun.
	Maintenant <-chan struct{}
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
		// Textes fixes : le message de Go contient l'heure courante (« current
		// time … is after … ») ; en étiquette, il créait une série de plus à
		// chaque essai (relecture du 21/11).
		var invalide x509.CertificateInvalidError
		var autorite x509.UnknownAuthorityError
		var nom x509.HostnameError
		switch {
		case errors.As(ce.Err, &invalide) && invalide.Reason == x509.Expired:
			return "certificat refusé : expiré ou pas encore valable"
		case errors.As(ce.Err, &autorite):
			return "certificat refusé : autorité inconnue (certificat fait maison ?)"
		case errors.As(ce.Err, &nom):
			return "certificat refusé : il n'est pas fait pour ce nom"
		}
		return "certificat refusé"
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
	// Relecture du 21/11 : un code attendu 3xx (redirection voulue) est
	// comparé à la première réponse, sans la suivre ; le certificat est
	// toujours celui du site sondé (première réponse), pas du site final.
	var premierTLS *tls.ConnectionState
	cl := &http.Client{Transport: tr, Timeout: Delai, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) == 1 && req.Response != nil && req.Response.TLS != nil {
			premierTLS = req.Response.TLS
		}
		if s.Code >= 300 && s.Code < 400 {
			return http.ErrUseLastResponse
		}
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
		// Certificat refusé : son échéance reste connue (relecture du 21/11 :
		// un certificat expiré faisait disparaître la série, et l'alerte
		// « certificat bientôt expiré » se résolvait au moment de l'expiration).
		var ce *tls.CertificateVerificationError
		if errors.As(err, &ce) && len(ce.UnverifiedCertificates) > 0 {
			return Resultat{FinCertificat: ce.UnverifiedCertificates[0].NotAfter}, err
		}
		return Resultat{}, err
	}
	defer rep.Body.Close()
	r := Resultat{Code: rep.StatusCode}
	etatTLS := rep.TLS
	if premierTLS != nil {
		etatTLS = premierTLS
	}
	if etatTLS != nil && len(etatTLS.PeerCertificates) > 0 {
		r.FinCertificat = etatTLS.PeerCertificates[0].NotAfter
	}
	if s.Code != 0 && rep.StatusCode != s.Code || s.Code == 0 && (rep.StatusCode < 200 || rep.StatusCode >= 400) {
		_, _ = io.Copy(io.Discard, io.LimitReader(rep.Body, tailleMaxCorps))
		return r, fmt.Errorf("code %d", rep.StatusCode)
	}
	if s.Contenu != "" || s.ChampJSON != "" {
		corps, err := io.ReadAll(io.LimitReader(rep.Body, tailleMaxCorps))
		if err != nil {
			return r, err
		}
		if s.Contenu != "" && !strings.Contains(string(corps), s.Contenu) {
			return r, errors.New("contenu attendu absent")
		}
		if s.ChampJSON != "" {
			return r, verifierJSON(corps, s.ChampJSON, s.ValeurJSON)
		}
	}
	return r, nil
}

// verifierJSON : le champ (chemin « a.b.0.c ») existe et vaut la valeur
// attendue (texte comparé tel quel ; nombres et booléens écrits comme en
// JSON : 1, 2.5, true). Rien n'est exécuté : simple lecture.
func verifierJSON(corps []byte, champ, attendu string) error {
	var v any
	dec := json.NewDecoder(bytes.NewReader(corps))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return errors.New("réponse non JSON")
	}
	for _, morceau := range strings.Split(champ, ".") {
		switch x := v.(type) {
		case map[string]any:
			var ok bool
			if v, ok = x[morceau]; !ok {
				return fmt.Errorf("champ JSON %s absent", champ)
			}
		case []any:
			i, err := strconv.Atoi(morceau)
			if err != nil || i < 0 || i >= len(x) {
				return fmt.Errorf("champ JSON %s absent", champ)
			}
			v = x[i]
		default:
			return fmt.Errorf("champ JSON %s absent", champ)
		}
	}
	if attendu == "" {
		return nil
	}
	var texte string
	switch x := v.(type) {
	case string:
		texte = x
	case json.Number:
		texte = x.String()
		if texte != attendu {
			// Valeur reçue non recopiée : un nombre change souvent d'un essai
			// à l'autre (une série de plus à chaque fois en étiquette,
			// relecture du 21/11).
			return fmt.Errorf("champ JSON %s différent de « %s »", champ, raccourcir(attendu, 40))
		}
	case bool:
		texte = strconv.FormatBool(x)
	case nil:
		texte = "null"
	default:
		return fmt.Errorf("champ JSON %s : objet ou liste, valeur simple attendue", champ)
	}
	if texte != attendu {
		return fmt.Errorf("champ JSON %s = « %s », attendu « %s »", champ, raccourcir(texte, 40), raccourcir(attendu, 40))
	}
	return nil
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
	// Vraie question posée au serveur visé (relecture du 21/11) : le
	// résolveur de Go lisait d'abord /etc/hosts du collecteur et ajoutait
	// les domaines de recherche locaux ; un nom présent dans /etc/hosts
	// restait « OK » même serveur DNS arrêté.
	var adresses []string
	inconnu := false
	for _, typ := range []dnsmessage.Type{dnsmessage.TypeA, dnsmessage.TypeAAAA} {
		a, rcode, err := questionDNS(ctx, serveur, s.Question, typ, o)
		if err != nil {
			return err
		}
		inconnu = inconnu || rcode == dnsmessage.RCodeNameError
		adresses = append(adresses, a...)
	}
	if len(adresses) == 0 {
		if inconnu {
			return errors.New("nom inconnu du serveur DNS")
		}
		return errors.New("aucune adresse")
	}
	if s.Contenu != "" {
		for _, a := range adresses {
			if a == s.Contenu {
				return nil
			}
		}
		// Adresses triées : l'ordre change d'une réponse à l'autre
		// (tourniquet DNS) ; 4 au plus dans le message.
		slices.Sort(adresses)
		if len(adresses) > 4 {
			adresses = append(adresses[:4:4], "…")
		}
		return errors.New("réponse " + strings.Join(adresses, ", ") + " sans " + s.Contenu)
	}
	return nil
}

// questionDNS : une question (A ou AAAA) en UDP, puis en TCP si la réponse
// est tronquée. Renvoie les adresses et le code de retour du serveur.
func questionDNS(ctx context.Context, serveur, nom string, typ dnsmessage.Type, o Options) ([]string, dnsmessage.RCode, error) {
	if !strings.HasSuffix(nom, ".") {
		nom += "." // nom absolu : jamais de domaine de recherche ajouté
	}
	n, err := dnsmessage.NewName(nom)
	if err != nil {
		return nil, 0, fmt.Errorf("question DNS invalide : %w", err)
	}
	var idb [2]byte
	if _, err := rand.Read(idb[:]); err != nil {
		return nil, 0, err
	}
	id := uint16(idb[0])<<8 | uint16(idb[1])
	q := dnsmessage.Message{Header: dnsmessage.Header{ID: id, RecursionDesired: true},
		Questions: []dnsmessage.Question{{Name: n, Type: typ, Class: dnsmessage.ClassINET}}}
	paquet, err := q.Pack()
	if err != nil {
		return nil, 0, err
	}
	rep, err := echangeDNS(ctx, "udp", serveur, paquet, o)
	if err == nil && rep.Header.Truncated {
		rep, err = echangeDNS(ctx, "tcp", serveur, paquet, o)
	}
	if err != nil {
		return nil, 0, err
	}
	if rep.Header.ID != id || !rep.Header.Response {
		return nil, 0, errors.New("réponse DNS qui ne correspond pas à la question")
	}
	if rep.Header.RCode != dnsmessage.RCodeSuccess && rep.Header.RCode != dnsmessage.RCodeNameError {
		return nil, rep.Header.RCode, fmt.Errorf("serveur DNS : %s", rep.Header.RCode)
	}
	var adresses []string
	for _, r := range rep.Answers {
		switch b := r.Body.(type) {
		case *dnsmessage.AResource:
			adresses = append(adresses, netip.AddrFrom4(b.A).String())
		case *dnsmessage.AAAAResource:
			adresses = append(adresses, netip.AddrFrom16(b.AAAA).String())
		}
	}
	return adresses, rep.Header.RCode, nil
}

// echangeDNS : envoie la question et lit une réponse (TCP : longueur sur
// 2 octets en tête, RFC 1035 § 4.2.2). Délai : celui du contexte.
func echangeDNS(ctx context.Context, reseau, serveur string, paquet []byte, o Options) (dnsmessage.Message, error) {
	var m dnsmessage.Message
	c, err := dialer(o).DialContext(ctx, reseau, serveur)
	if err != nil {
		return m, err
	}
	defer c.Close()
	if fin, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(fin)
	}
	tampon := make([]byte, 65535)
	var lu int
	if reseau == "tcp" {
		if len(paquet) > 0xFFFF {
			return m, errors.New("question DNS trop longue")
		}
		long := binary.BigEndian.AppendUint16(nil, uint16(len(paquet))) // #nosec G115 -- borné juste au-dessus
		if _, err := c.Write(append(long, paquet...)); err != nil {
			return m, err
		}
		if _, err := io.ReadFull(c, tampon[:2]); err != nil {
			return m, err
		}
		lu = int(tampon[0])<<8 | int(tampon[1])
		if _, err := io.ReadFull(c, tampon[:lu]); err != nil {
			return m, err
		}
	} else {
		if _, err := c.Write(paquet); err != nil {
			return m, err
		}
		if lu, err = c.Read(tampon); err != nil {
			return m, err
		}
	}
	err = m.Unpack(tampon[:lu])
	return m, err
}

// Etiquettes : sonde, type et cible (pour les métriques).
func Etiquettes(s Sonde) [][2]string {
	return [][2]string{{"sonde", s.Nom}, {"type", s.Type}, {"cible", s.Cible}}
}

// Valeurs : métriques d'un essai (nom → valeur).
//   - mirador_sonde_succes : 1 si l'essai a réussi ;
//   - mirador_sonde_duree_secondes : durée de l'essai ;
//   - mirador_sonde_lente : 1 si réussi mais plus lent que le seuil ;
//   - mirador_sonde_code_http : code HTTP reçu (HTTP seulement) ;
//   - mirador_sonde_certificat_fin_timestamp_seconds : fin du certificat
//     (HTTPS) ; mirador_sonde_certificat_alerte_jours : si l'alerte
//     certificat est demandée (SPEC § 81, § 89).
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
	// Fin du certificat : toujours (jours restants affichés, SPEC § 89) ;
	// seuil d'alerte seulement si demandé sur la sonde.
	if !r.FinCertificat.IsZero() {
		v["mirador_sonde_certificat_fin_timestamp_seconds"] = float64(r.FinCertificat.Unix())
		if s.Certificat > 0 {
			v["mirador_sonde_certificat_alerte_jours"] = float64(s.Certificat)
		}
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
		minuterie := time.NewTimer(attente)
		select {
		case <-ctx.Done():
			minuterie.Stop()
			return
		case <-minuterie.C:
		case <-o.Maintenant: // demandé dans l'interface : rythme repris ensuite
			minuterie.Stop()
		}
		r := Essayer(ctx, s, o)
		if !r.OK && ctx.Err() != nil {
			return // essai coupé par l'arrêt : pas de faux échec (relecture du 21/11)
		}
		rendre(s, r)
		attente = intervalle
	}
}
