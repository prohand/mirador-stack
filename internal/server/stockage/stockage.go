// SPDX-License-Identifier: GPL-3.0-or-later

// Package stockage définit les interfaces de stockage de mirador-server
// (SPEC.md § 7.2) : écrire, lire, purger. Chaque moteur (Prometheus TSDB,
// segments zstd, ou plus tard VictoriaMetrics / VictoriaLogs) les implémente,
// pour pouvoir changer de moteur sans toucher au reste.
package stockage

import (
	"context"
	"sort"
	"sync"
)

// Label : étiquette d'une série.
type Label struct {
	Nom, Valeur string
}

// Point : un échantillon (horodatage en millisecondes Unix).
type Point struct {
	T int64
	V float64
}

// Serie : étiquettes triées par nom (dont __name__) et points.
type Serie struct {
	Labels []Label
	Points []Point
}

// Valeur renvoie la valeur d'une étiquette (vide si absente).
func (s *Serie) Valeur(nom string) string {
	for _, l := range s.Labels {
		if l.Nom == nom {
			return l.Valeur
		}
	}
	return ""
}

// Remplacer pose une étiquette (ajout ou remplacement) en gardant le tri.
func (s *Serie) Remplacer(nom, valeur string) {
	for i := range s.Labels {
		if s.Labels[i].Nom == nom {
			s.Labels[i].Valeur = valeur
			return
		}
	}
	s.Labels = append(s.Labels, Label{nom, valeur})
	sort.Slice(s.Labels, func(i, j int) bool { return s.Labels[i].Nom < s.Labels[j].Nom })
}

// Ligne : une ligne de log. Champs : étiquettes (host, job, unit, level…).
type Ligne struct {
	T      int64 // millisecondes Unix
	Msg    string
	Champs map[string]string
}

// Metriques : moteur de stockage des métriques.
type Metriques interface {
	Ecrire(ctx context.Context, series []Serie) error
}

// Logs : moteur de stockage des logs.
type Logs interface {
	Ecrire(ctx context.Context, lignes []Ligne) error
}

// --- Implémentation en mémoire (tests et essais) --------------------------------

// Memoire garde tout en mémoire. Ne sert qu'aux tests.
type Memoire struct {
	mu     sync.Mutex
	Series []Serie
	Lignes []Ligne
}

// Ecrire (métriques).
func (m *Memoire) Ecrire(_ context.Context, series []Serie) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Series = append(m.Series, series...)
	return nil
}

// EcrireLogs (logs).
func (m *Memoire) EcrireLogs(_ context.Context, lignes []Ligne) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Lignes = append(m.Lignes, lignes...)
	return nil
}

// Lire donne accès au contenu sous verrou (tests concurrents).
func (m *Memoire) Lire(f func(series []Serie, lignes []Ligne)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f(m.Series, m.Lignes)
}

// LogsMemoire adapte Memoire à l'interface Logs.
type LogsMemoire struct{ *Memoire }

// Ecrire (logs).
func (l LogsMemoire) Ecrire(ctx context.Context, lignes []Ligne) error {
	return l.EcrireLogs(ctx, lignes)
}
