// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"mirador-stack/internal/server/alertes"
)

// Annotations des courbes d'une machine (SPEC § 31) : alertes de la période
// (bandes de couleur) et événements (redémarrage, nouvelle version de
// l'agent, changements faits dans l'interface pour cette machine).

// bandeAlerte : une alerte sur la période (secondes Unix).
type bandeAlerte struct {
	Debut   int64  `json:"debut"`
	Fin     int64  `json:"fin"`
	Nom     string `json:"nom"`
	Gravite string `json:"gravite"`
}

// evenementCourbe : un événement ponctuel.
type evenementCourbe struct {
	T   int64  `json:"t"`
	Nom string `json:"nom"`
}

// annotationsCourbe : jointes aux données d'une courbe d'une seule machine.
type annotationsCourbe struct {
	Alertes    []bandeAlerte     `json:"alertes,omitempty"`
	Evenements []evenementCourbe `json:"evenements,omitempty"`
	// Derniere : heure de la dernière donnée reçue de la machine (secondes
	// Unix, 0 = inconnue), pour la pastille « il y a 25 s ».
	Derniere int64 `json:"derniere,omitempty"`
}

// cacheAnnotations : une machine, une période → annotations (30 s) ; toutes
// les courbes d'une page demandent les mêmes.
type cacheAnnotations struct {
	mu   sync.Mutex
	tout map[string]entreeAnnotations
}

type entreeAnnotations struct {
	a      *annotationsCourbe
	expire time.Time
}

const maxEvenements = 20

// annotationsMachine : alertes et événements d'une machine sur [debut, fin].
func (c *Console) annotationsMachine(ctx context.Context, instance string, debut, fin time.Time) *annotationsCourbe {
	if instance == "" || !reValeurVariable.MatchString(instance) {
		return nil
	}
	cle := instance + "|" + strconv.FormatInt(debut.Unix()/60, 10) + "|" + strconv.FormatInt(fin.Unix()/60, 10)
	c.annot.mu.Lock()
	if e, ok := c.annot.tout[cle]; ok && fin.Before(e.expire) {
		c.annot.mu.Unlock()
		return e.a
	}
	c.annot.mu.Unlock()
	out := &annotationsCourbe{}
	// Alertes de la machine.
	if as, err := alertes.HistoriquePeriode(ctx, c.db, debut, fin, 2000); err == nil {
		for _, a := range as {
			if a.Machine() != instance {
				continue
			}
			f := a.Fin
			if f.IsZero() || f.After(fin) {
				f = fin
			}
			out.Alertes = append(out.Alertes, bandeAlerte{Debut: a.ActiveLe.Unix(), Fin: f.Unix(), Nom: a.Regle, Gravite: a.Gravite})
		}
	}
	if c.lecteur != nil {
		i := `instance="` + instance + `"`
		pas := pasPour(fin.Sub(debut))
		// Dernière donnée : le relevé d'information de l'agent part à chaque envoi.
		if ech, err := c.lecteur.Instant(ctx, `max(timestamp(mirador_agent_info{`+i+`}))`, fin); err == nil && len(ech) == 1 && !math.IsNaN(ech[0].Valeur) {
			out.Derniere = int64(ech[0].Valeur)
		}
		// Redémarrages : heures de démarrage relevées qui tombent dans la période.
		if cs, _, err := c.lecteur.Plage(ctx, `max(node_boot_time_seconds{`+i+`})`, debut, fin, pas); err == nil {
			vus := map[int64]bool{}
			for _, cb := range cs {
				for _, v := range cb.V {
					t := int64(math.Round(v))
					if !math.IsNaN(v) && t >= debut.Unix() && t <= fin.Unix() && !vus[t/60] {
						vus[t/60] = true
						out.Evenements = append(out.Evenements, evenementCourbe{T: t, Nom: "Redémarrage"})
					}
				}
			}
		}
		// Nouvelle version de l'agent : première apparition dans la période.
		if cs, _, err := c.lecteur.Plage(ctx, `max by (version) (mirador_agent_info{`+i+`})`, debut, fin, pas); err == nil && len(cs) > 1 {
			for _, cb := range cs {
				for k, v := range cb.V {
					if !math.IsNaN(v) {
						if t := cb.T[k] / 1000; t > debut.Add(pas).Unix() {
							out.Evenements = append(out.Evenements, evenementCourbe{T: t, Nom: "Agent " + cb.Etiquettes.Get("version")})
						}
						break
					}
				}
			}
		}
	}
	// Réglages faits pour la machine, changements repérés (paquets, noyau,
	// conteneurs ; redémarrages et agent sont lus ci-dessus) et maintenances :
	// lus dans le journal d'événements (SPEC § 53).
	for _, e := range c.journalEvenements(ctx, filtreEvenements{Debut: debut, Fin: fin, Machine: instance, SansAuto: true, Max: 4 * maxEvenements,
		Types: map[string]bool{evReglage: true, evChangement: true, evMaintenance: true}}) {
		if e.Type == evChangement && (strings.HasPrefix(e.Texte, "Redémarrage") || strings.HasPrefix(e.Texte, "Agent")) {
			continue
		}
		out.Evenements = append(out.Evenements, evenementCourbe{T: e.Le.Unix(), Nom: e.Texte})
	}
	sort.Slice(out.Evenements, func(a, b int) bool { return out.Evenements[a].T < out.Evenements[b].T })
	if len(out.Evenements) > maxEvenements {
		out.Evenements = out.Evenements[len(out.Evenements)-maxEvenements:]
	}
	c.annot.mu.Lock()
	if c.annot.tout == nil || len(c.annot.tout) > 500 {
		c.annot.tout = map[string]entreeAnnotations{}
	}
	c.annot.tout[cle] = entreeAnnotations{a: out, expire: fin.Add(30 * time.Second)}
	c.annot.mu.Unlock()
	return out
}

