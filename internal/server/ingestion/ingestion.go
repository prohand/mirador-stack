// SPDX-License-Identifier: GPL-3.0-or-later

// Package ingestion reçoit les métriques et les logs des machines
// (SPEC.md § 16.4).
//
// Ordre des contrôles d'une requête, du moins cher au plus cher :
//
//  1. nombre de requêtes traitées en même temps (mémoire bornée) ;
//  2. débit par adresse IP, puis échecs d'authentification par IP ;
//  3. token (empreinte SHA-256, cache borné), et adresses autorisées de la machine ;
//  4. débit par machine ;
//  5. décodage borné (taille, séries, points, lignes) ;
//  6. horloge de la machine (corrigée ou refusée) ;
//  7. nom de la machine imposé (instance / host), quotas de séries et de lignes ;
//  8. écriture dans le stockage.
//
// Codes HTTP renvoyés (l'agent agit selon le code, SPEC.md § 15.2) :
// 204 reçu ; 400 lot invalide, abandonné ; 401 token refusé ; 403 adresse
// non autorisée pour ce token ; 413 trop gros ;
// 429 trop de requêtes ou quota de logs (Retry-After) ; 503 serveur occupé.
package ingestion

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"mirador-stack/internal/protocole"
	"mirador-stack/internal/server/stockage"
	"mirador-stack/internal/server/web"
)

// Options de l'ingestion. Les valeurs nulles prennent les défauts.
type Options struct {
	DB        *sql.DB
	Metriques stockage.Metriques
	Logs      stockage.Logs
	Journal   *slog.Logger

	Decodage LimitesDecodage
	Lignes   LimitesLogs
	Series   LimitesSeries
	Horloge  LimitesHorloge

	MaxCorpsMetriques int64   // octets compressés
	MaxSimultanees    int     // requêtes décodées en même temps
	RequetesMachine   float64 // requêtes par seconde et par machine (rafale ×10)
	RequetesIP        float64 // requêtes par seconde et par IP (rafale ×4) ; plusieurs machines derrière un même NAT
	EchecsIP          float64 // échecs d'authentification par seconde et par IP (rafale 10)
	LignesMinute      int     // lignes de logs par minute et par machine (rafale : 1 minute)

	Maintenant func() time.Time // remplaçable dans les tests
}

func (o *Options) completer() {
	if o.Decodage == (LimitesDecodage{}) {
		o.Decodage = LimitesDecodageDefaut()
	}
	if o.Lignes == (LimitesLogs{}) {
		o.Lignes = LimitesLogsDefaut()
	}
	if o.Series == (LimitesSeries{}) {
		o.Series = LimitesSeriesDefaut()
	}
	if o.Horloge == (LimitesHorloge{}) {
		o.Horloge = LimitesHorlogeDefaut()
	}
	defaut := func(v *float64, d float64) {
		if *v <= 0 {
			*v = d
		}
	}
	defaut(&o.RequetesMachine, 5)
	defaut(&o.RequetesIP, 50)
	defaut(&o.EchecsIP, 0.2)
	if o.MaxCorpsMetriques <= 0 {
		o.MaxCorpsMetriques = 4 << 20
	}
	if o.MaxSimultanees <= 0 {
		// Pire cas ~16 Mo par requête : 4 × 16 = 64 Mo au plus (budget 256 Mio).
		o.MaxSimultanees = 4
	}
	if o.LignesMinute <= 0 {
		// L'agent limite déjà à 200 lignes/s (SPEC § 15.2 quater) : le serveur
		// laisse un peu de marge et protège contre un agent modifié.
		o.LignesMinute = 15_000
	}
	if o.Maintenant == nil {
		o.Maintenant = time.Now
	}
	if o.Journal == nil {
		o.Journal = slog.New(slog.DiscardHandler)
	}
}

