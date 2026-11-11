// SPDX-License-Identifier: GPL-3.0-or-later

package alertes

import (
	"errors"
	"fmt"
	"maps"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/promql/parser"
	"github.com/prometheus/prometheus/util/annotations"
)

// Langage des règles (SPEC.md § 17.1) : PromQL, le langage de vmalert et de
// Prometheus, évalué par la bibliothèque de Prometheus déjà présente pour le
// stockage. C'est un langage de requête : il lit des séries et calcule, il
// ne peut rien exécuter. On le restreint encore :
//   - seules les fonctions de la liste ci-dessous sont acceptées (pas de
//     fonction expérimentale, rien qui n'a pas été relu) ;
//   - pas de modificateur « @ » ni de décalage négatif (lecture dans le futur) ;
//   - taille de l'expression, nombre de points lus et durée bornés (moteur.go).
//
// Ajout mirador : lag(), reprise de MetricsQL (VictoriaMetrics), utilisée par
// 5 des règles actuelles (« source muette ») : secondes écoulées depuis le
// dernier point de chaque série dans la fenêtre.

// fonctionsPermises : fonctions PromQL acceptées dans une règle.
var fonctionsPermises = []string{
	"abs", "absent", "absent_over_time", "avg_over_time", "ceil", "changes",
	"clamp", "clamp_max", "clamp_min", "count_over_time", "day_of_month",
	"day_of_week", "day_of_year", "days_in_month", "delta", "deriv", "exp",
	"floor", "hour", "idelta", "increase", "irate", "label_join",
	"label_replace", "last_over_time", "ln", "log10", "log2", "max_over_time",
	"min_over_time", "minute", "month", "predict_linear", "present_over_time",
	"quantile_over_time", "rate", "resets", "round", "scalar", "sgn", "sort",
	"sort_desc", "sqrt", "stddev_over_time", "stdvar_over_time",
	"sum_over_time", "time", "timestamp", "vector", "year",
}

// analyseur : parseur PromQL limité aux fonctions permises (+ lag).
var analyseur parser.Parser

func init() {
	fs := make(map[string]*parser.Function, len(fonctionsPermises)+1)
	for _, nom := range fonctionsPermises {
		f, ok := parser.Functions[nom]
		if !ok {
			panic("fonction PromQL inconnue de la bibliothèque : " + nom)
		}
		fs[nom] = f
	}
	fs["lag"] = &parser.Function{Name: "lag", ArgTypes: []parser.ValueType{parser.ValueTypeMatrix},
		ReturnType: parser.ValueTypeVector}
	analyseur = parser.NewParser(parser.Options{Functions: maps.Clone(fs)})
	// Le moteur cherche les fonctions dans cette table globale : on y ajoute
	// lag. Seul notre parseur l'accepte (il n'existe pas en PromQL).
	promql.FunctionCalls["lag"] = fonctionLag
}

// fonctionLag : lag(serie[fenetre]) = secondes entre l'instant d'évaluation et
// le dernier point de la série dans la fenêtre (comme MetricsQL). Série sans
// point dans la fenêtre : rien (comme MetricsQL).
func fonctionLag(_ []promql.Vector, matrice promql.Matrix, _ parser.Expressions, enh *promql.EvalNodeHelper) (promql.Vector, annotations.Annotations) {
	if len(matrice) == 0 || len(matrice[0].Floats) == 0 {
		return enh.Out, nil
	}
	s := matrice[0]
	dernier := s.Floats[len(s.Floats)-1].T
	return append(enh.Out, promql.Sample{Metric: s.Metric, F: float64(enh.Ts-dernier) / 1000}), nil
}

// TailleMaxExpr : longueur maximale d'une expression (octets).
const TailleMaxExpr = 4096

// AnalyserExpr vérifie une expression de règle et renvoie son arbre.
// Refuse : erreur de syntaxe, fonction non permise, « @ », décalage négatif,
// résultat qui n'est ni un vecteur ni un nombre.
func AnalyserExpr(expr string) (parser.Expr, error) {
	if expr == "" {
		return nil, errors.New("expression vide")
	}
	if len(expr) > TailleMaxExpr {
		return nil, fmt.Errorf("expression trop longue (%d octets, %d au plus)", len(expr), TailleMaxExpr)
	}
	e, err := analyseur.ParseExpr(expr)
	if err != nil {
		return nil, fmt.Errorf("expression : %w", err)
	}
	if t := e.Type(); t != parser.ValueTypeVector && t != parser.ValueTypeScalar {
		return nil, fmt.Errorf("l'expression doit donner un vecteur, pas %s", t)
	}
	var refus error
	parser.Inspect(e, func(n parser.Node, _ []parser.Node) error {
		switch x := n.(type) {
		case *parser.VectorSelector:
			if x.Timestamp != nil || x.StartOrEnd != 0 {
				refus = errors.New("le modificateur @ n'est pas permis")
			}
			if x.OriginalOffset < 0 {
				refus = errors.New("décalage négatif non permis")
			}
		case *parser.SubqueryExpr:
			if x.Timestamp != nil || x.StartOrEnd != 0 {
				refus = errors.New("le modificateur @ n'est pas permis")
			}
			if x.OriginalOffset < 0 {
				refus = errors.New("décalage négatif non permis")
			}
		}
		return refus
	})
	if refus != nil {
		return nil, refus
	}
	return e, nil
}

// fenetreMax renvoie la plus longue période lue par l'expression (fenêtres
// [..], décalages, sous-requêtes). Sert à refuser ce que le stockage brut ne
// garde pas et à espacer l'évaluation des règles coûteuses.
func fenetreMax(e parser.Expr) (max int64) {
	parser.Inspect(e, func(n parser.Node, chemin []parser.Node) error {
		var d int64
		switch x := n.(type) {
		case *parser.MatrixSelector:
			d = x.Range.Milliseconds()
			if vs, ok := x.VectorSelector.(*parser.VectorSelector); ok {
				d += vs.OriginalOffset.Milliseconds()
			}
		case *parser.SubqueryExpr:
			d = x.Range.Milliseconds() + x.OriginalOffset.Milliseconds()
		case *parser.VectorSelector:
			d = x.OriginalOffset.Milliseconds()
		}
		// Les sous-requêtes imbriquées s'additionnent.
		for _, p := range chemin {
			if sq, ok := p.(*parser.SubqueryExpr); ok {
				d += sq.Range.Milliseconds() + sq.OriginalOffset.Milliseconds()
			}
		}
		if d > max {
			max = d
		}
		return nil
	})
	return max
}

// metriquesLues : noms des métriques lues par l'expression (rapport d'import :
// « source sans agent »). Nom donné par une expression régulière : le texte
// de l'expression est rendu tel quel.
func metriquesLues(e parser.Expr) []string {
	vus := map[string]bool{}
	var out []string
	parser.Inspect(e, func(n parser.Node, _ []parser.Node) error {
		vs, ok := n.(*parser.VectorSelector)
		if !ok {
			return nil
		}
		for _, m := range vs.LabelMatchers {
			if m.Name == labels.MetricName && (m.Type == labels.MatchEqual || m.Type == labels.MatchRegexp) && !vus[m.Value] {
				vus[m.Value] = true
				out = append(out, m.Value)
			}
		}
		return nil
	})
	return out
}
