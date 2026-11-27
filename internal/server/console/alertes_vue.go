// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"math"
	"mirador-stack/internal/server/flotte"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"

	"mirador-stack/internal/server/alertes"
	"mirador-stack/internal/server/notif"
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
	// Grand parc (SPEC § 141) : clé du groupe (actions de groupe) et
	// résumé (« 271 machines · Paris 88, Lyon 71, + 12 sites »).
	Cle    string
	Resume string
	Ouvert bool
}

// donneesEnCours : bloc « en cours » (page et fragment rechargé).
type donneesEnCours struct {
	Vue                              string // machine, regle, liste
	Critiques, Avertissements, Infos int
	Total                            int
	Groupes                          []groupeAlertes
	Liste                            []resumeAlerte
	Attente                          []resumeAlerte          // pas encore des alertes
	Prises                           []resumeAlerte          // prises en charge (SPEC § 30)
	Masquees                         []resumeAlerte          // conséquences d'une alerte plus grave (bloc replié)
	Sites                            []notif.SiteInjoignable // sites injoignables (SPEC § 39)
	Retour                           string                  // page après une action sur une ligne
	Grand                            bool                    // beaucoup d'alertes : groupes repliés, pas de mini-courbes
}

// maxCourbes : mini-courbes calculées par affichage (coût borné).
const maxCourbes = 30

func vueAlertesValide(v string) string {
	if v == "regle" || v == "liste" || v == "site" {
		return v
	}
	return "machine"
}

// cleGroupeAlerte : groupe d'une alerte selon la vue (machine, règle, site).
func cleGroupeAlerte(r resumeAlerte, vue string, sites map[string]string) (cle, titre, lien string) {
	switch {
	case vue == "site":
		s := sites[r.Machine]
		if s == "" {
			return "s:", "sans site", "/machines"
		}
		return "s:" + s, "Site " + s, "/machines?site=" + url.QueryEscape(s)
	case vue == "regle" || r.Machine == "":
		return "r:" + r.Regle, libelleRegle(r.Regle), "/alertes?regle=" + url.QueryEscape(r.Regle)
	}
	return r.Machine, r.Machine, "/machines/" + url.PathEscape(r.Machine)
}

// sitesMachines : site de chaque machine (vue « par site », résumés).
func (c *Console) sitesMachines(ctx context.Context) map[string]string {
	out := map[string]string{}
	if ms, err := flotte.ListerMachines(ctx, c.db); err == nil {
		for _, m := range ms {
			out[m.Nom] = m.Site
		}
	}
	return out
}

// enCoursVue : alertes en cours groupées, avec leurs mini-courbes.
func (c *Console) enCoursVue(ctx context.Context, vue string, t time.Time) donneesEnCours {
	d := donneesEnCours{Vue: vueAlertesValide(vue)}
	d.Retour = "/alertes?vue=" + url.QueryEscape(d.Vue)
	rs, attente := separerAttente(c.alertesEnCours(""))
	rs, d.Prises = separerPrises(rs)
	rs, d.Masquees = separerMasquees(rs)
	d.Attente = attente
	if c.opt.Aiguilleur != nil {
		d.Sites = c.opt.Aiguilleur.SitesInjoignables()
	}
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
	sites := c.sitesMachines(ctx)
	// Grand parc : au-delà de maxCourbes alertes, groupes repliés (le plus
	// grave ouvert) et pas de mini-courbes (30 requêtes toutes les 30 s
	// par onglet ouvert, pour des courbes que personne ne déplie).
	d.Grand = len(rs) > maxCourbes
	for _, r := range rs {
		cle, titre, lien := cleGroupeAlerte(r, d.Vue, sites)
		i, ok := index[cle]
		if !ok {
			i = len(d.Groupes)
			index[cle] = i
			d.Groupes = append(d.Groupes, groupeAlertes{Titre: titre, Lien: lien, Cle: cle, Ouvert: !d.Grand})
		}
		g := &d.Groupes[i]
		if !contientTexte(g.Gravites, r.Gravite) {
			g.Gravites = append(g.Gravites, r.Gravite)
		}
		v := vueAlerte{resumeAlerte: r}
		if !d.Grand && n < maxCourbes && c.lecteur != nil {
			n++
			v.Courbe = c.courbeAlerte(ctx, exprs[r.Regle], etiquettes[r.ID], r.Gravite, t, 6*time.Hour, 120, 28)
		}
		g.Alertes = append(g.Alertes, v)
	}
	if d.Grand {
		for i := range d.Groupes {
			d.Groupes[i].Resume = resumeGroupe(d.Groupes[i].Alertes, d.Vue, sites)
		}
		if len(d.Groupes) > 0 {
			d.Groupes[0].Ouvert = true
		}
	}
	return d
}