// Compteurs : métriques internes de l'ingestion (exposées en partie 7).
type Compteurs struct {
	Requetes, Refus400, Refus401, Refus403, Refus413, Refus429, Refus503 atomic.Uint64
	Series, Points, SeriesIgnorees, SeriesRefusees, PointsFuturs         atomic.Uint64
	Lignes, LignesIgnorees, LignesRefusees                               atomic.Uint64
	HorlogeCorrigee, HorlogeRefusee                                      atomic.Uint64
}

// Ingestion : points d'entrée des machines.
type Ingestion struct {
	opt    Options
	auth   *Auth
	quotas *Quotas
	places chan struct{}

	parIP, echecsIP, parMachine, lignes *Limiteur

	Compteurs Compteurs

	mu        sync.Mutex
	decalages map[int64]time.Duration // dernier décalage d'horloge mesuré par machine
	// lignesHote : lignes de logs reçues par hôte (host) depuis le démarrage
	// (courbes de volume des logs, SPEC § 18.20). 2 000 hôtes au plus.
	lignesHote map[string]uint64
	enCours    map[int64]int       // requêtes en cours par machine (2 au plus)
	avertis    map[int64]time.Time // dernier avertissement de quota (journal)
}

// Nouvelle prépare l'ingestion.
func Nouvelle(opt Options) *Ingestion {
	opt.completer()
	const maxCles = 10_000
	lm := float64(opt.LignesMinute)
	return &Ingestion{
		opt:        opt,
		auth:       NouvelleAuth(opt.DB),
		quotas:     NouveauxQuotas(opt.Series),
		places:     make(chan struct{}, opt.MaxSimultanees),
		parIP:      NouveauLimiteur(opt.RequetesIP, opt.RequetesIP*4, maxCles),
		echecsIP:   NouveauLimiteur(opt.EchecsIP, 10, maxCles),
		parMachine: NouveauLimiteur(opt.RequetesMachine, opt.RequetesMachine*10, maxCles),
		lignes:     NouveauLimiteur(lm/60, lm, maxCles),
		decalages:  make(map[int64]time.Duration),
		lignesHote: make(map[string]uint64),
		enCours:    make(map[int64]int),
		avertis:    make(map[int64]time.Time),
	}
}

// Auth donne accès au cache des tokens (révocation : Oublier).
func (g *Ingestion) Auth() *Auth { return g.auth }

// Decalage renvoie le dernier décalage d'horloge mesuré pour une machine
// (servira à l'alerte « horloge décalée », étape 4).
func (g *Ingestion) Decalage(machine int64) (time.Duration, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	d, ok := g.decalages[machine]
	return d, ok
}

// Boucle : tâches de fond (écriture de vu_le, oubli des séries muettes,
// nettoyage des limiteurs), une fois par minute. Rend la main quand ctx est
// annulé, après une dernière écriture de vu_le.
func (g *Ingestion) Boucle(ctx context.Context) {
	t := time.NewTicker(intervalleVuLe)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			fin, annuler := context.WithTimeout(context.Background(), 5*time.Second)
			defer annuler()
			if err := g.auth.EcrireVus(fin); err != nil {
				g.opt.Journal.Error("écriture de vu_le", "erreur", err)
			}
			return
		case <-t.C:
			if err := g.auth.EcrireVus(ctx); err != nil {
				g.opt.Journal.Error("écriture de vu_le", "erreur", err)
			}
			maintenant := g.opt.Maintenant()
			g.quotas.Nettoyer(maintenant)
			for _, l := range []*Limiteur{g.parIP, g.echecsIP, g.parMachine, g.lignes} {
				l.Nettoyer(maintenant)
			}
		}
	}
}

// Routes ajoute les points d'entrée au routeur des agents.
func (g *Ingestion) Routes(m *http.ServeMux) {
	m.HandleFunc("POST "+protocole.CheminMetriques, g.metriques)
	m.HandleFunc("POST "+protocole.CheminLogs, g.logs)
	// Entrée InfluxDB v2 (Telegraf d'OPNsense, SPEC § 23.9) : adresse à
	// donner à Telegraf = https://<ingestion>/api/agent/v1/influx (Telegraf
	// ajoute /api/v2/write). Sous /api/agent/ : même bloc nginx.
	m.HandleFunc("POST "+protocole.CheminInflux+"/api/v2/write", g.influx)
	m.HandleFunc("GET "+protocole.CheminInflux+"/health", g.santeInflux)
	m.HandleFunc("GET "+protocole.CheminInflux+"/ping", g.santeInflux)
}

