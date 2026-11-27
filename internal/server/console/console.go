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
	"mirador-stack/internal/server/auth"
	"mirador-stack/internal/server/direct"
	"mirador-stack/internal/server/modeles"
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
	"mirador-stack/internal/server/fluxbloques"
	"mirador-stack/internal/server/geoip"
	"mirador-stack/internal/server/ingestion"
	"mirador-stack/internal/server/motifs"
	"mirador-stack/internal/server/notif"
	"mirador-stack/internal/server/stockage/journaux"
	"mirador-stack/internal/server/stockage/metriques"
	"mirador-stack/internal/server/web"
)

// Options : ce dont l'interface a besoin. Les moteurs peuvent être nil
// (tests) : les pages concernées l'indiquent au lieu de planter.
type Options struct {
	// Redimensionner : applique tout de suite une nouvelle taille du parc
	// (plafonds du serveur, SPEC § 141). Nil : appliquée au prochain tour.
	Redimensionner func()
	DB             *sql.DB
	Cle            []byte // clé des secrets (2FA chiffrée en base)
	Journal        *slog.Logger
	Version        string
	Debut          time.Time
	Donnees        string // dossier des données (sauvegardes, versions)
	Alertes        *alertes.Moteur
	Aiguilleur     *notif.Aiguilleur
	Postier        *notif.Postier
	Metriques      *metriques.Moteur
	Logs           *journaux.Moteur
	Ingestion      *ingestion.Ingestion
	// Direct : relevés « en direct » des machines (SPEC § 97) ; nil : pas de direct.
	Direct *direct.Hub
	Motifs *motifs.Registre  // motifs des logs : « vu la 1re fois », « nouveau » (SPEC § 52)
	Flux   *fluxbloques.Flux // flux bloqués par les pare-feu (SPEC § 135) ; nil : page vide
	GeoIP  *geoip.Base       // pays des adresses (peut être nil)
	// Niveau : niveau du journal du serveur (mode debug temporaire) ; peut être nil.
	Niveau *slog.LevelVar
	// Maintenant : horloge (remplaçable dans les tests).
	Maintenant func() time.Time
	// MajDemande, MajEtat : dossiers d'échange avec mirador-maj-serveur
	// (SPEC § 102) ; vides : mise à jour du serveur depuis l'interface
	// désactivée.
	MajDemande, MajEtat string
}

// Console : l'interface en marche.
type Console struct {
	defis    defisConnexion // défis des connexions par passkey seule
	opt      Options
	db       *sql.DB
	journal  *slog.Logger
	sessions *auth.Magasin
	verrous  *auth.Verrous
	securite atomic.Pointer[Securite]
	mux      *http.ServeMux
	pages    *gabarits

	// prevenir : envoi d'un message de sécurité par les canaux de
	// notification (nil sans canaux ; remplaçable dans les tests).
	prevenir func(ctx context.Context, groupe, texte string)
	// informer : message d'information (résumé de la semaine, 27/11).
	informer func(ctx context.Context, groupe, texte string) bool
	messages antiRafale
	// dernierAuto : dernier passage de l'application automatique des modèles.
	dernierAuto  time.Time
	machinesAuto int // machines vues au dernier passage (rythme, SPEC § 141)
	// dernierVagues : dernier passage du déploiement par vagues.
	dernierVagues time.Time
	// dernierNettoyageConfiance : appareils de confiance expirés, une fois par heure.
	dernierNettoyageConfiance time.Time
	// pings : limite des appels /ping/<jeton> par adresse (SPEC § 80).
	pings limitePing
	// Liste de blocage (27/11) : lectures limitées, liste gardée 10 min.
	lecturesBlocage limitePing
	blocage         cacheBlocage
	// statuts : limite des lectures de la page de statut publique (SPEC § 84).
	statuts limitePing

	// derniere : dernière version du serveur publiée (SPEC § 102).
	derniere cacheDerniere
	// dernierMajNuit : dernier passage de la mise à jour de nuit (10 min).
	dernierMajNuit time.Time

	muPremier    sync.Mutex
	jetonPremier string // jeton de création du premier admin ('' = plus de besoin)

	muComptes sync.Mutex
	comptes   map[int64]compteCache

	lecteur    *alertes.Lecteur // requêtes PromQL des pages (nil sans métriques)
	recherches chan struct{}    // recherches de logs en cours (2 au plus)

	conservationAppliquee Conservation // dernier réglage donné aux moteurs de stockage
	interfaceCourante     atomic.Pointer[ReglagesInterface]
	tableaux              atomic.Pointer[etatTableaux]      // tableaux en service (fournis + base)
	sante                 cacheSante                        // pastille « Santé de mirador » (dernier calcul)
	cacheMet              cacheMetriques                    // métriques reçues (assistant de requête)
	cacheValider          cacheAValider                     // éléments « à valider » par machine (menu, liste)
	verrousLevesLe        atomic.Int64                      // dernière levée des verrous vue (commande de secours)
	nomsAnciens           atomic.Pointer[map[string]string] // ancien nom → nom actuel (historique des machines renommées)

	importMaj    etatImport          // import d'une version depuis GitHub (en arrière-plan)
	sourceGitHub flotte.SourceGitHub // adresse de GitHub (remplaçable dans les tests)
	versionsGH   cacheVersionsGitHub // 5 dernières versions du dépôt (SPEC § 37)

	muEtincelles     sync.Mutex
	etincellesCache  map[string]string
	etincellesExpire time.Time
	// Petites courbes du processeur des conteneurs (24 h, SPEC § 123).
	muEtincellesCt     sync.Mutex
	etincellesCtCache  map[string]string
	etincellesCtExpire time.Time
	cache              cacheRequetes
	annot              cacheAnnotations // alertes et événements des courbes d'une machine (SPEC § 31)
	prevDisques        cachePrevisions  // « plein dans X j » des disques (SPEC § 31)
	resumesTab         cacheResumes     // cartes de la liste des tableaux (maquette du 26/11)
	chaleur            cacheChaleur     // carte de chaleur du parc sur 24 h (SPEC § 31)
}

