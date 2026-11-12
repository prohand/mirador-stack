// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"database/sql"
	"strconv"
	"time"
)

// cleVerrousLeves : réglage écrit par la commande « utilisateur-secours »
// (autre processus) ; le serveur en marche lève ses verrous en le voyant.
const cleVerrousLeves = "console.verrous_leves"

// DemanderLeveeVerrous : à appeler par une commande de secours.
func DemanderLeveeVerrous(ctx context.Context, db *sql.DB, qui string, t time.Time) error {
	return ecrireReglage(ctx, db, cleVerrousLeves, strconv.FormatInt(t.UnixNano(), 10), qui, t)
}

// leverSiDemande : vu à chaque tour (5 s).
func (c *Console) leverSiDemande(ctx context.Context) {
	var v string
	if err := c.db.QueryRowContext(ctx, `SELECT valeur FROM reglages WHERE cle = ?`, cleVerrousLeves).Scan(&v); err != nil {
		return
	}
	n, _ := strconv.ParseInt(v, 10, 64)
	if n > c.verrousLevesLe.Load() {
		c.verrousLevesLe.Store(n)
		c.verrous.Lever()
		c.journal.Warn("console : Verrous de connexion levés (commande de secours)")
	}
}
