// SPDX-License-Identifier: GPL-3.0-or-later

package console

// Flux bloqués, vue « Mon réseau » d'un grand parc (SPEC § 140, maquette
// validée le 27/11) : au-delà de 20 pare-feu, la colonne du milieu montre
// les sites (ou les groupes) ; un clic sur un site montre ses machines ;
// un clic sur une machine, son détail. Recherche d'une machine par son nom.

import (
	"context"
	"net/url"
	"sort"

	"mirador-stack/internal/server/fluxbloques"
)

// seuilRegroupe : au-delà, regroupement par site d'office.
const seuilRegroupe = 20

// vueParc : schéma, classements, chemins et barre de choix de la page.
func (c *Console) vueParc(ctx context.Context, d *donneesFlux, b fluxbloques.Bilan, v url.Values, f fluxbloques.Filtre,
	tousPF []fluxbloques.Compte, rangs map[string]int, outils map[string]string) {
	nb := len(rangs)
	par := v.Get("par")
	if par != "site" && par != "groupe" && par != "machine" {
		par = ""
		if nb > seuilRegroupe {
			par = "site"
		}
	}
	grand := nb > maxPFSchema
	// Appartenance de chaque machine à un site ou un groupe (inventaire).
	var de map[string]string
	if par == "site" || par == "groupe" {
		de = c.appartenancePF(ctx, par)
	}
	groupeDe := func(pf string) string {
		if g := de[pf]; g != "" {
			return g
		}
		if par == "groupe" {
			return "Sans groupe"
		}
		return "Sans site"
	}
	choisi := ""
	if de != nil {
		choisi = v.Get(par)
	}
	if d.Vue != "reseau" || f.Pays != "" || f.Port != "" {
		// Vue Monde ou page filtrée sur un pays ou un port : pas de
		// regroupement, schéma et classements comme avant.
		de, choisi = nil, ""
	}

	// Recherche : toutes les machines (ou celles du site choisi).
	if grand {
		for _, x := range tousPF {
			if x.Nom != fluxbloques.Autres && (choisi == "" || groupeDe(x.Nom) == choisi) {
				d.ChercherPF = append(d.ChercherPF, x.Nom)
			}
		}
		d.NbTotalPF = len(d.ChercherPF)
		for _, k := range []string{"vue", "periode", "sens", "par", "site", "groupe"} {
			if x := v.Get(k); x != "" {
				d.ChampsCaches = append(d.ChampsCaches, [2]string{k, x})
			}
		}
		d.Regroupements = []lienFlux{{"Site", lienFluxAvec(v, "par", "site", false), par == "site"},
			{"Groupe", lienFluxAvec(v, "par", "groupe", false), par == "groupe"},
			{"Machine", lienFluxAvec(v, "par", "machine", false), par == "machine" || par == ""}}
	}
	nomNiveau := map[string]string{"site": "sites", "groupe": "groupes"}[par]
	tous := lienFluxAvec(v, "par", v.Get("par"), false) // même regroupement, rien de choisi

	switch {
	case de != nil && choisi == "" && f.PareFeu == "":
		// Tous les pare-feu, regroupés : un bloc par site.
		bg, membres := regrouperBilan(b, groupeDe)
		rg := map[string]int{}
		og := map[string]string{}
		for i, x := range bg.PareFeux {
			rg[x.Nom], og[x.Nom] = i, nbMachines(membres[x.Nom])
		}
		d.Sankey = construireSankey(bg, v, og, rg)
		if d.Sankey != nil {
			d.Sankey.NomPF = map[string]string{"site": "SITES", "groupe": "GROUPES"}[par]
			d.Sankey.NbPF = len(bg.PareFeux)
			for i := range d.Sankey.PareFeux {
				if k := d.Sankey.PareFeux[i].Cle; k != fluxbloques.Autres {
					d.Sankey.PareFeux[i].Lien = lienFluxAvec(v, par, k, false)
				}
			}
		}
		d.Classement = c.classementFlux(b, v, outils, rangs, d.Sortant, d.GeoIP)
		d.Chemins = cheminsVue(b.Chemins, v)
		d.Ariane = []lienFlux{{"Tous les pare-feu · " + nombreLisible(nb), "", true}}
		d.TitreChoix = map[string]string{"site": "Sites", "groupe": "Groupes"}[par]
		d.ChoixPF = []choixPF{{Nom: "Tous", N: nombreLisible(len(bg.PareFeux)) + " " + nomNiveau, Lien: tous, Actif: true}}
		for _, x := range bg.PareFeux {
			d.ChoixPF = append(d.ChoixPF, choixPF{Nom: x.Nom, N: og[x.Nom], Couleur: couleurPF(rg, x.Nom), Lien: lienFluxAvec(v, par, x.Nom, false)})
		}

	case de != nil && (choisi != "" || f.PareFeu != ""):
		// Un site choisi (ou une machine d'un grand parc) : les machines du site.
		site := choisi
		if f.PareFeu != "" {
			site = groupeDe(f.PareFeu)
		}
		dans := func(pf string) bool { return groupeDe(pf) == site }
		bs := b
		if f.PareFeu == "" {
			bs = restreindreBilan(b, dans)
			d.B.Total, d.B.Pays, d.B.Ports, d.B.NbPays = bs.Total, bs.Pays, bs.Ports, bs.NbPays
			d.B.SansAdresses, d.Comparaison = true, ""
		}
		// Couleurs : rang dans le site.
		rs := map[string]int{}
		var duSite []fluxbloques.Compte
		for _, x := range tousPF {
			if x.Nom != fluxbloques.Autres && dans(x.Nom) {
				rs[x.Nom] = len(rs)
				duSite = append(duSite, x)
			}
		}
		d.Sankey = construireSankey(bs, v, outils, rs)
		d.Classement = c.classementFlux(bs, v, outils, rs, d.Sortant, d.GeoIP)
		d.Chemins = cheminsVue(bs.Chemins, v)
		d.Ariane = []lienFlux{{"Tous les pare-feu", tous, false}}
		if f.PareFeu == "" {
			d.Ariane = append(d.Ariane, lienFlux{site + " · " + nbMachines(len(duSite)), "", true})
			d.TitreChoix = "Les plus actives"
		} else {
			d.Ariane = append(d.Ariane, lienFlux{site, lienFluxAvec(v, par, site, false), false}, lienFlux{f.PareFeu, "", true})
			d.TitreChoix = site
		}
		for i, x := range duSite {
			if i >= maxPFSchema && x.Nom != f.PareFeu {
				continue
			}
			d.ChoixPF = append(d.ChoixPF, choixPF{Nom: x.Nom, N: nombreLisible(x.N), Couleur: couleurPF(rs, x.Nom), Outil: outils[x.Nom],
				Lien: lienFluxAvec(v, "pf", x.Nom, false), Actif: f.PareFeu == x.Nom})
		}
		if len(duSite) > maxPFSchema && f.PareFeu == "" {
			d.NbPF = len(duSite)
			d.LienTous = lienFluxAvec(v, "tous", map[bool]string{false: "1", true: ""}[d.ToutPF], false)
		}

	default:
		// Petit parc, ou regroupement « Machine » : une machine par bloc.
		d.Sankey = construireSankey(b, v, outils, rangs)
		d.Classement = c.classementFlux(b, v, outils, rangs, d.Sortant, d.GeoIP)
		d.Chemins = cheminsVue(b.Chemins, v)
		if nb > 1 || f.PareFeu != "" {
			d.TitreChoix = "Pare-feu"
			d.ChoixPF = append(d.ChoixPF, choixPF{Nom: "Tous", N: nombreLisible(nb) + " pare-feu", Lien: lienFluxAvec(v, "pf", "", false), Actif: f.PareFeu == ""})
			for i, x := range tousPF {
				if x.Nom == fluxbloques.Autres || (grand && i >= maxPFSchema && x.Nom != f.PareFeu) {
					continue // grand parc : les 8 plus actives, la recherche pour les autres
				}
				d.ChoixPF = append(d.ChoixPF, choixPF{Nom: x.Nom, N: nombreLisible(x.N), Couleur: couleurPF(rangs, x.Nom), Outil: outils[x.Nom],
					Lien: lienFluxAvec(v, "pf", x.Nom, false), Actif: f.PareFeu == x.Nom})
			}
		}
		if grand && f.PareFeu == "" {
			d.NbPF = nb
			d.LienTous = lienFluxAvec(v, "tous", map[bool]string{false: "1", true: ""}[d.ToutPF], false)
		}
		if grand && f.PareFeu != "" {
			d.Ariane = []lienFlux{{"Tous les pare-feu", lienFluxAvec(v, "pf", "", false), false}, {f.PareFeu, "", true}}
		}
	}
}