type compteCache struct {
	u      auth.Utilisateur
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
	c := &Console{opt: opt, db: opt.DB, journal: opt.Journal, sessions: auth.NouveauMagasin(), verrous: auth.NouveauxVerrous(),
		comptes: map[int64]compteCache{}, cache: cacheRequetes{tout: map[string]entreeRequete{}},
		recherches: make(chan struct{}, 2)}
	if opt.Postier != nil {
		c.prevenir = opt.Postier.Prevenir
		c.informer = opt.Postier.Informer
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
	c.chargerPareFeu(ctx) // réglage « Pare-feu » de chaque machine (SPEC § 136)
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
	// Modèles personnels (SPEC § 24.11) : relus ici aussi (le serveur les
	// charge au démarrage ; un modèle abîmé est signalé, les autres servent).
	if err := modeles.ChargerPerso(ctx, c.db); err != nil && c.journal != nil {
		c.journal.Warn("modèles personnels", "erreur", err)
	}
	c.routes()
	return c, nil
}

func (c *Console) maintenant() time.Time { return c.opt.Maintenant() }

// preparerPremierAdmin : aucun compte par défaut (brief). Tant qu'aucun
// compte n'existe, un jeton aléatoire, valable jusqu'au prochain démarrage,
// est affiché dans le journal (docker logs mirador-server).
func (c *Console) preparerPremierAdmin(ctx context.Context) error {
	n, err := auth.NombreUtilisateurs(ctx, c.db)
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	c.jetonPremier = "mirador_premier_" + auth.Aleatoire(24)
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
	// Modèles détectés appliqués automatiquement (réglage), une fois par
	// minute ; grand parc (SPEC § 141) : 1 min de plus par 250 machines
	// (~4 requêtes par machine à chaque passage).
	if t.Sub(c.dernierAuto) >= time.Minute*time.Duration(1+c.machinesAuto/250) {
		c.dernierAuto = t
		c.appliquerAuto(ctx, t)
	}
	// Déploiement par vagues (SPEC § 141) : avancé une fois par minute.
	if t.Sub(c.dernierVagues) >= time.Minute {
		c.dernierVagues = t
		if msg, err := flotte.AvancerDeploiement(ctx, c.db, t); err != nil {
			c.journal.Error("déploiement par vagues", "erreur", err)
		} else if msg != "" {
			c.journal.Info(msg)
		}
	}
	c.lireNomsAnciens(ctx)
	c.tourSante(ctx, t) // pastille « Santé de mirador » (toutes les 2 min)
	c.majNuit(ctx, t)
	c.tourResume(ctx, t) // résumé de la semaine (27/11)
	c.appliquerConservation(ctx, t)
	c.appliquerInterface(ctx, t)
	s := c.securite.Load()
	// Sessions ordinaires ; une session d'appareil de confiance retirée de
	// la mémoire revient depuis le disque à la requête suivante.
	c.sessions.Nettoyer(t, time.Duration(s.Inactivite), time.Duration(s.DureeMax))
	if in, max := s.DureesConfiance(); t.Sub(c.dernierNettoyageConfiance) >= time.Hour {
		c.dernierNettoyageConfiance = t
		if err := auth.NettoyerConfiance(ctx, c.db, t, in, max); err != nil {
			c.journal.Warn("appareils de confiance expirés non retirés", "erreur", err)
		}
	}
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
	// Signal d'une tâche planifiée (SPEC § 80) : avant la liste d'adresses
	// autorisées (un NAS ou un script du réseau local n'y est pas
	// forcément) ; protégé par le jeton, ne permet qu'une chose.
	if strings.HasPrefix(r.URL.Path, "/ping/") {
		c.servirPing(w, r)
		return
	}
	// Liste de blocage (demande du 27/11) : lue par OPNsense et les VPS,
	// protégée par le jeton ; le script d'installation est public.
	if strings.HasPrefix(r.URL.Path, "/blocage/") {
		c.servirBlocage(w, r)
		return
	}
	// Page de statut publique (SPEC § 84) : même principe, en lecture seule.
	if strings.HasPrefix(r.URL.Path, "/statut/") || r.URL.Path == "/statut.css" {
		c.servirStatut(w, r)
		return
	}
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
	// Icônes demandées à la racine par les téléphones (raccourci de l'écran
	// d'accueil, retour du 23/11 : « on ne voit pas l'icône ») : publiques,
	// comme le reste de /statique/ (avant : redirigées vers la connexion).
	if vers, ok := iconesRacine[r.URL.Path]; ok {
		r2 := r.Clone(r.Context())
		r2.URL.Path = vers
		servirStatique(w, r2)
		return
	}
	if r.URL.Path == "/sw.js" { // service worker des notifications (SPEC § 35)
		servirSW(w, r)
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
	s  *auth.Session // copie
	u  auth.Utilisateur
	ip netip.Addr
	t  time.Time
}

// qui : nom noté dans l'audit.
func (q *req) qui() string { return "web:" + q.u.Nom }

func (q *req) admin() bool { return q.u.Role == auth.RoleAdmin }

type gestionnaire func(*req)

// lecture : page accessible à tout compte connecté.
func (c *Console) lecture(f gestionnaire) http.HandlerFunc { return c.proteger(auth.RoleLecture, f) }

// admin : action réservée au rôle admin.
func (c *Console) admin(f gestionnaire) http.HandlerFunc { return c.proteger(auth.RoleAdmin, f) }

func (c *Console) proteger(role string, f gestionnaire) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		t := c.maintenant()
		q := &req{w: w, r: r, ip: web.AdresseClient(r), t: t}
		ck, _ := r.Cookie(auth.NomCookie)
		if ck != nil {
			q.s = c.sessions.Lire(ck.Value)
			// Appareil de confiance (SPEC § 78) : session reprise depuis le
			// disque (PWA rouverte, serveur redémarré).
			if q.s == nil {
				q.s = c.restaurerConfiance(r.Context(), ck.Value, q.ip.String(), t)
			}
		}
		if q.s != nil && c.expiree(q.s, t) {
			c.sessions.Supprimer(q.s.Empreinte)
			if q.s.Confiance {
				_ = auth.OublierConfiance(r.Context(), c.db, q.s.Empreinte)
			}
			q.s = nil
		}
		if q.s == nil {
			c.versConnexion(w, r)
			return
		}
		if q.s.Etape != auth.EtapeComplet {
			http.Redirect(w, r, cheminEtape(q.s.Etape), http.StatusSeeOther)
			return
		}
		u, err := c.compte(r.Context(), q.s.UtilisateurID, t)
		if err != nil || u.Desactive || u.Generation != q.s.Generation || u.TOTP == "" {
			c.sessions.Supprimer(q.s.Empreinte)
			c.versConnexion(w, r)
			return
		}
		q.u = u
		// Rôle AVANT le jeton CSRF : lire le jeton d'un formulaire lit tout
		// le corps de la requête (jusqu'à 31 Mio pour la publication d'une
		// version). Un compte lecture est refusé sans que rien ne soit lu
		// (audit du 16/11, SPEC § 34).
		if role == auth.RoleAdmin && u.Role != auth.RoleAdmin {
			http.Error(w, "réservé au rôle admin", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			jeton := r.Header.Get("X-CSRF")
			if jeton == "" {
				jeton = r.PostFormValue("csrf")
			}
			if subtle.ConstantTimeCompare([]byte(jeton), []byte(q.s.CSRF)) != 1 {
				c.journal.Warn("console : jeton CSRF absent ou faux", "ip", q.ip.String(), "utilisateur", u.Nom, "chemin", r.URL.Path)
				// Formulaire (pas un appel de script) : rien n'est fait, la
				// page est rechargée avec un message (avant : page d'erreur
				// brute, retour du 21/11).
				// Seulement un jeton présent mais périmé (page restée ouverte) :
				// sans jeton du tout, refus net.
				if jeton != "" && r.Header.Get("X-CSRF") == "" && r.Header.Get("X-Requested-With") == "" {
					c.flash(q, "", "la page avait expiré : elle a été rechargée, rien n'a été fait ; recommencer")
					http.Redirect(w, r, pageDOrigine(r), http.StatusSeeOther) // #nosec G710 -- pageDOrigine : chemin du même site seulement, sinon « / »
					return
				}
				http.Error(w, "jeton de formulaire invalide : recharger la page", http.StatusForbidden)
				return
			}
		}
		c.sessions.Modifier(q.s.Empreinte, func(s *auth.Session) { s.Vue = t })
		c.toucherConfiance(r.Context(), q.s, t)
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			q.r = r.WithContext(avecMemoMachines(r.Context()))
		}
		f(q)
	}
}

