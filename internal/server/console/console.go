// SPDX-License-Identifier: GPL-3.0-or-later

// Package console : l'interface web de mirador-server (étape 5, SPEC § 18).
//
// Rendu côté serveur (html/template, échappement automatique), un peu de
// JavaScript sans framework, fichiers statiques embarqués et compressés.
// Chaque requête passe, dans l'ordre :
//
//  1. la liste d'adresses autorisées (réglage « Sécurité ») ;
//  2. les fichiers statiques (publics : la page de connexion en a besoin) ;
//  3. le contrôle d'origine (requêtes d'un autre site refusées) ;
//  4. la session (mot de passe + 2FA) et le jeton CSRF pour toute écriture ;
//  5. le rôle (lecture seule ou admin).
package console

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/prometheus/storage"

	"mirador-stack/internal/server/alertes"
	"mirador-stack/internal/server/flotte"
	"mirador-stack/internal/server/geoip"
	"mirador-stack/internal/server/ingestion"
	"mirador-stack/internal/server/notif"
	"mirador-stack/internal/server/stockage/journaux"
	"mirador-stack/internal/server/stockage/metriques"
	"mirador-stack/internal/server/web"
)

// Options : ce dont l'interface a besoin. Les moteurs peuvent être nil
// (tests) : les pages concernées l'indiquent au lieu de planter.
type Options struct {
	DB         *sql.DB
	Cle        []byte // clé des secrets (2FA chiffrée en base)
	Journal    *slog.Logger
	Version    string
	Debut      time.Time
	Donnees    string // dossier des données (sauvegardes, versions)
	Alertes    *alertes.Moteur
	Aiguilleur *notif.Aiguilleur
	Postier    *notif.Postier
	Metriques  *metriques.Moteur
	Logs       *journaux.Moteur
	Ingestion  *ingestion.Ingestion
	GeoIP      *geoip.Base // pays des adresses (peut être nil)
	// Niveau : niveau du journal du serveur (mode debug temporaire) ; peut être nil.
	Niveau *slog.LevelVar
	// Maintenant : horloge (remplaçable dans les tests).
	Maintenant func() time.Time
}

// Console : l'interface en marche.
type Console struct {
	opt      Options
	db       *sql.DB
	journal  *slog.Logger
	sessions *magasin
	verrous  *verrous
	securite atomic.Pointer[Securite]
	mux      *http.ServeMux
	pages    *gabarits

	// prevenir : envoi d'un message de sécurité par les canaux de
	// notification (nil sans canaux ; remplaçable dans les tests).
	prevenir func(ctx context.Context, groupe, texte string)
	messages antiRafale

	muPremier    sync.Mutex
	jetonPremier string // jeton de création du premier admin ('' = plus de besoin)

	muComptes sync.Mutex
	comptes   map[int64]compteCache

	lecteur    *alertes.Lecteur // requêtes PromQL des pages (nil sans métriques)
	recherches chan struct{}    // recherches de logs en cours (2 au plus)

	conservationAppliquee Conservation // dernier réglage donné aux moteurs de stockage
	interfaceCourante     atomic.Pointer[ReglagesInterface]
	tableaux              atomic.Pointer[etatTableaux]      // tableaux en service (fournis + base)
	cacheMet              cacheMetriques                    // métriques reçues (assistant de requête)
	verrousLevesLe        atomic.Int64                      // dernière levée des verrous vue (commande de secours)
	nomsAnciens           atomic.Pointer[map[string]string] // ancien nom → nom actuel (historique des machines renommées)

	importMaj    etatImport          // import d'une version depuis GitHub (en arrière-plan)
	sourceGitHub flotte.SourceGitHub // adresse de GitHub (remplaçable dans les tests)

	muEtincelles     sync.Mutex
	etincellesCache  map[string]string
	etincellesExpire time.Time
	cache            cacheRequetes
}

type compteCache struct {
	u      Utilisateur
	expire time.Time
}

// dureeCacheCompte : un compte désactivé ou une 2FA réinitialisée par une
// commande (autre processus) prend effet en 10 s au plus.
const dureeCacheCompte = 10 * time.Second