// influx : POST …/api/v2/write (line protocol, gzip accepté). Même accueil
// que les agents (token « Token … » ou « Bearer … », adresses, limites).
func (g *Ingestion) influx(w http.ResponseWriter, r *http.Request) {
	m, liberer, ok := g.Accueillir(w, r)
	if !ok {
		return
	}
	defer liberer()
	var compresse bool
	switch strings.ToLower(r.Header.Get("Content-Encoding")) {
	case "gzip":
		compresse = true
	case "", "identity":
	default:
		g.refuser(w, http.StatusBadRequest, "Content-Encoding : gzip ou rien", 0)
		return
	}
	maintenant := g.opt.Maintenant()
	series, ignorees, err := DecoderInflux(r.Body, compresse, r.URL.Query().Get("precision"), g.opt.Decodage, maintenant)
	if err != nil {
		g.erreurDecodage(w, m, err)
		return
	}
	g.Compteurs.SeriesIgnorees.Add(uint64(ignorees)) // #nosec G115 -- compteur ≥ 0
	// Pas d'en-tête d'heure chez Telegraf : seuls les points trop loin dans
	// le futur sont écartés (ranger).
	g.ranger(w, r, m, series, HeureBonne, 0, maintenant)
}

// santeInflux : GET …/health (et /ping), comme InfluxDB : certains clients
// vérifient la cible avant d'envoyer. Rien de secret, pas de token.
func (g *Ingestion) santeInflux(w http.ResponseWriter, r *http.Request) {
	if !g.parIP.Prendre(CleLimite(web.AdresseClient(r)), 1, g.opt.Maintenant()) {
		g.refuser(w, http.StatusTooManyRequests, "trop de requêtes", 1)
		return
	}
	if strings.HasSuffix(r.URL.Path, "/ping") {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = io.WriteString(w, `{"name":"mirador","message":"ready for queries and writes","status":"pass","checks":[],"version":"2.7.0"}`+"\n")
}

// erreurHTTP : réponse courte, en texte, sans détail interne.
func erreurHTTP(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	_, _ = io.WriteString(w, msg+"\n") // #nosec G705 -- texte brut (nosniff), messages fixes du serveur
}

func (g *Ingestion) refuser(w http.ResponseWriter, code int, msg string, attente int) {
	switch code {
	case http.StatusBadRequest:
		g.Compteurs.Refus400.Add(1)
	case http.StatusUnauthorized:
		g.Compteurs.Refus401.Add(1)
	case http.StatusForbidden:
		g.Compteurs.Refus403.Add(1)
	case http.StatusRequestEntityTooLarge:
		g.Compteurs.Refus413.Add(1)
	case http.StatusTooManyRequests:
		g.Compteurs.Refus429.Add(1)
	case http.StatusServiceUnavailable:
		g.Compteurs.Refus503.Add(1)
	}
	if attente > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(attente))
	}
	erreurHTTP(w, code, msg)
}

