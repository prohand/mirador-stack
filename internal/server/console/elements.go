// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"mirador-stack/internal/server/alertes"
	"mirador-stack/internal/server/flotte"
	"mirador-stack/internal/server/notif"
	"mirador-stack/internal/server/servicesmetier"
	essais "mirador-stack/internal/sondes"
)

// Registre des éléments (SPEC § 55, idée reprise de New Relic « entities ») :
// tout ce que mirador surveille, au même endroit et de la même façon —
// machines, collecteurs, équipements, conteneurs, sondes, services — avec
// leur état et leurs liens. Une seule construction (elementsDuParc), lue
// par la ruche, la page Éléments, les fiches et Ctrl+K : chaque page ne
// refait plus sa propre liste.

// Sortes d'éléments.
const (
	sorteMachine    = "machine"
	sorteCollecteur = "collecteur"
	sorteEquipement = "équipement"
	sorteConteneur  = "conteneur"
	sorteSonde      = "sonde"
	sorteService    = "service"
)

// lienElement : relation vers un autre élément.
type lienElement struct {
	Quoi, Nom, Lien string // « fait partie de », « Domotique », « /services »
}

// element : un élément surveillé.
type element struct {
	Cle        string // « m:pve1 », « c:vm-docker/ha », « s:HA », « v:Domotique »
	Nom, Sorte string
	Sous       string // petite info (CPU, état, temps de réponse)
	Etat       string // texte
	Classe     string // ok, av, cr, mu, ma (comme la ruche)
	Lien       string // fiche
	Groupe     string // site, machine du conteneur, origine de la sonde
	Liens      []lienElement
	Etiquettes map[string]string // § 59
}

// classesPastille : classe de ruche → classe de pastille.
var classesPastille = map[string]string{"ok": "ok", "av": "attention", "cr": "erreur", "mu": "", "ma": "info"}

// Pastille : classe de pastille de l'état.
func (e element) Pastille() string { return classesPastille[e.Classe] }