// Nouvelle prépare l'interface. Si aucun compte n'existe, un jeton de
// création du premier admin est écrit dans le journal du serveur.
func Nouvelle(ctx context.Context, opt Options) (*Console, error) {
	if opt.Maintenant == nil {
		opt.Maintenant = time.Now
	}
	c := &Console{opt: opt, db: opt.DB, journal: opt.Journal, sessions: nouveauMagasin(), verrous: nouveauxVerrous(),
		comptes: map[int64]compteCache{}, cache: cacheRequetes{tout: map[string]entreeRequete{}},
		recherches: make(chan struct{}, 2)}
	if opt.Postier != nil {
		c.prevenir = opt.Postier.Prevenir
	}
	if opt.Metriques != nil {
		// La base brute ou agrégée est choisie selon la période demandée.
		// Historique des machines renommées rattaché au nouveau nom (lecture).
		c.lecteur = alertes.NouveauLecteur(avecNomsAnciens(storage.QueryableFunc(func(mint, maxt int64) (storage.Querier, error) {
			q, _, err := opt.Metriques.Interroger(mint, maxt)
			return q, err
		}), c.anciensNoms), opt.Journal)
	}
	c.lireNomsAnciens(ctx)
	s, err := LireSecurite(ctx, opt.DB)
	if err != nil {
		// Réglage abîmé : on reste sur les défauts (plus stricts) et on le dit.
		c.journal.Error("console : réglages de sécurité illisibles, valeurs par défaut", "erreur", err)
	}
	c.securite.Store(&s)
	if c.pages, err = chargerGabarits(); err != nil {
		return nil, err
	}
	c.appliquerConservation(ctx, c.maintenant())
	c.appliquerInterface(ctx, c.maintenant())
	c.chargerTableaux(ctx)
	// Une levée de verrous ancienne ne compte pas (rien à lever au démarrage).
	var leve string
	if c.db.QueryRowContext(ctx, `SELECT valeur FROM reglages WHERE cle = ?`, cleVerrousLeves).Scan(&leve) == nil {
		n, _ := strconv.ParseInt(leve, 10, 64)
		c.verrousLevesLe.Store(n)
	}
	if err := c.preparerPremierAdmin(ctx); err != nil {
		return nil, err
	}
	c.mux = http.NewServeMux()
	c.routes()
	return c, nil
}

func (c *Console) maintenant() time.Time { return c.opt.Maintenant() }

// preparerPremierAdmin : aucun compte par défaut (brief). Tant qu'aucun
// compte n'existe, un jeton aléatoire, valable jusqu'au prochain démarrage,
// est affiché dans le journal (docker logs mirador-server).
func (c *Console) preparerPremierAdmin(ctx context.Context) error {
	n, err := NombreUtilisateurs(ctx, c.db)
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	c.jetonPremier = "mirador_premier_" + aleatoire(24)
	c.journal.Warn("console : aucun compte ; ouvrir /premier-admin avec ce jeton à usage unique (valable jusqu'au prochain redémarrage)",
		"jeton", c.jetonPremier)
	return nil
}

// GeoIPActive : téléchargement automatique de la base GéoIP (réglage
// « Interface », activé par défaut).
func (c *Console) GeoIPActive() bool { return lireInterface(context.Background(), c.db).GeoIP }

// Boucle : nettoyage des sessions, annulation des changements non confirmés,
// relecture des réglages de sécurité (changés par une commande).
func (c *Console) Boucle(ctx context.Context) {
	tic := time.NewTicker(5 * time.Second)
	defer tic.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tic.C:
			c.Tour(ctx)
		}
	}
}

// Tour : un passage de la boucle.
func (c *Console) Tour(ctx context.Context) {
	t := c.maintenant()
	if n, err := AnnulerExpires(ctx, c.db, t); err != nil {
		c.journal.Error("console : annulation des changements non confirmés", "erreur", err)
	} else if n > 0 {
		c.journal.Warn("console : changement non confirmé annulé (ancienne valeur remise)", "nombre", n)
		c.oublierComptes()
	}
	if s, err := LireSecurite(ctx, c.db); err == nil {
		c.securite.Store(&s)
	}
	c.leverSiDemande(ctx)
	c.lireNomsAnciens(ctx)
	c.appliquerConservation(ctx, t)
	c.appliquerInterface(ctx, t)
	s := c.securite.Load()
	c.sessions.nettoyer(t, time.Duration(s.Inactivite), time.Duration(s.DureeMax))
}

// lireNomsAnciens : table « ancien nom → nom actuel » (machines renommées),
// relue à chaque tour (5 s) et après un renommage.
func (c *Console) lireNomsAnciens(ctx context.Context) {
	if m, err := flotte.NomsAnciens(ctx, c.db); err == nil {
		c.nomsAnciens.Store(&m)
	}
}

// anciensNoms : table en service (jamais nil).
func (c *Console) anciensNoms() map[string]string {
	if m := c.nomsAnciens.Load(); m != nil {
		return *m
	}
	return nil
}