// Accueillir fait les contrôles 1 à 4 (aussi pour les autres points
// d'entrée des agents : configuration, battement, mises à jour). Si ok, l'appelant doit appeler
// liberer() à la fin.
func (g *Ingestion) Accueillir(w http.ResponseWriter, r *http.Request) (m Machine, liberer func(), ok bool) {
	g.Compteurs.Requetes.Add(1)
	select {
	case g.places <- struct{}{}:
	default:
		g.refuser(w, http.StatusServiceUnavailable, "serveur occupé", 2)
		return m, nil, false
	}
	liberer = func() { <-g.places }
	maintenant := g.opt.Maintenant()
	adresse := web.AdresseClient(r)
	ip := CleLimite(adresse) // IPv6 : le /64 entier (on ne contourne pas les limites en changeant d'adresse)
	if !g.parIP.Prendre(ip, 1, maintenant) {
		liberer()
		g.refuser(w, http.StatusTooManyRequests, "trop de requêtes", g.parIP.Attente(ip))
		return m, nil, false
	}
	// Trop d'échecs récents depuis cette IP : on ne regarde même pas le token.
	if !g.echecsIP.Prendre(ip, 0, maintenant) {
		liberer()
		g.refuser(w, http.StatusTooManyRequests, "trop d'échecs d'authentification", g.echecsIP.Attente(ip))
		return m, nil, false
	}
	token, present := extraireToken(r)
	var err error
	if present {
		m, err = g.auth.VerifierMachine(r.Context(), token, adresse, r.Header.Get(protocole.EnteteEmpreinte), maintenant)
	}
	switch {
	case errors.Is(err, ErrEmpreinte):
		// Token lié à une machine et présenté par une autre : token copié,
		// ou machine réinstallée (à relier dans l'interface). Compté comme un échec.
		g.echecsIP.Prendre(ip, 1, maintenant)
		liberer()
		g.opt.Journal.Warn("token utilisé par une autre machine que celle à laquelle il est lié", "machine", m.Nom, "ip", ip)
		g.refuser(w, http.StatusForbidden, "token lié à une autre machine", 0)
		return m, nil, false
	case errors.Is(err, ErrIP):
		// Token valable depuis une autre adresse : token volé, ou machine
		// qui a changé d'IP (à corriger dans l'interface). Compté comme un échec.
		g.echecsIP.Prendre(ip, 1, maintenant)
		liberer()
		g.opt.Journal.Warn("token utilisé depuis une adresse non autorisée", "machine", m.Nom, "ip", ip)
		g.refuser(w, http.StatusForbidden, "adresse non autorisée", 0)
		return m, nil, false
	case !present || errors.Is(err, ErrToken):
		g.echecsIP.Prendre(ip, 1, maintenant)
		liberer()
		g.opt.Journal.Warn("token refusé", "ip", ip)
		g.refuser(w, http.StatusUnauthorized, "token refusé", 0)
		return m, nil, false
	case err != nil:
		liberer()
		g.opt.Journal.Error("vérification du token", "erreur", err)
		g.refuser(w, http.StatusServiceUnavailable, "base indisponible", 5)
		return m, nil, false
	}
	cle := "m:" + strconv.FormatInt(m.ID, 10)
	if !g.parMachine.Prendre(cle, 1, maintenant) {
		liberer()
		g.refuser(w, http.StatusTooManyRequests, "trop de requêtes", g.parMachine.Attente(cle))
		return m, nil, false
	}
	// Envois (POST : métriques, logs, battement) : 3 au plus en cours par
	// machine, et un corps doit arriver en 30 s. Un token volé qui enverrait
	// des corps très lentement ne prend donc pas toutes les places. Les
	// lectures (GET : configuration, mise à jour) ne comptent pas.
	if r.Method != http.MethodPost {
		return m, liberer, true
	}
	g.mu.Lock()
	if g.enCours[m.ID] >= placesParMachine {
		g.mu.Unlock()
		liberer()
		g.refuser(w, http.StatusServiceUnavailable, "trop de requêtes en cours pour cette machine", 2)
		return m, nil, false
	}
	g.enCours[m.ID]++
	g.mu.Unlock()
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(delaiLectureCorps))
	libererPlace := liberer
	liberer = func() {
		g.mu.Lock()
		if g.enCours[m.ID]--; g.enCours[m.ID] <= 0 {
			delete(g.enCours, m.ID)
		}
		g.mu.Unlock()
		libererPlace()
	}
	return m, liberer, true
}

// placesParMachine, delaiLectureCorps : voir Accueillir.
const (
	placesParMachine  = 3
	delaiLectureCorps = 30 * time.Second
)