// pageDOrigine : page d'où vient le formulaire (en-tête Referer, même site
// seulement), sinon l'accueil. Jamais une adresse d'un autre site.
func pageDOrigine(r *http.Request) string {
	u, err := url.Parse(r.Referer())
	if err != nil || u.Host != r.Host || !strings.HasPrefix(u.Path, "/") || strings.HasPrefix(u.Path, "//") || strings.ContainsAny(u.Path, "\\\r\n") {
		return "/"
	}
	// Chemin et paramètres seulement : jamais d'hôte ni de schéma.
	v := u.EscapedPath()
	if u.RawQuery != "" {
		v += "?" + u.RawQuery
	}
	return v
}

// versConnexion : page de connexion pour une page, 401 pour un appel de
// script (fetch) ou une écriture.
//
// Deux aides pour les liens reçus ailleurs (notification Telegram, e-mail,
// favori), SPEC § 34.5 :
//   - la page demandée est gardée (« suite ») : on y arrive après la
//     connexion, au lieu de l'accueil ;
//   - lien ouvert depuis un autre site : le navigateur n'envoie pas le
//     cookie de session (SameSite=Strict), même si l'on est connecté. Une
//     page relais recharge alors la même adresse « de l'intérieur » : cette
//     fois le cookie part, et la page s'affiche sans se reconnecter. Le
//     cookie reste Strict, et le relais ne sert que pour afficher une page
//     (GET) : une écriture venue d'un autre site est toujours refusée.
func (c *Console) versConnexion(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && r.Header.Get("X-Requested-With") == "" {
		suite := suiteLocale(r.URL.RequestURI())
		if _, err := r.Cookie(auth.NomCookie); err != nil && suite != "" && lienExterne(r) {
			c.pages.relais(w, suite)
			return
		}
		vers := "/connexion"
		if suite != "" {
			vers += "?suite=" + url.QueryEscape(suite)
		}
		http.Redirect(w, r, vers, http.StatusSeeOther)
		return
	}
	http.Error(w, "session expirée : se reconnecter", http.StatusUnauthorized)
}

