// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/prometheus/model/labels"

	"mirador-stack/internal/server/alertes"
)

// --- Cache des requêtes ----------------------------------------------------------
//
// Deux personnes qui regardent le même tableau (ou une page rechargée) ne
// refont pas les calculs : résultats gardés 20 s, 400 entrées au plus.

type cacheRequetes struct {
	mu   sync.Mutex
	tout map[string]entreeRequete
}

type entreeRequete struct {
	ech    []alertes.Echantillon
	err    error
	expire time.Time
}

const (
	dureeCacheRequete = 20 * time.Second
	cacheRequetesMax  = 400
)

// instant : requête instantanée (avec cache).
func (c *Console) instant(ctx context.Context, expr string, t time.Time) ([]alertes.Echantillon, error) {
	if c.lecteur == nil {
		return nil, errors.New("métriques indisponibles")
	}
	c.cache.mu.Lock()
	e, ok := c.cache.tout[expr]
	c.cache.mu.Unlock()
	if ok && t.Before(e.expire) {
		return e.ech, e.err
	}
	ech, err := c.lecteur.Instant(ctx, expr, t)
	c.cache.mu.Lock()
	if len(c.cache.tout) >= cacheRequetesMax {
		c.cache.tout = map[string]entreeRequete{}
	}
	c.cache.tout[expr] = entreeRequete{ech: ech, err: err, expire: t.Add(dureeCacheRequete)}
	c.cache.mu.Unlock()
	return ech, err
}

// --- Vues des panneaux ---------------------------------------------------------------

type vueValeur struct {
	Texte, Couleur, Legende, Lien string
	Brut                          float64
}

type vueBarre struct {
	Nom, Texte, Couleur string
	Valeur, Max         float64
}

type vuePanneau struct {
	P        *Panneau
	Erreur   string
	Chiffres []vueValeur
	Barres   []vueBarre
	Entetes  []string
	Lignes   [][]vueValeur
	Courbe   string // adresse des données de la courbe
	// CourbeJSON : données déjà calculées (aperçu de l'éditeur : le panneau
	// n'est pas encore enregistré, Courbe vaut alors « apercu »).
	CourbeJSON string
}

type vueRangee struct {
	Titre    string
	Enroulee bool
	Douze    bool // une largeur est choisie : grille de 12 colonnes
	Panneaux []vuePanneau
}

type vueVariable struct {
	Nom, Titre, Valeur string
	Choix              []string
}

type donneesTableau struct {
	T           *Tableau
	Variables   []vueVariable
	Rangees     []vueRangee
	Periode     string
	Periodes    []string
	Requete     string // filtres et période, pour les liens
	Tableaux    []*Tableau
	LienMachine bool
	Installe    bool // false : aperçu d'un tableau du catalogue
}

var periodesProposees = []string{"1h", "6h", "24h", "7d", "30d"}

// lireFiltres : valeurs des variables (vérifiées) et période.
func lireFiltres(t *Tableau, v url.Values) (map[string]string, string, time.Duration, error) {
	vals := map[string]string{}
	for _, va := range t.Variables {
		x := strings.TrimSpace(v.Get(va.Nom))
		if x != "" && !reValeurVariable.MatchString(x) {
			return nil, "", 0, fmt.Errorf("filtre %s : caractères non permis", va.Nom)
		}
		vals[va.Nom] = x
	}
	p := v.Get("periode")
	if p == "" {
		p = t.Periode
	}
	d, err := lirePeriode(p)
	if err != nil {
		return nil, "", 0, err
	}
	return vals, p, d, nil
}

// pasPour : environ 300 points par courbe (lisible et léger).
func pasPour(periode time.Duration) time.Duration {
	p := (periode / 300).Truncate(time.Second)
	if p < 15*time.Second {
		p = 15 * time.Second
	}
	return p
}