// CleLimite : l'adresse (IPv4) ou son /64 (IPv6), clé des limites par adresse.
func CleLimite(a netip.Addr) string {
	a = a.Unmap()
	if a.Is6() {
		if p, err := a.Prefix(64); err == nil {
			return p.String()
		}
	}
	return a.String()
}

// horloge lit l'en-tête X-Mirador-Heure (millisecondes Unix au moment de
// l'envoi) et décide. Sans en-tête (ancien agent, autre client) : rien à
// corriger, seuls les points trop loin dans le futur seront écartés.
func (g *Ingestion) horloge(r *http.Request, m Machine, maintenant time.Time) (Decision, time.Duration, bool) {
	v := r.Header.Get(protocole.EnteteHeure)
	if v == "" {
		return HeureBonne, 0, true
	}
	ms, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, 0, false
	}
	dec, d := g.opt.Horloge.Juger(time.UnixMilli(ms), maintenant)
	g.mu.Lock()
	g.decalages[m.ID] = d
	g.mu.Unlock()
	switch dec {
	case HeureCorrige:
		g.Compteurs.HorlogeCorrigee.Add(1)
	case HeureRefuse:
		g.Compteurs.HorlogeRefusee.Add(1)
		g.opt.Journal.Warn("horloge trop décalée, données refusées", "machine", m.Nom, "decalage", d.String())
	}
	return dec, d, true
}

// imposeNom : un agent ne peut parler que pour lui-même. Un collecteur
// (étape 6) envoie pour plusieurs équipements : son nom est alors donné
// dans une étiquette à part (la liste des équipements autorisés viendra
// avec l'étape 6).
func imposeNom(m Machine) (etiquette string) {
	if m.Type == "collecteur" {
		return "collecteur"
	}
	return ""
}

func (g *Ingestion) metriques(w http.ResponseWriter, r *http.Request) {
	m, liberer, ok := g.Accueillir(w, r)
	if !ok {
		return
	}
	defer liberer()
	if ce := r.Header.Get("Content-Encoding"); ce != "snappy" {
		g.refuser(w, http.StatusBadRequest, "Content-Encoding: snappy attendu", 0)
		return
	}
	corps, err := io.ReadAll(io.LimitReader(r.Body, g.opt.MaxCorpsMetriques+1))
	if err != nil {
		g.erreurLecture(w, err)
		return
	}
	if int64(len(corps)) > g.opt.MaxCorpsMetriques {
		g.refuser(w, http.StatusRequestEntityTooLarge, "requête trop grosse", 0)
		return
	}
	maintenant := g.opt.Maintenant()
	dec, d, ok := g.horloge(r, m, maintenant)
	if !ok {
		g.refuser(w, http.StatusBadRequest, protocole.EnteteHeure+" invalide", 0)
		return
	}
	if dec == HeureRefuse {
		g.refuser(w, http.StatusBadRequest, "horloge de la machine trop décalée : "+d.Round(time.Second).String(), 0)
		return
	}
	series, ignorees, err := DecoderRemoteWrite(corps, g.opt.Decodage)
	if err != nil {
		g.erreurDecodage(w, m, err)
		return
	}
	g.Compteurs.SeriesIgnorees.Add(uint64(ignorees)) // #nosec G115 -- compteur ≥ 0
	g.ranger(w, r, m, series, dec, d, maintenant)
}

