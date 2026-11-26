// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"errors"
	"maps"
	"time"

	"mirador-stack/internal/motif"
	"mirador-stack/internal/server/stockage/journaux"
)

// Page « Logs » (SPEC § 18.5). Le contenu des logs vient des machines : il
// est TOUJOURS affiché comme du texte (html/template échappe tout, la CSP
// interdit les scripts en ligne). Recherche bornée par le moteur (31 j,
// 10 000 lignes, 10 s) et ici : 2 recherches en même temps au plus.

func (c *Console) routesLogs() {
	c.mux.HandleFunc("GET /logs", c.lecture(c.pageLogs))
	c.mux.HandleFunc("GET /logs/direct", c.lecture(c.logsDirect))
	c.mux.HandleFunc("GET /logs/flux", c.lecture(c.logsFlux)) // logs en direct poussés (SPEC § 99)
	c.mux.HandleFunc("GET /logs/export", c.lecture(c.logsExport))
	c.mux.HandleFunc("POST /logs/recherches", c.lecture(c.postRechercheLogs))
}

// champsLogs : filtres exacts proposés (étiquettes des lignes).
var champsLogs = []struct{ Nom, Titre string }{
	{"host", "Machine"}, {"job", "Source"}, {"level", "Niveau"}, {"unit", "Unité"}, {"container", "Conteneur"},
	{"site", "Site"}, {"env", "Type"}, {"groupe", "Groupe"},
}

var reValeurChamp = motif.Doit(`^[\p{L}\p{N} ._:/@+-]+$`, 1, 200)

// champLog : un filtre ; Choix = valeurs proposées dans la liste (la
// valeur actuelle y est toujours).
type champLog struct {
	Nom, Titre, Valeur string
	Choix              []string
}

// niveauxSyslog : niveaux standard, toujours proposés.
var niveauxSyslog = []string{"emerg", "alert", "crit", "err", "warning", "notice", "info", "debug"}

// choixLogs : valeurs proposées pour chaque filtre (SPEC § 18.5, demande du
// 14/11) : machines, sites, types et groupes de la base ; source, niveau,
// unité et conteneur vus dans les lignes reçues depuis le démarrage.
func (c *Console) choixLogs(ctx context.Context, machines []string) map[string][]string {
	l := c.lireListes(ctx)
	out := map[string][]string{"host": machines, "site": l.Sites, "env": l.Types, "groupe": l.Groupes}
	if c.opt.Logs != nil {
		for _, ch := range journaux.ChampsListes {
			out[ch] = c.opt.Logs.ValeursVues(ch)
		}
	}
	vu := map[string]bool{}
	for _, v := range out["level"] {
		vu[v] = true
	}
	for _, n := range niveauxSyslog {
		if !vu[n] {
			out["level"] = append(out["level"], n)
		}
	}
	return out
}

// avecValeur : la liste, plus la valeur actuelle si elle n'y est pas.
func avecValeur(choix []string, v string) []string {
	if v == "" {
		return choix
	}
	for _, c := range choix {
		if c == v {
			return choix
		}
	}
	return append([]string{v}, choix...)
}

var periodesLogs = []string{"15m", "1h", "6h", "24h", "7d", "30d"}

// chercherLogs : 2 recherches à la fois au plus (chacune peut lire beaucoup
// de blocs) ; une troisième attend jusqu'à 5 s puis renonce.
func (c *Console) chercherLogs(ctx context.Context, r journaux.Requete) (journaux.Resultat, error) {
	select {
	case c.recherches <- struct{}{}:
	case <-time.After(5 * time.Second):
		return journaux.Resultat{}, errors.New("serveur occupé par d'autres recherches : réessayer")
	case <-ctx.Done():
		return journaux.Resultat{}, ctx.Err()
	}
	defer func() { <-c.recherches }()
	// Machines renommées : l'historique de l'ancien nom est rattaché au
	// nouveau (filtre élargi, nom affiché remplacé ; SPEC § 18.23).
	anciens := c.anciensNoms()
	if h := r.Champs["host"]; h != "" && len(anciens) > 0 {
		for vieux, actuel := range anciens {
			if actuel == h {
				if r.Variantes == nil {
					r.Variantes = map[string][]string{}
				}
				r.Variantes["host"] = append(r.Variantes["host"], vieux)
			}
		}
	}
	res, err := c.opt.Logs.Chercher(ctx, r, journaux.LimitesRechercheDefaut())
	if len(anciens) > 0 {
		for i := range res.Lignes {
			if actuel, ok := anciens[res.Lignes[i].Champs["host"]]; ok {
				ch := maps.Clone(res.Lignes[i].Champs)
				ch["host"] = actuel
				res.Lignes[i].Champs = ch
			}
		}
	}
	return res, err
}

// --- Flux entrants bloqués --------------------------------------------------------------
//
// Calculés depuis les lignes du noyau gardées (« … IN=eth0 OUT= … SRC=… »),
// avec les filtres du tableau Grafana actuel (SPEC § 4.8) : sans interfaces
// virtuelles, sans diffusion ni multidiffusion, sans 0.0.0.0 ni fe80:.
// Carte par pays : base GéoIP (Réglages › Interface), toujours affichée
// quand le réglage est coché, avec la raison si elle est vide.