// appartenancePF : site (ou groupe) de chaque machine de l'inventaire.
func (c *Console) appartenancePF(ctx context.Context, par string) map[string]string {
	out := map[string]string{}
	ms, err := c.machines(ctx)
	if err != nil {
		return out
	}
	for _, m := range ms {
		if par == "groupe" {
			out[m.Nom] = m.Groupe
		} else {
			out[m.Nom] = m.Site
		}
	}
	return out
}

// regrouperBilan : pare-feu remplacés par leur site ; membres : machines
// vues de chaque site sur la période.
func regrouperBilan(b fluxbloques.Bilan, groupeDe func(string) string) (fluxbloques.Bilan, map[string]int) {
	r := b
	r.PaysPareFeu, r.PareFeuPort = map[fluxbloques.Paire]int{}, map[fluxbloques.Paire]int{}
	for k, n := range b.PaysPareFeu {
		r.PaysPareFeu[fluxbloques.Paire{A: k.A, B: groupeDe(k.B)}] += n
	}
	for k, n := range b.PareFeuPort {
		r.PareFeuPort[fluxbloques.Paire{A: groupeDe(k.A), B: k.B}] += n
	}
	totaux, membres := map[string]int{}, map[string]int{}
	for _, x := range b.PareFeux {
		if x.Nom == fluxbloques.Autres {
			continue
		}
		g := groupeDe(x.Nom)
		totaux[g] += x.N
		membres[g]++
	}
	r.PareFeux = trierComptes(totaux)
	r.Chemins = nil
	return r, membres
}

