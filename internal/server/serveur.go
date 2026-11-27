// SPDX-License-Identifier: GPL-3.0-or-later

// Package server assemble mirador-server : fichier de démarrage, base,
// migrations, écoutes HTTP, réception et stockage des données des machines.
// Commandes d'administration : commandes.go.
package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"mirador-stack/internal/server/charge"
	"mirador-stack/internal/server/direct"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"mirador-stack/internal/majserveur"
	"mirador-stack/internal/server/alertes"
	"mirador-stack/internal/server/base"
	"mirador-stack/internal/server/certsyslog"
	"mirador-stack/internal/server/changements"
	"mirador-stack/internal/server/config"
	"mirador-stack/internal/server/console"
	"mirador-stack/internal/server/flotte"
	"mirador-stack/internal/server/fluxbloques"
	"mirador-stack/internal/server/geoip"
	"mirador-stack/internal/server/ingestion"
	"mirador-stack/internal/server/modeles"
	"mirador-stack/internal/server/motifs"
	"mirador-stack/internal/server/notif"
	"mirador-stack/internal/server/sauvegarde"
	"mirador-stack/internal/server/stockage"
	"mirador-stack/internal/server/stockage/journaux"
	"mirador-stack/internal/server/stockage/metriques"
	"mirador-stack/internal/server/web"
	"mirador-stack/internal/version"
)

// NiveauJournal : niveau du journal du serveur (Info par défaut), changé à
// chaud par le « mode debug temporaire » de l'interface.
var NiveauJournal slog.LevelVar

// Serveur : état du serveur en marche.
type Serveur struct {
	cfg     config.Serveur
	cle     []byte // clé de chiffrement des secrets (jamais journalisée)
	db      *sql.DB
	web     *web.Serveurs
	journal *slog.Logger
	debut   time.Time

	ing       *ingestion.Ingestion
	flotte    *flotte.Flotte
	direct    *direct.Hub // relevés en direct des pages des machines (SPEC § 97)
	planif    *sauvegarde.Planificateur
	admin     *http.Server // socket Unix de consultation (admin.go)
	arretFond context.CancelFunc
	finFond   chan struct{}
	met       *metriques.Moteur // métriques (TSDB brute + agrégats)
	jx        *journaux.Moteur  // logs (segments zstd)
	alertes   *alertes.Moteur   // règles et alertes (étape 4)
	aiguille  *notif.Aiguilleur // regroupement, sourdine (étape 4)
	postier   *notif.Postier    // canaux, bascule, file d'attente, heartbeat
	console   *console.Console  // interface web (étape 5)
	geo       *geoip.Base       // pays des adresses (flux bloqués), base DB-IP mensuelle
	vus       vusParNom         // dernières données par machine (SPEC § 46)
	motifs    *motifs.Registre  // motifs des logs (SPEC § 52)
	flux      *fluxbloques.Flux
	dbFlux    [2]*sql.DB // flux.db (entrants, SPEC § 135) et flux-sortants.db (§ 136)
}

// Chemins dans le dossier des données.
func (s *Serveur) chemin(nom string) string { return filepath.Join(s.cfg.Donnees, nom) }

