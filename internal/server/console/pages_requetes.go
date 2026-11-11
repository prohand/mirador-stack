// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"

	"mirador-stack/internal/server/alertes"
)

// Aide à l'écriture des requêtes PromQL dans l'éditeur de panneau (demande
// du 10/11, SPEC § 18.18) :
//   - vérification en direct : erreur en français, nombre de séries, une
//     valeur d'exemple, étiquettes disponibles (pour la légende), phrase qui
//     explique la requête, unité proposée ;
//   - assistant en 4 choix (métrique reçue, calcul, regroupement, filtre) :
//     la requête est construite ici, côté serveur, puis reste modifiable.
// Tout passe par le lecteur restreint (mêmes limites que les tableaux).

func (c *Console) routesRequetes() {
	m := c.mux
	m.HandleFunc("POST /tableaux/{nom}/verifier", c.admin(c.postVerifierRequete))
	m.HandleFunc("GET /tableaux/{nom}/etiquettes", c.admin(c.getEtiquettesMetrique))
	m.HandleFunc("POST /tableaux/{nom}/construire", c.admin(c.postConstruireRequete))
}

func repondreJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// labelsLabel : une étiquette (nom, valeur).
type labelsLabel = labels.Label

// --- Vérification ----------------------------------------------------------------------

type etiquetteVue struct {
	Nom     string   `json:"nom"`
	Valeurs []string `json:"valeurs"` // quelques exemples
}

type resultatVerif struct {
	OK          bool           `json:"ok"`
	Erreur      string         `json:"erreur,omitempty"`
	Series      int            `json:"series"`
	Exemple     string         `json:"exemple,omitempty"`
	Etiquettes  []etiquetteVue `json:"etiquettes,omitempty"`
	Explication string         `json:"explication,omitempty"`
	Unite       string         `json:"unite,omitempty"`
	UniteNom    string         `json:"unite_nom,omitempty"`
}

func (c *Console) postVerifierRequete(q *req) {
	t, _ := c.tableauOuCatalogue(q.r.PathValue("nom"))
	if t == nil {
		repondreJSON(q.w, http.StatusNotFound, resultatVerif{Erreur: "tableau inconnu"})
		return
	}
	repondreJSON(q.w, http.StatusOK, c.verifierRequete(q.r.Context(), t, q.r.PostFormValue("expr"), q.r.PostFormValue("unite"), q.t))
}

// verifierRequete : unite (celle du panneau) n'est pas utilisée pour
// l'exemple : il s'affiche dans l'unité qui va avec la requête.
func (c *Console) verifierRequete(ctx context.Context, t *Tableau, expr, _ string, maintenant time.Time) resultatVerif {
	var r resultatVerif
	expr = strings.TrimSpace(expr)
	if expr == "" {
		r.Erreur = "requête vide"
		return r
	}
	vals := map[string]string{}
	for _, v := range t.Variables {
		vals[v.Nom] = ""
	}
	d, err := lirePeriode(t.Periode)
	if err != nil {
		d = 6 * time.Hour
	}
	finale := substituer(expr, vals, pasPour(d), d)
	arbre, err := alertes.AnalyserExpr(finale)
	if err != nil {
		r.Erreur = erreurLisible(err)
		return r
	}
	r.Explication = expliquer(arbre)
	r.Unite = proposerUnite(arbre)
	for _, u := range unitesProposees {
		if u.Code == r.Unite {
			r.UniteNom = u.Nom
		}
	}
	ech, err := c.instant(ctx, finale, maintenant)
	if err != nil {
		r.Erreur = erreurLisible(err)
		return r
	}
	r.OK, r.Series = true, len(ech)
	if len(ech) > 0 {
		sort.Slice(ech, func(i, j int) bool { return nomSerie(ech[i].Etiquettes) < nomSerie(ech[j].Etiquettes) })
		// Exemple dans l'unité qui va avec la requête (celle du panneau
		// peut être fausse : une charge affichée en °C…).
		r.Exemple = formater(ech[0].Valeur, r.Unite, nil) + " (" + nomSerie(ech[0].Etiquettes) + ")"
		vus := map[string]map[string]bool{}
		for _, e := range ech {
			e.Etiquettes.Range(func(l labelsLabel) {
				if l.Name == "__name__" {
					return
				}
				if vus[l.Name] == nil {
					vus[l.Name] = map[string]bool{}
				}
				if len(vus[l.Name]) < 4 {
					vus[l.Name][l.Value] = true
				}
			})
		}
		for nom, vs := range vus {
			ev := etiquetteVue{Nom: nom}
			for v := range vs {
				ev.Valeurs = append(ev.Valeurs, v)
			}
			sort.Strings(ev.Valeurs)
			r.Etiquettes = append(r.Etiquettes, ev)
		}
		sort.Slice(r.Etiquettes, func(i, j int) bool { return r.Etiquettes[i].Nom < r.Etiquettes[j].Nom })
	}
	return r
}

