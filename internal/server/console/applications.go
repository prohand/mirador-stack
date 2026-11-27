// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"mirador-stack/internal/server/flotte"
	"mirador-stack/internal/server/servicesmetier"
	essais "mirador-stack/internal/sondes"
)

// Page Applications, nouvelle présentation (maquette validée le 23/11,
// SPEC § 122) : bandeau de la panne (depuis quand, quel élément clé),
// disponibilité moyenne, applications proposées d'après Docker Compose,
// cartes avec cause, frise des 30 jours et état de chaque élément, éditeur
// sans syntaxe (on coche ; « clé » par élément ; machine hôte des
// conteneurs cochée d'office ; page de statut publique).
//
// Coût : historique lu dans la table des alertes (ApplicationEnPanne /
// ApplicationDegradee, comme la page de statut) ; deux requêtes
// instantanées de plus (conteneurs Docker) à l'ouverture de la page.

const joursFriseApp = 30

// jourApp : une barre de la frise (ok, deg, pan).
type jourApp struct{ Classe, Titre string }

// elementApp : un élément d'une application, avec son lien et un détail.
type elementApp struct {
	servicesmetier.EtatComposant
	Lien, Icone, Detail string
}

// infoApp : ce qui s'ajoute à l'état calculé (page et fiche).
type infoApp struct {
	Depuis   string // « depuis 12 min », « aucun problème en 30 j »
	Cause    string // « Sonde Nextcloud : Échec » (en panne ou dégradée)
	Frise    []jourApp
	Elements []elementApp
	Statut   bool // montrée sur la page de statut publique
}

// proposition : application proposée d'après un projet Docker Compose.
type proposition struct {
	Projet, Machine string
	Conteneurs      []string
}

func (p proposition) Lien() string {
	return "/applications?" + url.Values{"proposer": {p.Projet}, "machine": {p.Machine}}.Encode() + "#editeur"
}

// candidat : une ligne de l'éditeur.
type candidat struct {
	Sorte, Nom, Detail, Projet, Hote string
	Choisi, Cle                      bool
}

func (k candidat) Valeur() string { return k.Sorte + "|" + k.Nom }

// TexteSorte : libellé du groupe de l'éditeur.
func (k candidat) TexteSorte() string {
	switch k.Sorte {
	case servicesmetier.Conteneur:
		return "Conteneurs"
	case servicesmetier.Sonde:
		return "Sondes"
	}
	return "Machines"
}

// editeurApp : formulaire de création ou de modification.
type editeurApp struct {
	Ouvert       bool
	Ancien, Nom  string
	Candidats    []candidat
	Statut       bool
	StatutActive bool // page de statut publique activée
}

// historiqueApplications : périodes en panne / dégradées de chaque
// application (alertes gardées en base bien plus longtemps que les
// métriques brutes). Périodes en cours : fin = t.
func (c *Console) historiqueApplications(ctx context.Context, debut, t time.Time) map[string][]intervalleEtat {
	par := map[string][]intervalleEtat{}
	rows, err := c.db.QueryContext(ctx, `SELECT regle, etiquettes, debut, fin FROM alertes
		WHERE regle IN ('ApplicationEnPanne', 'ApplicationDegradee') AND active_le > 0 AND debut < ? AND (fin = 0 OR fin > ?)`,
		t.UnixMilli(), debut.UnixMilli())
	if err != nil {
		return par
	}
	defer rows.Close()
	for rows.Next() {
		var regle, etq string
		var deb, fin int64
		if rows.Scan(&regle, &etq, &deb, &fin) != nil {
			continue
		}
		var e map[string]string
		if json.Unmarshal([]byte(etq), &e) != nil {
			continue
		}
		if fin == 0 {
			fin = t.UnixMilli()
		}
		par[e["service"]] = append(par[e["service"]], intervalleEtat{deb / 1000, fin / 1000, regle == "ApplicationEnPanne"})
	}
	c.lectureIncomplete(rows.Err(), "historiqueApplications")
	return par
}

