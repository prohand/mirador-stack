// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"mirador-stack/internal/server/alertes"
	"mirador-stack/internal/server/flotte"
)

// Page « Conteneurs » (SPEC.md § 42.1, idée reprise de Beszel le 17/11 :
// idée seulement, aucun code repris) : tous les conteneurs Docker du parc
// sur une page, problèmes en premier, avec un lien vers leurs logs.
//
// Tout vient des métriques que l'agent envoie déjà (node_docker_container_*),
// plus l'heure de démarrage (nouvelle série, prochaine version de l'agent). Rien en base.
// Coût : 8 requêtes instantanées à l'ouverture de la page (gardées 20 s
// dans le cache des requêtes), et une au plus par page pour le compteur du
// menu.

// exprConteneursProblemes : conteneurs non sains, ou arrêtés alors qu'ils
// devraient redémarrer seuls (mêmes conditions que les règles
// ConteneurNonSain et ConteneurArrete). Compteur du menu.
const exprConteneursProblemes = `(count(node_docker_container_healthy == 0) or vector(0))` +
	` + on () (count(node_docker_container_running{restart_policy=~"always|unless-stopped"} == 0) or vector(0))`

// ligneConteneur : un conteneur d'une machine.
type ligneConteneur struct {
	Nom, Machine, Projet string
	Etat, ClasseEtat     string // En marche / Arrêté…, ok / attention / erreur
	Sante, ClasseSante   string // Sain / Non sain / "" (pas de test)
	CPU, Memoire, Limite string
	PctCPU, PctMemoire   int // -1 : pas de barre
	ClasseCPU, ClasseMem string
	Redemarrages         int
	Depuis               string // « 3 j », « 18 min » ; "" si inconnu
	DepuisS              int64  // pour le tri (secondes)
	LienLogs             string
	Probleme             bool
	OOM                  bool
	rang                 int
}

type donneesConteneurs struct {
	Lignes                                  []ligneConteneur
	EnMarche, NonSains, Arretes, Redemarres int
	Machines                                []string // proposées dans le filtre
	Machine, Etat                           string   // filtres choisis
	NbMachines, Total                       int
	SansDonnees                             bool
}

// lireConteneurs : un échantillon par (machine, conteneur), clé « machine\x00nom ».
func (c *Console) lireConteneurs(ctx context.Context, expr string, t time.Time) map[string]alertes.Echantillon {
	out := map[string]alertes.Echantillon{}
	ech, err := c.instant(ctx, expr, t)
	if err != nil {
		return out
	}
	for _, e := range ech {
		if math.IsNaN(e.Valeur) || math.IsInf(e.Valeur, 0) {
			continue
		}
		out[e.Etiquettes.Get("instance")+"\x00"+e.Etiquettes.Get("name")] = e
	}
	return out
}

// conteneursDuParc : toutes les lignes, problèmes en premier.
func (c *Console) conteneursDuParc(ctx context.Context, t time.Time) []ligneConteneur {
	if c.lecteur == nil {
		return nil
	}
	marche := c.lireConteneurs(ctx, `node_docker_container_running`, t)
	sante := c.lireConteneurs(ctx, `node_docker_container_healthy`, t)
	redem := c.lireConteneurs(ctx, `round(increase(node_docker_container_restart_count[24h]))`, t)
	oom := c.lireConteneurs(ctx, `node_docker_container_oom_killed`, t)
	cpu := c.lireConteneurs(ctx, `100 * rate(node_docker_container_cpu_seconds_total[5m])`+
		` / on (instance) group_left () count by (instance) (node_cpu_seconds_total{mode="idle"})`, t)
	mem := c.lireConteneurs(ctx, `node_docker_container_memory_bytes`, t)
	lim := c.lireConteneurs(ctx, `node_docker_container_memory_limit_bytes`, t)
	debut := c.lireConteneurs(ctx, `node_docker_container_started_timestamp_seconds`, t)

	out := make([]ligneConteneur, 0, len(marche))
	for cle, e := range marche {
		l := ligneConteneur{Nom: e.Etiquettes.Get("name"), Machine: e.Etiquettes.Get("instance"), Projet: e.Etiquettes.Get("project"),
			PctCPU: -1, PctMemoire: -1, CPU: "—", Memoire: "—"}
		politique := e.Etiquettes.Get("restart_policy")
		switch {
		case e.Valeur == 1:
			l.Etat, l.ClasseEtat = "En marche", "ok"
		case politique == "always" || politique == "unless-stopped":
			l.Etat, l.ClasseEtat, l.Probleme = "Arrêté", "erreur", true
		default:
			l.Etat, l.ClasseEtat = etatConteneur(e.Etiquettes.Get("state")), ""
		}
		if s, ok := sante[cle]; ok {
			if s.Valeur == 1 {
				l.Sante, l.ClasseSante = "Sain", "ok"
			} else {
				l.Sante, l.ClasseSante, l.Probleme = "Non sain", "erreur", true
			}
		}
		if o, ok := oom[cle]; ok && o.Valeur == 1 {
			l.OOM, l.Probleme = true, true
		}
		if r, ok := redem[cle]; ok && r.Valeur > 0 {
			l.Redemarrages = int(r.Valeur)
		}
		if v, ok := cpu[cle]; ok {
			l.CPU, l.PctCPU = virgule(formatPct(v.Valeur)), pctBarre(v.Valeur)
			l.ClasseCPU = classeSeuil(v.Valeur, 90)
		}
		if v, ok := mem[cle]; ok {
			l.Memoire = virgule(tailleLisible(int64(v.Valeur)))
			if m, ok := lim[cle]; ok && m.Valeur > 0 {
				p := 100 * v.Valeur / m.Valeur
				l.Limite = virgule(tailleLisible(int64(m.Valeur)))
				l.PctMemoire, l.ClasseMem = pctBarre(p), classeSeuil(p, 90)
			}
		}
		if d, ok := debut[cle]; ok && d.Valeur > 0 && l.Etat == "En marche" {
			duree := t.Sub(time.Unix(int64(d.Valeur), 0))
			if duree > 0 {
				l.Depuis, l.DepuisS = dureeLisible(duree), int64(duree.Seconds())
			}
		}
		q := url.Values{"host": {l.Machine}, "container": {l.Nom}}
		l.LienLogs = "/logs?" + q.Encode()
		out = append(out, l)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Probleme != b.Probleme {
			return a.Probleme
		}
		if a.Machine != b.Machine {
			return a.Machine < b.Machine
		}
		return a.Nom < b.Nom
	})
	return out
}