// lienExterne : page ouverte en cliquant un lien depuis un autre site ou
// une autre application (en-têtes Sec-Fetch-* des navigateurs récents).
func lienExterne(r *http.Request) bool {
	s := r.Header.Get("Sec-Fetch-Site")
	return (s == "cross-site" || s == "same-site") && r.Header.Get("Sec-Fetch-Mode") == "navigate" &&
		r.Header.Get("Sec-Fetch-Dest") == "document"
}

// suiteLocale : page de ce site où revenir après la connexion ("" : aucune,
// on ira à l'accueil). Jamais une adresse d'un autre site, ni les pages de
// connexion, ni les adresses appelées par les scripts.
func suiteLocale(v string) string {
	if v == "" || len(v) > 512 {
		return ""
	}
	s := retourLocal(v)
	if s == "/" {
		return ""
	}
	for _, p := range []string{"/connexion", "/deconnexion", "/premier-admin", "/statique/", "/fragments/", "/donnees/", "/sante"} {
		if strings.HasPrefix(s, p) {
			return ""
		}
	}
	return s
}

func cheminEtape(e string) string {
	if e == auth.EtapeCreer {
		return "/connexion/2fa-creer"
	}
	return "/connexion/code"
}

// compte : compte lu en base, gardé 10 s en mémoire.
func (c *Console) compte(ctx context.Context, id int64, t time.Time) (auth.Utilisateur, error) {
	c.muComptes.Lock()
	cc, ok := c.comptes[id]
	c.muComptes.Unlock()
	if ok && t.Before(cc.expire) {
		return cc.u, nil
	}
	u, err := auth.LireUtilisateurID(ctx, c.db, id)
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
	// Messages écrits par le serveur : « 1 règle(s) » → « 1 règle ».
	ok, erreur = accorderTexte(ok), accorderTexte(erreur)
	c.sessions.Modifier(q.s.Empreinte, func(s *auth.Session) { s.Flash, s.Alerte = ok, erreur })
}

