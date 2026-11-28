// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/prometheus/model/labels"

	"mirador-stack/internal/server/alertes"
	"mirador-stack/internal/server/flotte"
	"mirador-stack/internal/server/notif"
)

func (c *Console) routesAlertes() {
	m := c.mux
	m.HandleFunc("GET /alertes", c.lecture(c.pageAlertes))
	m.HandleFunc("GET /fragments/alertes", c.lecture(c.fragmentAlertes))
	m.HandleFunc("GET /alertes/{id}", c.lecture(c.pageAlerte))
	m.HandleFunc("POST /alertes/groupe", c.admin(c.postAlertesGroupe)) // actions de groupe (SPEC § 141)
	m.HandleFunc("POST /alertes/{id}", c.admin(c.postAlerte))
	m.HandleFunc("GET /sourdine", c.lecture(c.pageSourdine))
	m.HandleFunc("POST /sourdine", c.admin(c.postSourdine))
	m.HandleFunc("GET /envois", c.lecture(c.pageEnvois))
	m.HandleFunc("GET /comparaison", c.lecture(c.pageComparaison))
	m.HandleFunc("GET /coupures", c.lecture(c.pageCoupures))
	m.HandleFunc("GET /disponibilite", c.lecture(c.pageDisponibilite))
	m.HandleFunc("GET /evenements", c.lecture(c.pageEvenements))
	m.HandleFunc("GET /incidents", c.lecture(c.pageIncidents))
	m.HandleFunc("GET /incidents/{id}", c.lecture(c.pageIncident))
	m.HandleFunc("POST /incidents/{id}", c.admin(c.postNoteIncident))
	m.HandleFunc("POST /disponibilite", c.admin(c.postDisponibilite))
	m.HandleFunc("GET /comparaison.csv", c.lecture(c.csvComparaison))
	c.routesRegles()
}

// --- Alertes en cours et historique -------------------------------------------------

type ligneHistorique struct {
	alertes.Alerte
	Machine string
}

type donneesAlertes struct {
	EnCours    []resumeAlerte
	Vue        donneesEnCours // bloc « en cours » (SPEC § 28)
	Frise      donneesFrise   // frise et règles bruyantes (SPEC § 28)
	Historique []ligneHistorique
	Filtre     alertes.Filtre
	Suivant    int64
	Requete    template.URL // voir donneesLogs.Requete
	FiltreSur  filtreSur
}

// filtreSur : page filtrée depuis une fiche (bandeau « Filtré sur … »).
type filtreSur struct{ Texte, ToutVoir string }

func (c *Console) pageAlertes(q *req) {
	v := q.r.URL.Query()
	f := alertes.Filtre{Regle: v.Get("regle"), Machine: v.Get("machine"), Gravite: v.Get("gravite"), Limite: 100,
		Sonde: v.Get("sonde"), Application: v.Get("application"), Conteneur: v.Get("conteneur")}
	f.Avant, _ = strconv.ParseInt(v.Get("avant"), 10, 64)
	d := donneesAlertes{Vue: c.enCoursVue(q.r.Context(), v.Get("vue"), q.t), Frise: c.frise(q.r.Context(), v.Get("hp"), q.t), Filtre: f}
	hs, err := alertes.HistoriqueFiltre(q.r.Context(), c.db, f)
	if err != nil {
		c.flash(q, "", err.Error())
	}
	for _, a := range hs {
		d.Historique = append(d.Historique, ligneHistorique{Alerte: a, Machine: a.Machine()})
	}
	if len(hs) == f.Limite {
		d.Suivant = hs[len(hs)-1].ID
	}
	qs := url.Values{}
	for k, x := range map[string]string{"regle": f.Regle, "machine": f.Machine, "gravite": f.Gravite,
		"sonde": f.Sonde, "application": f.Application, "conteneur": f.Conteneur} {
		if x != "" {
			qs.Set(k, x)
		}
	}
	d.Requete = template.URL(qs.Encode()) // #nosec G203 -- url.Values.Encode : tout est encodé
	switch {
	case f.Sonde != "":
		d.FiltreSur = filtreSur{"la sonde « " + f.Sonde + " »", "/alertes"}
	case f.Application != "":
		d.FiltreSur = filtreSur{"l'application « " + f.Application + " »", "/alertes"}
	case f.Conteneur != "":
		d.FiltreSur = filtreSur{"le conteneur « " + f.Conteneur + " »", "/alertes"}
	}
	c.rendre(q, "alertes", "Alertes", "alertes", d)
}