// friseApp : un jour par barre (fuseau de l'interface), le plus grave du jour.
func friseApp(iv []intervalleEtat, t time.Time) []jourApp {
	loc := fuseau.Load()
	j0 := t.In(loc).AddDate(0, 0, -(joursFriseApp - 1))
	j0 = time.Date(j0.Year(), j0.Month(), j0.Day(), 0, 0, 0, 0, loc)
	out := make([]jourApp, 0, joursFriseApp)
	for k := 0; k < joursFriseApp; k++ {
		j := j0.AddDate(0, 0, k)
		jf := j.AddDate(0, 0, 1)
		if jf.After(t) {
			jf = t
		}
		classe := "ok"
		var pannes []intervalle
		for _, x := range iv {
			if x.d < jf.Unix() && x.f > j.Unix() {
				if x.panne {
					classe = "pan"
					pannes = append(pannes, intervalle{x.d, x.f})
				} else if classe == "ok" {
					classe = "deg"
				}
			}
		}
		titre := j.Format("02/01")
		switch classe {
		case "pan":
			titre += " : en panne " + dureeArret(dureeDans(reunir(pannes), j.Unix(), jf.Unix()))
		case "deg":
			titre += " : dégradée"
		default:
			titre += " : OK"
		}
		out = append(out, jourApp{classe, titre})
	}
	return out
}

// depuisApp : depuis quand l'application est dans son état.
func depuisApp(e servicesmetier.EtatService, iv []intervalleEtat, t time.Time) string {
	if e.Etat != servicesmetier.OK {
		var debut int64
		for _, x := range iv {
			if x.f >= t.Unix()-60 && x.panne == (e.Etat == servicesmetier.Panne) && (debut == 0 || x.d < debut) {
				debut = x.d
			}
		}
		if debut == 0 {
			return "depuis peu"
		}
		return "depuis " + dureeLisible(t.Sub(time.Unix(debut, 0)))
	}
	var fin int64
	for _, x := range iv {
		fin = max(fin, x.f)
	}
	if fin == 0 {
		return "aucun problème en " + strconv.Itoa(joursFriseApp) + " j"
	}
	return "OK depuis " + dureeLisible(t.Sub(time.Unix(fin, 0)))
}

// causeApp : le premier élément en difficulté (clé d'abord).
func causeApp(e servicesmetier.EtatService) string {
	if e.Etat == servicesmetier.OK {
		return ""
	}
	for _, cle := range []bool{true, false} {
		for _, x := range e.Composants {
			if x.Cle == cle && (x.Classe == "erreur" || x.Classe == "attention") {
				return libelleSorte(x.Sorte) + x.Nom + " : " + x.Texte
			}
		}
	}
	return ""
}

func libelleSorte(s string) string {
	switch s {
	case servicesmetier.Conteneur:
		return "Conteneur "
	case servicesmetier.Sonde:
		return "Sonde "
	}
	return ""
}

// elementsApp : lien, icône et détail de chaque élément.
func elementsApp(e servicesmetier.EtatService, machines map[string]flotte.InfoMachine, sondes map[string]essais.Sonde) []elementApp {
	out := make([]elementApp, 0, len(e.Composants))
	for _, x := range e.Composants {
		el := elementApp{EtatComposant: x}
		switch x.Sorte {
		case servicesmetier.Machine:
			el.Icone, el.Lien = "ecran", "/machines/"+url.PathEscape(x.Nom)
			if m, ok := machines[x.Nom]; ok {
				el.Detail = strings.Trim(m.Site+" · "+m.Env, " ·")
			}
		case servicesmetier.Conteneur:
			el.Icone = "conteneur"
			if hote, nom, ok := strings.Cut(x.Nom, "/"); ok {
				el.Lien = "/conteneurs/" + url.PathEscape(hote) + "/" + url.PathEscape(nom)
				el.Detail = "sur " + hote
			}
		case servicesmetier.Sonde:
			el.Icone, el.Lien = "globe", "/sondes/"+url.PathEscape(x.Nom)
			if s, ok := sondes[x.Nom]; ok {
				el.Detail = s.Cible
			}
		}
		out = append(out, el)
	}
	return out
}

