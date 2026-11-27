// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"sync"

	"mirador-stack/internal/server/flotte"
)

// Liste des machines lue une seule fois par affichage de page (mesure du
// grand parc, 27/11) : une page en demandait 2 à 4 (vue d'ensemble, menu,
// listes de choix), ~12 ms chacune pour 1 500 machines. Seulement pour les
// lectures (GET) : une action (POST) relit toujours la base.

type cleMemoMachines struct{}

type memoMachines struct {
	mu  sync.Mutex
	lu  bool
	ms  []flotte.InfoMachine
	err error
}

// avecMemoMachines : contexte d'une page, avec la place pour la liste.
func avecMemoMachines(ctx context.Context) context.Context {
	return context.WithValue(ctx, cleMemoMachines{}, &memoMachines{})
}

// machines : flotte.ListerMachines, gardée le temps de la requête.
// La liste est partagée entre les appelants d'une même page : la lire
// seulement (ne pas trier ni modifier ses éléments ; un append crée une
// copie grâce à la capacité limitée).
func (c *Console) machines(ctx context.Context) ([]flotte.InfoMachine, error) {
	m, _ := ctx.Value(cleMemoMachines{}).(*memoMachines)
	if m == nil {
		return flotte.ListerMachines(ctx, c.db)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.lu {
		m.ms, m.err = flotte.ListerMachines(ctx, c.db)
		m.lu = m.err == nil // une erreur n'est pas gardée
	}
	return m.ms[:len(m.ms):len(m.ms)], m.err
}
