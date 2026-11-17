// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"mirador-stack/internal/server/flotte"
)

// Panneau de détail au clic (SPEC.md § 42.4, idée reprise de Beszel le
// 17/11 : idée seulement, aucun code repris). Un clic sur une tuile
// « En ce moment » de la page machine ouvre un panneau à droite : processeur
// par cœur, mémoire détaillée, disques, réseau par interface.
//
// GET /machines/{nom}/detail/{quoi} : fragment HTML quand la page le demande
// (en-tête X-Requested-With: mirador), sinon une page complète (lien ouvert
// dans un nouvel onglet, navigateur sans JavaScript). Rien de nouveau n'est
// collecté. Coût : 3 à 6 requêtes instantanées par ouverture.

// ligneDetail : une ligne du panneau (barre si Pct >= 0).
type ligneDetail struct {
	Nom, Texte string
	Pct        int
	Classe     string
}

type blocDetail struct {
	Titre, Note string
	Lignes      []ligneDetail
}

type vueDetail struct {
	Machine, Quoi, Titre, Resume string
	Blocs                        []blocDetail
	Onglets                      []ongletDetail
}

type ongletDetail struct {
	Quoi, Titre string
	Actif       bool
}

var detailsMachine = []ongletDetail{{"cpu", "Processeur", false}, {"memoire", "Mémoire", false},
	{"disques", "Disques", false}, {"reseau", "Réseau", false}}

// interfacesIgnorees : comme les tableaux (boucle locale, ponts et veth Docker).
const interfacesIgnorees = `device!~"lo|veth.*|docker.*|br-.*|virbr.*|tap.*|fwbr.*|fwpr.*|fwln.*"`

func (c *Console) detailMachine(q *req) {
	m, ok := c.machine(q)
	quoi := q.r.PathValue("quoi")
	valide := false
	for _, o := range detailsMachine {
		valide = valide || o.Quoi == quoi
	}
	if !ok || !valide || m.Type != "agent" || !reValeurVariable.MatchString(m.Nom) {
		http.NotFound(q.w, q.r)
		return
	}
	d := c.vueDetailMachine(q.r.Context(), m, quoi, q.t)
	if q.r.Header.Get("X-Requested-With") == "mirador" {
		q.w.Header().Set("Cache-Control", "no-store")
		c.pages.executer(q.w, http.StatusOK, "detail-machine-fragment", Page{D: d})
		return
	}
	c.rendre(q, "detail-machine-page", d.Titre+" de "+m.Nom, "machines", d)
}