// Demarrer ouvre la base, applique les migrations, ouvre le stockage et les
// écoutes.
func Demarrer(ctx context.Context, cfg config.Serveur, journal *slog.Logger) (*Serveur, error) {
	cle, err := config.LireCle(cfg.FichierCle)
	if err != nil {
		return nil, fmt.Errorf("clé des secrets : %w", err)
	}
	s := &Serveur{cfg: cfg, cle: cle, journal: journal, debut: time.Now()}

	// Ce qui est déjà ouvert, à refermer si la suite du démarrage échoue
	// (dans l'ordre inverse de l'ouverture, comme Arreter).
	var ouverts []func() error
	echec := func(err error) (*Serveur, error) {
		for i := len(ouverts) - 1; i >= 0; i-- {
			err = errors.Join(err, ouverts[i]())
		}
		return nil, err
	}

	if s.db, err = base.Ouvrir(s.chemin("mirador.db")); err != nil {
		return nil, fmt.Errorf("base : %w", err)
	}
	ouverts = append(ouverts, s.db.Close)
	copie, err := base.Migrer(ctx, s.db, s.chemin("sauvegardes"))
	if err != nil {
		return echec(err)
	}
	if copie != "" {
		journal.Info("base migrée ; copie d'avant migration gardée", "copie", copie)
	}
	v, _ := base.Version(ctx, s.db)

	// Stockage (rétentions par défaut ; réglables dans l'interface, étape 5).
	if s.met, err = metriques.Ouvrir(metriques.Options{Dossier: s.chemin("metriques"),
		Journal: journal.With("composant", "metriques")}); err != nil {
		return echec(fmt.Errorf("stockage des métriques : %w", err))
	}
	ouverts = append(ouverts, s.met.Fermer)
	if s.jx, err = journaux.Ouvrir(journaux.Options{Dossier: s.chemin("logs"), Partitions: journaux.PartitionsPour(cfg.TailleParc),
		Journal: journal.With("composant", "logs")}); err != nil {
		return echec(fmt.Errorf("stockage des logs : %w", err))
	}
	ouverts = append(ouverts, s.jx.Fermer)

	// Alertes : règles mirador posées une fois, puis moteur (règles relues
	// à chaud quand elles changent en base).
	// Règles renommées (SPEC § 65) : avant de poser les défauts, sinon la
	// règle serait posée une seconde fois sous son nouveau nom.
	if n, err := alertes.RenommerRegles(ctx, s.db, time.Now()); err != nil {
		journal.Warn("renommage des règles", "erreur", err)
	} else if n > 0 {
		journal.Info("règles d'alerte renommées (SPEC § 65)", "nombre", n)
	}
	// Règle VMSansSauvegarde : invités exclus écartés (retour du 25/11).
	if ok, err := alertes.MettreAJourVMSansSauvegarde(ctx, s.db, time.Now()); err != nil {
		s.journal.Warn("règle VMSansSauvegarde non mise à jour", "erreur", err)
	} else if ok {
		s.journal.Info("règle VMSansSauvegarde : invités exclus écartés")
	}
	if n, err := alertes.PoserDefauts(ctx, s.db, time.Now()); err != nil {
		return echec(fmt.Errorf("règles par défaut : %w", err))
	} else if n > 0 {
		journal.Info("règles d'alerte mirador ajoutées", "nombre", n)
	}
	// Règles au défaut connu (SPEC § 45) : expression corrigée.
	if n, err := alertes.CorrigerRegles(ctx, s.db, time.Now()); err != nil {
		journal.Warn("correction des règles", "erreur", err)
	} else if n > 0 {
		journal.Info("règles d'alerte corrigées (SPEC § 45)", "nombre", n)
	}
	// Modèles personnels (SPEC § 24.11) : chargés avant leurs règles ; un
	// modèle abîmé est laissé de côté (journal), les autres servent.
	if err := modeles.ChargerPerso(ctx, s.db); err != nil {
		journal.Warn("modèles personnels", "erreur", err)
	}
	// Règles des modèles de surveillance (SPEC § 24, § 27) : gérées par les
	// modèles, remises à jour à chaque démarrage (choix « active » gardé).
	if n, err := modeles.Synchroniser(ctx, s.db, "mirador", time.Now()); err != nil {
		return echec(fmt.Errorf("règles des modèles : %w", err))
	} else if n > 0 {
		journal.Info("règles des modèles de surveillance mises à jour", "nombre", n)
	}
	s.vus.recharger(ctx, s)
	if s.alertes, err = alertes.Nouveau(ctx, alertes.Options{DB: s.db, Donnees: s.met.Requetes(), DerniereDonnee: s.derniereDonnee,
		Journal: journal.With("composant", "alertes"), Remplacees: modeles.EstRemplacee}); err != nil {
		return echec(fmt.Errorf("moteur d'alertes : %w", err))
	}
	if _, err := notif.PoserDefauts(ctx, s.db, time.Now()); err != nil {
		return echec(fmt.Errorf("inhibitions par défaut : %w", err))
	}
	if s.postier, err = notif.NouveauPostier(ctx, notif.OptionsPostier{DB: s.db, Cle: cle,
		Journal: journal.With("composant", "canaux"), DerniereEvaluationOK: s.alertes.DerniereEvaluationOK}); err != nil {
		return echec(fmt.Errorf("canaux de notification : %w", err))
	}
	if s.aiguille, err = notif.Nouveau(ctx, notif.Options{DB: s.db, EnCours: s.alertes.EnCours,
		Envoyeur: s.postier, Journal: journal.With("composant", "notifications"), Logs: chercheurExtraits(s.jx),
		Changement: s.dernierChangement}); err != nil {
		return echec(fmt.Errorf("notifications : %w", err))
	}
	s.alertes.DefinirRecepteur(s.aiguille)

	// Registre des motifs de logs (SPEC § 52) : toutes les lignes reçues.
	if s.motifs, err = motifs.OuvrirRegistre(ctx, s.db, journal.With("composant", "motifs"), time.Now()); err != nil {
		return echec(fmt.Errorf("registre des motifs : %w", err))
	}
	// Flux bloqués par les pare-feu (SPEC § 135) : base à part (flux.db),
	// hors sauvegardes (compteurs seulement, refaits au fil de l'eau).
	s.geo = &geoip.Base{Dossier: s.chemin("geoip"), Journal: journal.With("composant", "geoip")}
	var magasins [2]*fluxbloques.Magasin
	for i, nom := range []string{"flux.db", "flux-sortants.db"} {
		if s.dbFlux[i], err = base.Ouvrir(s.chemin(nom)); err != nil {
			return echec(fmt.Errorf("base des flux bloqués (%s) : %w", nom, err))
		}
		ouverts = append(ouverts, s.dbFlux[i].Close)
		if magasins[i], err = fluxbloques.Nouveau(ctx, fluxbloques.Options{DB: s.dbFlux[i], Pays: s.geo.Pays,
			Journal: journal.With("composant", "flux", "base", nom)}); err != nil {
			return echec(fmt.Errorf("flux bloqués : %w", err))
		}
	}
	s.flux = fluxbloques.NouveauFlux(magasins[0], magasins[1])
	// Plafonds d'après la taille du parc prévue (SPEC § 141).
	dims := cfg.Dimensions()
	series := ingestion.LimitesSeriesDefaut()
	series.Total = dims.Series
	s.ing = ingestion.Nouvelle(ingestion.Options{DB: s.db, Metriques: s.met, Logs: s.jx, Journal: journal,
		Series: series, MaxSimultanees: dims.Places, MaxLegeres: dims.Legeres, MaxTelechargements: dims.Telechargements,
		RequetesIP: dims.RequetesIP,
		ApresLignes: func(ls []stockage.Ligne) {
			for _, l := range ls {
				s.motifs.Ajouter(l.Msg, l.T/1000)
			}
		},
		Flux: s.flux.Lignes})
	hubDirect := direct.Nouveau() // direct des pages des machines (SPEC § 97)
	s.direct = hubDirect
	s.flotte = flotte.Nouvelle(flotte.Options{DB: s.db, Ingestion: s.ing, DossierVersions: s.chemin("versions"), Journal: journal, Cle: cle,
		Direct: hubDirect, ReveilAlertes: s.alertes.Reveiller})
	s.planif = &sauvegarde.Planificateur{Cle: cle, Version: version.Version, Journal: journal.With("composant", "sauvegarde"),
		Sources: sauvegarde.Sources{DB: s.db, Dossier: cfg.Donnees, FichierDemarrage: cfg.Fichier,
			Metriques: s.met, Logs: segmentsLogs{s.jx}}}
	if s.console, err = console.Nouvelle(ctx, console.Options{GeoIP: s.geo, DB: s.db, Cle: cle, Journal: journal.With("composant", "console"),
		Version: version.Complete(), Debut: s.debut, Donnees: cfg.Donnees, Alertes: s.alertes, Aiguilleur: s.aiguille,
		Postier: s.postier, Metriques: s.met, Logs: s.jx, Ingestion: s.ing, Niveau: &NiveauJournal, Motifs: s.motifs, Direct: hubDirect,
		MajDemande: cfg.MajDemande, MajEtat: cfg.MajEtat, Flux: s.flux}); err != nil {
		return echec(fmt.Errorf("interface web : %w", err))
	}
	// Certificats du syslog des collecteurs (SPEC § 23.14) : démarche ACME
	// faite par le serveur, nom lu dans la configuration de chaque collecteur.
	gestCerts := &certsyslog.Gestionnaire{DB: s.db, Journal: journal.With("composant", "certificats"),
		Emetteur: &certsyslog.ACME{DB: s.db, Cle: cle},
		NomDe: func(ctx context.Context, machine int64) (string, error) {
			c, err := flotte.ConfigEffective(ctx, s.db, machine)
			if err != nil || c.Collecteur == nil || c.Collecteur.Syslog == nil {
				return "", err
			}
			return c.Collecteur.Syslog.NomCertificat, nil
		},
		Apres: flotte.SignalerConfig}
	fond, arret := context.WithCancel(context.Background())
	s.arretFond, s.finFond = arret, make(chan struct{})
	go func() {
		defer close(s.finFond)
		taches := []func(context.Context){s.flotte.Boucle, s.planif.Boucle, s.alertes.Boucle, s.aiguille.Boucle,
			s.postier.Boucle, (&interne{s: s}).boucle, s.console.Boucle,
			(&sondeur{s: s}).boucle, s.motifs.Boucle,
			(&changements.Detecteur{DB: s.db, Lecteur: alertes.NouveauLecteur(s.met.Requetes(), journal.With("composant", "changements")),
				Journal: journal.With("composant", "changements")}).Boucle,
			func(ctx context.Context) { s.geo.Boucle(ctx, s.console.GeoIPActive) }, gestCerts.Tourner, s.flux.Boucle}
		fini := make(chan struct{}, len(taches))
		for _, f := range taches {
			go func() { f(fond); fini <- struct{}{} }()
		}
		s.ing.Boucle(fond)
		for range taches {
			<-fini
		}
	}()
	ouverts = append(ouverts, func() error { s.arreterFond(); return nil })

	s.web = web.Nouveau(web.Options{
		EcouteAgents:    cfg.EcouteAgents,
		EcouteInterface: cfg.EcouteInterface,
		TLSCertificat:   cfg.TLSCertificat,
		TLSCle:          cfg.TLSCle,
		Proxys:          cfg.Proxys,
		Journal:         journal,
		Agents:          s.routesAgents(),
		Interface:       s.routesInterface(),
	})
	if err := s.web.Demarrer(); err != nil {
		return echec(err)
	}
	ouverts = append(ouverts, func() error { s.web.Arreter(time.Second); return nil })
	if err := s.ouvrirAdmin(); err != nil {
		return echec(fmt.Errorf("socket d'administration : %w", err))
	}
	journal.Info("mirador-server démarré", "version", version.Complete(), "schema", v,
		"agents", cfg.EcouteAgents, "interface", cfg.EcouteInterface)
	noterPresence(cfg.MajDemande, journal)
	return s, nil
}

