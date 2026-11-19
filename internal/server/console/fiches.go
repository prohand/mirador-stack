// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"html/template"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"

	"mirador-stack/internal/server/alertes"
	"mirador-stack/internal/server/flotte"
	"mirador-stack/internal/server/notif"
	essais "mirador-stack/internal/sondes"
)

// Fiches (SPEC § 56, idée reprise de New Relic « entity overview ») : même
// présentation pour une sonde, un service ou un conteneur (la machine a
// déjà la sienne, qui reçoit les mêmes blocs Liens et Étiquettes) : en-tête,
// raccourcis (logs, alertes, disponibilité), 4 tuiles, une courbe sur 24 h,
// alertes en cours, liens, étiquettes, derniers événements.

type tuileFiche struct{ Titre, Valeur, Classe string }

type raccourciFiche struct{ Titre, Lien string }

type donneesFiche struct {
	E           element
	Tuiles      []tuileFiche
	Raccourcis  []raccourciFiche
	TitreCourbe string
	Courbe      template.HTML
	Alertes     []resumeAlerte
	Evenements  []evenement
	Etiquettes  string // formulaire
	Retour      string
	// Sonde : formulaire « Modifier la sonde » (retour du 19/11).
	Sonde  *essais.Sonde
	Depuis []string // serveur + collecteurs
}