// evaluerTableau calcule tous les panneaux (sauf les courbes, chargées par
// le navigateur). seulement : titres des rangées à garder (vide = toutes).
func (c *Console) evaluerTableau(ctx context.Context, t *Tableau, vals map[string]string, periode string, d time.Duration,
	maintenant time.Time, seulement map[string]bool) []vueRangee {
	pas := pasPour(d)
	qs := url.Values{}
	for k, v := range vals {
		if v != "" {
			qs.Set(k, v)
		}
	}
	qs.Set("periode", periode)
	var out []vueRangee
	for ri := range t.Rangees {
		r := &t.Rangees[ri]
		if seulement != nil && !seulement[r.Titre] {
			continue
		}
		vr := vueRangee{Titre: r.Titre, Enroulee: r.Enroulee, Douze: largeurChoisie(r)}
		for pi := range r.Panneaux {
			p := &r.Panneaux[pi]
			vp := vuePanneau{P: p}
			if p.Type == PanneauCourbe {
				q := url.Values{}
				for k, v := range qs {
					q[k] = v
				}
				q.Set("tableau", t.Nom)
				q.Set("panneau", strconv.Itoa(ri)+"."+strconv.Itoa(pi))
				vp.Courbe = "/donnees/courbe?" + q.Encode()
			} else if err := c.remplirPanneau(ctx, &vp, vals, pas, d, maintenant); err != nil {
				vp.Erreur = err.Error()
			}
			vr.Panneaux = append(vr.Panneaux, vp)
		}
		out = append(out, vr)
	}
	return out
}

func valeurVue(v float64, unite string, dec *int, seuils []Seuil, cs []Correspondance) vueValeur {
	if txt, coul, ok := correspondance(v, cs); ok {
		return vueValeur{Texte: txt, Couleur: coul, Brut: v}
	}
	return vueValeur{Texte: formater(v, unite, dec), Couleur: couleur(v, seuils), Brut: v}
}

func nomSerie(e labels.Labels) string {
	var parts []string
	e.Range(func(l labels.Label) {
		if l.Name != "__name__" {
			parts = append(parts, l.Name+"="+l.Value)
		}
	})
	if len(parts) == 0 {
		return e.Get("__name__")
	}
	return strings.Join(parts, ", ")
}

// remplirPanneau : chiffre, barres ou tableau (requêtes instantanées).
func (c *Console) remplirPanneau(ctx context.Context, vp *vuePanneau, vals map[string]string, pas, d time.Duration, t time.Time) error {
	p := vp.P
	switch p.Type {
	case PanneauChiffre, PanneauBarres:
		for _, rq := range p.Requetes {
			ech, err := c.instant(ctx, substituer(rq.Expr, vals, pas, d), t)
			if err != nil {
				return err
			}
			sort.Slice(ech, func(i, j int) bool { return nomSerie(ech[i].Etiquettes) < nomSerie(ech[j].Etiquettes) })
			max := 0.0
			for _, e := range ech {
				max = math.Max(max, e.Valeur)
			}
			if p.Unite == "percent" {
				max = 100
			} else if p.Unite == "percentunit" {
				max = 1
			}
			for _, e := range ech {
				nom := legende(rq.Legende, e.Etiquettes.Get, nomSerie(e.Etiquettes))
				v := valeurVue(e.Valeur, p.Unite, p.Decimales, p.Seuils, p.Correspondances)
				if p.Type == PanneauChiffre {
					v.Legende = nom
					vp.Chiffres = append(vp.Chiffres, v)
				} else {
					vp.Barres = append(vp.Barres, vueBarre{Nom: nom, Texte: v.Texte, Couleur: v.Couleur, Valeur: math.Max(0, e.Valeur), Max: max})
				}
			}
		}
		// Une seule valeur : pas besoin de légende.
		if len(vp.Chiffres) == 1 {
			vp.Chiffres[0].Legende = ""
		}
		if len(vp.Barres) > 60 {
			vp.Barres = vp.Barres[:60]
		}
	case PanneauTableau:
		return c.remplirTableau(ctx, vp, vals, pas, d, t)
	}
	return nil
}