// infosApps : informations de la page et de la fiche, pour chaque état.
func (c *Console) infosApps(ctx context.Context, etats []servicesmetier.EtatService, t time.Time) map[string]infoApp {
	hist := c.historiqueApplications(ctx, t.AddDate(0, 0, -joursFriseApp), t)
	machines := map[string]flotte.InfoMachine{}
	if ms, err := c.machines(ctx); err == nil {
		for _, m := range ms {
			machines[m.Nom] = m
		}
	}
	sondes := map[string]essais.Sonde{}
	if ss, err := flotte.LireSondes(ctx, c.db); err == nil {
		for _, s := range ss {
			sondes[s.Nom] = s
		}
	}
	statut := map[string]bool{}
	for _, n := range lireStatut(ctx, c.db).Applications {
		statut[n] = true
	}
	out := map[string]infoApp{}
	for _, e := range etats {
		out[e.Nom] = infoApp{Depuis: depuisApp(e, hist[e.Nom], t), Cause: causeApp(e), Frise: friseApp(hist[e.Nom], t),
			Elements: elementsApp(e, machines, sondes), Statut: statut[e.Nom]}
	}
	return out
}

// conteneursConnus : conteneurs Docker vus (machine, nom, projet Compose).
type conteneurConnu struct{ Machine, Nom, Projet string }

func (c *Console) conteneursConnus(ctx context.Context, t time.Time) []conteneurConnu {
	var out []conteneurConnu
	ech, err := c.instant(ctx, `max by (instance, name, project) (last_over_time(node_docker_container_running[1h]))`, t)
	if err != nil {
		return nil
	}
	for _, e := range ech {
		m, n := e.Etiquettes.Get("instance"), e.Etiquettes.Get("name")
		if m != "" && n != "" {
			out = append(out, conteneurConnu{m, n, e.Etiquettes.Get("project")})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Machine != out[j].Machine {
			return out[i].Machine < out[j].Machine
		}
		return out[i].Nom < out[j].Nom
	})
	return out
}