func (c *Console) ficheElement(q *req, cle string) {
	ctx := q.r.Context()
	acc := c.donneesAccueil(q)
	var e *element
	for _, x := range c.elementsDuParc(ctx, acc.Parc, q.t) {
		if x.Cle == cle {
			x := x
			e = &x
			break
		}
	}
	if e == nil {
		c.rendreCode(q, 404, "introuvable", "Élément introuvable", "elements", nil)
		return
	}
	d := donneesFiche{E: *e, Etiquettes: texteEtiquettesElement(e.Etiquettes), Retour: q.r.URL.Path}
	if nom, ok := strings.CutPrefix(cle, "s:"); ok {
		if ss, err := flotte.LireSondes(ctx, c.db); err == nil {
			for i := range ss {
				if ss[i].Nom == nom {
					d.Sonde = &ss[i]
				}
			}
		}
		d.Depuis = []string{essais.ServeurLocal}
		if cs, err := flotte.Collecteurs(ctx, c.db); err == nil {
			d.Depuis = append(d.Depuis, cs...)
		}
	}
	debut := q.t.Add(-24 * time.Hour)
	r, _ := notif.LireReglages(ctx, c.db)
	pannes := c.pannes(ctx, r.ReglesPanne, q.t.Add(-30*24*time.Hour), q.t)
	dispo := func(iv []intervalle) string {
		total := 30 * 24 * 3600
		return pct(100*(1-float64(dureeDans(reunir(iv), q.t.Add(-30*24*time.Hour).Unix(), q.t.Unix()))/float64(total))) + " %"
	}
	instant := func(expr string) (float64, bool) {
		if c.lecteur == nil {
			return 0, false
		}
		ech, err := c.instant(ctx, expr, q.t)
		if err != nil || len(ech) == 0 || math.IsNaN(ech[0].Valeur) {
			return 0, false
		}
		return ech[0].Valeur, true
	}
	courbe := func(expr string, seuil float64) {
		if c.lecteur == nil {
			return
		}
		if cs, _, err := c.lecteur.Plage(ctx, expr, debut, q.t, 5*time.Minute); err == nil && len(cs) > 0 {
			d.Courbe = svgCourbeEchelle(cs[0].T, cs[0].V, debut, q.t, seuil, !math.IsNaN(seuil), "", 900, 140, true)
		}
	}
	// Alertes en cours qui le concernent.
	concerne := func(a alertes.Alerte) bool {
		switch {
		case strings.HasPrefix(cle, "s:"):
			return a.Etiquettes.Get("sonde") == e.Nom
		case strings.HasPrefix(cle, "v:"):
			return a.Etiquettes.Get("service") == e.Nom
		case strings.HasPrefix(cle, "c:"):
			return a.Machine() == e.Groupe && a.Etiquettes.Get("name") == e.Nom
		}
		return false
	}
	if c.opt.Alertes != nil {
		for _, a := range c.opt.Alertes.EnCours() {
			if a.Etat == alertes.Active && concerne(a) {
				d.Alertes = append(d.Alertes, resumeAlerte{ID: a.ID, Regle: a.Regle, Gravite: a.Gravite, Machine: a.Machine(), Resume: a.Resume,
					Etat: a.Etat, Depuis: a.Debut.UnixMilli()})
			}
		}
	}
	nbAlertes := strconv.Itoa(len(d.Alertes))
	switch {
	case strings.HasPrefix(cle, "s:"):
		sel := `{sonde="` + echapperPromQL(e.Nom) + `"}`
		med, ok := instant(`max(quantile_over_time(0.5, mirador_sonde_duree_secondes` + sel + `[24h]))`)
		mediane := "—"
		if ok {
			mediane = strconv.FormatFloat(med*1000, 'f', 0, 64) + " ms"
		}
		d.Tuiles = []tuileFiche{{"Disponibilité 30 j", dispo(pannes[cle]), ""}, {"Temps médian 24 h", mediane, ""},
			{"État", e.Etat, e.Pastille()}, {"Alertes en cours", nbAlertes, ""}}
		d.TitreCourbe = "Temps de réponse — 24 h (secondes, pointillé : seuil « lente »)"
		courbe(`max(mirador_sonde_duree_secondes`+sel+`)`, 1)
		d.Raccourcis = []raccourciFiche{{"Page Sondes", "/sondes"}, {"Disponibilité", "/disponibilite"}, {"Alertes", "/alertes"}}
	case strings.HasPrefix(cle, "v:"):
		var iv []intervalle
		for _, l := range e.Liens {
			if l.Quoi != "comprend" {
				continue
			}
			sorte, nom, _ := strings.Cut(l.Nom, " ")
			switch sorte {
			case "machine":
				iv = append(iv, pannes["m:"+nom]...)
			case "sonde":
				iv = append(iv, pannes["s:"+nom]...)
			}
		}
		d.Tuiles = []tuileFiche{{"État", e.Etat, e.Pastille()}, {"Disponibilité 30 j", dispo(iv), ""},
			{"Éléments", strconv.Itoa(len(e.Liens)), ""}, {"Alertes en cours", nbAlertes, ""}}
		d.TitreCourbe = "État de l'application — 24 h (0 : OK, 1 : dégradée, 2 : en panne)"
		courbe(`max(mirador_service_etat{service="`+echapperPromQL(e.Nom)+`"})`, math.NaN())
		d.Raccourcis = []raccourciFiche{{"Page Applications", "/applications"}, {"Incidents", "/incidents"}}
	case strings.HasPrefix(cle, "c:"):
		sel := `{instance="` + echapperPromQL(e.Groupe) + `", name="` + echapperPromQL(e.Nom) + `"}`
		cpu, mem, redem := "—", "—", "0"
		if v, ok := instant(`max(100 * rate(node_docker_container_cpu_seconds_total` + sel + `[5m]))`); ok {
			cpu = virgule(formatPct(v)) + " d'un cœur"
		}
		if v, ok := instant(`max(node_docker_container_memory_bytes` + sel + `)`); ok {
			mem = virgule(tailleLisible(int64(v)))
		}
		if v, ok := instant(`max(round(increase(node_docker_container_restart_count` + sel + `[24h])))`); ok {
			redem = strconv.FormatFloat(v, 'f', 0, 64)
		}
		d.Tuiles = []tuileFiche{{"État", e.Etat, e.Pastille()}, {"Processeur", cpu, ""}, {"Mémoire", mem, ""}, {"Redémarrages 24 h", redem, ""}}
		d.TitreCourbe = "Processeur — 24 h (% d'un cœur)"
		courbe(`max(100 * rate(node_docker_container_cpu_seconds_total`+sel+`[5m]))`, math.NaN())
		qs := url.Values{"host": {e.Groupe}, "container": {e.Nom}}
		d.Raccourcis = []raccourciFiche{{"Logs du conteneur", "/logs?" + qs.Encode()}, {"Machine " + e.Groupe, "/machines/" + url.PathEscape(e.Groupe)},
			{"Page Conteneurs", "/conteneurs?machine=" + url.QueryEscape(e.Groupe)}}
	}
	// Derniers événements (7 jours) qui le citent.
	for _, ev := range c.journalEvenements(ctx, filtreEvenements{Debut: q.t.Add(-7 * 24 * time.Hour), Fin: q.t, SansAuto: true, Max: 300}) {
		if strings.Contains(ev.Texte, e.Nom) || (strings.HasPrefix(cle, "c:") && ev.Machine == e.Groupe && ev.Type == evChangement) {
			d.Evenements = append(d.Evenements, ev)
			if len(d.Evenements) == 15 {
				break
			}
		}
	}
	c.rendre(q, "fiche", e.Nom, "elements", d)
}

func (c *Console) ficheSonde(q *req)   { c.ficheElement(q, "s:"+q.r.PathValue("nom")) }
func (c *Console) ficheService(q *req) { c.ficheElement(q, "v:"+q.r.PathValue("nom")) }
func (c *Console) ficheConteneur(q *req) {
	c.ficheElement(q, "c:"+q.r.PathValue("machine")+"/"+q.r.PathValue("nom"))
}