// Prévision des disques (SPEC § 31) : jours avant d'être plein, d'après la
// pente de la place libre sur 7 jours ; seulement si moins de 30 jours.
// Pente sur 7 jours moins 1 heure (167 h) : la lecture reste dans la base
// brute (gardée 7 jours par défaut). La place libre actuelle est lue à part
// (base brute). Grand parc (SPEC § 141) : une requête rend au plus 2 000
// séries ; au-delà (~500 machines), la prévision tombait en erreur sans
// bruit. Le parc est découpé en paquets d'au plus seriesParPaquet disques.
// Résultat (échec compris) gardé 10 minutes.
const (
	exprLibreDisque = `node_filesystem_avail_bytes{%s}`
	exprPenteDisque = `-deriv(node_filesystem_avail_bytes{%s}[167h]) > 0` // octets perdus par seconde
	// Pente du dernier jour (retour du 26/11) : sup « plein dans 15 j » le
	// lendemain de 2,4 Go libérés (journal réduit), à cause d'une hausse
	// ponctuelle dans la semaine. Le disque doit perdre de la place sur 7 j
	// ET sur le dernier jour ; on retient la pente la plus lente.
	exprPenteDisqueJour = `-deriv(node_filesystem_avail_bytes{%s}[24h]) > 0`
	// exprDisquesParMachine : nombre de disques de chaque machine (découpage).
	exprDisquesParMachine = `count by (instance) (node_filesystem_avail_bytes{%s})`
	seriesParPaquet       = 1500 // < 2 000 séries rendues au plus par requête
)

// filtreDisques : systèmes de fichiers réels (comme les barres d'occupation).
const filtreDisques = `instance=~"$instance", fstype!~"tmpfs|overlay|squashfs|ramfs|nsfs|fuse.lxcfs"`

const joursPrevision = 30

// pleinDans : « plein dans 18 j » (vide au-delà de 30 jours ou sans pente).
func pleinDans(jours float64) string {
	switch {
	case math.IsNaN(jours) || math.IsInf(jours, 0) || jours < 0 || jours >= joursPrevision:
		return ""
	case jours < 1:
		return "plein dans moins d'un jour"
	}
	return fmt.Sprintf("plein dans %.0f j", jours)
}