// propositionsCompose : un projet Docker Compose (dès 1 conteneur) dont aucun
// conteneur n'est encore dans une application.
func propositionsCompose(cts []conteneurConnu, svcs []servicesmetier.Service) []proposition {
	pris := map[string]bool{}
	for _, s := range svcs {
		for _, x := range s.Composants {
			if x.Sorte == servicesmetier.Conteneur {
				pris[x.Nom] = true
			}
		}
	}
	par := map[string]*proposition{}
	var ordre []string
	for _, k := range cts {
		if k.Projet == "" {
			continue
		}
		cle := k.Machine + "\x00" + k.Projet
		p := par[cle]
		if p == nil {
			p = &proposition{Projet: k.Projet, Machine: k.Machine}
			par[cle] = p
			ordre = append(ordre, cle)
		}
		p.Conteneurs = append(p.Conteneurs, k.Nom)
	}
	var out []proposition
	for _, cle := range ordre {
		p := par[cle]
		libre := true
		for _, n := range p.Conteneurs {
			if pris[p.Machine+"/"+n] {
				libre = false
			}
		}
		if libre {
			out = append(out, *p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Projet < out[j].Projet })
	return out
}

// editeurDe : formulaire (vide, prérempli d'après une application ou une
// proposition). Ouvert si la page le demande.
func (c *Console) editeurDe(ctx context.Context, q *req, svcs []servicesmetier.Service, cts []conteneurConnu) editeurApp {
	v := q.r.URL.Query()
	ed := editeurApp{Ouvert: v.Get("nouvelle") != "" || v.Get("modifier") != "" || v.Get("proposer") != ""}
	st := lireStatut(ctx, c.db)
	ed.StatutActive = st.Actif
	index := map[string]int{}
	ajouter := func(k candidat) int {
		if i, ok := index[k.Valeur()]; ok {
			return i
		}
		ed.Candidats = append(ed.Candidats, k)
		index[k.Valeur()] = len(ed.Candidats) - 1
		return len(ed.Candidats) - 1
	}
	sondes, _ := flotte.LireSondes(ctx, c.db)
	for _, s := range sondes {
		d := s.Cible
		if s.Machine != "" {
			d += " · machine liée " + s.Machine
		}
		ajouter(candidat{Sorte: servicesmetier.Sonde, Nom: s.Nom, Detail: d, Hote: s.Machine})
	}
	for _, k := range cts {
		d := "sur " + k.Machine
		if k.Projet != "" {
			d += " · projet " + k.Projet
		}
		ajouter(candidat{Sorte: servicesmetier.Conteneur, Nom: k.Machine + "/" + k.Nom, Detail: d, Projet: k.Projet, Hote: k.Machine})
	}
	if ms, err := c.machines(ctx); err == nil {
		for _, m := range ms {
			if m.Statut == "approuvee" {
				ajouter(candidat{Sorte: servicesmetier.Machine, Nom: m.Nom, Detail: strings.Trim(m.Site+" · "+m.Env, " ·")})
			}
		}
	}
	choisir := func(sorte, nom string, cle bool) {
		i := ajouter(candidat{Sorte: sorte, Nom: nom, Detail: "plus vu"})
		ed.Candidats[i].Choisi = true
		ed.Candidats[i].Cle = ed.Candidats[i].Cle || cle
	}
	switch {
	case v.Get("modifier") != "":
		for _, s := range svcs {
			if s.Nom == v.Get("modifier") {
				ed.Ancien, ed.Nom = s.Nom, s.Nom
				for _, x := range s.Composants {
					choisir(x.Sorte, x.Nom, x.Cle)
				}
			}
		}
		for _, n := range st.Applications {
			if n == ed.Ancien {
				ed.Statut = true
			}
		}
	case v.Get("proposer") != "":
		p, m := v.Get("proposer"), v.Get("machine")
		ed.Nom = majusculeDebut(p)
		for _, k := range cts {
			if k.Projet == p && k.Machine == m {
				choisir(servicesmetier.Conteneur, k.Machine+"/"+k.Nom, false)
			}
		}
		choisir(servicesmetier.Machine, m, false)
		// Sonde liée à la machine et qui porte le nom du projet.
		for _, s := range sondes {
			if s.Machine == m && (strings.Contains(strings.ToLower(s.Nom), strings.ToLower(p)) || strings.Contains(strings.ToLower(s.Cible), strings.ToLower(p))) {
				choisir(servicesmetier.Sonde, s.Nom, true)
			}
		}
		ed.Statut = st.Actif
	}
	return ed
}

// lireElementsFormulaire : éléments cochés (« el » = sorte|nom, « cle »).
// Ancien formulaire (zone de texte « elements ») toujours accepté.
func lireElementsFormulaire(q *req) ([]servicesmetier.Composant, error) {
	if err := q.r.ParseForm(); err != nil {
		return nil, err
	}
	els := q.r.PostForm["el"]
	if len(els) == 0 {
		return servicesmetier.LireLignes(q.r.PostFormValue("elements"))
	}
	cles := map[string]bool{}
	for _, k := range q.r.PostForm["cle"] {
		cles[k] = true
	}
	var out []servicesmetier.Composant
	vus := map[string]bool{}
	for _, el := range els {
		sorte, nom, ok := strings.Cut(el, "|")
		if !ok || vus[el] {
			continue
		}
		vus[el] = true
		out = append(out, servicesmetier.Composant{Sorte: sorte, Nom: nom, Cle: cles[el]})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("choisir au moins un élément")
	}
	return out, nil
}

// statutApplication : ajoute, retire ou renomme l'application dans la page
// de statut publique (si la case est présente dans le formulaire).
func (c *Console) statutApplication(ctx context.Context, q *req, ancien, nom string, montrer, supprimer bool) error {
	r := lireStatut(ctx, c.db)
	avant := strings.Join(r.Applications, ",")
	var garde []string
	for _, a := range r.Applications {
		if a != ancien && a != nom {
			garde = append(garde, a)
		}
	}
	if montrer && !supprimer {
		garde = append(garde, nom)
	}
	r.Applications = garde
	if strings.Join(r.Applications, ",") == avant {
		return nil
	}
	brut, _ := json.Marshal(r)
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // sans effet après Commit
	if _, err := tx.ExecContext(ctx, `INSERT INTO reglages(cle, valeur, modifie_le, modifie_par) VALUES (?, ?, ?, ?)
		ON CONFLICT(cle) DO UPDATE SET valeur = excluded.valeur, modifie_le = excluded.modifie_le, modifie_par = excluded.modifie_par`,
		cleStatut, string(brut), q.t.Unix(), q.qui()); err != nil {
		return err
	}
	if err := Auditer(ctx, tx, q.qui(), "page_statut", "", avant, "applications="+strings.Join(r.Applications, ","), q.ip.String(), q.t); err != nil {
		return err
	}
	return tx.Commit()
}