type etiquette struct{ Nom, Valeur string }

type donneesAlerte struct {
	A          alertes.Alerte
	Procedure  string // « Que faire ? » de la règle, montré replié (demande du 27/11)
	SSHConnue  bool   // bouton « C'est moi » (connexion SSH, 27/11)
	Machine    string
	Etiquettes []etiquette
	Sourdine   string
	EnCours    bool
	Selecteur  string        // pour le silence en un clic
	Courbe     template.HTML // valeur suivie sur 24 h et seuil (SPEC § 28)
	CourbeJSON string        // la même pour graphes.js (axes, survol, seuil)
	Absente    string        // données absentes depuis (SPEC § 46), vide sinon
	Incident   *incident     // incident dont l'alerte fait partie (SPEC § 54)
	// Cause probable (SPEC § 98) : processus les plus gourmands au début de
	// l'alerte (relevé de l'agent chaque minute) ; CauseQuoi : « CPU » ou « mémoire ».
	Cause     []processusCause
	CauseQuoi string
	CauseLe   time.Time
	// VuToutDeSuite : pannes vues par l'agent autour du début (SPEC § 98).
	VuToutDeSuite []flotte.Panne
	// « Vérifier maintenant » (SPEC § 127) : alerte d'une machine à agent,
	// en cours ou fermée depuis moins de 10 min (résultat d'une vérification).
	Verif    *vueVerif
	Resultat *resultatVerification
	// ExclureVM : alerte « VM sans sauvegarde planifiée » : bouton « Ne pas
	// surveiller » (retour du 25/11).
	ExclureVM *vmSansSauvegarde
}

type processusCause struct{ Nom, Groupe, Valeur string }

func (c *Console) pageAlerte(q *req) {
	id, _ := strconv.ParseInt(q.r.PathValue("id"), 10, 64)
	a, err := alertes.LireAlerte(q.r.Context(), c.db, id)
	if err != nil {
		c.rendreCode(q, http.StatusNotFound, "introuvable", "Alerte introuvable", "alertes", nil)
		return
	}
	d := donneesAlerte{A: a, Machine: a.Machine(), EnCours: a.Etat != "resolue", Selecteur: selecteurSilence(&a)}
	d.SSHConnue = a.Regle == "SshAdresseNouvelle" && a.Etiquettes.Get("adresse") != ""
	if inc := c.incidentPar(q.r.Context(), a.ID, q.t); inc != nil && len(inc.Alertes) > 1 {
		d.Incident = inc
	}
	if d.EnCours && c.opt.Alertes != nil { // état tenu par le moteur, pas en base
		for _, x := range c.opt.Alertes.EnCours() {
			if x.ID == a.ID && !x.AbsenteDepuis.IsZero() {
				d.Absente = dureeLisible(c.maintenant().Sub(x.AbsenteDepuis)) + " (dernières données le " + x.AbsenteDepuis.In(fuseau.Load()).Format("02/01 15:04") + ")"
			}
		}
	}
	exprRegle := ""
	if rs, err := alertes.ListerRegles(q.r.Context(), c.db); err == nil {
		for _, r := range rs {
			if r.Nom == a.Regle {
				d.Procedure = r.Procedure // « Que faire ? » (demande du 27/11)
				if c.lecteur != nil {
					fin := q.t
					if !d.EnCours && !a.Fin.IsZero() {
						fin = a.Fin.Add(2 * time.Hour)
					}
					exprRegle = r.Expr
					d.CourbeJSON = c.courbeAlerteJSON(q.r.Context(), r.Expr, libelleRegle(a.Regle), a.Etiquettes, fin, 24*time.Hour)
					if d.CourbeJSON == "" { // repli : simple trait
						d.Courbe = c.courbeAlerte(q.r.Context(), r.Expr, a.Etiquettes, a.Gravite, fin, 24*time.Hour, 720, 160)
					}
				}
			}
		}
	}
	if c.lecteur != nil { // l'expression de la règle, et son nom (règle supprimée depuis)
		d.Cause, d.CauseQuoi, d.CauseLe = c.causeProbable(q.r.Context(), d.Machine, exprRegle+" "+a.Regle, a.Debut)
	}
	if d.Machine != "" && !a.Debut.IsZero() {
		d.VuToutDeSuite, _ = flotte.ListerPannes(q.r.Context(), c.db, d.Machine, a.Debut.Add(-10*time.Minute), a.Debut.Add(10*time.Minute), 5)
	}
	if d.Machine != "" {
		if ms, err := c.machines(q.r.Context()); err == nil {
			for _, m := range ms {
				if m.Nom == d.Machine {
					d.Verif = c.verifMachine(m, q.t)
				}
			}
		}
		if d.Verif != nil && !d.EnCours && d.Verif.Etat == "" {
			d.Verif = nil // alerte terminée, sans vérification récente : rien à proposer
		}
		d.Resultat = resultatAlerte(d.Verif, d.EnCours, a.Resume)
	}
	a.Etiquettes.Range(func(l labels.Label) { d.Etiquettes = append(d.Etiquettes, etiquette{l.Name, l.Value}) })
	if a.Regle == "VMSansSauvegarde" && a.Etiquettes.Get("instance") != "" && a.Etiquettes.Get("id") != "" {
		d.ExclureVM = &vmSansSauvegarde{Machine: a.Etiquettes.Get("instance"), ID: a.Etiquettes.Get("id"),
			Type: a.Etiquettes.Get("type"), Nom: a.Etiquettes.Get("nom")}
	}
	if c.opt.Aiguilleur != nil && d.EnCours {
		for _, e := range c.opt.Alertes.EnCours() {
			if e.ID == a.ID {
				d.Sourdine = c.opt.Aiguilleur.RaisonSourdine(&e)
			}
		}
	}
	c.rendre(q, "alerte", a.Regle, "alertes", d)
}

