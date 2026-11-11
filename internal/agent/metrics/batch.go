// SPDX-License-Identifier: GPL-3.0-or-later

// Package metrics contient le lot de métriques rempli par les collecteurs à
// chaque cycle.
//
// Objectif de performance (SPEC.md § 6.3) : aucune allocation mémoire par point
// en régime établi. Pour cela :
//   - le lot est réutilisé d'un cycle à l'autre (Reset garde la mémoire) ;
//   - les étiquettes d'une série sont rangées dans un tableau de taille fixe ;
//   - les valeurs d'étiquettes lues dans /proc (des []byte) passent par un
//     « interneur » qui renvoie toujours la même chaîne pour le même contenu.
package metrics

// MaxLabels est le nombre maximal d'étiquettes propres à une série (sans les
// étiquettes communes instance, job, site, env). 8 suffit pour toutes les
// métriques de SPEC.md § 4.1.
const MaxLabels = 8

// Label est une étiquette : nom et valeur.
type Label struct {
	Name, Value string
}

// L construit une étiquette (écriture courte dans les collecteurs).
func L(nom, valeur string) Label { return Label{Name: nom, Value: valeur} }

// Sample est un point : nom de métrique, étiquettes propres, valeur.
type Sample struct {
	Name   string
	Labels [MaxLabels]Label
	N      int // nombre d'étiquettes utilisées dans Labels
	Value  float64
}

// Batch est le lot d'un cycle de collecte.
type Batch struct {
	// Horodatage commun à tous les points du lot, en millisecondes Unix.
	TimestampMs int64
	// Common : étiquettes ajoutées à chaque série (instance, job, site, env).
	Common []Label
	// Samples : les points du cycle.
	Samples []Sample
	// Ignored : points refusés (trop d'étiquettes), signe d'un bogue.
	Ignored int
}

// NewBatch prépare un lot avec une capacité initiale (évite les
// agrandissements pendant les premiers cycles).
func NewBatch(capacite int, common []Label) *Batch {
	return &Batch{Common: common, Samples: make([]Sample, 0, capacite)}
}

// Reset vide le lot en gardant la mémoire déjà réservée.
func (b *Batch) Reset(timestampMs int64) {
	b.TimestampMs = timestampMs
	b.Samples = b.Samples[:0]
	b.Ignored = 0
}

// Add ajoute un point. Les étiquettes sont copiées : l'appelant peut réutiliser
// ses variables. Les arguments variadiques ne s'échappent pas : pas d'allocation.
func (b *Batch) Add(nom string, valeur float64, labels ...Label) {
	if len(labels) > MaxLabels {
		b.Ignored++
		return
	}
	// Agrandit d'un élément sans allouer quand la capacité suffit.
	b.Samples = append(b.Samples, Sample{})
	s := &b.Samples[len(b.Samples)-1]
	s.Name = nom
	s.Value = valeur
	s.N = copy(s.Labels[:], labels)
}

// Len renvoie le nombre de points du lot.
func (b *Batch) Len() int { return len(b.Samples) }

// Interner renvoie toujours la même chaîne pour un même contenu d'octets.
//
// Les noms de disques, d'interfaces ou de points de montage changent rarement :
// après le premier cycle, toutes les recherches tombent juste et ne coûtent
// aucune allocation (Go optimise m[string(octets)]).
type Interner struct {
	m   map[string]string
	max int
}

// NewInterner crée un interneur limité à max chaînes. Au-delà (cas anormal :
// une étiquette qui change à chaque cycle), il se vide pour ne pas grossir
// sans fin.
func NewInterner(max int) *Interner {
	return &Interner{m: make(map[string]string, 64), max: max}
}

// Get renvoie la chaîne correspondant à b.
func (in *Interner) Get(b []byte) string {
	if s, ok := in.m[string(b)]; ok {
		return s
	}
	if len(in.m) >= in.max {
		clear(in.m)
	}
	s := string(b)
	in.m[s] = s
	return s
}

// Len renvoie le nombre de chaînes gardées.
func (in *Interner) Len() int { return len(in.m) }