func (c *Console) vueDetailMachine(ctx context.Context, m flotte.InfoMachine, quoi string, t time.Time) vueDetail {
	d := vueDetail{Machine: m.Nom, Quoi: quoi}
	for _, o := range detailsMachine {
		o.Actif = o.Quoi == quoi
		if o.Actif {
			d.Titre = o.Titre
		}
		d.Onglets = append(d.Onglets, o)
	}
	i := `instance="` + m.Nom + `"`
	lignes := func(expr, etiquette, unite string, pct func(v float64) (int, string)) []ligneDetail {
		ech, err := c.instant(ctx, expr, t)
		if err != nil {
			return nil
		}
		var out []ligneDetail
		for _, e := range ech {
			if math.IsNaN(e.Valeur) || math.IsInf(e.Valeur, 0) {
				continue
			}
			l := ligneDetail{Nom: e.Etiquettes.Get(etiquette), Texte: formater(e.Valeur, unite, &un), Pct: -1}
			if pct != nil {
				l.Pct, l.Classe = pct(e.Valeur)
			}
			out = append(out, l)
		}
		return out
	}
	une := func(expr string) (float64, bool) {
		ech, err := c.instant(ctx, expr, t)
		if err != nil || len(ech) == 0 || math.IsNaN(ech[0].Valeur) {
			return 0, false
		}
		return ech[0].Valeur, true
	}
	barre := func(seuil float64) func(float64) (int, string) {
		return func(v float64) (int, string) { return pctBarre(v), classeSeuil(v, seuil) }
	}
	switch quoi {
	case "cpu":
		coeurs := lignes(`100 * (1 - rate(node_cpu_seconds_total{`+i+`, mode="idle"}[5m]))`, "cpu", "percent", barre(90))
		sort.Slice(coeurs, func(a, b int) bool { return numeroCoeur(coeurs[a].Nom) < numeroCoeur(coeurs[b].Nom) })
		for k := range coeurs {
			coeurs[k].Nom = "Cœur " + coeurs[k].Nom
		}
		if moy, ok := une(`100 * (1 - avg(rate(node_cpu_seconds_total{` + i + `, mode="idle"}[5m])))`); ok {
			d.Resume = strconv.Itoa(len(coeurs)) + " " + accorder(len(coeurs), "cœur(s)") + " · moyenne " + formater(moy, "percent", &un) + " sur 5 min"
		}
		if l5, ok := une(`node_load5{` + i + `}`); ok && len(coeurs) > 0 {
			d.Resume += " · charge " + formater(l5, "", &un) + " (" + formater(l5/float64(len(coeurs)), "", &un) + " par cœur)"
		}
		d.Blocs = append(d.Blocs, blocDetail{Titre: "Par cœur (5 dernières minutes)", Lignes: coeurs})
		modes := lignes(`100 * sum by (mode) (rate(node_cpu_seconds_total{`+i+`, mode!="idle"}[5m]))`+
			` / scalar(count(node_cpu_seconds_total{`+i+`, mode="idle"}))`, "mode", "percent", nil)
		for k := range modes {
			modes[k].Nom = nomModeCPU(modes[k].Nom)
		}
		sort.Slice(modes, func(a, b int) bool { return modes[a].Nom < modes[b].Nom })
		d.Blocs = append(d.Blocs, blocDetail{Titre: "Répartition du temps", Lignes: modes})
		if g := c.gourmands(ctx, m, t); g != nil && len(g.CPU) > 0 {
			b := blocDetail{Titre: "Conteneurs les plus gourmands", Note: "part de toute la machine"}
			for _, x := range g.CPU {
				b.Lignes = append(b.Lignes, ligneDetail{Nom: x.Nom, Texte: x.Texte, Pct: x.Pct})
			}
			d.Blocs = append(d.Blocs, b)
		}
	case "memoire":
		total, ok := une(`node_memory_MemTotal_bytes{` + i + `}`)
		if !ok || total <= 0 {
			break
		}
		part := func(v float64) (int, string) { return pctBarre(100 * v / total), "ok" }
		var b blocDetail
		b.Titre = "Mémoire vive (" + formater(total, "bytes", &un) + ")"
		ajouter := func(nom, expr string) {
			if v, ok := une(expr); ok {
				p, cl := part(v)
				b.Lignes = append(b.Lignes, ligneDetail{Nom: nom, Texte: formater(v, "bytes", &un), Pct: p, Classe: cl})
			}
		}
		ajouter("Utilisée par les programmes", `node_memory_MemTotal_bytes{`+i+`} - node_memory_MemAvailable_bytes{`+i+`}`)
		ajouter("Cache des fichiers (rendu au besoin)", `node_memory_Cached_bytes{`+i+`} + node_memory_Buffers_bytes{`+i+`}`)
		ajouter("Disponible", `node_memory_MemAvailable_bytes{`+i+`}`)
		if dispo, ok := une(`node_memory_MemAvailable_bytes{` + i + `}`); ok {
			d.Resume = formater(100*(1-dispo/total), "percent", &un) + " utilisée · " + formater(dispo, "bytes", &un) + " disponibles"
		}
		d.Blocs = append(d.Blocs, b)
		if st, ok := une(`node_memory_SwapTotal_bytes{` + i + `}`); ok && st > 0 {
			if sl, ok := une(`node_memory_SwapFree_bytes{` + i + `}`); ok {
				p := 100 * (st - sl) / st
				d.Blocs = append(d.Blocs, blocDetail{Titre: "Swap (" + formater(st, "bytes", &un) + ")",
					Lignes: []ligneDetail{{Nom: "Utilisé", Texte: formater(st-sl, "bytes", &un), Pct: pctBarre(p), Classe: classeSeuil(p, 50)}}})
			}
		}
		if g := c.gourmands(ctx, m, t); g != nil && len(g.Memoire) > 0 {
			b := blocDetail{Titre: "Conteneurs les plus gourmands"}
			for _, x := range g.Memoire {
				b.Lignes = append(b.Lignes, ligneDetail{Nom: x.Nom, Texte: x.Texte, Pct: x.Pct})
			}
			d.Blocs = append(d.Blocs, b)
		}
	case "disques":
		pleins := lignes(`100 * (1 - node_filesystem_avail_bytes{`+i+`, `+fsIgnoresMaintenant+`} / node_filesystem_size_bytes{`+i+`, `+fsIgnoresMaintenant+`})`,
			"mountpoint", "percent", barre(90))
		sort.Slice(pleins, func(a, b int) bool { return pleins[a].Nom < pleins[b].Nom })
		d.Blocs = append(d.Blocs, blocDetail{Titre: "Place utilisée par point de montage", Lignes: pleins})
		lec := lignes(`rate(node_disk_read_bytes_total{`+i+`}[5m])`, "device", "Bps", nil)
		ecr := lignes(`rate(node_disk_written_bytes_total{`+i+`}[5m])`, "device", "Bps", nil)
		occ := lignes(`100 * rate(node_disk_io_time_seconds_total{`+i+`}[5m])`, "device", "percent", barre(90))
		d.Blocs = append(d.Blocs, blocDetail{Titre: "Par disque (5 dernières minutes)", Note: "barre : temps où le disque est occupé",
			Lignes: fusionner(occ, lec, ecr)})
		if len(pleins) > 0 {
			d.Resume = strconv.Itoa(len(pleins)) + " " + accorder(len(pleins), "point(s) de montage") + " · " + strconv.Itoa(len(occ)) + " " + accorder(len(occ), "disque(s)")
		}
	case "reseau":
		rx := lignes(`8 * rate(node_network_receive_bytes_total{`+i+`, `+interfacesIgnorees+`}[5m])`, "device", "bps", nil)
		tx := lignes(`8 * rate(node_network_transmit_bytes_total{`+i+`, `+interfacesIgnorees+`}[5m])`, "device", "bps", nil)
		err := lignes(`rate(node_network_receive_errs_total{`+i+`, `+interfacesIgnorees+`}[5m]) + rate(node_network_transmit_errs_total{`+i+`, `+interfacesIgnorees+`}[5m])`+
			` + rate(node_network_receive_drop_total{`+i+`, `+interfacesIgnorees+`}[5m])`, "device", "pps", nil)
		b := blocDetail{Titre: "Par interface (5 dernières minutes)", Note: "reçu · envoyé"}
		envoye := map[string]string{}
		for _, l := range tx {
			envoye[l.Nom] = l.Texte
		}
		for _, l := range rx {
			b.Lignes = append(b.Lignes, ligneDetail{Nom: l.Nom, Texte: l.Texte + " · " + envoye[l.Nom], Pct: -1})
		}
		sort.Slice(b.Lignes, func(x, y int) bool { return b.Lignes[x].Nom < b.Lignes[y].Nom })
		d.Blocs = append(d.Blocs, b)
		var pertes []ligneDetail
		for _, l := range err {
			if !strings.HasPrefix(l.Texte, "0 ") {
				pertes = append(pertes, l)
			}
		}
		if len(pertes) > 0 {
			d.Blocs = append(d.Blocs, blocDetail{Titre: "Erreurs et paquets perdus", Lignes: pertes})
		}
		d.Resume = strconv.Itoa(len(b.Lignes)) + " " + accorder(len(b.Lignes), "interface(s)")
	}
	return d
}