// selecteurSilence : règle + machine (ou toutes les étiquettes utiles).
func selecteurSilence(a *alertes.Alerte) string {
	parts := []string{`alertname="` + echapperPromQL(a.Regle) + `"`}
	for _, n := range []string{"instance", "host", "mountpoint", "name", "device"} {
		if v := a.Etiquettes.Get(n); v != "" {
			parts = append(parts, n+`="`+echapperPromQL(v)+`"`)
		}
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// echapperPromQL : valeur dans une chaîne PromQL entre guillemets.
func echapperPromQL(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
}

func (c *Console) postAlerte(q *req) {
	id, _ := strconv.ParseInt(q.r.PathValue("id"), 10, 64)
	vers := "/alertes/" + strconv.FormatInt(id, 10)
	ctx := q.r.Context()
	var err error
	ok := "fait"
	switch q.r.PostFormValue("action") {
	case "ssh_connue":
		// « C'est moi » (demande du 27/11) : l'adresse vient de l'alerte,
		// pas du formulaire.
		// Code 2FA (ou mode administration ouvert) : une session volée ne
		// doit pas pouvoir rendre son adresse « connue ». L'ajout à la main
		// depuis Réglages › Sécurité (plage libre) redemande toujours le
		// code (revue du 28/11) ; ici l'adresse vient de l'alerte.
		if err = c.verifierCode(q); err != nil {
			break
		}
		var a alertes.Alerte
		if a, err = alertes.LireAlerte(ctx, c.db, id); err == nil {
			adr := a.Etiquettes.Get("adresse")
			if a.Regle != "SshAdresseNouvelle" || adr == "" {
				err = errors.New("pas une alerte de connexion SSH")
			} else {
				// Toutes les machines, ou seulement celle de l'alerte (retour du 27/11).
				machine, ou := "", "toutes les machines"
				if q.r.PostFormValue("portee") == "machine" {
					machine = a.Etiquettes.Get("instance")
					ou = machine
				}
				if _, err = flotte.AjouterSSHConnue(ctx, c.db, adr, machine, "C'est moi (alerte)", q.qui(), q.t); err == nil {
					_ = Auditer(ctx, c.db, q.qui(), "ssh_adresse_connue", adr+" ("+ou+")", "", "", q.ip.String(), q.t)
					ok = adr + " est maintenant connue (" + ou + ") : l'alerte se ferme dans la minute. Liste : Réglages › Sécurité"
				}
			}
		}
	case "acquitter":
		// Durée de la prise en charge (SPEC § 30) : 4 h par défaut, 24 h, ou
		// jusqu'à la fin de l'alerte ; ensuite, rappel si elle dure.
		var jusqua time.Time
		switch q.r.PostFormValue("duree_ack") {
		case "fin":
			ok = "alerte prise en charge jusqu'à sa fin : plus de rappel (la fin sera annoncée)"
		case "24h":
			jusqua = q.t.Add(24 * time.Hour)
		default:
			jusqua = q.t.Add(notif.DureeAcquittement)
		}
		if !jusqua.IsZero() {
			ok = "alerte prise en charge jusqu'à " + jusqua.In(fuseau.Load()).Format("15:04") + " : ensuite, rappel si elle dure"
		}
		// Heure dans le fuseau de l'interface : c'est elle que la ligne
		// d'audit écrit (relecture du 21/11 : l'audit l'écrivait en UTC).
		if !jusqua.IsZero() {
			jusqua = jusqua.In(fuseau.Load())
		}
		err = notif.Acquitter(ctx, c.db, id, q.qui(), strings.TrimSpace(q.r.PostFormValue("commentaire")), jusqua, q.t)
	case "reprendre":
		err = notif.Reprendre(ctx, c.db, id, q.qui(), q.t)
		ok = "alerte de nouveau à traiter"
	case "silence":
		var a alertes.Alerte
		if a, err = alertes.LireAlerte(ctx, c.db, id); err == nil {
			var d alertes.Duree
			if d, err = alertes.LireDuree(q.r.PostFormValue("duree")); err == nil && time.Duration(d) > silenceSansCode {
				err = c.verifierCode(q) // silence long : comme sur la page « Silences »
			}
			if err == nil {
				_, err = notif.AjouterSilence(ctx, c.db, selecteurSilence(&a), q.t, time.Duration(d), q.qui(),
					strings.TrimSpace(q.r.PostFormValue("commentaire")), q.t)
				ok = "silence posé pour " + d.String()
			}
		}
	default:
		err = errors.New("action inconnue")
	}
	if r := q.r.PostFormValue("retour"); r != "" {
		vers = retourLocal(r)
	}
	c.retour(q, vers, err, ok)
}

// --- Sourdine : silences, maintenances, inhibitions, dépendances ---------------------

type donneesSourdine struct {
	Prerempli    string // cible de maintenance proposée (Ctrl+K, SPEC § 58)
	Silences     []notif.Silence
	Maintenances []ligneMaintenance
	Inhibitions  []notif.Inhibition
	Dependances  []notif.Dependance
	Machines     []string
	Sites        []string // cibles « site:NOM » (SPEC § 81)
	Groupes      []string
	Jours        []string
	JoursMois    []int
	// ReglesPanne : règles qui veulent dire « machine en panne » (réglage
	// des notifications, modifié ici depuis le 16/11, SPEC § 38).
	ReglesPanne []string
}

func (c *Console) pageSourdine(q *req) {
	ctx := q.r.Context()
	var d donneesSourdine
	if m := q.r.URL.Query().Get("maintenance"); len(m) <= 200 && (strings.HasPrefix(m, "machine:") || strings.HasPrefix(m, "site:")) {
		d.Prerempli = m
	}
	var e [5]error
	d.Silences, e[0] = notif.ListerSilences(ctx, c.db, false, q.t)
	var mts []notif.Maintenance
	mts, e[1] = notif.ListerMaintenances(ctx, c.db)
	for _, m := range mts {
		d.Maintenances = append(d.Maintenances, decrireMaintenance(m, q.t))
	}
	d.Inhibitions, e[2] = notif.ListerInhibitions(ctx, c.db)
	d.Dependances, e[3] = notif.ListerDependances(ctx, c.db)
	var ms []flotte.InfoMachine
	ms, e[4] = c.machines(ctx)
	vuS, vuG := map[string]bool{}, map[string]bool{}
	for _, m := range ms {
		d.Machines = append(d.Machines, m.Nom)
		if m.Site != "" && !vuS[m.Site] {
			vuS[m.Site] = true
			d.Sites = append(d.Sites, m.Site)
		}
		if m.Groupe != "" && !vuG[m.Groupe] {
			vuG[m.Groupe] = true
			d.Groupes = append(d.Groupes, m.Groupe)
		}
	}
	sort.Strings(d.Sites)
	sort.Strings(d.Groupes)
	d.Jours = []string{"lun", "mar", "mer", "jeu", "ven", "sam", "dim"}
	for j := 1; j <= 31; j++ {
		d.JoursMois = append(d.JoursMois, j)
	}
	if rn, err := notif.LireReglages(ctx, c.db); err == nil {
		d.ReglesPanne = rn.ReglesPanne
	}
	if err := errors.Join(e[:]...); err != nil {
		c.flash(q, "", err.Error())
	}
	c.rendre(q, "sourdine", "Sourdine", "alertes", d)
}

// ligneMaintenance : une maintenance avec son texte et sa prochaine fenêtre
// (SPEC § 81).
type ligneMaintenance struct {
	notif.Maintenance
	Quand     string // « chaque dim à 03:00 pendant 2h » ou « 20/11 03:00 → 05:00 »
	Prochaine string // « dim 23/11 03:00 → 04:00 » (récurrente, à venir)
	EnCours   string // « jusqu'à dim 23/11 04:00 »
	Finie     bool
}

func decrireMaintenance(m notif.Maintenance, t time.Time) ligneMaintenance {
	loc := fuseau.Load()
	l := ligneMaintenance{Maintenance: m}
	debut, fin := m.Debut, m.Fin
	if m.Recurrence != nil {
		l.Quand = m.Recurrence.Texte()
		debut, fin = m.Recurrence.Prochaine(t)
	} else {
		l.Quand = m.Debut.In(loc).Format("02/01 15:04") + " → " + m.Fin.In(loc).Format("02/01 15:04")
	}
	switch {
	case debut.IsZero() || !fin.After(t):
		l.Finie = true
	case !t.Before(debut):
		l.EnCours = "jusqu'à " + joursFr(fin.In(loc).Format("Mon 02/01 15:04"))
	case m.Recurrence != nil:
		l.Prochaine = joursFr(debut.In(loc).Format("Mon 02/01 15:04")) + " → " + fin.In(loc).Format("15:04")
		if fin.Sub(debut) >= 24*time.Hour || fin.In(loc).Day() != debut.In(loc).Day() {
			l.Prochaine = joursFr(debut.In(loc).Format("Mon 02/01 15:04")) + " → " + joursFr(fin.In(loc).Format("Mon 02/01 15:04"))
		}
	}
	return l
}

// lireMaintenance : formulaire « Nouvelle maintenance » (SPEC § 81) :
// une seule fois (début + durée), ou chaque jour / semaine / mois de
// « de » à « à » (« à » plus tôt que « de » : le lendemain).
func lireMaintenance(f func(string) string, jours []string, t time.Time) (debut time.Time, d time.Duration, rec *notif.Recurrence, err error) {
	rythme := f("repeter")
	if rythme == "" || rythme == "non" {
		if debut, err = lireDebut(f("debut"), t); err != nil {
			return
		}
		var dd alertes.Duree
		dd, err = alertes.LireDuree(f("duree"))
		return debut, time.Duration(dd), nil, err
	}
	de, err1 := time.Parse("15:04", f("de"))
	a, err2 := time.Parse("15:04", f("a"))
	if err1 != nil || err2 != nil {
		return t, 0, nil, errors.New("heures « de » et « à » attendues (HH:MM)")
	}
	d = a.Sub(de)
	if d <= 0 {
		d += 24 * time.Hour
	}
	rec = &notif.Recurrence{Rythme: rythme, Jours: jours, Heure: f("de"), Duree: alertes.Duree(d), Fuseau: fuseau.Load().String()}
	if rythme == "mois" {
		rec.JourMois, _ = strconv.Atoi(f("jour_mois"))
	}
	return t, d, rec, rec.Valider()
}

// lireDebut : « AAAA-MM-JJTHH:MM » (champ datetime-local), heure de Paris ;
// vide = maintenant.
func lireDebut(s string, t time.Time) (time.Time, error) {
	if s == "" {
		return t, nil
	}
	d, err := time.ParseInLocation("2006-01-02T15:04", s, fuseau.Load())
	if err != nil {
		return t, errors.New("date de début illisible")
	}
	return d, nil
}

// silenceSansCode : durée d'un silence posé sans code 2FA (au-delà : code
// ou mode administration).
const silenceSansCode = 24 * time.Hour

func (c *Console) postSourdine(q *req) {
	ctx := q.r.Context()
	f := func(k string) string { return strings.TrimSpace(q.r.PostFormValue(k)) }
	id, _ := strconv.ParseInt(f("id"), 10, 64)
	var err error
	ok := "fait"
	// Ce qui fait taire des alertes longtemps demande le code 2FA ou le mode
	// administration (audit du 16/11, SPEC § 34) : maintenance, inhibition,
	// dépendance, silence de plus de silenceSansCode. Un silence court,
	// annuler ou supprimer restent sans code (gestes courants, sans risque).
	switch f("action") {
	case "maintenance", "inhibition", "dependance":
		if err := c.verifierCode(q); err != nil {
			c.retour(q, "/sourdine", err, "")
			return
		}
	}
	switch f("action") {
	case "silence":
		var d alertes.Duree
		var debut time.Time
		if d, err = alertes.LireDuree(f("duree")); err == nil && time.Duration(d) > silenceSansCode {
			err = c.verifierCode(q)
		}
		if err == nil {
			if debut, err = lireDebut(f("debut"), q.t); err == nil {
				_, err = notif.AjouterSilence(ctx, c.db, f("selecteur"), debut, time.Duration(d), q.qui(), f("commentaire"), q.t)
			}
		}
		ok = "silence posé"
	case "silence_annuler":
		err = notif.AnnulerSilence(ctx, c.db, id, q.qui(), q.t)
		ok = "silence annulé"
	case "maintenance":
		debut, d, rec, e := lireMaintenance(f, q.r.PostForm["jours"], q.t)
		if err = e; err == nil {
			_, err = notif.AjouterMaintenance(ctx, c.db, f("cible"), debut, debut.Add(d), rec, q.qui(), f("commentaire"), q.t)
		}
		ok = "maintenance enregistrée"
	case "inhibition":
		var egales []string
		for _, e := range strings.Split(f("egales"), ",") {
			if e = strings.TrimSpace(e); e != "" {
				egales = append(egales, e)
			}
		}
		_, err = notif.AjouterInhibition(ctx, c.db, f("source"), f("cible"), egales, q.qui(), f("commentaire"), q.t)
	case "dependance":
		_, err = notif.AjouterDependance(ctx, c.db, f("enfant"), f("parent"), q.qui(), f("commentaire"), q.t)
	case "supprimer":
		err = notif.SupprimerElement(ctx, c.db, f("table"), id, q.qui(), q.t)
		ok = "supprimé"
	default:
		err = errors.New("action inconnue")
	}
	c.retour(q, "/sourdine", err, ok)
}

// --- Historique des envois -------------------------------------------------------

func (c *Console) pageEnvois(q *req) {
	es, err := notif.ListerEnvois(q.r.Context(), c.db, 300)
	if err != nil {
		c.flash(q, "", err.Error())
	}
	c.rendre(q, "envois", "Historique des envois", "alertes", es)
}

// --- Comparaison avec vmalert (mode ombre) ---------------------------------------------

type donneesComparaison struct {
	Reglee   bool
	Erreur   string
	Ecarts   []alertes.Ecart
	Communes int
	Episodes []alertes.Episode
}

func (c *Console) pageComparaison(q *req) {
	ctx := q.r.Context()
	var d donneesComparaison
	reg, err := alertes.LireReglagesVmalert(ctx, c.db)
	d.Reglee = err == nil && reg.URL != ""
	if d.Reglee && c.opt.Alertes != nil {
		vm, err := alertes.LireVmalert(ctx, reg.URL)
		if err != nil {
			d.Erreur = err.Error()
		} else if ms, err := c.machines(ctx); err == nil {
			connues := map[string]bool{}
			for _, m := range ms {
				if m.Statut == "approuvee" {
					connues[m.Nom] = true
				}
			}
			for _, e := range alertes.Comparer(reg, vm, c.opt.Alertes.EnCours(), connues) {
				if e.Type == "commune" {
					d.Communes++
				} else {
					d.Ecarts = append(d.Ecarts, e)
				}
			}
		}
	}
	d.Episodes, _ = alertes.ListerEpisodes(ctx, c.db, q.t.Add(-7*24*time.Hour))
	if len(d.Episodes) > 500 {
		d.Episodes = d.Episodes[:500]
	}
	c.rendre(q, "comparaison", "Comparaison avec vmalert", "alertes", d)
}

// csvComparaison : rapport exportable (30 jours), pour un tableur.
func (c *Console) csvComparaison(q *req) {
	eps, err := alertes.ListerEpisodes(q.r.Context(), c.db, q.t.Add(-30*24*time.Hour))
	if err != nil {
		http.Error(q.w, err.Error(), http.StatusInternalServerError)
		return
	}
	q.w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	q.w.Header().Set("Content-Disposition", `attachment; filename="comparaison-vmalert.csv"`)
	w := csv.NewWriter(q.w)
	_ = w.Write([]string{"regle", "machine", "type", "debut", "fin", "duree_minutes"})
	for _, e := range eps {
		fin, d := "", q.t.Sub(e.Debut)
		if !e.Fin.IsZero() {
			fin, d = e.Fin.UTC().Format(time.RFC3339), e.Fin.Sub(e.Debut)
		}
		// Cellules commençant par = + - @ : préfixées (injection de formule dans un tableur).
		_ = w.Write([]string{celluleSure(e.Regle), celluleSure(e.Machine), e.Type, e.Debut.UTC().Format(time.RFC3339), fin,
			fmt.Sprintf("%.0f", d.Minutes())})
	}
	w.Flush()
}

func celluleSure(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}

// causeProbable (SPEC § 98) : pour une alerte de CPU, de charge ou de
// mémoire (d'après l'expression et le nom de la règle), les 3 processus les plus gourmands relevés par l'agent juste
// après le début (relevé de la minute qui contient le début).
func (c *Console) causeProbable(ctx context.Context, machine, expr string, debut time.Time) ([]processusCause, string, time.Time) {
	if machine == "" || debut.IsZero() {
		return nil, "", time.Time{}
	}
	e := strings.ToLower(expr)
	serie, quoi := "", ""
	switch {
	case strings.Contains(e, "cpu") || strings.Contains(e, "load") || strings.Contains(e, "charge"):
		serie, quoi = "mirador_processus_cpu_pourcent", "CPU"
	case strings.Contains(e, "mem") || strings.Contains(e, "mém") || strings.Contains(e, "oom") || strings.Contains(e, "swap"):
		serie, quoi = "mirador_processus_memoire_octets", "mémoire"
	default:
		return nil, "", time.Time{}
	}
	t := debut.Add(90 * time.Second) // le relevé qui couvre la minute du début
	if t.After(c.maintenant()) {
		t = c.maintenant()
	}
	ech, err := c.instant(ctx, `topk(3, `+serie+`{instance="`+echapperPromQL(machine)+`"})`, t)
	if err != nil || len(ech) == 0 {
		return nil, "", time.Time{}
	}
	sort.Slice(ech, func(i, j int) bool { return ech[i].Valeur > ech[j].Valeur })
	var out []processusCause
	for _, x := range ech {
		v := strconv.FormatFloat(x.Valeur, 'f', 0, 64) + " %"
		if quoi == "mémoire" {
			v = tailleLisible(int64(x.Valeur))
		}
		out = append(out, processusCause{Nom: x.Etiquettes.Get("nom"), Groupe: x.Etiquettes.Get("groupe"), Valeur: v})
	}
	return out, quoi, t
}