var rePositionErreur = regexp.MustCompile(`(^|: )\d+:(\d+): `)

// erreurLisible : messages du moteur PromQL, en plus simple.
func erreurLisible(err error) string {
	s := rePositionErreur.ReplaceAllString(err.Error(), "${1}vers le caractère $2 : ")
	remplacements := []struct{ de, vers string }{
		{"expression : ", ""},
		{"parse error: ", "erreur de syntaxe : "},
		{"unexpected end of input", "la requête s'arrête trop tôt (parenthèse ou accolade non fermée ?)"},
		{"unexpected right parenthesis", "parenthèse fermante en trop"},
		{"unclosed left parenthesis", "parenthèse non fermée"},
		{"unclosed left brace", "accolade non fermée"},
		{"unexpected identifier", "mot inattendu"},
		{"unknown function with name", "fonction inconnue :"},
		{"expected type range vector in call to function", "cette fonction attend une fenêtre de temps, par ex. [5m], dans l'appel à"},
		{"ranges only allowed for vector selectors", "une fenêtre [..] ne se met qu'après une métrique"},
		{"vector cannot contain metrics with the same labelset", "plusieurs séries ont les mêmes étiquettes : ajouter « by (...) »"},
		{"many-to-many matching not allowed", "plusieurs séries de chaque côté : préciser « on (...) »"},
		{"bad number or duration syntax", "nombre ou durée mal écrit"},
	}
	for _, r := range remplacements {
		s = strings.ReplaceAll(s, r.de, r.vers)
	}
	return s
}

// --- Explication en français -------------------------------------------------------------

var nomsAgregations = map[string]string{
	"sum": "la somme", "avg": "la moyenne", "max": "le maximum", "min": "le minimum", "count": "le nombre",
	"topk": "les plus grandes valeurs", "bottomk": "les plus petites valeurs", "quantile": "le quantile",
	"stddev": "l'écart type", "group": "la liste", "count_values": "le nombre par valeur",
}

var nomsFonctions = map[string]string{
	"rate": "le débit par seconde", "irate": "le débit instantané par seconde", "increase": "l'augmentation",
	"delta": "la variation", "avg_over_time": "la moyenne dans le temps", "max_over_time": "le maximum dans le temps",
	"min_over_time": "le minimum dans le temps", "last_over_time": "la dernière valeur", "sum_over_time": "la somme dans le temps",
	"count_over_time": "le nombre de points", "changes": "le nombre de changements", "resets": "le nombre de remises à zéro",
	"deriv": "la pente", "predict_linear": "la prévision", "abs": "la valeur absolue", "round": "l'arrondi",
	"ceil": "l'arrondi au-dessus", "floor": "l'arrondi au-dessous", "absent": "l'absence", "sort": "le tri croissant",
	"sort_desc": "le tri décroissant", "clamp_max": "le plafonnement", "clamp_min": "le plancher", "lag": "l'ancienneté de la dernière valeur",
	"label_replace": "le renommage d'étiquette", "histogram_quantile": "le quantile de l'histogramme",
}

