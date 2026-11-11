// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"regexp"
	"sort"
	"strings"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/tsdb/chunkenc"
	"github.com/prometheus/prometheus/util/annotations"
)

// Historique d'une machine renommée (demande du 11/11, SPEC § 18.23).
//
// La base des métriques et les fichiers de logs ne savent pas réécrire les
// données passées. L'ancien nom est donc rattaché au nouveau AU MOMENT DE LA
// LECTURE, pour l'interface seulement (graphiques, tableaux, logs) :
//   - un filtre sur instance="nouveau" trouve aussi les séries de l'ancien
//     nom ;
//   - ces séries sont montrées sous le nouveau nom, et fusionnées avec
//     celles du nouveau nom quand le reste des étiquettes est identique :
//     une seule courbe continue.
// Rien n'est fait tant qu'aucune machine n'a été renommée (coût nul). Les
// règles d'alerte, elles, lisent les données telles quelles (elles ne
// regardent que les dernières minutes, toujours sous le nouveau nom).

// etiquetteNom : l'étiquette des métriques qui porte le nom de la machine
// (imposée par l'ingestion).
const etiquetteNom = "instance"

// avecNomsAnciens : enveloppe une source de métriques. anciens donne, à
// chaque lecture, la table « ancien nom → nom actuel » (vide = rien à faire).
func avecNomsAnciens(source storage.Queryable, anciens func() map[string]string) storage.Queryable {
	return storage.QueryableFunc(func(mint, maxt int64) (storage.Querier, error) {
		q, err := source.Querier(mint, maxt)
		if err != nil {
			return nil, err
		}
		a := anciens()
		if len(a) == 0 {
			return q, nil
		}
		return &querierNoms{Querier: q, anciens: a}, nil
	})
}

type querierNoms struct {
	storage.Querier
	anciens map[string]string
}

// affiche : nom montré pour une valeur de l'étiquette.
func (q *querierNoms) affiche(v string) string {
	if n, ok := q.anciens[v]; ok {
		return n
	}
	return v
}

// Select : filtres traduits vers les noms réellement stockés, puis séries
// renommées et fusionnées.
func (q *querierNoms) Select(ctx context.Context, sortSeries bool, hints *storage.SelectHints, matchers ...*labels.Matcher) storage.SeriesSet {
	ms := make([]*labels.Matcher, 0, len(matchers))
	for _, m := range matchers {
		if m.Name != etiquetteNom {
			ms = append(ms, m)
			continue
		}
		nm, vide, err := q.traduire(ctx, m)
		if err != nil {
			return storage.ErrSeriesSet(err)
		}
		if vide {
			return storage.EmptySeriesSet()
		}
		ms = append(ms, nm)
	}
	ss := q.Querier.Select(ctx, false, hints, ms...)
	return q.renommer(ss)
}

// traduire : un filtre sur le nom de la machine porte sur le nom AFFICHÉ.
// Il devient la liste exacte des noms stockés dont le nom affiché convient
// (vide = aucune série possible).
func (q *querierNoms) traduire(ctx context.Context, m *labels.Matcher) (*labels.Matcher, bool, error) {
	valeurs, _, err := q.Querier.LabelValues(ctx, etiquetteNom, nil)
	if err != nil {
		return nil, false, err
	}
	var garder []string
	for _, v := range valeurs {
		if m.Matches(q.affiche(v)) {
			garder = append(garder, regexp.QuoteMeta(v))
		}
	}
	if m.Matches("") {
		garder = append(garder, "") // séries sans l'étiquette (ex. instance!="x")
	}
	if len(garder) == 0 {
		return nil, true, nil
	}
	nm, err := labels.NewMatcher(labels.MatchRegexp, etiquetteNom, strings.Join(garder, "|"))
	return nm, false, err
}

// renommer : séries de l'ancien nom montrées sous le nouveau ; celles qui
// ont alors les mêmes étiquettes sont fusionnées (une courbe continue).
// Les séries sont rendues triées par étiquettes (exigence du moteur).
func (q *querierNoms) renommer(ss storage.SeriesSet) storage.SeriesSet {
	var tout []storage.Series
	for ss.Next() {
		s := ss.At()
		l := s.Labels()
		if n, ok := q.anciens[l.Get(etiquetteNom)]; ok {
			b := labels.NewBuilder(l)
			b.Set(etiquetteNom, n)
			s = serieRenommee{Series: s, l: b.Labels()}
		}
		tout = append(tout, s)
	}
	if err := ss.Err(); err != nil {
		return storage.ErrSeriesSet(err)
	}
	sort.SliceStable(tout, func(i, j int) bool { return labels.Compare(tout[i].Labels(), tout[j].Labels()) < 0 })
	var fusion []storage.Series
	for i := 0; i < len(tout); {
		j := i + 1
		for j < len(tout) && labels.Equal(tout[i].Labels(), tout[j].Labels()) {
			j++
		}
		if j-i == 1 {
			fusion = append(fusion, tout[i])
		} else {
			fusion = append(fusion, storage.ChainedSeriesMerge(tout[i:j]...))
		}
		i = j
	}
	return &listeSeries{s: fusion, i: -1, avert: ss.Warnings()}
}

// LabelValues : noms anciens remplacés par le nom actuel (sans doublon).
func (q *querierNoms) LabelValues(ctx context.Context, name string, hints *storage.LabelHints, matchers ...*labels.Matcher) ([]string, annotations.Annotations, error) {
	vs, an, err := q.Querier.LabelValues(ctx, name, hints, matchers...)
	if err != nil || name != etiquetteNom {
		return vs, an, err
	}
	vu := map[string]bool{}
	out := make([]string, 0, len(vs))
	for _, v := range vs {
		v = q.affiche(v)
		if !vu[v] {
			vu[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out, an, nil
}

type serieRenommee struct {
	storage.Series
	l labels.Labels
}

func (s serieRenommee) Labels() labels.Labels { return s.l }

func (s serieRenommee) Iterator(it chunkenc.Iterator) chunkenc.Iterator { return s.Series.Iterator(it) }

// listeSeries : SeriesSet sur une liste déjà prête.
type listeSeries struct {
	s     []storage.Series
	i     int
	avert annotations.Annotations
}

func (l *listeSeries) Next() bool                        { l.i++; return l.i < len(l.s) }
func (l *listeSeries) At() storage.Series                { return l.s[l.i] }
func (l *listeSeries) Err() error                        { return nil }
func (l *listeSeries) Warnings() annotations.Annotations { return l.avert }
