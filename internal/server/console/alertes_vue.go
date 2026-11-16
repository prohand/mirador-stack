// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"fmt"
	"html/template"
	"math"
	"net/url"
	"strings"
	"time"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"

	"mirador-stack/internal/server/alertes"
)

// Page Alertes (SPEC § 28, maquette validée le 15/11) : compteurs par
// gravité, alertes en cours groupées par machine ou par règle, actions sur
// la ligne (acquitter, silence, graphique), mini-courbe des 6 dernières
// heures avec le seuil, rechargement toutes les 30 s.

// vueAlerte : une alerte en cours prête à afficher.
type vueAlerte struct {
	resumeAlerte
	Courbe template.HTML // mini-courbe SVG (vide : pas de courbe)
}

// groupeAlertes : alertes d'une machine (ou d'une règle).
type groupeAlertes struct {
	Titre, Lien string
	Gravites    []string
	Alertes     []vueAlerte
}

// donneesEnCours : bloc « en cours » (page et fragment rechargé).
type donneesEnCours struct {
	Vue                              string // machine, regle, liste
	Critiques, Avertissements, Infos int
	Total                            int
	Groupes                          []groupeAlertes
	Liste                            []resumeAlerte
	Attente                          []resumeAlerte // pas encore des alertes
	Prises                           []resumeAlerte // prises en charge (SPEC § 30)
	Masquees                         []resumeAlerte // conséquences d'une alerte plus grave (bloc replié)
	Retour                           string         // page après une action sur une ligne
}

// maxCourbes : mini-courbes calculées par affichage (coût borné).
const maxCourbes = 30

func vueAlertesValide(v string) string {
	if v == "regle" || v == "liste" {
		return v
	}
	return "machine"
}

// enCoursVue : alertes en cours groupées, avec leurs mini-courbes.
func (c *Console) enCoursVue(ctx context.Context, vue string, t time.Time) donneesEnCours {
	d := donneesEnCours{Vue: vueAlertesValide(vue)}
	d.Retour = "/alertes?vue=" + url.QueryEscape(d.Vue)
	rs, attente := separerAttente(c.alertesEnCours(""))
	rs, d.Prises = separerPrises(rs)
	rs, d.Masquees = separerMasquees(rs)
	d.Attente = attente
	d.Total = len(rs)
	for _, r := range rs {
		switch r.Gravite {
		case alertes.Critique:
			d.Critiques++
		case alertes.Avertissement:
			d.Avertissements++
		default:
			d.Infos++
		}
	}
	if d.Vue == "liste" {
		d.Liste = rs
		return d
	}
	exprs := map[string]string{}
	if regles, err := alertes.ListerRegles(ctx, c.db); err == nil {
		for _, r := range regles {
			exprs[r.Nom] = r.Expr
		}
	}
	etiquettes := map[int64]labels.Labels{}
	if c.opt.Alertes != nil {
		for _, a := range c.opt.Alertes.EnCours() {
			etiquettes[a.ID] = a.Etiquettes
		}
	}
	index := map[string]int{}
	n := 0
	for _, r := range rs {
		cle, titre, lien := r.Machine, r.Machine, "/machines/"+url.PathEscape(r.Machine)
		if d.Vue == "regle" || r.Machine == "" {
			cle, titre, lien = "r:"+r.Regle, r.Regle, "/alertes?regle="+url.QueryEscape(r.Regle)
		}
		i, ok := index[cle]
		if !ok {
			i = len(d.Groupes)
			index[cle] = i
			d.Groupes = append(d.Groupes, groupeAlertes{Titre: titre, Lien: lien})
		}
		g := &d.Groupes[i]
		if !contientTexte(g.Gravites, r.Gravite) {
			g.Gravites = append(g.Gravites, r.Gravite)
		}
		v := vueAlerte{resumeAlerte: r}
		if n < maxCourbes && c.lecteur != nil {
			n++
			v.Courbe = c.courbeAlerte(ctx, exprs[r.Regle], etiquettes[r.ID], r.Gravite, t, 6*time.Hour, 120, 28)
		}
		g.Alertes = append(g.Alertes, v)
	}
	return d
}

