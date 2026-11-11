// SPDX-License-Identifier: GPL-3.0-or-later

package server

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"mirador-stack/internal/server/alertes"
	"mirador-stack/internal/server/console"
	"mirador-stack/internal/server/flotte"
	"mirador-stack/internal/server/notif"
	"mirador-stack/internal/server/sauvegarde"
	"mirador-stack/internal/server/stockage"
)

// Métriques internes (supervision de la supervision, SPEC.md § 17.3).
//
// Chaque minute, le serveur écrit quelques séries sur lui-même dans sa propre
// base, sous instance="mirador-server", job="mirador". Les règles mirador
// (alertes.ReglesMirador) les lisent comme n'importe quelle métrique, et
// l'interface pourra les afficher. Coût : quelques dizaines de séries.

// InstanceServeur : étiquette instance des métriques internes.
const InstanceServeur = "mirador-server"

// Historiques de la base (alertes résolues, envois, audit) : durées réglées
// dans « Conservation des données » (SPEC § 18.16), purge une fois par jour.

type interne struct {
	s          *Serveur
	cumulAvant time.Duration
	compareLe  time.Time
	erreurVm   string
	purgeeLe   time.Time
	mesureeLe  time.Time
}

func (i *interne) boucle(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		i.tour(ctx, time.Now())
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// tour écrit les métriques internes de l'instant t.
func (i *interne) tour(ctx context.Context, t time.Time) {
	s := i.s
	ms := t.UnixMilli()
	var series []stockage.Serie
	ajout := func(nom string, v float64, etq ...string) {
		ls := []stockage.Label{{Nom: "__name__", Valeur: nom}, {Nom: "instance", Valeur: InstanceServeur}, {Nom: "job", Valeur: "mirador"}}
		for k := 0; k+1 < len(etq); k += 2 {
			if etq[k] == "instance" { // série qui concerne une machine
				ls[1].Valeur = etq[k+1]
				continue
			}
			ls = append(ls, stockage.Label{Nom: etq[k], Valeur: etq[k+1]})
		}
		serie := stockage.Serie{Points: []stockage.Point{{T: ms, V: v}}}
		for _, l := range ls {
			serie.Remplacer(l.Nom, l.Valeur)
		}
		series = append(series, serie)
	}

	// Sauvegardes : dernière réussite (à défaut, le démarrage du serveur :
	// l'alerte part si aucune sauvegarde n'a réussi 26 h après) et dernier échec.
	if ok, trouve, err := sauvegarde.DerniereReussie(ctx, s.db); err == nil {
		quand := s.debut
		if trouve {
			quand = time.Unix(ok.Fin, 0)
		}
		ajout("mirador_sauvegarde_derniere_reussie_timestamp_seconds", float64(quand.Unix()))
	}
	if h, err := sauvegarde.Historique(ctx, s.db, 5); err == nil {
		echec := 0.0
		for _, e := range h { // la plus récente terminée
			if e.Statut == "ok" {
				break
			}
			if e.Statut == "echec" {
				echec = 1
				break
			}
		}
		ajout("mirador_sauvegarde_derniere_en_echec", echec)
	}

	// Machines : décalage d'horloge, mise à jour en échec.
	if ms, err := flotte.ListerMachines(ctx, s.db); err == nil {
		for _, m := range ms {
			if m.Statut != "approuvee" {
				continue
			}
			if d, ok := s.ing.Decalage(m.ID); ok {
				ajout("mirador_machine_decalage_horloge_secondes", d.Seconds(), "instance", m.Nom)
			}
			if m.MajEchec != "" {
				ajout("mirador_machine_maj_echec", 1, "instance", m.Nom, "version", m.MajEchec)
			}
		}
	}

	// Volume des logs : lignes reçues par hôte (compteur depuis le démarrage).
	for h, n := range s.ing.LignesParHote() {
		if h != "" {
			ajout("mirador_logs_recues_total", float64(n), "instance", h)
		}
	}

	// Disque des données, taille des bases.
	var st syscall.Statfs_t
	if err := syscall.Statfs(s.cfg.Donnees, &st); err == nil {
		ajout("mirador_disque_libre_octets", float64(st.Bavail)*float64(st.Bsize))
		ajout("mirador_disque_total_octets", float64(st.Blocks)*float64(st.Bsize))
	}
	if fi, err := os.Stat(s.chemin("mirador.db")); err == nil {
		ajout("mirador_base_octets", float64(fi.Size()))
	}
	if t.Sub(i.mesureeLe) >= 10*time.Minute { // parcours des dossiers : rarement
		i.mesureeLe = t
		brut, agrege := s.met.Taille()
		ajout("mirador_metriques_octets", float64(brut), "base", "brut")
		ajout("mirador_metriques_octets", float64(agrege), "base", "agrege")
	}

	// Processus et moteur d'alertes.
	ajout("mirador_processus_rss_octets", float64(rss()))
	ajout("mirador_points_recus_total", float64(s.met.Points.Load()))
	ajout("mirador_points_rejetes_total", float64(s.met.Rejetes.Load()))
	if s.alertes != nil {
		cumul := s.alertes.DureeCumulee()
		ajout("mirador_alertes_duree_cycle_secondes", (cumul - i.cumulAvant).Seconds())
		i.cumulAvant = cumul
		par := map[string]int{alertes.Critique: 0, alertes.Avertissement: 0, alertes.Info: 0}
		for _, a := range s.alertes.EnCours() {
			if a.Etat == alertes.Active {
				par[a.Gravite]++
			}
		}
		for g, n := range par {
			ajout("mirador_alertes_actives", float64(n), "severity", g)
		}
		erreurs := 0
		for _, r := range s.alertes.Sante() {
			if r.Erreur != "" {
				erreurs++
			}
		}
		ajout("mirador_regles_en_erreur", float64(erreurs))
	}

	if s.postier != nil {
		canaux, hbOK, _ := s.postier.Etat()
		for _, c := range canaux {
			panne := 0.0
			if c.EnPanne {
				panne = 1
			}
			ajout("mirador_canal_en_panne", panne, "canal", c.Nom, "role", c.Role)
		}
		ajout("mirador_file_notifications", float64(s.postier.TailleFile(ctx)))
		if !hbOK.IsZero() {
			ajout("mirador_heartbeat_dernier_ok_timestamp_seconds", float64(hbOK.Unix()))
		}
	}

	if err := s.met.Ecrire(ctx, series); err != nil {
		s.journal.Error("métriques internes", "erreur", err)
	}

	if t.Sub(i.compareLe) >= 5*time.Minute {
		i.compareLe = t
		i.comparerVmalert(ctx, t)
	}

	if t.Sub(i.purgeeLe) >= 24*time.Hour {
		i.purgeeLe = t
		cons, err := console.LireConservation(ctx, s.db)
		if err != nil {
			s.journal.Error("conservation illisible : défauts pour la purge", "erreur", err)
		}
		hist := time.Duration(cons.Historique)
		if _, err := notif.PurgerEnvois(ctx, s.db, t.Add(-hist)); err != nil {
			s.journal.Error("purge de l'historique des envois", "erreur", err)
		}
		if n, err := alertes.PurgerHistorique(ctx, s.db, t.Add(-hist)); err != nil {
			s.journal.Error("purge de l'historique des alertes", "erreur", err)
		} else if n > 0 {
			s.journal.Info("historique des alertes purgé", "lignes", n)
		}
		if n, err := console.PurgerAudit(ctx, s.db, t.Add(-time.Duration(cons.Audit))); err != nil {
			s.journal.Error("purge du journal d'audit", "erreur", err)
		} else if n > 0 {
			s.journal.Info("journal d'audit purgé", "lignes", n, "conservation", cons.Audit.String())
			_ = console.Auditer(ctx, s.db, "tâche de fond", "audit_purge", "audit", "", fmt.Sprintf("%d lignes de plus de %s", n, cons.Audit), "", t)
		}
	}
}

// rss : mémoire résidente du processus (octets), lue dans /proc/self/status.
func rss() int64 {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, "VmRSS:"); ok {
			n, _ := strconv.ParseInt(strings.TrimSuffix(strings.TrimSpace(v), " kB"), 10, 64)
			return n * 1024
		}
	}
	return 0
}