// ranger : fin commune des métriques (remote write et InfluxDB) : points
// sans heure ou trop loin dans le futur écartés, nom (et site, type, groupe)
// imposés, quotas, écriture, puis 204.
func (g *Ingestion) ranger(w http.ResponseWriter, r *http.Request, m Machine, series []stockage.Serie, dec Decision, d time.Duration, maintenant time.Time) {
	limiteFutur := maintenant.Add(g.opt.Horloge.Maximum).UnixMilli()
	etiquette := imposeNom(m)
	points, futurs := 0, 0
	for i := range series {
		s := &series[i]
		if dec == HeureCorrige {
			for j := range s.Points {
				s.Points[j].T -= d.Milliseconds()
			}
		}
		// Points sans heure ou trop loin dans le futur : écartés.
		gardes := s.Points[:0]
		for _, p := range s.Points {
			if p.T <= 0 || p.T > limiteFutur {
				futurs++
				continue
			}
			gardes = append(gardes, p)
		}
		s.Points = gardes
		points += len(gardes)
		if etiquette == "" {
			s.Remplacer("instance", m.Nom)
			// Site et type choisis dans l'interface (agent seulement : un
			// collecteur envoie pour d'autres appareils, avec leurs étiquettes).
			if m.Site != "" {
				s.Remplacer("site", m.Site)
			}
			if m.Env != "" {
				s.Remplacer("env", m.Env)
			}
			if m.Groupe != "" {
				s.Remplacer("groupe", m.Groupe)
			}
		} else {
			s.Remplacer(etiquette, m.Nom)
		}
	}
	series = sansSeriesVides(series)
	series, refusees := g.quotas.Filtrer(m.ID, series, maintenant)
	g.Compteurs.PointsFuturs.Add(uint64(futurs))     // #nosec G115 -- compteur ≥ 0
	g.Compteurs.SeriesRefusees.Add(uint64(refusees)) // #nosec G115 -- compteur ≥ 0
	if refusees > 0 {
		g.avertirQuota(m, "séries refusées (quota ou cardinalité)", refusees)
	}
	if len(series) > 0 {
		if err := g.opt.Metriques.Ecrire(r.Context(), series); err != nil {
			g.opt.Journal.Error("écriture des métriques", "machine", m.Nom, "erreur", err)
			g.refuser(w, http.StatusServiceUnavailable, "stockage indisponible", 10)
			return
		}
	}
	g.Compteurs.Series.Add(uint64(len(series)))
	g.Compteurs.Points.Add(uint64(points)) // #nosec G115 -- compteur ≥ 0
	w.WriteHeader(http.StatusNoContent)
}

func sansSeriesVides(series []stockage.Serie) []stockage.Serie {
	out := series[:0]
	for _, s := range series {
		if len(s.Points) > 0 {
			out = append(out, s)
		}
	}
	return out
}

func (g *Ingestion) logs(w http.ResponseWriter, r *http.Request) {
	m, liberer, ok := g.Accueillir(w, r)
	if !ok {
		return
	}
	defer liberer()
	var compresse bool
	switch strings.ToLower(r.Header.Get("Content-Encoding")) {
	case "gzip":
		compresse = true
	case "", "identity":
	default:
		g.refuser(w, http.StatusBadRequest, "Content-Encoding: gzip attendu", 0)
		return
	}
	maintenant := g.opt.Maintenant()
	dec, d, ok := g.horloge(r, m, maintenant)
	if !ok {
		g.refuser(w, http.StatusBadRequest, protocole.EnteteHeure+" invalide", 0)
		return
	}
	if dec == HeureRefuse {
		g.refuser(w, http.StatusBadRequest, "horloge de la machine trop décalée : "+d.Round(time.Second).String(), 0)
		return
	}
	lignes, ignorees, err := DecoderJsonline(r.Body, compresse, g.opt.Lignes, maintenant)
	if err != nil {
		g.erreurDecodage(w, m, err)
		return
	}
	g.Compteurs.LignesIgnorees.Add(uint64(ignorees)) // #nosec G115 -- compteur ≥ 0
	cle := "m:" + strconv.FormatInt(m.ID, 10)
	if len(lignes) > 0 && !g.lignes.Prendre(cle, float64(len(lignes)), maintenant) {
		// Quota de logs dépassé : 429, l'agent garde le lot et réessaie plus
		// tard (sa file est bornée : en cas de déluge durable, il jette les
		// plus anciens lots, rien ne déborde côté serveur).
		g.Compteurs.LignesRefusees.Add(uint64(len(lignes)))
		g.avertirQuota(m, "quota de lignes de logs par minute dépassé", 0)
		g.refuser(w, http.StatusTooManyRequests, "quota de logs dépassé", g.lignes.Attente(cle))
		return
	}
	limiteFutur := maintenant.Add(g.opt.Horloge.Maximum).UnixMilli()
	etiquette := imposeNom(m)
	for i := range lignes {
		l := &lignes[i]
		if dec == HeureCorrige {
			l.T -= d.Milliseconds()
		}
		if l.T <= 0 || l.T > limiteFutur {
			l.T = maintenant.UnixMilli() // un log reste utile : heure de réception
		}
		if etiquette == "" {
			l.Champs["host"] = m.Nom
			if m.Site != "" {
				l.Champs["site"] = m.Site
			}
			if m.Env != "" {
				l.Champs["env"] = m.Env
			}
			if m.Groupe != "" {
				l.Champs["groupe"] = m.Groupe
			}
		} else {
			l.Champs[etiquette] = m.Nom
		}
	}
	if len(lignes) > 0 {
		if err := g.opt.Logs.Ecrire(r.Context(), lignes); err != nil {
			g.opt.Journal.Error("écriture des logs", "machine", m.Nom, "erreur", err)
			g.refuser(w, http.StatusServiceUnavailable, "stockage indisponible", 10)
			return
		}
	}
	g.Compteurs.Lignes.Add(uint64(len(lignes)))
	g.compterLignes(lignes)
	w.WriteHeader(http.StatusNoContent)
}

