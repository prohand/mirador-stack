// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"database/sql"
	"time"
)

type execer interface {
	ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error)
}

// Auditer écrit une ligne d'audit (jamais de secret : l'appelant masque).
// Le paquet auth écrit dans la même table pour les comptes (auth.auditer).
func Auditer(ctx context.Context, x execer, qui, action, objet, avant, apres, adresse string, t time.Time) error {
	_, err := x.ExecContext(ctx, `INSERT INTO audit(le, qui, action, objet, avant, apres, adresse) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		t.Unix(), qui, action, objet, avant, apres, adresse)
	return err
}
