// SPDX-License-Identifier: GPL-3.0-or-later

package alertes

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/storage"
)

// Lecteur : requêtes PromQL pour l'interface (graphiques, tableaux, « tester
// la règle »). Même langage restreint que les règles (liste blanche de
// fonctions, pas de « @ »), et ses propres limites : au plus 2 requêtes en
// même temps, 500 000 points chargés, 10 s, 2 000 séries et 1 500 pas par
// courbe. Une page lourde ne peut donc ni saturer la mémoire ni ralentir les
// alertes.
type Lecteur struct {
	moteur  *promql.Engine
	donnees storage.Queryable
	jetons  chan struct{}
}

// Limites du lecteur.
const (
	LecteurPointsMax = 500_000
	LecteurSeriesMax = 2000
	LecteurPasMax    = 1500
	lecteurDelai     = 10 * time.Second
)

// NouveauLecteur : donnees choisit la base (brute ou agrégée) selon la période.
func NouveauLecteur(donnees storage.Queryable, journal *slog.Logger) *Lecteur {
	if journal == nil {
		journal = slog.New(slog.DiscardHandler)
	}
	return &Lecteur{
		donnees: donnees,
		jetons:  make(chan struct{}, 2),
		moteur: promql.NewEngine(promql.EngineOpts{
			Logger:        journal.With("composant", "promql-interface"),
			MaxSamples:    LecteurPointsMax,
			Timeout:       lecteurDelai,
			LookbackDelta: 5 * time.Minute,
			Parser:        analyseur,
		}),
	}
}

// Echantillon : une valeur d'une série à un instant.
type Echantillon struct {
	Etiquettes labels.Labels
	Valeur     float64
}

// Courbe : une série sur une période (horodatages en millisecondes).
type Courbe struct {
	Etiquettes labels.Labels
	T          []int64
	V          []float64
}

// ErrOccupe : trop de requêtes en même temps.
var ErrOccupe = errors.New("serveur occupé : réessayer dans un instant")

func (l *Lecteur) prendre(ctx context.Context) error {
	select {
	case l.jetons <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(5 * time.Second):
		return ErrOccupe
	}
}

// Instant : valeur de l'expression à l'instant t.
func (l *Lecteur) Instant(ctx context.Context, expr string, t time.Time) ([]Echantillon, error) {
	if _, err := AnalyserExpr(expr); err != nil {
		return nil, err
	}
	if err := l.prendre(ctx); err != nil {
		return nil, err
	}
	defer func() { <-l.jetons }()
	q, err := l.moteur.NewInstantQuery(ctx, l.donnees, nil, expr, t)
	if err != nil {
		return nil, err
	}
	defer q.Close()
	res := q.Exec(ctx)
	if res.Err != nil {
		return nil, res.Err
	}
	switch v := res.Value.(type) {
	case promql.Vector:
		if len(v) > LecteurSeriesMax {
			return nil, fmt.Errorf("trop de séries (%d, %d au plus) : affiner", len(v), LecteurSeriesMax)
		}
		out := make([]Echantillon, 0, len(v))
		for _, s := range v {
			if s.H == nil {
				out = append(out, Echantillon{Etiquettes: s.Metric, Valeur: s.F})
			}
		}
		return out, nil
	case promql.Scalar:
		return []Echantillon{{Etiquettes: labels.EmptyLabels(), Valeur: v.V}}, nil
	}
	return nil, fmt.Errorf("résultat de type %s inattendu", res.Value.Type())
}

// Plage : courbes de l'expression sur [debut, fin], un point par pas. Le
// pas est agrandi si la période demande plus de 1 500 points par courbe.
func (l *Lecteur) Plage(ctx context.Context, expr string, debut, fin time.Time, pas time.Duration) ([]Courbe, time.Duration, error) {
	if _, err := AnalyserExpr(expr); err != nil {
		return nil, 0, err
	}
	if !fin.After(debut) || fin.Sub(debut) > 31*24*time.Hour {
		return nil, 0, errors.New("période invalide (31 jours au plus)")
	}
	if min := fin.Sub(debut) / LecteurPasMax; pas < min {
		pas = min
	}
	if pas < 10*time.Second {
		pas = 10 * time.Second
	}
	pas = pas.Truncate(time.Second)
	if err := l.prendre(ctx); err != nil {
		return nil, 0, err
	}
	defer func() { <-l.jetons }()
	q, err := l.moteur.NewRangeQuery(ctx, l.donnees, nil, expr, debut, fin, pas)
	if err != nil {
		return nil, 0, err
	}
	defer q.Close()
	res := q.Exec(ctx)
	if res.Err != nil {
		return nil, 0, res.Err
	}
	m, ok := res.Value.(promql.Matrix)
	if !ok {
		return nil, 0, fmt.Errorf("résultat de type %s inattendu", res.Value.Type())
	}
	if len(m) > LecteurSeriesMax {
		return nil, 0, fmt.Errorf("trop de séries (%d, %d au plus) : affiner", len(m), LecteurSeriesMax)
	}
	out := make([]Courbe, 0, len(m))
	for _, s := range m {
		c := Courbe{Etiquettes: s.Metric, T: make([]int64, len(s.Floats)), V: make([]float64, len(s.Floats))}
		for i, p := range s.Floats {
			c.T[i], c.V[i] = p.T, p.F
		}
		out = append(out, c)
	}
	return out, pas, nil
}