// noterPresence : « je tourne en version X » pour mirador-maj-serveur (SPEC
// § 102), une fois prêt à servir. Dossier absent (service non installé) :
// rien.
func noterPresence(dossier string, journal *slog.Logger) {
	if dossier == "" {
		return
	}
	if info, err := os.Stat(dossier); err != nil || !info.IsDir() {
		return
	}
	p := majserveur.Presence{Version: version.Version, DemarreLe: time.Now().Unix()}
	if err := majserveur.EcrireJSON(dossier, majserveur.NomPresence, p, 0o644); err != nil { // #nosec G306 -- lu par le service du VPS (root)
		journal.Warn("présence pour la mise à jour du serveur non écrite", "erreur", err)
	}
}

// Arreter ferme proprement, dans l'ordre : les écoutes (plus de nouvelles
// données), les tâches de fond, le stockage (bloc de logs en cours écrit),
// puis la base.
func (s *Serveur) Arreter() {
	s.web.Arreter(10 * time.Second)
	s.fermerAdmin()
	s.arreterFond()
	if err := s.jx.Fermer(); err != nil {
		s.journal.Error("fermeture du stockage des logs", "erreur", err)
	}
	if err := s.met.Fermer(); err != nil {
		s.journal.Error("fermeture du stockage des métriques", "erreur", err)
	}
	if err := s.db.Close(); err != nil {
		s.journal.Error("fermeture de la base", "erreur", err)
	}
	for _, db := range s.dbFlux {
		if err := db.Close(); err != nil {
			s.journal.Error("fermeture d'une base des flux bloqués", "erreur", err)
		}
	}
}