// retour : après une écriture, message puis redirection (303 : le
// navigateur recharge en GET, pas de double envoi du formulaire).
func (c *Console) retour(q *req, vers string, err error, ok string) {
	if err != nil {
		c.flash(q, "", err.Error())
		// Retour du 22/11 : le message d'erreur est en haut de la page ; sans
		// l'ancre, la page s'ouvre là où on le voit (avec l'ancre, elle
		// sautait au formulaire et l'erreur passait inaperçue). Les grands
		// champs de texte sont réaffichés tels que saisis.
		if i := strings.IndexByte(vers, '#'); i >= 0 {
			vers = vers[:i]
		}
		c.garderSaisie(q)
	} else {
		c.flash(q, ok, "")
	}
	http.Redirect(q.w, q.r, vers, http.StatusSeeOther)
}

// champsGardes : grands champs de texte réaffichés après un refus (jamais
// un secret ni un code).
var champsGardes = []string{"sondes", "emetteurs", "sonde_adresse", "sonde_nom", "emetteur_adresse"}

// garderSaisie : texte des champs gardés, pour un seul affichage.
func (c *Console) garderSaisie(q *req) {
	saisie := map[string]string{}
	for _, nom := range champsGardes {
		if v := q.r.PostFormValue(nom); v != "" && len(v) <= 20000 {
			saisie[nom] = v
		}
	}
	if len(saisie) > 0 {
		c.sessions.Modifier(q.s.Empreinte, func(s *auth.Session) { s.Saisie = saisie })
	}
}

// delaiEssaiPush : temps entre deux notifications d'essai d'une session.
const delaiEssaiPush = 10 * time.Second

// essaisSensiblesMax : codes 2FA (ou mots de passe) faux tolérés pendant
// une session complète ; au-delà, la session est fermée (un cookie volé ne
// permet pas d'essayer les codes sans fin, revue du 11/11).
const essaisSensiblesMax = 5

var (
	// errCodeDemande : action sensible lancée sans code 2FA, mode
	// administration fermé.
	errCodeDemande   = errors.New("action protégée : code 2FA demandé (ou ouvrir le mode administration dans « Mon compte »)")
	errTropEssais    = errors.New("trop d'essais : réessayer plus tard")
	errSessionFermee = errors.New("trop d'essais : session fermée, se reconnecter")
)

// sensibleBloque : adresse ou compte verrouillé (vérifié AVANT tout calcul).
func (c *Console) sensibleBloque(q *req) bool {
	return c.verrous.BloqueAdresse(q.ip.String(), q.t) || c.verrous.BloqueCompte(q.u.Nom, q.ip.String(), q.t)
}

// debuterSensible : essai réservé avant la vérification d'un code ou d'un
// mot de passe (rafale de requêtes simultanées : voir verrou.go).
func (c *Console) debuterSensible(q *req) (finir func(), ok bool) {
	return c.verrous.Debuter(q.u.Nom, q.ip.String(), c.securite.Load().Echecs, q.t)
}

