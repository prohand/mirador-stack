// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"errors"
	"maps"
	"net/netip"
	"regexp"
	"sort"
	"strings"
	"time"

	"mirador-stack/internal/motif"
	"mirador-stack/internal/server/stockage"
	"mirador-stack/internal/server/stockage/journaux"
)

// Page « Logs » (SPEC § 18.5). Le contenu des logs vient des machines : il
// est TOUJOURS affiché comme du texte (html/template échappe tout, la CSP
// interdit les scripts en ligne). Recherche bornée par le moteur (31 j,
// 10 000 lignes, 10 s) et ici : 2 recherches en même temps au plus.

func (c *Console) routesLogs() {
	c.mux.HandleFunc("GET /logs", c.lecture(c.pageLogs))
	c.mux.HandleFunc("GET /logs/direct", c.lecture(c.logsDirect))
	c.mux.HandleFunc("GET /logs/flux", c.lecture(c.logsFlux)) // logs en direct poussés (SPEC § 99)
	c.mux.HandleFunc("GET /logs/export", c.lecture(c.logsExport))
	c.mux.HandleFunc("POST /logs/recherches", c.lecture(c.postRechercheLogs))
	c.mux.HandleFunc("GET /flux-bloques", c.lecture(c.pageFlux))
}

// champsLogs : filtres exacts proposés (étiquettes des lignes).
var champsLogs = []struct{ Nom, Titre string }{
	{"host", "Machine"}, {"job", "Source"}, {"level", "Niveau"}, {"unit", "Unité"}, {"container", "Conteneur"},
	{"site", "Site"}, {"env", "Type"}, {"groupe", "Groupe"},
}

var reValeurChamp = motif.Doit(`^[\p{L}\p{N} ._:/@+-]+$`, 1, 200)

type compte struct {
	Nom string
	N   int
}

// champLog : un filtre ; Choix = valeurs proposées dans la liste (la
// valeur actuelle y est toujours).
type champLog struct {
	Nom, Titre, Valeur string
	Choix              []string
}

// niveauxSyslog : niveaux standard, toujours proposés.
var niveauxSyslog = []string{"emerg", "alert", "crit", "err", "warning", "notice", "info", "debug"}

// choixLogs : valeurs proposées pour chaque filtre (SPEC § 18.5, demande du
// 14/11) : machines, sites, types et groupes de la base ; source, niveau,
// unité et conteneur vus dans les lignes reçues depuis le démarrage.
func (c *Console) choixLogs(ctx context.Context, machines []string) map[string][]string {
	l := c.lireListes(ctx)
	out := map[string][]string{"host": machines, "site": l.Sites, "env": l.Types, "groupe": l.Groupes}
	if c.opt.Logs != nil {
		for _, ch := range journaux.ChampsListes {
			out[ch] = c.opt.Logs.ValeursVues(ch)
		}
	}
	vu := map[string]bool{}
	for _, v := range out["level"] {
		vu[v] = true
	}
	for _, n := range niveauxSyslog {
		if !vu[n] {
			out["level"] = append(out["level"], n)
		}
	}
	return out
}

// avecValeur : la liste, plus la valeur actuelle si elle n'y est pas.
func avecValeur(choix []string, v string) []string {
	if v == "" {
		return choix
	}
	for _, c := range choix {
		if c == v {
			return choix
		}
	}
	return append([]string{v}, choix...)
}

var periodesLogs = []string{"15m", "1h", "6h", "24h", "7d", "30d"}