// arreterFond arrête la boucle de l'ingestion (dernière écriture de vu_le).
func (s *Serveur) arreterFond() {
	s.arretFond()
	<-s.finFond
}

// routesAgents : API des agents (santé, métriques, logs, enrôlement,
// configuration, battement, mises à jour). Tout autre chemin : 501.
func (s *Serveur) routesAgents() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /sante", s.sante)
	s.ing.Routes(m)
	s.flotte.Routes(m)
	m.HandleFunc("/api/agent/v1/", func(w http.ResponseWriter, _ *http.Request) {
		repondreJSON(w, http.StatusNotImplemented, map[string]string{"erreur": "pas encore disponible"})
	})
	return limiterCorps(m, 32<<20)
}

// routesInterface : interface web (étape 5) ; /sante reste hors session
// (healthcheck Docker et nginx).
func (s *Serveur) routesInterface() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /sante", s.sante)
	m.Handle("/", s.console)
	normal := limiterCorps(m, 1<<20)
	publication := limiterCorps(m, console.TailleMaxPublication+(1<<20))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Temps de calcul de l'interface (charge, retour du 26/11) : pas les
		// flux en direct (EventSource), qui attendent sans calculer.
		if !strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
			defer charge.Noter(charge.Interface, time.Now())
		}
		// Seule la publication d'une version de l'agent dépasse 1 Mio.
		if r.URL.Path == "/mises-a-jour/publier" {
			publication.ServeHTTP(w, r)
			return
		}
		normal.ServeHTTP(w, r)
	})
}