// elementsDuParc : tous les éléments, avec leurs liens.
func (c *Console) elementsDuParc(ctx context.Context, parc []ligneParc, t time.Time) []element {
	var out []element
	maint := c.machinesEnMaintenance(ctx, t)
	releves, _ := flotte.RelevesParCollecteur(ctx, c.db)
	types := map[string]string{}
	if ms, err := flotte.ListerMachines(ctx, c.db); err == nil {
		for _, m := range ms {
			types[m.Nom] = m.Type
		}
	}
	etiq := c.lireEtiquettes(ctx)
	// Services (et leurs éléments) d'abord : liens « fait partie de ».
	parties := map[string][]string{}
	var etatsServices []servicesmetier.EtatService
	if svcs, err := servicesmetier.Lire(ctx, c.db); err == nil && len(svcs) > 0 {
		r, _ := notif.LireReglages(ctx, c.db)
		var enCours []alertes.Alerte
		if c.opt.Alertes != nil {
			enCours = c.opt.Alertes.EnCours()
		}
		etatsServices = servicesmetier.Calculer(ctx, c.lecteur, enCours, r.ReglesPanne, svcs, t)
		for _, s := range svcs {
			for _, x := range s.Composants {
				k := map[string]string{servicesmetier.Machine: "m:", servicesmetier.Conteneur: "c:", servicesmetier.Sonde: "s:"}[x.Sorte] + x.Nom
				parties[k] = append(parties[k], s.Nom)
			}
		}
	}
	partieDe := func(k string) []lienElement {
		var l []lienElement
		for _, s := range parties[k] {
			l = append(l, lienElement{"fait partie de", s, "/services"})
		}
		return l
	}
	parMachine := map[string]int{}
	for _, m := range parc {
		e := element{Cle: "m:" + m.Nom, Nom: m.Nom, Sorte: sorteMachine, Etat: m.Etat, Classe: "ok", Lien: "/machines/" + url.PathEscape(m.Nom), Groupe: m.Site}
		if m.CPU != "—" {
			e.Sous = m.CPU + " CPU"
		}
		switch {
		case maint[m.Nom]:
			e.Classe, e.Etat, e.Sous = "ma", "Maintenance", "maintenance"
		case m.Muette:
			e.Classe, e.Sous = "mu", "muette "+strings.TrimPrefix(depuis(m.VuLe*1000), "il y a ")
		case m.ClasseEtat == "erreur":
			e.Classe = "cr"
		case m.ClasseEtat == "attention":
			e.Classe = "av"
		}
		if types[m.Nom] == "collecteur" {
			e.Sorte = sorteCollecteur
		}
		if col, ok := releves[m.Nom]; ok {
			e.Sorte = sorteEquipement
			e.Liens = append(e.Liens, lienElement{"relevé par", col, "/machines/" + url.PathEscape(col)})
		}
		if e.Groupe == "" {
			e.Groupe = "Sans site"
		}
		e.Liens = append(e.Liens, partieDe(e.Cle)...)
		e.Etiquettes = etiq[e.Cle]
		parMachine[m.Nom] = len(out)
		out = append(out, e)
	}
	for _, l := range c.conteneursDuParc(ctx, t) {
		e := element{Cle: "c:" + l.Machine + "/" + l.Nom, Nom: l.Nom, Sorte: sorteConteneur, Etat: l.Etat, Classe: "ok", Groupe: l.Machine,
			Lien: "/conteneurs/" + url.PathEscape(l.Machine) + "/" + url.PathEscape(l.Nom), Sous: l.Etat}
		switch {
		case l.Probleme && l.ClasseEtat == "erreur" || l.ClasseSante == "erreur" || l.OOM:
			e.Classe = "cr"
		case l.Etat != "En marche":
			e.Classe = "mu"
		}
		if l.Sante != "" && l.Etat == "En marche" {
			e.Sous, e.Etat = l.Sante, l.Etat+" · "+l.Sante
		}
		e.Liens = append([]lienElement{{"sur la machine", l.Machine, "/machines/" + url.PathEscape(l.Machine)}}, partieDe(e.Cle)...)
		e.Etiquettes = etiq[e.Cle]
		out = append(out, e)
	}
	if ss, err := flotte.LireSondes(ctx, c.db); err == nil && len(ss) > 0 {
		lues := c.sondesLues(ctx, t)
		for _, s := range ss {
			k := cleSonde(instanceDe(s), s.Nom)
			e := element{Cle: "s:" + s.Nom, Nom: s.Nom, Sorte: sorteSonde + " " + s.Type, Classe: "mu", Etat: "Pas encore d'essai", Sous: "pas d'essai",
				Lien: "/sondes/" + url.PathEscape(s.Nom)}
			if v, ok := lues["succes"][k]; ok {
				ms := strconv.FormatFloat(lues["duree"][k].Valeur*1000, 'f', 0, 64) + " ms"
				switch {
				case v.Valeur == 0:
					e.Classe, e.Etat, e.Sous = "cr", "Échec", "échec"
				case lues["lente"][k].Valeur == 1:
					e.Classe, e.Etat, e.Sous = "av", "Lente · "+ms, "lente · "+ms
				default:
					e.Classe, e.Etat, e.Sous = "ok", "OK · "+ms, ms
				}
			}
			if s.Depuis == essais.ServeurLocal {
				e.Groupe = "Sondes depuis le serveur"
				e.Liens = append(e.Liens, lienElement{"faite depuis", "le serveur", ""})
			} else {
				e.Groupe = "Sondes depuis " + s.Depuis
				e.Liens = append(e.Liens, lienElement{"faite depuis", s.Depuis, "/machines/" + url.PathEscape(s.Depuis)})
			}
			e.Liens = append(e.Liens, partieDe(e.Cle)...)
			e.Etiquettes = etiq[e.Cle]
			out = append(out, e)
		}
	}
	for _, s := range etatsServices {
		classe := map[int]string{servicesmetier.OK: "ok", servicesmetier.Degrade: "av", servicesmetier.Panne: "cr"}[s.Etat]
		e := element{Cle: "v:" + s.Nom, Nom: s.Nom, Sorte: sorteService, Etat: s.Texte, Classe: classe, Sous: s.Texte, Groupe: "Services",
			Lien: "/services/" + url.PathEscape(s.Nom)}
		for _, x := range s.Composants {
			e.Liens = append(e.Liens, lienElement{"comprend", x.Sorte + " " + x.Nom, ""})
		}
		e.Etiquettes = etiq[e.Cle]
		out = append(out, e)
	}
	return out
}

// --- Page « Éléments » ----------------------------------------------------------------

type donneesElements struct {
	Lignes  []element
	Sorte   string
	Compte  map[string]int
	Sortes  []string
	Filtre  string // étiquette « clé=valeur » (§ 59)
}

func (c *Console) pageElements(q *req) {
	ctx := q.r.Context()
	d := donneesElements{Sorte: q.r.URL.Query().Get("sorte"), Filtre: strings.TrimSpace(q.r.URL.Query().Get("etiquette")), Compte: map[string]int{}}
	acc := c.donneesAccueil(q)
	cleF, valF, avecFiltre := strings.Cut(d.Filtre, "=")
	for _, e := range c.elementsDuParc(ctx, acc.Parc, q.t) {
		sorte := strings.SplitN(e.Sorte, " ", 2)[0]
		if avecFiltre && e.Etiquettes[strings.TrimSpace(cleF)] != strings.TrimSpace(valF) {
			continue
		}
		d.Compte[sorte]++
		d.Compte[""]++
		if d.Sorte != "" && sorte != d.Sorte {
			continue
		}
		d.Lignes = append(d.Lignes, e)
	}
	sort.SliceStable(d.Lignes, func(i, j int) bool {
		a, b := rangsRuche[d.Lignes[i].Classe], rangsRuche[d.Lignes[j].Classe]
		if a != b {
			return a < b
		}
		return d.Lignes[i].Nom < d.Lignes[j].Nom
	})
	d.Sortes = []string{sorteMachine, sorteCollecteur, sorteEquipement, sorteConteneur, sorteSonde, sorteService}
	c.rendre(q, "elements", "Éléments", "elements", d)
}
