// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"net/netip"
	"net/url"
	"slices"
	"strings"
)

// Page Machines (SPEC § 36) : machines regroupées par site (ou par groupe,
// ou en simple liste), filtres et recherche lus dans l'adresse de la page
// (/machines?vue=groupe&filtre=problemes&q=web), problèmes en premier.
// Tout est calculé côté serveur : pas de JavaScript, rien en plus en base.

// Vues et filtres admis (toute autre valeur revient au choix par défaut).
var (
	vuesMachines    = []string{"site", "groupe", "liste"}
	filtresMachines = []string{"", "problemes", "agents", "collecteurs"}
)

// attenteVisibles : machines en attente montrées d'office ; au-delà, un
// lien « Voir les N autres ».
const attenteVisibles = 5

// filtresPage : choix lus dans l'adresse de la page.
type filtresPage struct {
	Vue, Filtre, Q string
}

// lireFiltresMachines : vue, filtre et recherche, ramenés aux valeurs admises.
func lireFiltresMachines(vue, filtre, q string) filtresPage {
	f := filtresPage{Vue: "site", Q: strings.TrimSpace(q)}
	if slices.Contains(vuesMachines, vue) {
		f.Vue = vue
	}
	if slices.Contains(filtresMachines, filtre) {
		f.Filtre = filtre
	}
	if len(f.Q) > 100 { // recherche bornée : rien à gagner au-delà
		f.Q = f.Q[:100]
	}
	return f
}

// Lien : adresse de la page avec ces choix, en changeant une seule clé.
// Les valeurs par défaut sont omises (adresse courte).
func (f filtresPage) Lien(cle, valeur string) string {
	switch cle {
	case "vue":
		f.Vue = valeur
	case "filtre":
		f.Filtre = valeur
	}
	var p []string
	if f.Vue != "site" {
		p = append(p, "vue="+f.Vue)
	}
	if f.Filtre != "" {
		p = append(p, "filtre="+f.Filtre)
	}
	if f.Q != "" {
		p = append(p, "q="+url.QueryEscape(f.Q))
	}
	if len(p) == 0 {
		return "/machines"
	}
	return "/machines?" + strings.Join(p, "&")
}

// compteursFiltres : nombre de machines derrière chaque filtre (après la
// recherche), affiché sur les boutons.
type compteursFiltres struct {
	Toutes, Problemes, Agents, Collecteurs int
}

// groupeMachines : un bloc de la page (un site, un groupe, ou la liste).
type groupeMachines struct {
	Titre     string // vide : « sans site » / « sans groupe » ; vue liste : pas de titre
	Machines  []vueMachine
	Problemes int
	Tuiles    []tuileMachine // présentation en tuiles (SPEC § 120)
}

// Probleme : machine muette ou dont la mise à jour a échoué.
func (v vueMachine) Probleme() bool { return v.Muette || v.MajEchec != "" }

// Collecteur : machine de type collecteur du LAN.
func (v vueMachine) Collecteur() bool { return v.Type == "collecteur" }

// garde : la machine passe-t-elle la recherche et le filtre ?
func (f filtresPage) garde(v vueMachine) bool {
	if !f.trouve(v) {
		return false
	}
	switch f.Filtre {
	case "problemes":
		return v.Probleme()
	case "agents":
		return !v.Collecteur() && v.ReleveePar == ""
	case "collecteurs":
		return v.Collecteur() || v.ReleveePar != ""
	}
	return true
}

// trouve : la recherche (sans casse) est dans le nom, le site, le type ou le groupe.
func (f filtresPage) trouve(v vueMachine) bool {
	if f.Q == "" {
		return true
	}
	// « site:x », « type:x », « groupe:x » : valeur exacte de ce champ
	// (lien « +N » de Réglages › Sites, relecture du 21/11 : la recherche
	// libre comptait aussi les machines dont le nom contient le mot).
	if cle, val, ok := strings.Cut(f.Q, ":"); ok {
		switch cle {
		case "site":
			return strings.EqualFold(v.Site, val)
		case "type":
			return strings.EqualFold(v.Env, val)
		case "groupe":
			return strings.EqualFold(v.Groupe, val)
		}
	}
	q := strings.ToLower(f.Q)
	for _, champ := range []string{v.Nom, v.Site, v.Env, v.Groupe} {
		if strings.Contains(strings.ToLower(champ), q) {
			return true
		}
	}
	return false
}

// compter : nombre de machines par filtre, recherche appliquée.
func (f filtresPage) compter(ms []vueMachine) compteursFiltres {
	var c compteursFiltres
	for _, v := range ms {
		if !f.trouve(v) {
			continue
		}
		c.Toutes++
		if v.Probleme() {
			c.Problemes++
		}
		if v.Collecteur() || v.ReleveePar != "" {
			c.Collecteurs++
		} else {
			c.Agents++
		}
	}
	return c
}