// sante : utilisé par Docker (healthcheck) et nginx. Ne donne aucune
// information sensible (ni version, ni chemins).
func (s *Serveur) sante(w http.ResponseWriter, r *http.Request) {
	ctx, annuler := context.WithTimeout(r.Context(), 2*time.Second)
	defer annuler()
	if err := s.db.PingContext(ctx); err != nil {
		repondreJSON(w, http.StatusServiceUnavailable, map[string]string{"etat": "base indisponible"})
		return
	}
	repondreJSON(w, http.StatusOK, map[string]string{"etat": "ok"})
}

// limiterCorps borne la taille des requêtes (protection mémoire).
func limiterCorps(h http.Handler, max int64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, max)
		h.ServeHTTP(w, r)
	})
}

func repondreJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// segmentsLogs adapte le moteur de logs au paquet sauvegarde.
type segmentsLogs struct{ m *journaux.Moteur }

func (a segmentsLogs) Segments() ([]sauvegarde.Segment, error) {
	segs, err := a.m.Segments()
	if err != nil {
		return nil, err
	}
	out := make([]sauvegarde.Segment, len(segs))
	for i, x := range segs {
		out[i] = sauvegarde.Segment{Nom: x.Nom, Chemin: x.Chemin, Taille: x.Taille}
	}
	return out, nil
}