// resumeGroupe : « 271 machines · Paris 88, Lyon 71, + 12 sites » (vue par
// règle), « 48 machines » (vue par site).
func resumeGroupe(as []vueAlerte, vue string, sites map[string]string) string {
	machines := map[string]bool{}
	parSite := map[string]int{}
	for _, a := range as {
		if a.Machine != "" && !machines[a.Machine] {
			machines[a.Machine] = true
			if s := sites[a.Machine]; s != "" {
				parSite[s]++
			}
		}
	}
	if len(machines) < 2 {
		return ""
	}
	r := nbMachines(len(machines))
	if vue == "regle" && len(parSite) > 0 {
		cs := trierComptes(parSite)
		var l []string
		for i, x := range cs {
			if i == 2 {
				l = append(l, "+ "+strconv.Itoa(len(cs)-2)+" "+accorder(len(cs)-2, "site(s)"))
				break
			}
			l = append(l, x.Nom+" "+strconv.Itoa(x.N))
		}
		r += " · " + strings.Join(l, ", ")
	}
	return r
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

// etiquettesOn : étiquettes du « on (…) » de la comparaison principale
// d'une règle (vide sans « on »).
func etiquettesOn(expr string) []string {
	e, err := alertes.AnalyserExpr(expr)
	if err != nil {
		return nil
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
			if x.Op.IsComparisonOperator() && x.VectorMatching != nil && x.VectorMatching.On {
				return x.VectorMatching.MatchingLabels
			}
		}
		return nil
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
	ts, vs, seuil, aSeuil, ok := c.serieAlerte(ctx, expr, alerte, t, duree, 90)
	if !ok {
		return ""
	}
	return svgCourbe(ts, vs, t.Add(-duree), t, seuil, aSeuil, gravite, largeur, hauteur)
}

// courbeAlerteJSON : mêmes données pour la grande courbe de la page d'une
// alerte, dessinée par graphes.js (axes, valeur au survol, seuil en
// pointillé, unité) au lieu d'un simple trait sans échelle (retour du 19/11).
func (c *Console) courbeAlerteJSON(ctx context.Context, expr, nom string, alerte labels.Labels, t time.Time, duree time.Duration) string {
	ts, vs, seuil, aSeuil, ok := c.serieAlerte(ctx, expr, alerte, t, duree, 288)
	if !ok {
		return ""
	}
	d := jsonCourbe{Unite: uniteDeRegle(expr), Series: []jsonSerie{{Nom: nom}}}
	for i, v := range vs {
		d.X = append(d.X, ts[i]/1000)
		if math.IsNaN(v) || math.IsInf(v, 0) {
			d.Series[0].V = append(d.Series[0].V, nil)
			continue
		}
		v := v
		d.Series[0].V = append(d.Series[0].V, &v)
	}
	if aSeuil && !math.IsNaN(seuil) && !math.IsInf(seuil, 0) {
		d.Seuil = &seuil
	}
	b, err := json.Marshal(d)
	if err != nil {
		return ""
	}
	return string(b)
}

// uniteDeRegle : unité devinée d'après le nom de la métrique suivie.
func uniteDeRegle(expr string) string {
	e := strings.ToLower(expr)
	switch {
	case strings.Contains(e, "celsius") || strings.Contains(e, "temperature"):
		return "celsius"
	case strings.Contains(e, "percent") || strings.Contains(e, "_pct") || strings.Contains(e, "* 100"):
		return "percent"
	case strings.Contains(e, "_bytes"):
		return "bytes"
	case strings.Contains(e, "_seconds") && !strings.Contains(e, "timestamp"):
		return "s"
	}
	return ""
}

// serieAlerte : la série suivie par la règle pour cette alerte (n points
// environ sur la durée) et son seuil.
func (c *Console) serieAlerte(ctx context.Context, expr string, alerte labels.Labels, t time.Time, duree time.Duration, n int) (ts []int64, vs []float64, seuil float64, aSeuil bool, ok bool) {
	lhs, rhs, ok := decomposerRegle(expr)
	if !ok || alerte.IsEmpty() {
		return nil, nil, 0, false, false
	}
	pas := (duree / time.Duration(n)).Truncate(time.Minute)
	if pas < time.Minute {
		pas = time.Minute
	}
	courbes, _, err := c.lecteur.Plage(ctx, lhs.String(), t.Add(-duree), t, pas)
	if err != nil {
		return nil, nil, 0, false, false
	}
	meilleur, score := -1, -1
	for i, cb := range courbes {
		if s := compatible(cb.Etiquettes, alerte); s > score {
			meilleur, score = i, s
		}
	}
	if meilleur < 0 || len(courbes[meilleur].V) < 2 {
		return nil, nil, 0, false, false
	}
	seuil, aSeuil = math.NaN(), false
	switch x := rhs.(type) {
	case *parser.NumberLiteral:
		seuil, aSeuil = x.Val, true
	default:
		// Seuil par machine (modèles : « > on (instance) group_left ()
		// mirador_modele_parametre{…} ») : seules les étiquettes du « on (…) »
		// comptent ; la série du seuil a job="mirador", l'alerte job="node"
		// (sans cela, aucun seuil trouvé, pas de pointillé : retour du 19/11).
		sur := etiquettesOn(expr)
		if ech, err := c.instant(ctx, rhs.String(), t); err == nil {
			s := -1
			for _, e := range ech {
				ls := e.Etiquettes
				if len(sur) > 0 {
					ls = ls.MatchLabels(true, sur...)
				}
				if k := compatible(ls, alerte); k > s {
					seuil, aSeuil, s = e.Valeur, true, k
				}
			}
		}
	}
	cb := courbes[meilleur]
	return cb.T, cb.V, seuil, aSeuil, true
}

// svgCourbe : courbe de la valeur, seuil en pointillé. Que des nombres et
// des couleurs fixes dans le SVG.
func svgCourbe(ts []int64, vs []float64, debut, fin time.Time, seuil float64, aSeuil bool, gravite string, l, h float64) template.HTML {
	return svgCourbeEchelle(ts, vs, debut, fin, seuil, aSeuil, gravite, l, h, false)
}

// svgCourbeEchelle : idem ; depuisZero : l'échelle part de 0 (temps de
// réponse des sondes, SPEC § 48).
func svgCourbeEchelle(ts []int64, vs []float64, debut, fin time.Time, seuil float64, aSeuil bool, gravite string, l, h float64, depuisZero bool) template.HTML {
	const marge = 2.0
	mn, mx := math.Inf(1), math.Inf(-1)
	if depuisZero {
		mn = 0
	}
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
	couleur := map[string]string{alertes.Critique: "var(--erreur)", alertes.Avertissement: "var(--attention)", "ok": "var(--ok)"}[gravite]
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