// ServeHTTP : point d'entrée de toutes les requêtes de l'interface.
func (c *Console) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ip := web.AdresseClient(r)
	if !c.securite.Load().AdresseAutorisee(ip) {
		c.journal.Warn("console : adresse non autorisée", "ip", ip.String(), "chemin", r.URL.Path)
		http.Error(w, "adresse non autorisée", http.StatusForbidden)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/statique/") {
		servirStatique(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead && !memeOrigine(r) {
		c.journal.Warn("console : requête d'un autre site refusée", "ip", ip.String(), "chemin", r.URL.Path,
			"origine", r.Header.Get("Origin"))
		http.Error(w, "requête refusée (origine)", http.StatusForbidden)
		return
	}
	c.mux.ServeHTTP(w, r)
}

// memeOrigine : refuse une écriture lancée depuis un autre site (CSRF), en
// plus du cookie SameSite=Strict et du jeton CSRF de la session.
// Sec-Fetch-Site (envoyé par tous les navigateurs récents) fait foi ; sinon
// l'en-tête Origin doit désigner ce serveur (nginx doit transmettre Host).
func memeOrigine(r *http.Request) bool {
	if s := r.Header.Get("Sec-Fetch-Site"); s != "" {
		return s == "same-origin" || s == "none"
	}
	if o := r.Header.Get("Origin"); o != "" {
		u, err := url.Parse(o)
		return err == nil && u.Host == r.Host
	}
	return true
}

// --- Requête authentifiée ----------------------------------------------------------

// req : une requête avec sa session et son compte.
type req struct {
	w  http.ResponseWriter
	r  *http.Request
	s  *Session // copie
	u  Utilisateur
	ip netip.Addr
	t  time.Time
}

// qui : nom noté dans l'audit.
func (q *req) qui() string { return "web:" + q.u.Nom }

func (q *req) admin() bool { return q.u.Role == RoleAdmin }

type gestionnaire func(*req)

// lecture : page accessible à tout compte connecté.
func (c *Console) lecture(f gestionnaire) http.HandlerFunc { return c.proteger(RoleLecture, f) }

// admin : action réservée au rôle admin.
func (c *Console) admin(f gestionnaire) http.HandlerFunc { return c.proteger(RoleAdmin, f) }

func (c *Console) proteger(role string, f gestionnaire) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		t := c.maintenant()
		q := &req{w: w, r: r, ip: web.AdresseClient(r), t: t}
		ck, _ := r.Cookie(NomCookie)
		if ck != nil {
			q.s = c.sessions.lire(ck.Value)
		}
		sec := c.securite.Load()
		if q.s != nil && expiree(q.s, t, time.Duration(sec.Inactivite), time.Duration(sec.DureeMax)) {
			c.sessions.supprimer(q.s.empreinte)
			q.s = nil
		}
		if q.s == nil {
			c.versConnexion(w, r)
			return
		}
		if q.s.Etape != etapeComplet {
			http.Redirect(w, r, cheminEtape(q.s.Etape), http.StatusSeeOther)
			return
		}
		u, err := c.compte(r.Context(), q.s.UtilisateurID, t)
		if err != nil || u.Desactive || u.Generation != q.s.Generation || u.TOTP == "" {
			c.sessions.supprimer(q.s.empreinte)
			c.versConnexion(w, r)
			return
		}
		q.u = u
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			jeton := r.Header.Get("X-CSRF")
			if jeton == "" {
				jeton = r.PostFormValue("csrf")
			}
			if subtle.ConstantTimeCompare([]byte(jeton), []byte(q.s.CSRF)) != 1 {
				c.journal.Warn("console : jeton CSRF absent ou faux", "ip", q.ip.String(), "utilisateur", u.Nom, "chemin", r.URL.Path)
				http.Error(w, "jeton de formulaire invalide : recharger la page", http.StatusForbidden)
				return
			}
		}
		if role == RoleAdmin && u.Role != RoleAdmin {
			http.Error(w, "réservé au rôle admin", http.StatusForbidden)
			return
		}
		c.sessions.modifier(q.s.empreinte, func(s *Session) { s.Vue = t })
		f(q)
	}
}

// versConnexion : page de connexion pour une page, 401 pour un appel de
// script (fetch) ou une écriture.
func (c *Console) versConnexion(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && r.Header.Get("X-Requested-With") == "" {
		http.Redirect(w, r, "/connexion", http.StatusSeeOther)
		return
	}
	http.Error(w, "session expirée : se reconnecter", http.StatusUnauthorized)
}