var nomsOperateurs = map[string]string{
	"+": "plus", "-": "moins", "*": "fois", "/": "divisé par", "%": "modulo", "^": "puissance",
	">": "gardé seulement s'il est supérieur à", "<": "gardé seulement s'il est inférieur à", ">=": "gardé s'il est au moins", "<=": "gardé s'il est au plus",
	"==": "gardé s'il vaut", "!=": "gardé s'il ne vaut pas", "and": "et seulement là où il y a aussi", "or": "sinon", "unless": "sauf là où il y a",
}

// expliquer : une phrase courte (300 caractères au plus).
func expliquer(e parser.Expr) string {
	s := strings.TrimSpace(expliquerNoeud(e))
	// Contractions : « de le » → « du », « de les » → « des ».
	s = strings.NewReplacer(" de le ", " du ", " de les ", " des ").Replace(s)
	if s != "" {
		s = strings.ToUpper(s[:1]) + s[1:]
	}
	if r := []rune(s); len(r) > 300 {
		s = string(r[:300]) + "…"
	}
	return s + "."
}

func expliquerNoeud(e parser.Expr) string {
	switch x := e.(type) {
	case *parser.ParenExpr:
		return expliquerNoeud(x.Expr)
	case *parser.NumberLiteral:
		return strconv.FormatFloat(x.Val, 'g', -1, 64)
	case *parser.StringLiteral:
		return "« " + x.Val + " »"
	case *parser.UnaryExpr:
		return "moins " + expliquerNoeud(x.Expr)
	case *parser.VectorSelector:
		return decrireSelecteur(x)
	case *parser.MatrixSelector:
		return expliquerNoeud(x.VectorSelector)
	case *parser.SubqueryExpr:
		return expliquerNoeud(x.Expr) + " calculé sur " + dureeLisible(x.Range)
	case *parser.AggregateExpr:
		nom := nomsAgregations[x.Op.String()]
		if nom == "" {
			nom = x.Op.String()
		}
		if x.Param != nil && (x.Op == parser.TOPK || x.Op == parser.BOTTOMK) {
			nom = "les " + expliquerNoeud(x.Param) + strings.TrimPrefix(nom, "les")
		}
		par := ""
		if len(x.Grouping) > 0 {
			if x.Without {
				par = " (sans tenir compte de " + strings.Join(x.Grouping, ", ") + ")"
			} else {
				par = " par " + strings.Join(x.Grouping, ", ")
			}
		}
		return nom + par + " de " + expliquerNoeud(x.Expr)
	case *parser.Call:
		f := x.Func.Name
		switch f {
		case "time":
			return "l'heure actuelle"
		case "vector":
			if len(x.Args) == 1 {
				return expliquerNoeud(x.Args[0])
			}
		}
		nom := nomsFonctions[f]
		if nom == "" {
			nom = "la fonction " + f
		}
		var args []string
		fenetre := ""
		for _, a := range x.Args {
			if m, ok := a.(*parser.MatrixSelector); ok {
				fenetre = " sur " + dureeLisible(m.Range)
			}
			if _, ok := a.(*parser.StringLiteral); ok {
				continue
			}
			args = append(args, expliquerNoeud(a))
		}
		if len(args) == 0 {
			return nom
		}
		return nom + fenetre + " de " + strings.Join(args, ", ")
	case *parser.BinaryExpr:
		g, d := expliquerNoeud(x.LHS), expliquerNoeud(x.RHS)
		// « 100 * X » ou « X * 100 » : un pourcentage.
		if n, ok := x.LHS.(*parser.NumberLiteral); ok && n.Val == 100 && x.Op == parser.MUL {
			return d + ", en pourcentage"
		}
		if n, ok := x.RHS.(*parser.NumberLiteral); ok && n.Val == 100 && x.Op == parser.MUL {
			return g + ", en pourcentage"
		}
		if n, ok := x.RHS.(*parser.NumberLiteral); ok && n.Val == 86400 && x.Op == parser.DIV {
			return g + ", en jours"
		}
		op := nomsOperateurs[x.Op.String()]
		if op == "" {
			op = x.Op.String()
		}
		return g + " " + op + " " + d
	}
	return e.String()
}