func contientTexte(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// decomposerRegle : partie gauche (la valeur suivie) et partie droite (le
// seuil) de la comparaison d'une règle, en passant « and / unless »
// (règles de modèle) et les parenthèses. Faux si la règle n'est pas une
// simple comparaison (ex. « a or b »).
func decomposerRegle(expr string) (parser.Expr, parser.Expr, bool) {
	if expr == "" {
		return nil, nil, false
	}
	e, err := alertes.AnalyserExpr(expr)
	if err != nil {
		return nil, nil, false
	}
	for {
		switch x := e.(type) {
		case *parser.ParenExpr:
			e = x.Expr
			continue
		case *parser.BinaryExpr:
			if x.Op == parser.LAND || x.Op == parser.LUNLESS {
				e = x.LHS
				continue
			}
			if x.Op.IsComparisonOperator() {
				return x.LHS, x.RHS, true
			}
		}
		return nil, nil, false
	}
}

// compatible : nombre d'étiquettes communes à la série et à l'alerte, ou
// -1 si l'une d'elles diffère.
func compatible(serie, alerte labels.Labels) int {
	n := 0
	ok := true
	serie.Range(func(l labels.Label) {
		if l.Name == labels.MetricName {
			return
		}
		if v := alerte.Get(l.Name); v != "" {
			if v != l.Value {
				ok = false
			}
			n++
		}
	})
	if !ok {
		return -1
	}
	return n
}

// courbeAlerte : valeur suivie sur la durée (90 points environ) et seuil,
// en SVG de la taille donnée.
func (c *Console) courbeAlerte(ctx context.Context, expr string, alerte labels.Labels, gravite string, t time.Time, duree time.Duration, largeur, hauteur float64) template.HTML {
	lhs, rhs, ok := decomposerRegle(expr)
	if !ok || alerte.IsEmpty() {
		return ""
	}
	pas := (duree / 90).Truncate(time.Minute)
	if pas < time.Minute {
		pas = time.Minute
	}
	courbes, _, err := c.lecteur.Plage(ctx, lhs.String(), t.Add(-duree), t, pas)
	if err != nil {
		return ""
	}
	meilleur, score := -1, -1
	for i, cb := range courbes {
		if s := compatible(cb.Etiquettes, alerte); s > score {
			meilleur, score = i, s
		}
	}
	if meilleur < 0 || len(courbes[meilleur].V) < 2 {
		return ""
	}
	seuil, aSeuil := math.NaN(), false
	switch x := rhs.(type) {
	case *parser.NumberLiteral:
		seuil, aSeuil = x.Val, true
	default:
		if ech, err := c.instant(ctx, rhs.String(), t); err == nil {
			s := -1
			for _, e := range ech {
				if k := compatible(e.Etiquettes, alerte); k > s {
					seuil, aSeuil, s = e.Valeur, true, k
				}
			}
		}
	}
	cb := courbes[meilleur]
	return svgCourbe(cb.T, cb.V, t.Add(-duree), t, seuil, aSeuil, gravite, largeur, hauteur)
}

// svgCourbe : courbe de la valeur, seuil en pointillé. Que des nombres et
// des couleurs fixes dans le SVG.
func svgCourbe(ts []int64, vs []float64, debut, fin time.Time, seuil float64, aSeuil bool, gravite string, l, h float64) template.HTML {
	const marge = 2.0
	mn, mx := math.Inf(1), math.Inf(-1)
	for _, v := range vs {
		if !math.IsNaN(v) && !math.IsInf(v, 0) {
			mn, mx = math.Min(mn, v), math.Max(mx, v)
		}
	}
	if aSeuil && !math.IsNaN(seuil) && !math.IsInf(seuil, 0) {
		mn, mx = math.Min(mn, seuil), math.Max(mx, seuil)
	}
	if math.IsInf(mn, 0) {
		return ""
	}
	if mx == mn {
		mx, mn = mx+1, mn-1
	}
	d0, dt := debut.UnixMilli(), float64(fin.Sub(debut)/time.Millisecond)
	x := func(t int64) float64 { return float64(t-d0) / dt * l }
	y := func(v float64) float64 { return marge + (h-2*marge)*(1-(v-mn)/(mx-mn)) }
	var pts []string
	for i, v := range vs {
		if !math.IsNaN(v) && !math.IsInf(v, 0) {
			pts = append(pts, fmt.Sprintf("%.1f,%.1f", x(ts[i]), y(v)))
		}
	}
	couleur := map[string]string{alertes.Critique: "var(--erreur)", alertes.Avertissement: "var(--attention)"}[gravite]
	if couleur == "" {
		couleur = "var(--info)"
	}
	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="courbe-alerte" width="%.0f" height="%.0f" viewBox="0 0 %.0f %.0f" preserveAspectRatio="none" role="img" aria-label="Valeur suivie et seuil">`, l, h, l, h)
	if aSeuil && !math.IsNaN(seuil) && !math.IsInf(seuil, 0) {
		fmt.Fprintf(&b, `<line x1="0" y1="%.1f" x2="%.0f" y2="%.1f" class="seuil"/>`, y(seuil), l, y(seuil))
	}
	fmt.Fprintf(&b, `<polyline fill="none" stroke="%s" stroke-width="1.8" vector-effect="non-scaling-stroke" points="%s"/></svg>`, couleur, strings.Join(pts, " "))
	return template.HTML(b.String()) // #nosec G203 -- SVG fait de nombres formatés et de couleurs fixes, aucune donnée externe
}

// fragmentAlertes : bloc « en cours », rechargé toutes les 30 s.
func (c *Console) fragmentAlertes(q *req) {
	d := c.enCoursVue(q.r.Context(), q.r.URL.Query().Get("vue"), q.t)
	c.pages.executer(q.w, 200, "alertes-fragment", Page{CSRF: q.s.CSRF, Admin: q.admin(), D: d})
}