// echecSensible : compté par adresse ET par compte, et dans la session ;
// au 5e, la session est fermée.
func (c *Console) echecSensible(q *req, raison string) error {
	c.echecConnexion(q.ip.String(), q.u.Nom, true, raison, q.t)
	n := 0
	c.sessions.Modifier(q.s.Empreinte, func(s *auth.Session) { s.EssaisSensibles++; n = s.EssaisSensibles })
	if n >= essaisSensiblesMax {
		c.sessions.Supprimer(q.s.Empreinte)
		// Appareil de confiance : oublié aussi, sinon la session revenait
		// aussitôt depuis le disque avec un compteur à zéro (relecture du
		// 21/11).
		_ = auth.OublierConfiance(q.r.Context(), c.db, q.s.Empreinte)
		c.journal.Warn("console : session fermée après trop d'essais", "ip", q.ip.String(), "utilisateur", q.u.Nom)
		return errSessionFermee
	}
	return nil
}

// DureeModeAdmin : durée par défaut du mode administration (réglable dans
// Réglages › Sécurité : 0 à 60 min).
const DureeModeAdmin = 10 * time.Minute

// dureeModeAdmin : durée réglée (0 = mode administration désactivé).
func (c *Console) dureeModeAdmin() time.Duration {
	if s := c.securite.Load(); s != nil {
		return time.Duration(s.ModeAdmin)
	}
	return DureeModeAdmin
}

// verifierCode : code 2FA d'une action sensible (brief). Mode administration
// (demande du 14/11) : sans code saisi, accepté tant que le mode est ouvert
// (10 min après le dernier code juste, comme le « sudo » de GitHub).
func (c *Console) verifierCode(q *req) error {
	if strings.TrimSpace(q.r.PostFormValue("code")) == "" {
		if q.t.Before(q.s.AdminJusqua) && c.dureeModeAdmin() > 0 {
			return nil
		}
		// Aucun code saisi et mode fermé : message clair, et ce n'est pas
		// un essai manqué (rien n'est compté).
		return errCodeDemande
	}
	if err := c.verifierCodeFrais(q); err != nil {
		return err
	}
	if d := c.dureeModeAdmin(); d > 0 {
		jusqua := q.t.Add(d)
		c.sessions.Modifier(q.s.Empreinte, func(s *auth.Session) { s.AdminJusqua = jusqua })
		q.s.AdminJusqua = jusqua
	}
	return nil
}

// verifierCodeFrais : nouvelle saisie du code 2FA, toujours (sécurité des
// comptes : mots de passe, 2FA, passkeys, utilisateurs). Un code ne sert
// qu'une fois.
func (c *Console) verifierCodeFrais(q *req) error {
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
	pas, ok := auth.VerifierTOTP(secret, code, q.t, q.u.TOTPDernier)
	if ok {
		ok, err = auth.NoterPasTOTP(q.r.Context(), c.db, q.u.ID, pas, q.t)
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

// postOuvrirModeAdmin : ouvre le mode administration avec un code 2FA, sans
// attendre une action sensible (page « Mon compte », et repli sans
// JavaScript des boutons protégés). Retour à la page d'où l'on vient.
func (c *Console) postOuvrirModeAdmin(q *req) {
	vers := retourLocal(q.r.PostFormValue("retour"))
	if c.dureeModeAdmin() <= 0 {
		c.retour(q, vers, errors.New("mode administration désactivé (Réglages › Sécurité) : le code est demandé à chaque action"), "")
		return
	}
	if err := c.verifierCodeFrais(q); err != nil {
		c.retour(q, vers, err, "")
		return
	}
	jusqua := q.t.Add(c.dureeModeAdmin())
	c.sessions.Modifier(q.s.Empreinte, func(s *auth.Session) { s.AdminJusqua = jusqua })
	c.retour(q, vers, nil, "mode administration ouvert jusqu'à "+jusqua.In(fuseau.Load()).Format("15:04"))
}

// postQuitterModeAdmin : ferme le mode administration avant la fin des
// 10 minutes (le code 2FA est de nouveau demandé).
func (c *Console) postQuitterModeAdmin(q *req) {
	c.sessions.Modifier(q.s.Empreinte, func(s *auth.Session) { s.AdminJusqua = time.Time{} })
	c.retour(q, "/", nil, "mode administration fermé : le code 2FA est de nouveau demandé")
}

// iconesRacine : adresses d'icônes que les navigateurs et les téléphones
// demandent d'eux-mêmes, sans lire la page.
var iconesRacine = map[string]string{
	"/favicon.ico":                      "/statique/icones/favicon.ico",
	"/apple-touch-icon.png":             "/statique/icones/icone-apple-180.png",
	"/apple-touch-icon-precomposed.png": "/statique/icones/icone-apple-180.png",
}
