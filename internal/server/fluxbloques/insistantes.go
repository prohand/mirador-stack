// SPDX-License-Identifier: GPL-3.0-or-later

package fluxbloques

import (
	"context"
	"time"
)

// Insistantes : adresses bloquées sur [debut, fin) par au moins minPF
// pare-feu différents, ou au moins minN fois en tout (liste de blocage,
// demande du 27/11). Lu dans les heures écrites (adresses gardées 7 jours) ;
// les plus bloquées d'abord, max au plus. Compte.PareFeux : nombre de
// pare-feu.
func (m *Magasin) Insistantes(ctx context.Context, debut, fin time.Time, minPF, minN, max int) ([]Compte, error) {
	if m.opt.DB == nil {
		return nil, nil
	}
	rows, err := m.opt.DB.QueryContext(ctx, `SELECT a, count(DISTINCT b), sum(n) FROM heures
		WHERE genre = 'i' AND heure >= ? AND heure < ? GROUP BY a
		HAVING count(DISTINCT b) >= ? OR sum(n) >= ? ORDER BY 3 DESC LIMIT ?`,
		debut.Unix()/3600*3600, fin.Unix(), minPF, minN, max)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Compte
	for rows.Next() {
		var c Compte
		if err := rows.Scan(&c.Nom, &c.PareFeux, &c.N); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
