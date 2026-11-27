// SPDX-License-Identifier: GPL-3.0-or-later

package server

// Taille du parc (SPEC § 141) : plafonds ajustés à chaud d'après les
// machines approuvées (avec 20 % de marge) ou la taille prévue dans
// l'interface. Rien à régler pour un petit parc : jusqu'à 50 machines, ce
// sont les plafonds de toujours.

import (
	"context"
	"os"
	"runtime/debug"
	"time"

	"mirador-stack/internal/server/config"
	"mirador-stack/internal/server/console"
	"mirador-stack/internal/server/stockage/journaux"
)

// dimensionner applique les plafonds de la taille retenue ; renvoie la taille.
func (s *Serveur) dimensionner(ctx context.Context) int {
	t := console.LireTailleParc(ctx, s.db)
	d := config.DimensionsPour(t.Retenue)
	if os.Getenv("GOMEMLIMIT") == "" { // le conteneur le fixe : on n'y touche pas
		debug.SetMemoryLimit(d.Memoire)
	}
	s.ing.Dimensionner(d.Series, d.Places, d.Legeres, d.Telechargements, d.RequetesIP)
	if err := s.jx.ReglerPartitions(journaux.PartitionsPour(t.Retenue)); err != nil {
		s.journal.Error("logs : changement des tampons", "erreur", err)
	}
	return t.Retenue
}

// boucleParc : au démarrage, toutes les 5 minutes, et dès qu'une taille est
// enregistrée dans l'interface.
func (s *Serveur) boucleParc(ctx context.Context) {
	derniere := -1
	tic := time.NewTicker(5 * time.Minute)
	defer tic.Stop()
	for {
		if n := s.dimensionner(ctx); n != derniere {
			if derniere >= 0 {
				s.journal.Info("taille du parc : plafonds ajustés", "machines", n)
			}
			derniere = n
		}
		select {
		case <-ctx.Done():
			return
		case <-tic.C:
		case <-s.redim:
		}
	}
}

// redimensionner : demande d'application immédiate (sans attendre).
func (s *Serveur) redimensionner() {
	select {
	case s.redim <- struct{}{}:
	default:
	}
}
