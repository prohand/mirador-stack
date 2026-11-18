// SPDX-License-Identifier: GPL-3.0-or-later

package flotte

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	essais "mirador-stack/internal/sondes"
)

// Sondes de services (SPEC § 48) : une seule liste, gardée en base
// (réglage « sondes »). Chaque sonde est faite « depuis » le serveur ou un
// collecteur ; la part d'un collecteur lui est envoyée dans sa
// configuration (clé collecteur.services).

const cleSondes = "sondes"

// LireSondes : toutes les sondes, triées par nom.
func LireSondes(ctx context.Context, db *sql.DB) ([]essais.Sonde, error) {
	var v string
	err := db.QueryRowContext(ctx, `SELECT valeur FROM reglages WHERE cle = ?`, cleSondes).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []essais.Sonde
	if err := json.Unmarshal([]byte(v), &out); err != nil {
		return nil, fmt.Errorf("réglage « sondes » illisible : %w", err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Nom < out[j].Nom })
	return out, nil
}

// EnregistrerSondes : liste complète, vérifiée (réglages, noms uniques,
// « depuis » = serveur ou collecteur approuvé) ; audit ; collecteurs
// prévenus (configuration en direct).
func EnregistrerSondes(ctx context.Context, db *sql.DB, liste []essais.Sonde, qui string, maintenant time.Time) error {
	if len(liste) > essais.MaxSondes {
		return fmt.Errorf("%d sondes au plus", essais.MaxSondes)
	}
	collecteurs, err := Collecteurs(ctx, db)
	if err != nil {
		return err
	}
	permis := map[string]bool{essais.ServeurLocal: true}
	for _, c := range collecteurs {
		permis[c] = true
	}
	noms := map[string]bool{}
	for i := range liste {
		s := &liste[i]
		if err := s.Valider(); err != nil {
			return err
		}
		if !permis[s.Depuis] {
			return fmt.Errorf("sonde %s : « %s » n'est ni le serveur ni un collecteur approuvé", s.Nom, s.Depuis)
		}
		if noms[s.Nom] {
			return fmt.Errorf("deux sondes s'appellent %s", s.Nom)
		}
		noms[s.Nom] = true
	}
	avant, _ := LireSondes(ctx, db)
	brutAvant, _ := json.Marshal(avant)
	brut, err := json.Marshal(liste)
	if err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // sans effet après Commit
	if _, err := tx.ExecContext(ctx, `INSERT INTO reglages(cle, valeur, modifie_le, modifie_par) VALUES (?, ?, ?, ?)
		ON CONFLICT(cle) DO UPDATE SET valeur = excluded.valeur, modifie_le = excluded.modifie_le, modifie_par = excluded.modifie_par`,
		cleSondes, string(brut), maintenant.Unix(), qui); err != nil {
		return err
	}
	if err := audit(ctx, tx, maintenant.Unix(), qui, "sondes", "", string(brutAvant), string(brut), ""); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	SignalerConfig()
	return nil
}

// sondesDe : sondes faites depuis ce collecteur.
func sondesDe(ctx context.Context, db *sql.DB, collecteur string) ([]essais.Sonde, error) {
	toutes, err := LireSondes(ctx, db)
	if err != nil {
		return nil, err
	}
	var out []essais.Sonde
	for _, s := range toutes {
		if s.Depuis == collecteur {
			out = append(out, s)
		}
	}
	return out, nil
}