// decrireSelecteur : la métrique (avec sa description connue) et ses
// filtres fixes ; les filtres du tableau ($variables) sont résumés.
func decrireSelecteur(v *parser.VectorSelector) string {
	nom := v.Name
	var filtres []string
	variables := false
	for _, m := range v.LabelMatchers {
		if m.Name == "__name__" {
			if nom == "" {
				nom = m.Value
			}
			continue
		}
		if m.Value == ".*" || m.Value == "a^" {
			variables = true // filtre du tableau, vide
			continue
		}
		op := map[string]string{"=": "=", "!=": "≠", "=~": "parmi", "!~": "hors de"}[m.Type.String()]
		filtres = append(filtres, m.Name+" "+op+" "+m.Value)
	}
	s := nom
	if d := descriptionMetrique(nom); d != "" {
		s = d + " (" + nom + ")"
	}
	if len(filtres) > 0 {
		s += " où " + strings.Join(filtres, ", ")
	}
	if variables {
		s += ", selon les filtres du tableau"
	}
	return s
}

// --- Unité proposée ------------------------------------------------------------------

func proposerUnite(e parser.Expr) string {
	var noms []string
	dansRate, pourcent, jours := false, false, false
	parser.Inspect(e, func(n parser.Node, chemin []parser.Node) error {
		switch x := n.(type) {
		case *parser.VectorSelector:
			nom := x.Name
			for _, m := range x.LabelMatchers {
				if m.Name == "__name__" && nom == "" {
					nom = m.Value
				}
			}
			noms = append(noms, nom)
		case *parser.Call:
			if x.Func.Name == "rate" || x.Func.Name == "irate" {
				dansRate = true
			}
		case *parser.BinaryExpr:
			if n, ok := x.LHS.(*parser.NumberLiteral); ok && n.Val == 100 && x.Op == parser.MUL {
				pourcent = true
			}
			if n, ok := x.RHS.(*parser.NumberLiteral); ok && n.Val == 100 && x.Op == parser.MUL {
				pourcent = true
			}
			if n, ok := x.RHS.(*parser.NumberLiteral); ok && n.Val == 86400 && x.Op == parser.DIV {
				jours = true
			}
		}
		return nil
	})
	switch {
	case pourcent:
		return "percent"
	case jours:
		return "d"
	}
	for _, n := range noms {
		switch {
		case strings.HasSuffix(n, "_celsius") || strings.Contains(n, "temp"):
			return "celsius"
		case strings.HasSuffix(n, "_bytes_total") || (strings.HasSuffix(n, "_bytes") && dansRate):
			if dansRate {
				return "Bps"
			}
			return "bytes"
		case strings.HasSuffix(n, "_bytes"):
			return "bytes"
		case strings.HasSuffix(n, "_ratio"):
			return "percentunit"
		case strings.HasSuffix(n, "_seconds_total") && dansRate:
			return "percentunit"
		case strings.HasSuffix(n, "_seconds") && !strings.Contains(n, "timestamp"):
			return "s"
		case strings.HasSuffix(n, "_watts"):
			return "watt"
		case strings.HasSuffix(n, "_bps"):
			return "bps"
		}
	}
	return ""
}

// --- Assistant : métriques reçues, étiquettes, construction ------------------------------

type metriqueVue struct {
	Nom, Description string
	Compteur         bool // nom en _total : on regarde plutôt son débit
	Series           int
}

type cacheMetriques struct {
	mu      sync.Mutex
	liste   []metriqueVue
	expire  time.Time
	etiq    map[string][]etiquetteVue
	etiqExp map[string]time.Time
}

