// SPDX-License-Identifier: GPL-3.0-or-later

package flotte

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"

	"mirador-stack/internal/agent/majagent"
	"mirador-stack/internal/server/ingestion"
	"mirador-stack/internal/server/web"
)

// Clés publiques de signature des versions de l'agent (SPEC § 18.25) :
// saisies dans l'interface (Mises à jour, code 2FA), servies au script
// d'installation qui les écrit dans un fichier root sur la machine. Le
// serveur s'en sert aussi pour vérifier une version au moment de la publier.
// Changer la clé ici ne touche PAS les agents déjà installés (leur fichier
// ne change que si l'administrateur relance le script avec --cle-publique).

const cleClesPubliques = "maj.cles_publiques"

// LireClesPubliques : clés réglées (liste nettoyée), sans les clés intégrées.
func LireClesPubliques(ctx context.Context, db *sql.DB) ([]string, error) {
	var v string
	err := db.QueryRowContext(ctx, `SELECT valeur FROM reglages WHERE cle = ?`, cleClesPubliques).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return majagent.ValiderCles(v)
}

// clesVerification : clés réglées + clés intégrées au serveur (format
// attendu par majagent : séparées par des virgules).
func clesVerification(ctx context.Context, db *sql.DB) (string, error) {
	ls, err := LireClesPubliques(ctx, db)
	if err != nil {
		return "", err
	}
	if majagent.ClesPubliques != "" {
		for _, c := range strings.Split(majagent.ClesPubliques, ",") {
			if !contientCle(ls, c) {
				ls = append(ls, c)
			}
		}
	}
	return strings.Join(ls, ","), nil
}

func contientCle(l []string, v string) bool {
	for _, x := range l {
		if x == v {
			return true
		}
	}
	return false
}

// DefinirClesPubliques : remplace les clés réglées (vide = aucune), avec
// audit. Lecture de l'ancienne valeur AVANT la transaction (une connexion).
func DefinirClesPubliques(ctx context.Context, db *sql.DB, brut, qui string, maintenant time.Time) error {
	ls, err := majagent.ValiderCles(brut)
	if err != nil {
		return err
	}
	avant, err := LireClesPubliques(ctx, db)
	if err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // sans effet après Commit
	v := strings.Join(ls, ",")
	if _, err := tx.ExecContext(ctx, `INSERT INTO reglages(cle, valeur, modifie_le, modifie_par) VALUES (?, ?, ?, ?)
		ON CONFLICT(cle) DO UPDATE SET valeur = excluded.valeur, modifie_le = excluded.modifie_le, modifie_par = excluded.modifie_par`,
		cleClesPubliques, v, maintenant.Unix(), qui); err != nil {
		return err
	}
	if err := audit(ctx, tx, maintenant.Unix(), qui, "cles_publiques", "", strings.Join(avant, ","), v, ""); err != nil {
		return err
	}
	return tx.Commit()
}

// cles : GET /api/agent/v1/cles, public (une clé publique n'est pas un
// secret) ; une clé par ligne, 404 si aucune n'est réglée.
func (f *Flotte) cles(w http.ResponseWriter, r *http.Request) {
	if !f.demandes.Prendre(ingestion.CleLimite(web.AdresseClient(r)), 1, f.opt.Maintenant()) {
		w.Header().Set("Retry-After", "30")
		erreur(w, http.StatusTooManyRequests, "trop de demandes")
		return
	}
	ls, err := clesVerification(r.Context(), f.opt.DB)
	if err != nil {
		f.opt.Journal.Error("clés publiques illisibles", "erreur", err)
		erreur(w, http.StatusInternalServerError, "erreur interne")
		return
	}
	if ls == "" {
		erreur(w, http.StatusNotFound, "aucune clé publique réglée (interface : Mises à jour des agents)")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(strings.ReplaceAll(ls, ",", "\n") + "\n"))
}