// maxHotesCompteurs : au-delà, les hôtes nouveaux comptent dans « autres »
// (un collecteur mal réglé ne peut pas faire grossir la mémoire).
const maxHotesCompteurs = 2000

func (g *Ingestion) compterLignes(lignes []stockage.Ligne) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for i := range lignes {
		h := lignes[i].Champs["host"]
		if _, ok := g.lignesHote[h]; !ok && len(g.lignesHote) >= maxHotesCompteurs {
			h = "autres"
		}
		g.lignesHote[h]++
	}
}

// LignesParHote : lignes reçues par hôte depuis le démarrage (copie).
func (g *Ingestion) LignesParHote() map[string]uint64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make(map[string]uint64, len(g.lignesHote))
	for h, n := range g.lignesHote {
		out[h] = n
	}
	return out
}

// erreurLecture : corps coupé ou trop gros (limite globale du routeur).
func (g *Ingestion) erreurLecture(w http.ResponseWriter, err error) {
	var trop *http.MaxBytesError
	if errors.As(err, &trop) {
		g.refuser(w, http.StatusRequestEntityTooLarge, "requête trop grosse", 0)
		return
	}
	g.refuser(w, http.StatusBadRequest, "lecture de la requête impossible", 0)
}

func (g *Ingestion) erreurDecodage(w http.ResponseWriter, m Machine, err error) {
	var trop *http.MaxBytesError
	switch {
	case errors.Is(err, ErrTropGros), errors.As(err, &trop):
		g.refuser(w, http.StatusRequestEntityTooLarge, "requête trop grosse", 0)
	default:
		g.opt.Journal.Warn("lot invalide", "machine", m.Nom, "erreur", err)
		g.refuser(w, http.StatusBadRequest, "lot invalide", 0)
	}
}

// avertirQuota écrit au plus un avertissement toutes les 5 minutes par
// machine. msg : texte FIXE (jamais tiré de la requête) ; le nombre à part.
func (g *Ingestion) avertirQuota(m Machine, msg string, nombre int) {
	maintenant := g.opt.Maintenant()
	g.mu.Lock()
	dernier := g.avertis[m.ID]
	if maintenant.Sub(dernier) < 5*time.Minute {
		g.mu.Unlock()
		return
	}
	g.avertis[m.ID] = maintenant
	g.mu.Unlock()
	g.opt.Journal.Warn(msg, "machine", m.Nom, "nombre", nombre, "series_actives", g.quotas.Actives(m.ID))
}
