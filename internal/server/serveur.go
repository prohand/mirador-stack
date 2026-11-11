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
	"net/http"
	"path/filepath"
	"time"

	"mirador-stack/internal/server/alertes"
	"mirador-stack/internal/server/base"
	"mirador-stack/internal/server/config"
	"mirador-stack/internal/server/console"
	"mirador-stack/internal/server/flotte"
	"mirador-stack/internal/server/geoip"
	"mirador-stack/internal/server/ingestion"
	"mirador-stack/internal/server/notif"
	"mirador-stack/internal/server/sauvegarde"
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

	if s.db, err = base.Ouvrir(s.chemin("mirador.db")); err != nil {
		return nil, fmt.Errorf("base : %w", err)
	}
	copie, err := base.Migrer(ctx, s.db, s.chemin("sauvegardes"))
	if err != nil {
		return nil, errors.Join(err, s.db.Close())
	}
	if copie != "" {
		journal.Info("base migrée ; copie d'avant migration gardée", "copie", copie)
	}
	v, _ := base.Version(ctx, s.db)

	// Stockage (rétentions par défaut ; réglables dans l'interface, étape 5).
	if s.met, err = metriques.Ouvrir(metriques.Options{Dossier: s.chemin("metriques"),
		Journal: journal.With("composant", "metriques")}); err != nil {
		return nil, errors.Join(fmt.Errorf("stockage des métriques : %w", err), s.db.Close())
	}
	if s.jx, err = journaux.Ouvrir(journaux.Options{Dossier: s.chemin("logs"),
		Journal: journal.With("composant", "logs")}); err != nil {
		return nil, errors.Join(fmt.Errorf("stockage des logs : %w", err), s.met.Fermer(), s.db.Close())
	}

	// Alertes : règles mirador posées une fois, puis moteur (règles relues
	// à chaud quand elles changent en base).
	if n, err := alertes.PoserDefauts(ctx, s.db, time.Now()); err != nil {
		return nil, errors.Join(fmt.Errorf("règles par défaut : %w", err), s.jx.Fermer(), s.met.Fermer(), s.db.Close())
	} else if n > 0 {
		journal.Info("règles d'alerte mirador ajoutées", "nombre", n)
	}
	if s.alertes, err = alertes.Nouveau(ctx, alertes.Options{DB: s.db, Donnees: s.met.Requetes(),
		Journal: journal.With("composant", "alertes")}); err != nil {
		return nil, errors.Join(fmt.Errorf("moteur d'alertes : %w", err), s.jx.Fermer(), s.met.Fermer(), s.db.Close())
	}
	if _, err := notif.PoserDefauts(ctx, s.db, time.Now()); err != nil {
		return nil, errors.Join(fmt.Errorf("inhibitions par défaut : %w", err), s.jx.Fermer(), s.met.Fermer(), s.db.Close())
	}
	if s.postier, err = notif.NouveauPostier(ctx, notif.OptionsPostier{DB: s.db, Cle: cle,
		Journal: journal.With("composant", "canaux"), DerniereEvaluationOK: s.alertes.DerniereEvaluationOK}); err != nil {
		return nil, errors.Join(fmt.Errorf("canaux de notification : %w", err), s.jx.Fermer(), s.met.Fermer(), s.db.Close())
	}
	if s.aiguille, err = notif.Nouveau(ctx, notif.Options{DB: s.db, EnCours: s.alertes.EnCours,
		Envoyeur: s.postier, Journal: journal.With("composant", "notifications")}); err != nil {
		return nil, errors.Join(fmt.Errorf("notifications : %w", err), s.jx.Fermer(), s.met.Fermer(), s.db.Close())
	}
	s.alertes.DefinirRecepteur(s.aiguille)

	s.ing = ingestion.Nouvelle(ingestion.Options{DB: s.db, Metriques: s.met, Logs: s.jx, Journal: journal})
	s.flotte = flotte.Nouvelle(flotte.Options{DB: s.db, Ingestion: s.ing, DossierVersions: s.chemin("versions"), Journal: journal})
	s.planif = &sauvegarde.Planificateur{Cle: cle, Version: version.Version, Journal: journal.With("composant", "sauvegarde"),
		Sources: sauvegarde.Sources{DB: s.db, Dossier: cfg.Donnees, FichierDemarrage: cfg.Fichier,
			Metriques: s.met, Logs: segmentsLogs{s.jx}}}
	s.geo = &geoip.Base{Dossier: s.chemin("geoip"), Journal: journal.With("composant", "geoip")}
	if s.console, err = console.Nouvelle(ctx, console.Options{GeoIP: s.geo, DB: s.db, Cle: cle, Journal: journal.With("composant", "console"),
		Version: version.Complete(), Debut: s.debut, Donnees: cfg.Donnees, Alertes: s.alertes, Aiguilleur: s.aiguille,
		Postier: s.postier, Metriques: s.met, Logs: s.jx, Ingestion: s.ing, Niveau: &NiveauJournal}); err != nil {
		return nil, errors.Join(fmt.Errorf("interface web : %w", err), s.jx.Fermer(), s.met.Fermer(), s.db.Close())
	}
	fond, arret := context.WithCancel(context.Background())
	s.arretFond, s.finFond = arret, make(chan struct{})
	go func() {
		defer close(s.finFond)
		taches := []func(context.Context){s.flotte.Boucle, s.planif.Boucle, s.alertes.Boucle, s.aiguille.Boucle,
			s.postier.Boucle, (&interne{s: s}).boucle, s.console.Boucle,
			func(ctx context.Context) { s.geo.Boucle(ctx, s.console.GeoIPActive) }}
		fini := make(chan struct{}, len(taches))
		for _, f := range taches {
			go func() { f(fond); fini <- struct{}{} }()
		}
		s.ing.Boucle(fond)
		for range taches {
			<-fini
		}
	}()

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
		s.arreterFond()
		return nil, errors.Join(err, s.jx.Fermer(), s.met.Fermer(), s.db.Close())
	}
	if err := s.ouvrirAdmin(); err != nil {
		s.web.Arreter(time.Second)
		s.arreterFond()
		return nil, errors.Join(fmt.Errorf("socket d'administration : %w", err), s.jx.Fermer(), s.met.Fermer(), s.db.Close())
	}
	journal.Info("mirador-server démarré", "version", version.Complete(), "schema", v,
		"agents", cfg.EcouteAgents, "interface", cfg.EcouteInterface)
	return s, nil
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