// restreindreBilan : seulement les pare-feu gardés (un site). Les adresses
// ne sont pas suivies par pare-feu : la liste est laissée vide.
func restreindreBilan(b fluxbloques.Bilan, garder func(string) bool) fluxbloques.Bilan {
	r := fluxbloques.Bilan{PaysPareFeu: map[fluxbloques.Paire]int{}, PareFeuPort: map[fluxbloques.Paire]int{}, Pic: -1,
		SansAdresses: true, Estime: b.Estime, Barres: b.Barres, LargeurBarre: b.LargeurBarre}
	pays, ports := map[string]int{}, map[string]int{}
	for k, n := range b.PaysPareFeu {
		if garder(k.B) {
			r.PaysPareFeu[k] += n
			pays[k.A] += n
			r.Total += n
		}
	}
	for k, n := range b.PareFeuPort {
		if garder(k.A) {
			r.PareFeuPort[k] += n
			ports[k.B] += n
		}
	}
	for _, x := range b.PareFeux {
		if x.Nom != fluxbloques.Autres && garder(x.Nom) {
			r.PareFeux = append(r.PareFeux, x)
		}
	}
	for _, x := range b.Chemins {
		if garder(x.PareFeu) {
			r.Chemins = append(r.Chemins, x)
		}
	}
	r.Pays, r.Ports = trierComptes(pays), trierComptes(ports)
	for _, x := range r.Pays {
		if x.Nom != fluxbloques.Autres && x.Nom != "" {
			r.NbPays++
		}
	}
	return r
}

// trierComptes : du plus gros au plus petit (nom en cas d'égalité).
func trierComptes(m map[string]int) []fluxbloques.Compte {
	out := make([]fluxbloques.Compte, 0, len(m))
	for k, n := range m {
		out = append(out, fluxbloques.Compte{Nom: k, N: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].N != out[j].N {
			return out[i].N > out[j].N
		}
		return out[i].Nom < out[j].Nom
	})
	return out
}

// nbMachines : « 1 machine », « 9 machines ».
func nbMachines(n int) string {
	if n == 1 {
		return "1 machine"
	}
	return nombreLisible(n) + " machines"
}