// fusionner : une ligne par disque (barre d'occupation, lecture · écriture).
func fusionner(occ, lec, ecr []ligneDetail) []ligneDetail {
	l, e := map[string]string{}, map[string]string{}
	for _, x := range lec {
		l[x.Nom] = x.Texte
	}
	for _, x := range ecr {
		e[x.Nom] = x.Texte
	}
	out := make([]ligneDetail, 0, len(occ))
	for _, o := range occ {
		var parts []string
		if v := l[o.Nom]; v != "" {
			parts = append(parts, "lu "+v)
		}
		if v := e[o.Nom]; v != "" {
			parts = append(parts, "écrit "+v)
		}
		o.Texte = strings.Join(append(parts, "occupé "+o.Texte), " · ")
		out = append(out, o)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Nom < out[b].Nom })
	return out
}

// nomModeCPU : modes du noyau en français.
func nomModeCPU(m string) string {
	switch m {
	case "user":
		return "Programmes (user)"
	case "system":
		return "Noyau (system)"
	case "iowait":
		return "Attente des disques (iowait)"
	case "irq", "softirq":
		return "Interruptions (" + m + ")"
	case "steal":
		return "Pris par l'hyperviseur (steal)"
	case "nice":
		return "Programmes de basse priorité (nice)"
	}
	return m
}

func numeroCoeur(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 1 << 30
		}
		n = n*10 + int(r-'0')
	}
	return n
}