const dureeCacheMetriques = 5 * time.Minute

// metriquesRecues : noms des métriques présentes (cache 5 min).
func (c *Console) metriquesRecues(ctx context.Context, t time.Time) []metriqueVue {
	c.cacheMet.mu.Lock()
	if t.Before(c.cacheMet.expire) {
		l := c.cacheMet.liste
		c.cacheMet.mu.Unlock()
		return l
	}
	c.cacheMet.mu.Unlock()
	var out []metriqueVue
	if c.lecteur != nil {
		if ech, err := c.lecteur.Instant(ctx, `count by (__name__) ({__name__=~".+"})`, t); err == nil {
			for _, e := range ech {
				n := e.Etiquettes.Get("__name__")
				out = append(out, metriqueVue{Nom: n, Description: descriptionMetrique(n), Compteur: strings.HasSuffix(n, "_total"), Series: int(e.Valeur)})
			}
		}
	}
	// Métriques décrites d'abord (les plus utiles), puis les autres.
	sort.Slice(out, func(i, j int) bool {
		if (out[i].Description != "") != (out[j].Description != "") {
			return out[i].Description != ""
		}
		return out[i].Nom < out[j].Nom
	})
	c.cacheMet.mu.Lock()
	c.cacheMet.liste, c.cacheMet.expire = out, t.Add(dureeCacheMetriques)
	c.cacheMet.mu.Unlock()
	return out
}

