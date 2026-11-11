// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"encoding/csv"
	"errors"
	"fmt"
	"net/http"
	"net/url"
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
	m.HandleFunc("GET /alertes/{id}", c.lecture(c.pageAlerte))
	m.HandleFunc("POST /alertes/{id}", c.admin(c.postAlerte))
	m.HandleFunc("GET /sourdine", c.lecture(c.pageSourdine))
	m.HandleFunc("POST /sourdine", c.admin(c.postSourdine))
	m.HandleFunc("GET /envois", c.lecture(c.pageEnvois))
	m.HandleFunc("GET /comparaison", c.lecture(c.pageComparaison))
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
	Historique []ligneHistorique
	Filtre     alertes.Filtre
	Suivant    int64
	Requete    string
}

func (c *Console) pageAlertes(q *req) {
	v := q.r.URL.Query()
	f := alertes.Filtre{Regle: v.Get("regle"), Machine: v.Get("machine"), Gravite: v.Get("gravite"), Limite: 100}
	f.Avant, _ = strconv.ParseInt(v.Get("avant"), 10, 64)
	d := donneesAlertes{EnCours: c.alertesEnCours(""), Filtre: f}
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
	for k, x := range map[string]string{"regle": f.Regle, "machine": f.Machine, "gravite": f.Gravite} {
		if x != "" {
			qs.Set(k, x)
		}
	}
	d.Requete = qs.Encode()
	c.rendre(q, "alertes", "Alertes", "alertes", d)
}

type etiquette struct{ Nom, Valeur string }

type donneesAlerte struct {
	A          alertes.Alerte
	Machine    string
	Etiquettes []etiquette
	Sourdine   string
	EnCours    bool
	Selecteur  string // pour le silence en un clic
}

func (c *Console) pageAlerte(q *req) {
	id, _ := strconv.ParseInt(q.r.PathValue("id"), 10, 64)
	a, err := alertes.LireAlerte(q.r.Context(), c.db, id)
	if err != nil {
		c.rendreCode(q, http.StatusNotFound, "introuvable", "Alerte introuvable", "alertes", nil)
		return
	}
	d := donneesAlerte{A: a, Machine: a.Machine(), EnCours: a.Etat != "resolue", Selecteur: selecteurSilence(&a)}
	a.Etiquettes.Range(func(l labels.Label) { d.Etiquettes = append(d.Etiquettes, etiquette{l.Name, l.Value}) })
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
	case "acquitter":
		err = notif.Acquitter(ctx, c.db, id, q.qui(), strings.TrimSpace(q.r.PostFormValue("commentaire")), q.t)
		ok = "alerte acquittée : plus de rappel (la fin sera annoncée)"
	case "silence":
		var a alertes.Alerte
		if a, err = alertes.LireAlerte(ctx, c.db, id); err == nil {
			var d alertes.Duree
			if d, err = alertes.LireDuree(q.r.PostFormValue("duree")); err == nil {
				_, err = notif.AjouterSilence(ctx, c.db, selecteurSilence(&a), q.t, time.Duration(d), q.qui(),
					strings.TrimSpace(q.r.PostFormValue("commentaire")), q.t)
				ok = "silence posé pour " + d.String()
			}
		}
	default:
		err = errors.New("action inconnue")
	}
	c.retour(q, vers, err, ok)
}

// --- Sourdine : silences, maintenances, inhibitions, dépendances ---------------------

type donneesSourdine struct {
	Silences     []notif.Silence
	Maintenances []notif.Maintenance
	Inhibitions  []notif.Inhibition
	Dependances  []notif.Dependance
	Machines     []string
	Jours        []string
}

func (c *Console) pageSourdine(q *req) {
	ctx := q.r.Context()
	var d donneesSourdine
	var e [5]error
	d.Silences, e[0] = notif.ListerSilences(ctx, c.db, false, q.t)
	d.Maintenances, e[1] = notif.ListerMaintenances(ctx, c.db)
	d.Inhibitions, e[2] = notif.ListerInhibitions(ctx, c.db)
	d.Dependances, e[3] = notif.ListerDependances(ctx, c.db)
	var ms []flotte.InfoMachine
	ms, e[4] = flotte.ListerMachines(ctx, c.db)
	for _, m := range ms {
		d.Machines = append(d.Machines, m.Nom)
	}
	d.Jours = []string{"lun", "mar", "mer", "jeu", "ven", "sam", "dim"}
	if err := errors.Join(e[:]...); err != nil {
		c.flash(q, "", err.Error())
	}
	c.rendre(q, "sourdine", "Sourdine", "alertes", d)
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

func (c *Console) postSourdine(q *req) {
	ctx := q.r.Context()
	f := func(k string) string { return strings.TrimSpace(q.r.PostFormValue(k)) }
	id, _ := strconv.ParseInt(f("id"), 10, 64)
	var err error
	ok := "fait"
	switch f("action") {
	case "silence":
		var d alertes.Duree
		var debut time.Time
		if d, err = alertes.LireDuree(f("duree")); err == nil {
			if debut, err = lireDebut(f("debut"), q.t); err == nil {
				_, err = notif.AjouterSilence(ctx, c.db, f("selecteur"), debut, time.Duration(d), q.qui(), f("commentaire"), q.t)
			}
		}
		ok = "silence posé"
	case "silence_annuler":
		err = notif.AnnulerSilence(ctx, c.db, id, q.qui(), q.t)
		ok = "silence annulé"
	case "maintenance":
		var debut time.Time
		var d alertes.Duree
		if debut, err = lireDebut(f("debut"), q.t); err == nil {
			if d, err = alertes.LireDuree(f("duree")); err == nil {
				var rec *notif.Recurrence
				if jours := q.r.PostForm["jours"]; len(jours) > 0 {
					rec = &notif.Recurrence{Jours: jours, Heure: f("heure"), Duree: d, Fuseau: "Europe/Paris"}
				}
				_, err = notif.AjouterMaintenance(ctx, c.db, f("cible"), debut, debut.Add(time.Duration(d)), rec, q.qui(), f("commentaire"), q.t)
			}
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
		} else if ms, err := flotte.ListerMachines(ctx, c.db); err == nil {
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