func cheminEtape(e string) string {
	if e == etapeCreer {
		return "/connexion/2fa-creer"
	}
	return "/connexion/code"
}

// compte : compte lu en base, gardé 10 s en mémoire.
func (c *Console) compte(ctx context.Context, id int64, t time.Time) (Utilisateur, error) {
	c.muComptes.Lock()
	cc, ok := c.comptes[id]
	c.muComptes.Unlock()
	if ok && t.Before(cc.expire) {
		return cc.u, nil
	}
	u, err := LireUtilisateurID(ctx, c.db, id)
	if err != nil {
		return u, err
	}
	c.muComptes.Lock()
	if len(c.comptes) > 1000 {
		c.comptes = map[int64]compteCache{}
	}
	c.comptes[id] = compteCache{u: u, expire: t.Add(dureeCacheCompte)}
	c.muComptes.Unlock()
	return u, nil
}

// oublierComptes : après une modification faite ici, relire tout de suite.
func (c *Console) oublierComptes() {
	c.muComptes.Lock()
	c.comptes = map[int64]compteCache{}
	c.muComptes.Unlock()
}

// flash : message affiché une fois sur la page suivante.
func (c *Console) flash(q *req, ok, erreur string) {
	c.sessions.modifier(q.s.empreinte, func(s *Session) { s.Flash, s.Alerte = ok, erreur })
}

// retour : après une écriture, message puis redirection (303 : le
// navigateur recharge en GET, pas de double envoi du formulaire).
func (c *Console) retour(q *req, vers string, err error, ok string) {
	if err != nil {
		c.flash(q, "", err.Error())
	} else {
		c.flash(q, ok, "")
	}
	http.Redirect(q.w, q.r, vers, http.StatusSeeOther)
}

// essaisSensiblesMax : codes 2FA (ou mots de passe) faux tolérés pendant
// une session complète ; au-delà, la session est fermée (un cookie volé ne
// permet pas d'essayer les codes sans fin, revue du 11/11).
const essaisSensiblesMax = 5

var (
	errTropEssais    = errors.New("trop d'essais : réessayer plus tard")
	errSessionFermee = errors.New("trop d'essais : session fermée, se reconnecter")
)

// sensibleBloque : adresse ou compte verrouillé (vérifié AVANT tout calcul).
func (c *Console) sensibleBloque(q *req) bool {
	return c.verrous.bloqueAdresse(q.ip.String(), q.t) || c.verrous.bloqueCompte(q.u.Nom, q.ip.String(), q.t)
}

// debuterSensible : essai réservé avant la vérification d'un code ou d'un
// mot de passe (rafale de requêtes simultanées : voir verrou.go).
func (c *Console) debuterSensible(q *req) (finir func(), ok bool) {
	return c.verrous.debuter(q.u.Nom, q.ip.String(), c.securite.Load().Echecs, q.t)
}

// echecSensible : compté par adresse ET par compte, et dans la session ;
// au 5e, la session est fermée.
func (c *Console) echecSensible(q *req, raison string) error {
	c.echecConnexion(q.ip.String(), q.u.Nom, raison, q.t)
	n := 0
	c.sessions.modifier(q.s.empreinte, func(s *Session) { s.EssaisSensibles++; n = s.EssaisSensibles })
	if n >= essaisSensiblesMax {
		c.sessions.supprimer(q.s.empreinte)
		c.journal.Warn("console : session fermée après trop d'essais", "ip", q.ip.String(), "utilisateur", q.u.Nom)
		return errSessionFermee
	}
	return nil
}

// verifierCode : nouvelle saisie du code 2FA pour une action sensible
// (brief). Un code ne sert qu'une fois.
func (c *Console) verifierCode(q *req) error {
	code := q.r.PostFormValue("code")
	if c.sensibleBloque(q) {
		return errTropEssais
	}
	finir, libre := c.debuterSensible(q)
	if !libre {
		return errTropEssais
	}
	defer finir()
	secret, err := c.dechiffrerTOTP(q.u.TOTP)
	if err != nil {
		return err
	}
	pas, ok := VerifierTOTP(secret, code, q.t, q.u.TOTPDernier)
	if ok {
		ok, err = NoterPasTOTP(q.r.Context(), c.db, q.u.ID, pas, q.t)
		if err != nil {
			return err
		}
	}
	c.oublierComptes()
	if !ok {
		if err := c.echecSensible(q, "code 2FA faux (action sensible)"); err != nil {
			return err
		}
		return errors.New("code 2FA faux ou déjà utilisé")
	}
	return nil
}