// comparerVmalert : mode ombre, écarts avec vmalert (SPEC § 17.8).
func (i *interne) comparerVmalert(ctx context.Context, t time.Time) {
	s := i.s
	r, err := alertes.LireReglagesVmalert(ctx, s.db)
	if err != nil || !r.Actif {
		return
	}
	ecarts, err := s.ecartsVmalert(ctx, r)
	if err == nil {
		err = alertes.NoterEcarts(ctx, s.db, ecarts, t)
	}
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	if msg != i.erreurVm { // journal sans répétition
		if msg != "" {
			s.journal.Warn("comparaison avec vmalert", "erreur", msg)
		} else {
			s.journal.Info("comparaison avec vmalert rétablie")
		}
		i.erreurVm = msg
	}
}

// ecartsVmalert : comparaison immédiate (machines approuvées seulement).
func (s *Serveur) ecartsVmalert(ctx context.Context, r alertes.ReglagesVmalert) ([]alertes.Ecart, error) {
	vm, err := alertes.LireVmalert(ctx, r.URL)
	if err != nil {
		return nil, err
	}
	ms, err := flotte.ListerMachines(ctx, s.db)
	if err != nil {
		return nil, err
	}
	machines := map[string]bool{}
	for _, m := range ms {
		if m.Statut == "approuvee" {
			machines[m.Nom] = true
		}
	}
	return alertes.Comparer(r, vm, s.alertes.EnCours(), machines), nil
}