// chercherLogs : 2 recherches à la fois au plus (chacune peut lire beaucoup
// de blocs) ; une troisième attend jusqu'à 5 s puis renonce.
func (c *Console) chercherLogs(ctx context.Context, r journaux.Requete) (journaux.Resultat, error) {
	select {
	case c.recherches <- struct{}{}:
	case <-time.After(5 * time.Second):
		return journaux.Resultat{}, errors.New("serveur occupé par d'autres recherches : réessayer")
	case <-ctx.Done():
		return journaux.Resultat{}, ctx.Err()
	}
	defer func() { <-c.recherches }()
	// Machines renommées : l'historique de l'ancien nom est rattaché au
	// nouveau (filtre élargi, nom affiché remplacé ; SPEC § 18.23).
	anciens := c.anciensNoms()
	if h := r.Champs["host"]; h != "" && len(anciens) > 0 {
		for vieux, actuel := range anciens {
			if actuel == h {
				if r.Variantes == nil {
					r.Variantes = map[string][]string{}
				}
				r.Variantes["host"] = append(r.Variantes["host"], vieux)
			}
		}
	}
	res, err := c.opt.Logs.Chercher(ctx, r, journaux.LimitesRechercheDefaut())
	if len(anciens) > 0 {
		for i := range res.Lignes {
			if actuel, ok := anciens[res.Lignes[i].Champs["host"]]; ok {
				ch := maps.Clone(res.Lignes[i].Champs)
				ch["host"] = actuel
				res.Lignes[i].Champs = ch
			}
		}
	}
	return res, err
}

// --- Flux entrants bloqués --------------------------------------------------------------
//
// Calculés depuis les lignes du noyau gardées (« … IN=eth0 OUT= … SRC=… »),
// avec les filtres du tableau Grafana actuel (SPEC § 4.8) : sans interfaces
// virtuelles, sans diffusion ni multidiffusion, sans 0.0.0.0 ni fe80:.
// Carte par pays : base GéoIP (Réglages › Interface), toujours affichée
// quand le réglage est coché, avec la raison si elle est vide.

var (
	reIN      = regexp.MustCompile(`\bIN=(\S*)`)
	reOUT     = regexp.MustCompile(`\bOUT=(\S*)`)
	reSRC     = regexp.MustCompile(`\bSRC=(\S+)`)
	reDPT     = regexp.MustCompile(`\bDPT=(\d+)`)
	rePROTO   = regexp.MustCompile(`\bPROTO=(\S+)`)
	reVirtuel = regexp.MustCompile(`^(lo|veth|docker|br-|virbr|tap|fw)`)
)

type flux struct {
	T                                          int64
	Machine, Regle, IP, Port, Proto, Interface string
	Pays                                       string
}

type donneesFlux struct {
	Periode    string
	Periodes   []string
	Total      int
	IPs        int
	Tronque    bool
	ParIP      []compte
	ParPort    []compte
	ParRegle   []compte
	ParMachine []compte
	ParPays    []compte
	Carte      []paysVue // carte du monde (vide sans GéoIP)
	CarteVide  string    // pourquoi aucun pays n'est coloré (vide = carte remplie)
	Pays       int
	GeoIP      bool
	Derniers   []flux
}

func lireFlux(l stockage.Ligne) (flux, bool) {
	m := l.Msg
	in, out, src := reIN.FindStringSubmatch(m), reOUT.FindStringSubmatch(m), reSRC.FindStringSubmatch(m)
	if in == nil || out == nil || src == nil || in[1] == "" || out[1] != "" || reVirtuel.MatchString(in[1]) {
		return flux{}, false
	}
	ip, err := netip.ParseAddr(src[1])
	if err != nil || ip.IsUnspecified() || ip.IsMulticast() || ip.IsLinkLocalUnicast() || ip == netip.AddrFrom4([4]byte{255, 255, 255, 255}) {
		return flux{}, false
	}
	f := flux{T: l.T, Machine: l.Champs["host"], IP: ip.String(), Interface: in[1]}
	if p := reDPT.FindStringSubmatch(m); p != nil {
		f.Port = p[1]
	}
	if p := rePROTO.FindStringSubmatch(m); p != nil {
		f.Proto = p[1]
	}
	// Règle = ce qui précède « IN= » (préfixe du journal du pare-feu), sans
	// l'horodatage du noyau entre crochets.
	f.Regle = strings.TrimSpace(m[:strings.Index(m, "IN=")])
	if i := strings.LastIndex(f.Regle, "] "); i >= 0 && strings.HasPrefix(f.Regle, "[") {
		f.Regle = strings.TrimSpace(f.Regle[i+2:])
	}
	if len(f.Regle) > 60 {
		f.Regle = f.Regle[:60]
	}
	return f, true
}

