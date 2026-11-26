// SPDX-License-Identifier: GPL-3.0-or-later

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"mirador-stack/internal/server/charge"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"mirador-stack/internal/server/alertes"
	"mirador-stack/internal/server/certsyslog"
	"mirador-stack/internal/server/console"
	"mirador-stack/internal/server/flotte"
	"mirador-stack/internal/server/modeles"
	"mirador-stack/internal/server/notif"
	"mirador-stack/internal/server/sauvegarde"
	"mirador-stack/internal/server/servicesmetier"
	"mirador-stack/internal/server/stockage"
	"mirador-stack/internal/server/taches"
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
	lecteur    *alertes.Lecteur // états des services (SPEC § 50)
	executions taches.Noteur    // historique des tâches (SPEC § 95)
}

// delaiServicesAttendus : sans donnée reçue d'une machine depuis ce temps,
// ses « services attendus » ne sont plus écrits. Plus court que le « pour »
// de la règle (15 min) : la série disparaît avant que l'alerte ne parte.
const delaiServicesAttendus = 3 * time.Minute

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
	defer charge.Noter(charge.Fond, time.Now())
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

	// VM et conteneurs Proxmox exclus de la règle VMSansSauvegarde (retour
	// du 25/11) : une série par invité exclu, que la règle écarte.
	if l, err := alertes.LireInvitesExclus(ctx, s.db); err == nil {
		for _, e := range l {
			ajout(alertes.MetriqueInviteExclue, 1, "instance", e.Machine, "id", e.ID)
		}
	}

	// Machines : décalage d'horloge, mise à jour en échec.
	if ms, err := flotte.ListerMachines(ctx, s.db); err == nil {
		s.vus.remplacer(ms)                                  // vu_le relu chaque minute (SPEC § 46)
		releves, _ := flotte.RelevesParCollecteur(ctx, s.db) // équipements (nom → collecteur)
		for _, m := range ms {
			if m.Statut != "approuvee" {
				continue
			}
			// Dernières métriques reçues (règle AgentMuet au-delà de 24 h,
			// SPEC § 45). Avant toute réception depuis le démarrage : vu_le
			// (dernière requête, enregistrée en base). Pas pour les
			// équipements relevés par un collecteur : ils n'ont pas
			// node_uname_info, AgentMuet ne les concerne pas.
			if _, equipement := releves[m.Nom]; !equipement {
				recue, ok := s.ing.DerniereReception(m.ID)
				if !ok {
					recue = m.VuLe
				}
				if recue > 0 {
					ajout(alertes.MetriqueDerniereReception, float64(recue), "instance", m.Nom)
				}
			}
			if d, ok := s.ing.Decalage(m.ID); ok {
				ajout("mirador_machine_decalage_horloge_secondes", d.Seconds(), "instance", m.Nom)
			}
			if m.MajEchec != "" {
				ajout("mirador_machine_maj_echec", 1, "instance", m.Nom, "version", m.MajEchec)
			}
			// Certificat du syslog d'un collecteur (SPEC § 23.14) : échéance et
			// dernier essai en échec (règles CertificatSyslog…).
			if m.Type == "collecteur" {
				if e, err := certsyslog.LireEtat(ctx, s.db, m.ID); err == nil && e.CSR {
					if e.Fin > 0 {
						ajout("mirador_collecteur_certificat_fin_timestamp_seconds", float64(e.Fin), "instance", m.Nom)
					}
					echec := 0.0
					if e.Erreur != "" {
						echec = 1
					}
					ajout("mirador_collecteur_certificat_echec", echec, "instance", m.Nom)
				}
			}
			// Modèles de surveillance (SPEC § 24.2) : seuil effectif de chaque
			// paramètre (les règles des modèles s'y comparent) et règles
			// importées remplacées pour cette machine.
			if effs, err := modeles.Effectifs(ctx, s.db, m.ID, m.Groupe); err == nil {
				for _, e := range effs {
					ajout("mirador_machine_modele", 1, "instance", m.Nom, "modele", e.Modele.Nom)
					for p, v := range e.Parametres {
						ajout("mirador_modele_parametre", v, "instance", m.Nom, "modele", e.Modele.Nom, "parametre", p)
					}
					for _, r := range e.Modele.Regles {
						if r.Remplace != "" {
							ajout("mirador_machine_regle_remplacee", 1, "instance", m.Nom, "regle", r.Remplace)
						}
					}
					// Services attendus : seulement si la machine répond. Une
					// machine muette n'envoie plus l'état de ses services ;
					// sans cela, la règle ServiceDuModeleAbsent les croyait
					// tous désinstallés et ajoutait une alerte par service à
					// côté d'AgentMuet (retour du 16/11, SPEC § 34.6).
					if t.Sub(time.Unix(m.VuLe, 0)) > delaiServicesAttendus {
						continue
					}
					for _, svc := range e.Services {
						ajout("mirador_modele_service", 1, "instance", m.Nom, "modele", e.Modele.Nom, "name", svc)
					}
				}
			}
		}
	}

	// Services (SPEC § 50) : état de chaque service (0 OK, 1 dégradé,
	// 2 en panne), lu par les règles ApplicationEnPanne et ApplicationDegradee (§ 65).
	if svcs, err := servicesmetier.Lire(ctx, s.db); err == nil && len(svcs) > 0 && s.alertes != nil {
		if i.lecteur == nil {
			i.lecteur = alertes.NouveauLecteur(s.met.Requetes(), s.journal.With("composant", "services"))
		}
		r, _ := notif.LireReglages(ctx, s.db)
		for _, e := range servicesmetier.Calculer(ctx, i.lecteur, s.alertes.EnCours(), r.ReglesPanne, svcs, t) {
			ajout("mirador_service_etat", float64(e.Etat), "service", e.Nom)
		}
	}

	// Tâches planifiées suivies (SPEC § 80) : échéance de chacune (règle
	// TacheEnRetard) et, pour les tâches « ping », leur dernier signal
	// (règle TacheEnEchec).
	// Historique (SPEC § 95) : chaque nouvelle exécution est notée, suivie ou
	// non (l'historique est déjà là le jour où on la suit) ; les anomalies
	// (durée, taille) des tâches suivies sont écrites pour la règle TacheAnormale.
	if ts, err := taches.Lister(ctx, s.db); err == nil {
		if i.lecteur == nil {
			i.lecteur = alertes.NouveauLecteur(s.met.Requetes(), s.journal.With("composant", "services"))
		}
		det := taches.Detectees(func(expr string) ([]alertes.Echantillon, error) { return i.lecteur.Instant(ctx, expr, t) })
		ts = taches.AvecReleves(ts, det)
		for k, d := range det {
			if err := i.executions.Noter(ctx, s.db, k, d.Releve); err != nil {
				s.journal.Warn("historique des tâches", "err", err)
				break
			}
		}
		cles := make([]string, 0, len(ts))
		for _, x := range ts {
			if x.Source == taches.SourcePing {
				_ = i.executions.Noter(ctx, s.db, x.Cle, x.Releve)
			}
			cles = append(cles, x.Cle)
		}
		anomalies := map[string][]taches.Anomalie{}
		if hist, err := taches.Historiques(ctx, s.db, cles, 30); err == nil {
			for _, x := range ts {
				if a := taches.Anomalies(x.Releve, hist[x.Cle], t); len(a) > 0 {
					anomalies[x.Cle] = a
				}
			}
		}
		for _, se := range taches.Series(ts, anomalies) {
			ajout(se.Nom, se.Valeur, se.Etiquettes...)
		}
	}

	// Étiquettes libres (SPEC § 59) : une série par étiquette, pour les
	// règles (« and on (instance) mirador_element_etiquette{cle="critique", valeur="oui"} »).
	var brutEtq string
	if s.db.QueryRowContext(ctx, `SELECT valeur FROM reglages WHERE cle = 'etiquettes_elements'`).Scan(&brutEtq) == nil {
		var etq map[string]map[string]string
		if json.Unmarshal([]byte(brutEtq), &etq) == nil {
			for el, kv := range etq {
				for k, v := range kv {
					if nom, ok := strings.CutPrefix(el, "m:"); ok {
						ajout("mirador_element_etiquette", 1, "instance", nom, "cle", k, "valeur", v)
					} else {
						ajout("mirador_element_etiquette", 1, "element", el, "cle", k, "valeur", v)
					}
				}
			}
		}
	}

	// Motifs de logs gardés par le registre (SPEC § 52, 5 000 au plus).
	if s.motifs != nil {
		ajout("mirador_logs_motifs", float64(s.motifs.Nombre()))
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
	rssTotal, anon := memoireProcessus()
	ajout("mirador_processus_rss_octets", float64(rssTotal))
	// Mémoire propre du programme (retour du 26/11 : la RSS compte aussi le
	// cache des fichiers de la base de mesures, que le noyau reprend).
	ajout("mirador_processus_anon_octets", float64(anon))
	// Temps de calcul (charge) : CPU réel, ramasse-miettes, temps par partie.
	ajout("mirador_serveur_cpu_secondes_total", charge.CPUProcessus().Seconds())
	ajout("mirador_serveur_cpu_gc_secondes_total", charge.CPUGC().Seconds())
	for p, nom := range charge.Noms {
		ajout("mirador_serveur_temps_secondes_total", charge.Cumul(charge.Partie(p)).Seconds(), "partie", nom)
	}
	if s.alertes != nil {
		ajout("mirador_serveur_temps_secondes_total", s.alertes.DureeCumulee().Seconds(), "partie", "alertes")
	}
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
		donnees := max(time.Duration(cons.Brut), time.Duration(cons.Agrege), time.Duration(cons.Logs))
		if n, err := purgerTables(ctx, s.db, t, hist, donnees); err != nil {
			s.journal.Error("purge des tables anciennes", "erreur", err)
		} else if n > 0 {
			s.journal.Info("tables anciennes purgées", "lignes", n)
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
	r, _ := memoireProcessus()
	return r
}

// memoireProcessus : RSS totale et part anonyme (mémoire propre, sans le
// cache des fichiers projetés), en octets.
func memoireProcessus() (total, anon int64) {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0, 0
	}
	lire := func(v string) int64 {
		n, _ := strconv.ParseInt(strings.TrimSuffix(strings.TrimSpace(v), " kB"), 10, 64)
		return n * 1024
	}
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, "VmRSS:"); ok {
			total = lire(v)
		} else if v, ok := strings.CutPrefix(l, "RssAnon:"); ok {
			anon = lire(v)
		}
	}
	return total, anon
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