// remplirTableau : une ligne par jeu d'étiquettes, une colonne par requête
// (comme la transformation « merge » de Grafana).
func (c *Console) remplirTableau(ctx context.Context, vp *vuePanneau, vals map[string]string, pas, d time.Duration, t time.Time) error {
	p := vp.P
	type ligne struct {
		etq    map[string]string
		valeur map[string]float64
	}
	lignes := map[string]*ligne{}
	var ordre []string
	masque := map[string]bool{"__name__": true, "Time": true}
	for _, m := range p.Masquer {
		masque[m] = true
	}
	etiquettes := map[string]bool{}
	for _, rq := range p.Requetes {
		ech, err := c.instant(ctx, substituer(rq.Expr, vals, pas, d), t)
		if err != nil {
			return err
		}
		for _, e := range ech {
			etq := map[string]string{}
			var cle []string
			e.Etiquettes.Range(func(l labels.Label) {
				if !masque[l.Name] {
					etq[l.Name] = l.Value
					cle = append(cle, l.Name+"="+l.Value)
					etiquettes[l.Name] = true
				}
			})
			k := strings.Join(cle, ",")
			li := lignes[k]
			if li == nil {
				li = &ligne{etq: etq, valeur: map[string]float64{}}
				lignes[k] = li
				ordre = append(ordre, k)
			}
			li.valeur[rq.ID] = e.Valeur
		}
	}
	cols := p.Colonnes
	if len(cols) == 0 {
		var noms []string
		for n := range etiquettes {
			noms = append(noms, n)
		}
		sort.Strings(noms)
		for _, n := range noms {
			cols = append(cols, Colonne{Champ: n, Titre: n})
		}
		for _, rq := range p.Requetes {
			cols = append(cols, Colonne{Champ: rq.ID, Titre: rq.ID})
		}
	}
	ids := map[string]bool{}
	for _, rq := range p.Requetes {
		ids[rq.ID] = true
	}
	var gardees []Colonne
	for _, co := range cols {
		if !masque[co.Champ] && (ids[co.Champ] || etiquettes[co.Champ]) {
			gardees = append(gardees, co)
		}
	}
	for _, co := range gardees {
		vp.Entetes = append(vp.Entetes, co.Titre)
	}
	sort.Strings(ordre)
	if len(ordre) > 500 {
		ordre = ordre[:500]
	}
	for _, k := range ordre {
		li := lignes[k]
		var rang []vueValeur
		for _, co := range gardees {
			if ids[co.Champ] {
				v, ok := li.valeur[co.Champ]
				if !ok {
					rang = append(rang, vueValeur{Texte: ""})
					continue
				}
				u, dec, s, cs := co.Unite, co.Decimales, co.Seuils, co.Correspondances
				if u == "" {
					u = p.Unite
				}
				if dec == nil {
					dec = p.Decimales
				}
				if s == nil && len(p.Colonnes) == 0 {
					s = p.Seuils
				}
				if cs == nil && len(p.Colonnes) == 0 {
					cs = p.Correspondances
				}
				rang = append(rang, valeurVue(v, u, dec, s, cs))
				continue
			}
			vv := vueValeur{Texte: li.etq[co.Champ]}
			if co.Champ == "instance" && vv.Texte != "" {
				vv.Lien = "/machines/" + url.PathEscape(vv.Texte)
			}
			rang = append(rang, vv)
		}
		vp.Lignes = append(vp.Lignes, rang)
	}
	return nil
}

// variables : choix de chaque filtre (valeurs vues sur le dernier jour).
func (c *Console) variables(ctx context.Context, t *Tableau, vals map[string]string, maintenant time.Time) []vueVariable {
	var out []vueVariable
	for _, v := range t.Variables {
		vv := vueVariable{Nom: v.Nom, Titre: v.Titre, Valeur: vals[v.Nom]}
		if expr, err := requeteVariable(v.Requete, vals); err == nil {
			if ech, err := c.instant(ctx, expr, maintenant); err == nil {
				nomEtq := reLabelValues.FindStringSubmatch(v.Requete)[2]
				for _, e := range ech {
					if x := e.Etiquettes.Get(nomEtq); x != "" && reValeurVariable.MatchString(x) {
						vv.Choix = append(vv.Choix, x)
					}
				}
				sort.Strings(vv.Choix)
			}
		}
		out = append(out, vv)
	}
	return out
}

func (c *Console) pageTableau(q *req) {
	t, installe := c.tableauOuCatalogue(q.r.PathValue("nom"))
	if t == nil {
		c.rendreCode(q, http.StatusNotFound, "introuvable", "Tableau introuvable", "tableaux", nil)
		return
	}
	vals, periode, d, err := lireFiltres(t, q.r.URL.Query())
	if err != nil {
		c.flash(q, "", err.Error())
		vals, periode, d = map[string]string{}, t.Periode, 6*time.Hour
	}
	ctx := q.r.Context()
	dt := donneesTableau{T: t, Installe: installe, Periode: periode, Periodes: periodesProposees, Tableaux: c.listeTableaux(),
		Variables: c.variables(ctx, t, vals, q.t), Rangees: c.evaluerTableau(ctx, t, vals, periode, d, q.t, nil)}
	c.rendre(q, "tableau", t.Titre, "tableaux", dt)
}

// --- Données d'une courbe (JSON pour graphes.js) -------------------------------------

type jsonSerie struct {
	Nom string     `json:"nom"`
	V   []*float64 `json:"v"`
}

type jsonCourbe struct {
	Unite  string      `json:"unite"`
	X      []int64     `json:"x"` // secondes Unix
	Series []jsonSerie `json:"series"`
	Erreur string      `json:"erreur,omitempty"`
}

const seriesParCourbeMax = 50

