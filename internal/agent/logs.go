// SPDX-License-Identifier: GPL-3.0-or-later

package agent

import (
	"context"
	"log/slog"
	"net/url"
	"sync"
	"time"

	"mirador-stack/internal/agent/config"
	"mirador-stack/internal/agent/dockerlogs"
	"mirador-stack/internal/agent/envoi"
	"mirador-stack/internal/agent/journal"
	"mirador-stack/internal/agent/logs"
	mm "mirador-stack/internal/agent/metrics"
)

// chaineLogs relie : lecture du journal → traitement → lot → file → envoi.
type chaineLogs struct {
	lecteur    *journal.Lecteur
	suiveur    *dockerlogs.Suiveur // nil sans Docker
	lotDocker  *logs.Lot
	traitement *logs.Traitement
	lot        *logs.Lot
	file       *envoi.File
	exp        *envoi.Expediteur
	journal    *slog.Logger
}

// Envoi d'un lot : toutes les 5 s, ou dès qu'il atteint 512 Kio (comme Alloy).
const (
	attenteLotLogs = 5 * time.Second
	tailleLotLogs  = 512 << 10
)

// nouvelleChaineLogs prépare la chaîne, ou renvoie nil si les logs sont
// désactivés (pas d'adresse, ou MIRADOR_JOURNAL=non).
func nouvelleChaineLogs(cfg config.Agent, token string, j *slog.Logger) *chaineLogs {
	journalActif := cfg.Journal
	dockerActif := cfg.LogsDocker && cfg.Docker != ""
	if cfg.URLLogs == "" || (!journalActif && !dockerActif) {
		return nil
	}
	masque, err := logs.NewMasqueur(cfg.Masquage, cfg.MasquagePerso)
	if err != nil { // impossible : déjà validé par la configuration
		j.Error("masquage invalide, logs désactivés", "erreur", err)
		return nil
	}
	c := &chaineLogs{
		lot:     logs.NewLot(cfg.Nom, "journal", cfg.Site, cfg.Env, tailleLotLogs),
		file:    envoi.NewFile(cfg.TamponLogs),
		journal: j,
	}
	if dockerActif {
		c.lotDocker = logs.NewLot(cfg.Nom, "docker", cfg.Site, cfg.Env, tailleLotLogs)
	}
	c.traitement = logs.NewTraitement(logs.Options{
		Bruit: cfg.LogsBruit, Debit: cfg.LogsDebit, Rafale: cfg.LogsRafale, Masque: masque,
	}, c.lot, c.lotDocker)
	if journalActif {
		c.lecteur = journal.NewLecteur(journal.Options{FichierCurseur: cfg.FichierCurseur, Journal: j})
	}
	if dockerActif {
		s, err := dockerlogs.NewSuiveur(cfg.Docker, c.traitement.TraiterDocker, j)
		if err != nil {
			j.Error("logs Docker désactivés", "erreur", err)
		} else {
			c.suiveur = s
		}
	}
	c.exp = envoi.NewExpediteur(c.file, envoi.Options{
		URL:   urlJsonline(cfg.URLLogs),
		Token: token,
		Entetes: map[string]string{
			"Content-Type":     "application/stream+json",
			"Content-Encoding": "gzip",
		},
		Journal: j,
	})
	return c
}

// urlJsonline ajoute les paramètres de VictoriaLogs : quels champs forment le
// « flux » (mêmes étiquettes qu'Alloy), et où trouver l'heure et le message.
func urlJsonline(brute string) string {
	u, err := url.Parse(brute)
	if err != nil {
		return brute
	}
	q := u.Query()
	if q.Get("_stream_fields") == "" {
		// Les champs absents d'une ligne sont ignorés : la même liste sert au
		// journal (unit, level, ident) et à Docker (container, stream).
		q.Set("_stream_fields", "host,job,site,env,unit,level,ident,container,stream")
	}
	if q.Get("_time_field") == "" {
		q.Set("_time_field", "_time")
	}
	if q.Get("_msg_field") == "" {
		q.Set("_msg_field", "_msg")
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// tourner lance la lecture, la mise en lot et l'envoi jusqu'à l'arrêt de
// ctxLecture ; l'envoi continue jusqu'à l'arrêt de ctxEnvoi.
func (c *chaineLogs) tourner(ctxLecture, ctxEnvoi context.Context) {
	go c.exp.Tourner(ctxEnvoi)
	// Les lecteurs sont attendus avant de rendre la main : leur dernier
	// curseur est écrit avant l'arrêt (sinon une écriture tardive pouvait
	// se perdre, ou viser un dossier déjà effacé).
	var lecteurs sync.WaitGroup
	if c.lecteur != nil {
		lecteurs.Add(1)
		go func() { defer lecteurs.Done(); c.lecteur.Tourner(ctxLecture, c.traitement.Traiter) }()
	}
	if c.suiveur != nil {
		lecteurs.Add(1)
		go func() { defer lecteurs.Done(); c.suiveur.Tourner(ctxLecture) }()
	}

	t := time.NewTicker(time.Second)
	defer t.Stop()
	dernier := time.Now()
	for {
		select {
		case <-ctxLecture.Done():
			lecteurs.Wait() // plus aucune ligne ne peut arriver
			c.vider()       // dernier lot avant l'arrêt
			return
		case <-t.C:
		}
		if c.lot.Plein() || (c.lotDocker != nil && c.lotDocker.Plein()) || time.Since(dernier) >= attenteLotLogs {
			c.vider()
			dernier = time.Now()
		}
	}
}

func (c *chaineLogs) vider() {
	for _, lot := range []*logs.Lot{c.lot, c.lotDocker} {
		if lot == nil {
			continue
		}
		donnees, n := lot.Vider()
		if donnees != nil && !c.file.Ajouter(donnees) {
			c.journal.Warn("lot de logs trop gros pour le tampon, abandonné", "lignes", n)
		}
	}
}

// metriques ajoute les compteurs des logs au lot de métriques.
func (c *chaineLogs) metriques(b *mm.Batch) {
	c.traitement.Metriques(b)
	if c.lecteur != nil {
		b.Add("mirador_logs_journal_lines_read_total", float64(c.lecteur.Lignes.Load()))
		b.Add("mirador_logs_journal_unreadable_total", float64(c.lecteur.Illisibles.Load()))
		b.Add("mirador_logs_journal_restarts_total", float64(c.lecteur.Relances.Load()))
	}
	if c.suiveur != nil {
		b.Add("mirador_logs_docker_streams", float64(c.suiveur.Suivis()))
	}
	lots, octets, jetes := c.file.Etat()
	b.Add("mirador_logs_queue_batches", float64(lots))
	b.Add("mirador_logs_queue_bytes", float64(octets))
	b.Add("mirador_logs_queue_dropped_total", float64(jetes))
	b.Add("mirador_logs_send_total", float64(c.exp.Envoyes.Load()), mm.L("result", "ok"))
	b.Add("mirador_logs_send_total", float64(c.exp.Echecs.Load()), mm.L("result", "error"))
	b.Add("mirador_logs_send_total", float64(c.exp.Rejetes.Load()), mm.L("result", "rejected"))
	b.Add("mirador_logs_sent_bytes_total", float64(c.exp.OctetsEnvoy.Load()))
}
