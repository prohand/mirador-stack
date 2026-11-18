// SPDX-License-Identifier: GPL-3.0-or-later

package server

import (
	"context"
	"sync"
	"time"

	"mirador-stack/internal/server/changements"
	"mirador-stack/internal/server/flotte"
)

// Dernières données reçues de chaque machine, par nom (étiquette instance),
// pour « si les données disparaissent » (SPEC § 46) : une alerte dont la
// machine n'envoie plus rien reste ouverte.
//
// Source : l'ingestion (en mémoire, à chaque envoi de métriques accepté) ;
// avant le premier envoi depuis le démarrage, vu_le en base (relu chaque
// minute avec la liste des machines). Une machine inconnue (supprimée) n'est
// jamais muette : ses alertes suivent leurs séries.
type vusParNom struct {
	mu   sync.Mutex
	base map[string]int64 // nom → vu_le (machines approuvées)
}

// recharger relit vu_le de toutes les machines approuvées.
func (v *vusParNom) recharger(ctx context.Context, s *Serveur) {
	ms, err := flotte.ListerMachines(ctx, s.db)
	if err != nil {
		return
	}
	v.remplacer(ms)
}

// remplacer : nouvelle liste (déjà lue par l'appelant).
func (v *vusParNom) remplacer(ms []flotte.InfoMachine) {
	base := make(map[string]int64, len(ms))
	for _, m := range ms {
		if m.Statut == "approuvee" {
			base[m.Nom] = m.VuLe
		}
	}
	v.mu.Lock()
	v.base = base
	v.mu.Unlock()
}

// derniereDonnee : pour le moteur d'alertes (alertes.Options.DerniereDonnee).
func (s *Serveur) derniereDonnee(instance string) (time.Time, bool) {
	s.vus.mu.Lock()
	vuLe, connue := s.vus.base[instance]
	s.vus.mu.Unlock()
	if !connue {
		return time.Time{}, false
	}
	if s.ing != nil {
		if t, ok := s.ing.DerniereDonneeNom(instance); ok {
			return time.Unix(t, 0), true
		}
	}
	if vuLe <= 0 {
		return time.Time{}, false // jamais rien reçu : rien à garder
	}
	return time.Unix(vuLe, 0), true
}

// dernierChangement : pour les messages d'alerte (SPEC § 47), le dernier
// changement de la machine dans l'heure qui précède le début de l'alerte.
func (s *Serveur) dernierChangement(ctx context.Context, machine string, avant time.Time) (time.Time, string, bool) {
	c, ok := changements.Avant(ctx, s.db, machine, avant, time.Hour)
	if !ok {
		return time.Time{}, "", false
	}
	return c.Le, c.Resume(), true
}