func (c *Console) donneesCourbe(q *req) {
	w := q.w
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	t, _ := c.tableauOuCatalogue(q.r.URL.Query().Get("tableau"))
	var p *Panneau
	if t != nil {
		var ri, pi int
		if _, err := fmt.Sscanf(q.r.URL.Query().Get("panneau"), "%d.%d", &ri, &pi); err == nil &&
			ri >= 0 && ri < len(t.Rangees) && pi >= 0 && pi < len(t.Rangees[ri].Panneaux) {
			p = &t.Rangees[ri].Panneaux[pi]
		}
	}
	if p == nil || p.Type != PanneauCourbe {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(jsonCourbe{Erreur: "panneau inconnu"})
		return
	}
	vals, _, d, err := lireFiltres(t, q.r.URL.Query())
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(jsonCourbe{Erreur: err.Error()})
		return
	}
	ecrireCourbe(q, c.courbe(q.r.Context(), p.Requetes, p.Unite, vals, d, q.t))
}

// ecrireCourbe : JSON d'une courbe (gzip si accepté : des nombres en JSON,
// gzip les divise par ~5 pour un téléphone ou une liaison lente).
func ecrireCourbe(q *req, donnees jsonCourbe) {
	w := q.w
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if strings.Contains(q.r.Header.Get("Accept-Encoding"), "gzip") {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Vary", "Accept-Encoding")
		zw, _ := gzip.NewWriterLevel(w, gzip.BestSpeed)
		defer zw.Close()
		_ = json.NewEncoder(zw).Encode(donnees)
		return
	}
	_ = json.NewEncoder(w).Encode(donnees)
}

// donneesVolumeLogs : lignes de logs reçues par minute, par machine (toutes,
// ou une seule avec ?instance=) ; compteur du serveur (SPEC § 18.20).
func (c *Console) donneesVolumeLogs(q *req) {
	v := q.r.URL.Query()
	d, err := lirePeriode(v.Get("periode"))
	if err != nil {
		d = 24 * time.Hour
	}
	sel := `mirador_logs_recues_total{job="mirador"}`
	if inst := v.Get("instance"); inst != "" {
		if !reValeurVariable.MatchString(inst) {
			q.w.WriteHeader(http.StatusBadRequest)
			ecrireCourbe(q, jsonCourbe{Erreur: "machine invalide"})
			return
		}
		sel = `mirador_logs_recues_total{job="mirador", instance="` + inst + `"}`
	}
	expr := `60 * sum by (instance) (rate(` + sel + `[$__rate_interval]))`
	ecrireCourbe(q, c.courbe(q.r.Context(), []Requete{{ID: "A", Expr: expr, Legende: "{{instance}}"}}, "lignes/min", map[string]string{}, d, q.t))
}

// courbe : séries alignées sur une même suite d'instants (format uPlot).
func (c *Console) courbe(ctx context.Context, rs []Requete, unite string, vals map[string]string, d time.Duration, t time.Time) jsonCourbe {
	out := jsonCourbe{Unite: unite}
	if c.lecteur == nil {
		out.Erreur = "métriques indisponibles"
		return out
	}
	pas := pasPour(d)
	fin := t.Truncate(pas)
	debut := fin.Add(-d)
	n := int(d/pas) + 1
	out.X = make([]int64, n)
	for i := range out.X {
		out.X[i] = debut.Add(time.Duration(i) * pas).Unix()
	}
	for _, rq := range rs {
		courbes, pasReel, err := c.lecteur.Plage(ctx, substituer(rq.Expr, vals, pas, d), debut, fin, pas)
		if err != nil {
			out.Erreur = err.Error()
			return out
		}
		if pasReel != pas {
			out.Erreur = "pas agrandi"
		}
		sort.Slice(courbes, func(i, j int) bool { return nomSerie(courbes[i].Etiquettes) < nomSerie(courbes[j].Etiquettes) })
		for _, cb := range courbes {
			if len(out.Series) >= seriesParCourbeMax {
				out.Erreur = fmt.Sprintf("%d séries au plus affichées : filtrer", seriesParCourbeMax)
				break
			}
			s := jsonSerie{Nom: legende(rq.Legende, cb.Etiquettes.Get, nomSerie(cb.Etiquettes)), V: make([]*float64, n)}
			for i, ts := range cb.T {
				k := int((ts/1000 - out.X[0]) / int64(pas/time.Second))
				if k >= 0 && k < n && !math.IsNaN(cb.V[i]) && !math.IsInf(cb.V[i], 0) {
					v := cb.V[i]
					s.V[k] = &v
				}
			}
			out.Series = append(out.Series, s)
		}
	}
	return out
}