var reNomMetrique = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]{0,199}$`)

func (c *Console) etiquettesMetrique(ctx context.Context, nom string, t time.Time) ([]etiquetteVue, error) {
	if !reNomMetrique.MatchString(nom) {
		return nil, errors.New("nom de métrique invalide")
	}
	c.cacheMet.mu.Lock()
	if l, ok := c.cacheMet.etiq[nom]; ok && t.Before(c.cacheMet.etiqExp[nom]) {
		c.cacheMet.mu.Unlock()
		return l, nil
	}
	c.cacheMet.mu.Unlock()
	if c.lecteur == nil {
		return nil, errors.New("métriques indisponibles")
	}
	ech, err := c.lecteur.Instant(ctx, "last_over_time("+nom+"[15m])", t)
	if err != nil {
		return nil, err
	}
	vus := map[string]map[string]bool{}
	for _, e := range ech {
		e.Etiquettes.Range(func(l labelsLabel) {
			if l.Name == "__name__" {
				return
			}
			if vus[l.Name] == nil {
				vus[l.Name] = map[string]bool{}
			}
			if len(vus[l.Name]) < 50 {
				vus[l.Name][l.Value] = true
			}
		})
	}
	var out []etiquetteVue
	for n, vs := range vus {
		ev := etiquetteVue{Nom: n}
		for v := range vs {
			ev.Valeurs = append(ev.Valeurs, v)
		}
		sort.Strings(ev.Valeurs)
		out = append(out, ev)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Nom < out[j].Nom })
	c.cacheMet.mu.Lock()
	if c.cacheMet.etiq == nil || len(c.cacheMet.etiq) > 200 {
		c.cacheMet.etiq, c.cacheMet.etiqExp = map[string][]etiquetteVue{}, map[string]time.Time{}
	}
	c.cacheMet.etiq[nom], c.cacheMet.etiqExp[nom] = out, t.Add(time.Minute)
	c.cacheMet.mu.Unlock()
	return out, nil
}

func (c *Console) getEtiquettesMetrique(q *req) {
	l, err := c.etiquettesMetrique(q.r.Context(), q.r.URL.Query().Get("metrique"), q.t)
	if err != nil {
		repondreJSON(q.w, http.StatusBadRequest, map[string]string{"erreur": err.Error()})
		return
	}
	repondreJSON(q.w, http.StatusOK, map[string]any{"etiquettes": l})
}

// Choix de l'assistant.
var (
	calculsAssistant = []struct{ Code, Nom string }{
		{"valeur", "Valeur telle quelle"}, {"debit", "Débit par seconde (compteurs _total)"},
		{"augmentation", "Augmentation sur la période affichée"}, {"moyenne-temps", "Moyenne sur 5 min"},
	}
	combinaisonsAssistant = []struct{ Code, Nom string }{
		{"", "Aucune (une courbe par série)"}, {"sum", "Somme"}, {"avg", "Moyenne"}, {"max", "Maximum"},
		{"min", "Minimum"}, {"count", "Nombre de séries"},
	}
)

type requeteConstruite struct {
	Expr    string `json:"expr,omitempty"`
	Legende string `json:"legende,omitempty"`
	Unite   string `json:"unite,omitempty"`
	Type    string `json:"type,omitempty"`
	Erreur  string `json:"erreur,omitempty"`
}

// construireRequete : PromQL à partir des 4 choix (tout est vérifié : rien
// de ce qui est reçu n'est recopié sans contrôle).
func construireRequete(t *Tableau, metrique, calcul, combiner, par, filtreEtiq, filtreValeur string, filtresTab bool) (requeteConstruite, error) {
	var r requeteConstruite
	if !reNomMetrique.MatchString(metrique) {
		return r, errors.New("choisir une métrique")
	}
	var filtres []string
	if filtresTab {
		filtres = filtresTableau(t)
	}
	if filtreEtiq != "" || filtreValeur != "" {
		if !reNomEtiquette.MatchString(filtreEtiq) || !reValeurVariable.MatchString(filtreValeur) {
			return r, errors.New("filtre : étiquette et valeur simples (lettres, chiffres, . _ : / @ + -)")
		}
		filtres = append(filtres, filtreEtiq+`="`+filtreValeur+`"`)
	}
	sel := metrique
	if len(filtres) > 0 {
		sel += "{" + strings.Join(filtres, ", ") + "}"
	}
	switch calcul {
	case "valeur", "":
	case "debit":
		sel = "rate(" + sel + "[$__rate_interval])"
	case "augmentation":
		sel = "increase(" + sel + "[$__range])"
	case "moyenne-temps":
		sel = "avg_over_time(" + sel + "[5m])"
	default:
		return r, errors.New("calcul inconnu")
	}
	ok := false
	for _, x := range combinaisonsAssistant {
		ok = ok || x.Code == combiner
	}
	if !ok {
		return r, errors.New("combinaison inconnue")
	}
	if par != "" && !reNomEtiquette.MatchString(par) {
		return r, errors.New("regroupement invalide")
	}
	r.Expr, r.Type = sel, PanneauCourbe
	switch {
	case combiner != "" && par != "":
		r.Expr = combiner + " by (" + par + ") (" + sel + ")"
		r.Legende = "{{" + par + "}}"
	case combiner != "":
		r.Expr = combiner + "(" + sel + ")"
		r.Type = PanneauChiffre
	default:
		r.Legende = "{{instance}}"
	}
	if a, err := alertes.AnalyserExpr(substituer(r.Expr, map[string]string{}, time.Minute, 6*time.Hour)); err == nil {
		r.Unite = proposerUnite(a)
		if combiner == "count" {
			r.Unite = ""
		}
	}
	return r, nil
}

func (c *Console) postConstruireRequete(q *req) {
	t, _ := c.tableauOuCatalogue(q.r.PathValue("nom"))
	if t == nil {
		repondreJSON(q.w, http.StatusNotFound, requeteConstruite{Erreur: "tableau inconnu"})
		return
	}
	f := champs{q}
	r, err := construireRequete(t, f.texte("metrique"), f.texte("calcul"), f.texte("combiner"), f.texte("par"),
		f.texte("filtre_etiquette"), f.texte("filtre_valeur"), f.coche("filtres_tableau"))
	if err != nil {
		repondreJSON(q.w, http.StatusBadRequest, requeteConstruite{Erreur: err.Error()})
		return
	}
	repondreJSON(q.w, http.StatusOK, r)
}
