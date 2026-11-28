// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"time"

	"mirador-stack/internal/server/auth"
)

// Flux longs (EventSource : logs en direct 30 min, relevés en direct 15 min,
// blocages en direct) : la session n'était vérifiée qu'à l'ouverture. Une
// session fermée (déconnexion, « fermer la session », 5 codes faux) ou un
// compte désactivé gardait donc le flux jusqu'à sa fin (revue du 28/11).
// Désormais, la session et le compte sont revérifiés au plus toutes les
// 10 s, à chaque tour de boucle du flux (battement de 15 s compris : coupure
// en 15 à 20 s au pire) ; le flux est coupé sinon. Coût : une lecture en
// mémoire (et le compte, gardé en cache 10 s) par page ouverte et par
// battement.

// verifFlux : intervalle minimal entre deux vérifications (plus court que
// le battement de 15 s, pour vérifier à chaque battement ; variable pour
// les tests).
var verifFlux = 10 * time.Second

// gardeFlux renvoie une fonction à appeler à chaque tour de boucle d'un
// flux : false = la session ou le compte ne sont plus valables, couper.
// La vérification réelle n'a lieu qu'une fois par verifFlux.
func (c *Console) gardeFlux(q *req) func() bool {
	derniere := time.Now()
	return func() bool {
		if time.Since(derniere) < verifFlux {
			return true
		}
		derniere = time.Now()
		return c.fluxValable(q)
	}
}

// fluxValable : même règle que proteger (session en mémoire, non expirée,
// compte actif de même génération, 2FA présente).
func (c *Console) fluxValable(q *req) bool {
	t := c.maintenant()
	s := c.sessions.LireEmpreinte(q.s.Empreinte)
	if s == nil || s.Etape != auth.EtapeComplet || c.expiree(s, t) {
		return false
	}
	u, err := c.compte(q.r.Context(), s.UtilisateurID, t)
	return err == nil && !u.Desactive && u.Generation == s.Generation && u.TOTP != ""
}