// previsionsDisques : instance|point de montage → jours, pour le filtre donné.
func (c *Console) previsionsDisques(ctx context.Context, filtre string, t time.Time) map[string]float64 {
	c.prevDisques.mu.Lock()
	if e, ok := c.prevDisques.tout[filtre]; ok && t.Before(e.expire) {
		c.prevDisques.mu.Unlock()
		return e.v
	}
	c.prevDisques.mu.Unlock()
	out := map[string]float64{}
	for _, f := range paquetsDisques(c.instant(ctx, fmt.Sprintf(exprDisquesParMachine, filtre), t)) {
		c.previsionsPaquet(ctx, filtre+f, t, out)
	}
	c.prevDisques.mu.Lock()
	if c.prevDisques.tout == nil || len(c.prevDisques.tout) > 100 {
		c.prevDisques.tout = map[string]entreePrevision{}
	}
	c.prevDisques.tout[filtre] = entreePrevision{v: out, expire: t.Add(10 * time.Minute)}
	c.prevDisques.mu.Unlock()
	return out
}

type cachePrevisions struct {
	mu   sync.Mutex
	tout map[string]entreePrevision
}

type entreePrevision struct {
	v      map[string]float64
	expire time.Time
}

// pleinDansMachine : prévision la plus courte par machine (vue d'ensemble).
func pleinDansMachine(prev map[string]float64) map[string]float64 {
	out := map[string]float64{}
	for k, j := range prev {
		inst := k
		if i := strings.LastIndexByte(k, '|'); i >= 0 {
			inst = k[:i]
		}
		if v, ok := out[inst]; !ok || j < v {
			out[inst] = j
		}
	}
	return out
}

// paquetsDisques : filtres en plus (« , instance=~"a|b" ») qui découpent le
// parc en paquets d'au plus seriesParPaquet disques ; un seul filtre vide
// si le parc tient en une requête (ou si le décompte a échoué).
func paquetsDisques(parMachine []alertes.Echantillon, err error) []string {
	total := 0
	for _, e := range parMachine {
		total += int(e.Valeur)
	}
	if err != nil || total <= seriesParPaquet {
		return []string{""}
	}
	sort.Slice(parMachine, func(i, j int) bool {
		return parMachine[i].Etiquettes.Get("instance") < parMachine[j].Etiquettes.Get("instance")
	})
	var out []string
	var noms []string
	n := 0
	fermer := func() {
		if len(noms) > 0 {
			out = append(out, `, instance=~"`+strings.Join(noms, "|")+`"`)
			noms, n = nil, 0
		}
	}
	for _, e := range parMachine {
		k := int(e.Valeur)
		if n+k > seriesParPaquet {
			fermer()
		}
		noms = append(noms, regexp.QuoteMeta(e.Etiquettes.Get("instance")))
		n += k
	}
	fermer()
	return out
}

// previsionsPaquet : prévisions d'un paquet de machines, ajoutées à out.
func (c *Console) previsionsPaquet(ctx context.Context, filtre string, t time.Time, out map[string]float64) {
	cle := func(e alertes.Echantillon) string {
		return e.Etiquettes.Get("instance") + "|" + e.Etiquettes.Get("mountpoint")
	}
	pentes, err := c.instant(ctx, fmt.Sprintf(exprPenteDisque, filtre), t)
	if err != nil {
		return
	}
	jour, err := c.instant(ctx, fmt.Sprintf(exprPenteDisqueJour, filtre), t)
	if err != nil {
		return
	}
	penteJour := map[string]float64{}
	for _, e := range jour {
		penteJour[cle(e)] = e.Valeur
	}
	libres, err := c.instant(ctx, fmt.Sprintf(exprLibreDisque, filtre), t)
	if err != nil {
		return
	}
	libre := map[string]float64{}
	for _, e := range libres {
		libre[cle(e)] = e.Valeur
	}
	for _, e := range pentes {
		pj, perdJour := penteJour[cle(e)]
		if l, ok := libre[cle(e)]; ok && e.Valeur > 0 && perdJour && pj > 0 {
			out[cle(e)] = l / min(e.Valeur, pj) / 86400
		}
	}
}
