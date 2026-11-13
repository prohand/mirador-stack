// SPDX-License-Identifier: GPL-3.0-or-later

package auth

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Adresses connues d'un compte, gardées en base (SPEC § 18.28, correction du
// 14/11) : le message « connexion depuis une nouvelle adresse » ne revient
// plus après un redémarrage du serveur. Une adresse non revue depuis
// DureeAdresseConnue redevient « nouvelle » ; elle est alors effacée.

// DureeAdresseConnue : comme la confiance du verrouillage (30 jours).
const DureeAdresseConnue = dureeConfiance

// AdresseDejaVue : le compte s'est-il connecté depuis cette adresse (ou ce
// /64) dans les 30 derniers jours ?
func AdresseDejaVue(ctx context.Context, db *sql.DB, utilisateur int64, ip string, t time.Time) (bool, error) {
	var vue int64
	err := db.QueryRowContext(ctx, `SELECT vue_le FROM adresses_connues WHERE utilisateur_id = ? AND adresse = ?`,
		utilisateur, CleAdresse(ip)).Scan(&vue)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return t.Sub(time.Unix(vue, 0)) < DureeAdresseConnue, nil
}

// NoterAdresse : connexion réussie depuis cette adresse ; les adresses du
// compte trop anciennes sont effacées au passage.
func NoterAdresse(ctx context.Context, db *sql.DB, utilisateur int64, ip string, t time.Time) error {
	if _, err := db.ExecContext(ctx, `INSERT INTO adresses_connues(utilisateur_id, adresse, vue_le) VALUES (?, ?, ?)
		ON CONFLICT(utilisateur_id, adresse) DO UPDATE SET vue_le = excluded.vue_le`, utilisateur, CleAdresse(ip), t.Unix()); err != nil {
		return err
	}
	_, err := db.ExecContext(ctx, `DELETE FROM adresses_connues WHERE utilisateur_id = ? AND vue_le < ?`,
		utilisateur, t.Add(-DureeAdresseConnue).Unix())
	return err
}