// etatConteneur : état Docker en français.
func etatConteneur(s string) string {
	switch s {
	case "exited":
		return "Arrêté"
	case "created":
		return "Créé"
	case "paused":
		return "En pause"
	case "restarting":
		return "Redémarre"
	case "dead":
		return "Mort"
	case "removing":
		return "Suppression"
	}
	if s == "" {
		return "—"
	}
	return s
}

// formatPct : « 22 % », « 0.4 % » (virgule mise ensuite).
func formatPct(v float64) string {
	if v > 0 && v < 10 {
		return strings.TrimSuffix(strconv.FormatFloat(v, 'f', 1, 64), ".0") + " %"
	}
	return strconv.FormatFloat(v, 'f', 0, 64) + " %"
}

// nbConteneursProblemes : compteur du menu (0 sans métriques).
func (c *Console) nbConteneursProblemes(ctx context.Context, t time.Time) int {
	if c.lecteur == nil {
		return 0
	}
	ech, err := c.instant(ctx, exprConteneursProblemes, t)
	if err != nil || len(ech) == 0 || math.IsNaN(ech[0].Valeur) {
		return 0
	}
	return int(ech[0].Valeur)
}

func (c *Console) pageConteneurs(q *req) {
	ctx := q.r.Context()
	d := donneesConteneurs{Machine: q.r.URL.Query().Get("machine"), Etat: q.r.URL.Query().Get("etat")}
	toutes := c.conteneursDuParc(ctx, q.t)
	connues := map[string]bool{}
	if ms, err := flotte.ListerMachines(ctx, c.db); err == nil {
		for _, m := range ms {
			if m.Statut == "approuvee" {
				connues[m.Nom] = true
			}
		}
	}
	vues := map[string]bool{}
	for _, l := range toutes {
		if len(connues) > 0 && !connues[l.Machine] {
			continue // machine supprimée depuis : séries encore en mémoire
		}
		vues[l.Machine] = true
		switch {
		case l.Etat == "En marche":
			d.EnMarche++
		case l.ClasseEtat == "erreur":
			d.Arretes++
		}
		if l.ClasseSante == "erreur" {
			d.NonSains++
		}
		if l.Redemarrages > 0 {
			d.Redemarres++
		}
		if d.Machine != "" && l.Machine != d.Machine {
			continue
		}
		switch d.Etat {
		case "problemes":
			if !l.Probleme {
				continue
			}
		case "marche":
			if l.Etat != "En marche" {
				continue
			}
		case "arretes":
			if l.Etat == "En marche" {
				continue
			}
		}
		d.Lignes = append(d.Lignes, l)
	}
	for m := range vues {
		d.Machines = append(d.Machines, m)
	}
	sort.Strings(d.Machines)
	d.NbMachines, d.Total, d.SansDonnees = len(vues), len(d.Lignes), len(toutes) == 0
	c.rendre(q, "conteneurs", "Conteneurs", "conteneurs", d)
}