// regrouperMachines : machines filtrées, rangées en blocs selon la vue. Dans un
// bloc : problèmes en premier, puis par nom ; un équipement relevé par un
// collecteur du même bloc est placé juste sous lui (Sous = vrai).
func regrouperMachines(ms []vueMachine, f filtresPage) []groupeMachines {
	cle := func(v vueMachine) string {
		switch f.Vue {
		case "groupe":
			return v.Groupe
		case "liste":
			return ""
		}
		return v.Site
	}
	parCle := map[string][]vueMachine{}
	var ordre []string
	for _, v := range ms {
		if !f.garde(v) {
			continue
		}
		k := cle(v)
		if _, ok := parCle[k]; !ok {
			ordre = append(ordre, k)
		}
		parCle[k] = append(parCle[k], v)
	}
	// Blocs par nom ; le bloc « sans site » (titre vide) en dernier.
	slices.SortFunc(ordre, func(a, b string) int {
		if (a == "") != (b == "") {
			if a == "" {
				return 1
			}
			return -1
		}
		return strings.Compare(a, b)
	})
	out := make([]groupeMachines, 0, len(ordre))
	for _, k := range ordre {
		g := groupeMachines{Titre: k, Machines: ranger(parCle[k])}
		for _, v := range g.Machines {
			if v.Probleme() {
				g.Problemes++
			}
		}
		out = append(out, g)
	}
	return out
}

// ranger : ordre d'un bloc (voir regrouperMachines).
func ranger(ms []vueMachine) []vueMachine {
	presents := map[string]bool{}
	for _, v := range ms {
		presents[v.Nom] = true
	}
	// Équipements placés sous leur collecteur (s'il est dans le bloc).
	enfants := map[string][]vueMachine{}
	var tete []vueMachine
	for _, v := range ms {
		if v.ReleveePar != "" && presents[v.ReleveePar] {
			v.Sous = true
			enfants[v.ReleveePar] = append(enfants[v.ReleveePar], v)
			continue
		}
		tete = append(tete, v)
	}
	// Un collecteur dont un équipement a un problème remonte avec lui.
	probleme := func(v vueMachine) bool {
		if v.Probleme() {
			return true
		}
		return slices.ContainsFunc(enfants[v.Nom], vueMachine.Probleme)
	}
	parProbleme := func(a, b vueMachine) int {
		pa, pb := probleme(a), probleme(b)
		if pa != pb {
			if pa {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Nom, b.Nom)
	}
	slices.SortFunc(tete, parProbleme)
	out := make([]vueMachine, 0, len(ms))
	for _, v := range tete {
		out = append(out, v)
		e := enfants[v.Nom]
		slices.SortFunc(e, parProbleme)
		out = append(out, e...)
	}
	return out
}

// --- Machines en attente : deux signaux pour regarder à deux fois ---------
// Ce ne sont que des indices (pastilles orange) : rien n'est bloqué.

// nomsBanals : noms d'hôte laissés par défaut par les installations
// courantes. Une alerte « debian ne répond plus » ne dit pas de quelle
// machine il s'agit, et un second « debian » serait refusé.
var nomsBanals = map[string]bool{
	"localhost": true, "localhost.localdomain": true, "localdomain": true,
	"debian": true, "ubuntu": true, "raspberrypi": true, "raspberry": true, "pi": true,
	"pve": true, "proxmox": true, "dietpi": true, "armbian": true, "alpine": true,
	"centos": true, "fedora": true, "rocky": true, "almalinux": true, "archlinux": true,
	"linux": true, "server": true, "serveur": true, "vps": true, "host": true,
	"machine": true, "test": true, "default": true, "node": true, "srv": true,
}

// prefixesBanals : noms générés par les hébergeurs (« vps-1a2b3c4d » chez
// OVH, « ip-10-0-0-5 » chez AWS, « ubuntu-s-1vcpu-1gb… » chez DigitalOcean).
var prefixesBanals = []string{"vps-", "ip-", "ubuntu-", "debian-", "localhost"}

// nomBanal : nom proposé par la machine (pas choisi au jeton) et trop
// commun pour reconnaître la machine.
func nomBanal(nom string) bool {
	n := strings.ToLower(nom)
	if nomsBanals[n] {
		return true
	}
	for _, p := range prefixesBanals {
		if strings.HasPrefix(n, p) {
			return true
		}
	}
	return false
}

// reseauDe : réseau d'une adresse (/24 en IPv4, /64 en IPv6). Deux
// machines derrière la même box ou chez le même hébergeur y tombent souvent.
func reseauDe(ip string) (netip.Prefix, bool) {
	a, err := netip.ParseAddr(strings.TrimSpace(ip))
	if err != nil {
		// Adresse avec port (« 203.0.113.7:51234 ») : on garde l'adresse.
		ap, err2 := netip.ParseAddrPort(strings.TrimSpace(ip))
		if err2 != nil {
			return netip.Prefix{}, false
		}
		a = ap.Addr()
	}
	a = a.Unmap()
	bits := 64
	if a.Is4() {
		bits = 24
	}
	p, err := a.Prefix(bits)
	return p, err == nil
}

// reseauxConnus : réseaux des machines approuvées (dernière adresse vue et
// adresse d'enrôlement).
func reseauxConnus(approuvees []vueMachine) map[netip.Prefix]bool {
	out := map[netip.Prefix]bool{}
	for _, m := range approuvees {
		for _, ip := range []string{m.DerniereIP, m.IPEnrolement} {
			if p, ok := reseauDe(ip); ok {
				out[p] = true
			}
		}
	}
	return out
}

// adresseInconnue : la demande vient d'un réseau où aucune machine
// approuvée n'a été vue. Rien n'est signalé tant qu'aucune machine n'est
// connue (première machine) ou si l'adresse est illisible.
func adresseInconnue(ip string, connus map[netip.Prefix]bool) bool {
	if len(connus) == 0 {
		return false
	}
	p, ok := reseauDe(ip)
	return ok && !connus[p]
}
