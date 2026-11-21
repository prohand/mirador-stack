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
	Sonde *essais.Sonde
	// « Vérifier maintenant » (SPEC § 108) : possible, sinon pourquoi ;
	// EnEssai : page ouverte juste après une demande (mise à jour seule).
	Verifiable    bool
	PasVerifiable string
	EnEssai       bool
	Depuis        []string // serveur + collecteurs
	Toutes        []string // machines approuvées (« Machine liée »)
	// Dernières alertes de l'élément, avec le retour à la normale (§ 72).
	Dernieres *blocDernieresAlertes
}

func (c *Console) ficheElement(q *req, cle string) {
	ctx := q.r.Context()
	var e *element
	for _, x := range c.elementsDuParc(ctx, c.parcLeger(q), q.t) {
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
					d.Verifiable, d.PasVerifiable = essaiPossible(ss[i], c.collecteursParNom(ctx))
					d.EnEssai = enEssai(q)
				}
			}
		}
		d.Depuis = []string{essais.ServeurLocal}
		d.Toutes = c.nomsMachines(ctx)
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
		n := url.QueryEscape(e.Nom)
		d.Raccourcis = []raccourciFiche{{"Page Sondes", "/sondes?sonde=" + n}, {"Disponibilité", "/disponibilite?element=" + url.QueryEscape(cle)},
			{"Alertes", "/alertes?sonde=" + n}, {"Incidents", "/incidents?sonde=" + n}, {"Événements", "/evenements?element=" + n}}
		if d.Sonde != nil && d.Sonde.Machine != "" {
			d.Raccourcis = append(d.Raccourcis, raccourciFiche{"Machine " + d.Sonde.Machine, "/machines/" + url.PathEscape(d.Sonde.Machine) + "#sondes"})
		}
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
		n := url.QueryEscape(e.Nom)
		d.Raccourcis = []raccourciFiche{{"Page Applications", "/applications?nom=" + n}, {"Incidents", "/incidents?application=" + n},
			{"Alertes", "/alertes?application=" + n}, {"Événements", "/evenements?element=" + n}}
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
			{"Page Conteneurs", "/conteneurs?machine=" + url.QueryEscape(e.Groupe)},
			{"Alertes", "/alertes?" + url.Values{"machine": {e.Groupe}, "conteneur": {e.Nom}}.Encode()},
			{"Événements", "/evenements?" + url.Values{"machine": {e.Groupe}, "element": {e.Nom}}.Encode()}}
	}
	// Dernières alertes de l'élément (§ 72).
	switch {
	case strings.HasPrefix(cle, "s:"):
		d.Dernieres = c.dernieresAlertes(ctx, alertes.Filtre{Sonde: e.Nom}, "/alertes?sonde="+url.QueryEscape(e.Nom), q.t)
	case strings.HasPrefix(cle, "v:"):
		d.Dernieres = c.dernieresAlertes(ctx, alertes.Filtre{Application: e.Nom}, "/alertes?application="+url.QueryEscape(e.Nom), q.t)
	case strings.HasPrefix(cle, "c:"):
		d.Dernieres = c.dernieresAlertes(ctx, alertes.Filtre{Machine: e.Groupe, Conteneur: e.Nom},
			"/alertes?"+url.Values{"machine": {e.Groupe}, "conteneur": {e.Nom}}.Encode(), q.t)
	}
	// Derniers événements (7 jours) qui le citent.
	// Filtre « Cite » appliqué dans les requêtes (relecture du 21/11 : on
	// cherchait avant dans les 300 derniers événements du parc seulement).
	periode := filtreEvenements{Debut: q.t.Add(-7 * 24 * time.Hour), Fin: q.t, SansAuto: true, Max: 15}
	cite := periode
	cite.Cite = e.Nom
	d.Evenements = c.journalEvenements(ctx, cite)
	if strings.HasPrefix(cle, "c:") && e.Groupe != "" {
		// Conteneur : aussi les changements de sa machine.
		chgt := periode
		chgt.Machine, chgt.Types = e.Groupe, map[string]bool{evChangement: true}
		d.Evenements = fusionnerEvenements(d.Evenements, c.journalEvenements(ctx, chgt), 15)
	}
	// Lien du menu en surbrillance : la liste de cette sorte d'élément.
	menu := "elements"
	switch {
	case strings.HasPrefix(cle, "s:"):
		menu = "sondes"
	case strings.HasPrefix(cle, "v:"):
		menu = "services"
	case strings.HasPrefix(cle, "c:"):
		menu = "conteneurs"
	}
	c.rendre(q, "fiche", e.Nom, menu, d)
}

func (c *Console) ficheSonde(q *req)   { c.ficheElement(q, "s:"+q.r.PathValue("nom")) }
func (c *Console) ficheService(q *req) { c.ficheElement(q, "v:"+q.r.PathValue("nom")) }
func (c *Console) ficheConteneur(q *req) {
	c.ficheElement(q, "c:"+q.r.PathValue("machine")+"/"+q.r.PathValue("nom"))
}
