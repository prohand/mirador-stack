// SPDX-License-Identifier: GPL-3.0-or-later

package changements

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
	"time"
)

// charger : état gardé en base (au premier tour).
func (d *Detecteur) charger(ctx context.Context) error {
	rows, err := d.DB.QueryContext(ctx, `SELECT cle, valeur FROM changements_etat`)
	if err != nil {
		return err
	}
	defer rows.Close()
	d.etat = map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return err
		}
		d.etat[k] = v
	}
	if err := rows.Err(); err != nil {
		return err
	}
	d.charge = true
	return nil
}

// fenetreFusion : des paquets installés à moins de 30 min d'un changement
// « paquets » de la même machine y sont ajoutés (une mise à jour = une ligne).
const fenetreFusion = 30 * time.Minute

// tailleMaxDetail : au-delà, la liste des paquets est coupée.
const tailleMaxDetail = 4000

// ecrire : état et changements en une transaction ; ménage de temps en temps.
func (d *Detecteur) ecrire(ctx context.Context, maj map[string]string, evs []nouveau, t time.Time) error {
	purger := t.Sub(d.purgeeLe) >= 6*time.Hour
	if len(maj) == 0 && len(evs) == 0 && !purger {
		return nil
	}
	tx, err := d.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // sans effet après Commit
	for k, v := range maj {
		if _, err := tx.ExecContext(ctx, `INSERT INTO changements_etat(cle, valeur) VALUES (?, ?)
			ON CONFLICT(cle) DO UPDATE SET valeur = excluded.valeur`, k, v); err != nil {
			return err
		}
	}
	for _, e := range evs {
		if e.fusion {
			var id int64
			var detail string
			err := tx.QueryRowContext(ctx, `SELECT id, detail FROM changements WHERE machine = ? AND type = ? AND le >= ?
				ORDER BY le DESC LIMIT 1`, e.Machine, e.Type, e.Le.Add(-fenetreFusion).Unix()).Scan(&id, &detail)
			if err == nil {
				if _, err := tx.ExecContext(ctx, `UPDATE changements SET detail = ? WHERE id = ?`, couper(detail+", "+e.Detail), id); err != nil {
					return err
				}
				continue
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO changements(machine, le, type, detail) VALUES (?, ?, ?, ?)`,
			e.Machine, e.Le.Unix(), e.Type, couper(e.Detail)); err != nil {
			return err
		}
	}
	if purger {
		if _, err := tx.ExecContext(ctx, `DELETE FROM changements WHERE le < ?`, t.Add(-Conservation).Unix()); err != nil {
			return err
		}
		// Paquets : l'agent ne les envoie que 48 h ; état gardé 4 jours.
		if _, err := tx.ExecContext(ctx, `DELETE FROM changements_etat WHERE cle LIKE 'pkg|%' AND CAST(valeur AS INTEGER) < ?`,
			t.Add(-96*time.Hour).Unix()); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	for k, v := range maj {
		d.etat[k] = v
	}
	if purger {
		d.purgeeLe = t
		for k, v := range d.etat {
			if strings.HasPrefix(k, "pkg|") {
				if le, err := strconv.ParseInt(v, 10, 64); err == nil && le < t.Add(-96*time.Hour).Unix() {
					delete(d.etat, k)
				}
			}
		}
	}
	return nil
}

func couper(s string) string {
	if len(s) <= tailleMaxDetail {
		return s
	}
	i := strings.LastIndex(s[:tailleMaxDetail], ", ")
	if i < 0 {
		i = tailleMaxDetail
	}
	return s[:i] + "…"
}

// Lister : changements d'une machine depuis une date, les plus récents
// d'abord (max lignes).
func Lister(ctx context.Context, db *sql.DB, machine string, depuis time.Time, max int) ([]Changement, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, machine, le, type, detail FROM changements WHERE machine = ? AND le >= ?
		ORDER BY le DESC, id DESC LIMIT ?`, machine, depuis.Unix(), max)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Changement
	for rows.Next() {
		var c Changement
		var le int64
		if err := rows.Scan(&c.ID, &c.Machine, &le, &c.Type, &c.Detail); err != nil {
			return nil, err
		}
		c.Le = time.Unix(le, 0)
		out = append(out, c)
	}
	return out, rows.Err()
}

// Avant : dernier changement d'une machine dans la fenêtre qui précède t
// (message d'alerte : « 12 min avant : … »).
func Avant(ctx context.Context, db *sql.DB, machine string, t time.Time, fenetre time.Duration) (Changement, bool) {
	var c Changement
	var le int64
	err := db.QueryRowContext(ctx, `SELECT id, machine, le, type, detail FROM changements WHERE machine = ? AND le <= ? AND le >= ?
		ORDER BY le DESC, id DESC LIMIT 1`, machine, t.Unix(), t.Add(-fenetre).Unix()).Scan(&c.ID, &c.Machine, &le, &c.Type, &c.Detail)
	if err != nil {
		return Changement{}, false
	}
	c.Le = time.Unix(le, 0)
	return c, true
}

// Titre : « Paquets », « Redémarrage »… (affichage).
func Titre(typ string) string {
	switch typ {
	case Paquets:
		return "Paquets"
	case Redemarrage:
		return "Redémarrage"
	case Noyau:
		return "Noyau"
	case Agent:
		return "Agent"
	case Conteneur:
		return "Conteneur"
	}
	return typ
}

// Resume : texte court (message, repère sur les courbes).
func (c Changement) Resume() string {
	switch c.Type {
	case Paquets:
		n := strings.Count(c.Detail, ", ") + 1
		if strings.HasSuffix(c.Detail, "…") {
			return "Paquets mis à jour (plus de " + strconv.Itoa(n) + ")"
		}
		if n == 1 {
			return "1 paquet mis à jour : " + c.Detail
		}
		return strconv.Itoa(n) + " paquets mis à jour"
	case Agent:
		return c.Detail
	}
	return c.Detail
}