func compter(m map[string]int, n int) []compte {
	var cs []compte
	for k, v := range m {
		cs = append(cs, compte{k, v})
	}
	sort.Slice(cs, func(i, j int) bool { return cs[i].N > cs[j].N || (cs[i].N == cs[j].N && cs[i].Nom < cs[j].Nom) })
	if len(cs) > n {
		cs = cs[:n]
	}
	return cs
}

func (c *Console) pageFlux(q *req) {
	v := q.r.URL.Query()
	d := donneesFlux{Periode: v.Get("periode"), Periodes: []string{"15m", "1h", "6h", "24h"}}
	if d.Periode == "" {
		d.Periode = "1h"
	}
	periode, err := lirePeriode(d.Periode)
	if err == nil && periode > 24*time.Hour {
		err = errors.New("24 h au plus (les lignes sont relues une à une)")
	}
	if err == nil && c.opt.Logs != nil {
		var res journaux.Resultat
		res, err = c.chercherLogs(q.r.Context(), journaux.Requete{Debut: q.t.Add(-periode).UnixMilli(), Fin: q.t.UnixMilli(),
			Mots: "SRC DPT", Limite: 10000})
		d.Tronque = res.Tronque
		ips, ports, regles, machines, pays := map[string]int{}, map[string]int{}, map[string]int{}, map[string]int{}, map[string]int{}
		for _, l := range res.Lignes {
			f, ok := lireFlux(l)
			if !ok {
				continue
			}
			d.Total++
			if c.opt.GeoIP != nil {
				if f.Pays = c.opt.GeoIP.Pays(netip.MustParseAddr(f.IP)); f.Pays != "" {
					d.GeoIP = true
				}
				pays[f.Pays]++
			}
			ips[f.IP]++
			if f.Port != "" {
				ports[f.Proto+"/"+f.Port]++
			}
			regles[f.Regle]++
			machines[f.Machine]++
			if len(d.Derniers) < 100 {
				d.Derniers = append(d.Derniers, f)
			}
		}
		d.IPs = len(ips)
		delete(pays, "")
		d.Pays, d.ParPays = len(pays), compter(pays, 30)
		if c.opt.GeoIP != nil && lireInterface(q.r.Context(), c.db).GeoIP {
			d.Carte = carteFlux(pays)
			if !d.GeoIP {
				fichier, _, _ := c.opt.GeoIP.Etat()
				switch {
				case fichier.IsZero():
					d.CarteVide = "base GéoIP pas encore téléchargée (Réglages › Interface)"
				case d.Total == 0:
					d.CarteVide = "aucun flux bloqué sur la période"
				default:
					d.CarteVide = "aucune adresse publique (adresses du réseau local : pas de pays)"
				}
			}
		}
		d.ParIP, d.ParPort, d.ParRegle, d.ParMachine = compter(ips, 20), compter(ports, 20), compter(regles, 20), compter(machines, 30)
		// Pays de chaque adresse du palmarès (après le calcul du palmarès).
		for i := range d.ParIP {
			if c.opt.GeoIP != nil {
				if p := c.opt.GeoIP.Pays(netip.MustParseAddr(d.ParIP[i].Nom)); p != "" {
					d.ParIP[i].Nom += " (" + p + ")"
				}
			}
		}
	}
	if err != nil {
		c.flash(q, "", err.Error())
	}
	c.rendre(q, "flux", "Flux entrants bloqués", "tableaux", d)
}
