// SPDX-License-Identifier: GPL-3.0-or-later

package flotte

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"mirador-stack/internal/server/certsyslog"
)

// Collecteurs (SPEC § 23.13) : une machine de type « collecteur »
// (mirador-collector, sur le LAN) relève des équipements sans agent
// (Synology, UniFi, imprimante…). Chaque équipement est rattaché à un seul
// collecteur ; le serveur refuse les données d'un équipement non rattaché.

// Equipement : un équipement rattaché à un collecteur.
type Equipement struct {
	ID                     int64
	Nom, Site, Env, Groupe string
	Statut                 string
	VuLe                   int64 // dernière donnée reçue pour lui (0 : jamais)
}

// typeDe : type d'une machine (agent, collecteur).
func typeDe(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id int64) (nom, typ string, err error) {
	err = q.QueryRowContext(ctx, `SELECT nom, type FROM machines WHERE id = ?`, id).Scan(&nom, &typ)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", errors.New("machine inconnue")
	}
	return nom, typ, err
}

// RattacherEquipement : l'équipement est relevé par ce collecteur (déplacé
// s'il l'était par un autre). Audit dans la même transaction.
func RattacherEquipement(ctx context.Context, db *sql.DB, collecteur, equipement int64, qui string, maintenant time.Time) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // sans effet après Commit
	nomC, typC, err := typeDe(ctx, tx, collecteur)
	if err != nil {
		return err
	}
	if typC != "collecteur" {
		return fmt.Errorf("%s n'est pas un collecteur", nomC)
	}
	nomE, typE, err := typeDe(ctx, tx, equipement)
	if err != nil {
		return err
	}
	if typE == "collecteur" || equipement == collecteur {
		return errors.New("un collecteur ne peut pas être rattaché à un collecteur")
	}
	var avant string
	_ = tx.QueryRowContext(ctx, `SELECT m.nom FROM equipements_collecteur e JOIN machines m ON m.id = e.collecteur_id WHERE e.machine_id = ?`,
		equipement).Scan(&avant)
	if _, err := tx.ExecContext(ctx, `INSERT INTO equipements_collecteur(machine_id, collecteur_id, rattache_le, rattache_par) VALUES (?, ?, ?, ?)
		ON CONFLICT(machine_id) DO UPDATE SET collecteur_id = excluded.collecteur_id, rattache_le = excluded.rattache_le, rattache_par = excluded.rattache_par`,
		equipement, collecteur, maintenant.Unix(), qui); err != nil {
		return err
	}
	if err := audit(ctx, tx, maintenant.Unix(), qui, "equipement_rattache", nomE, avant, nomC, ""); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	SignalerConfig() // le collecteur reçoit sa nouvelle liste tout de suite
	return nil
}

// DetacherEquipement : l'équipement n'est plus relevé par aucun collecteur.
func DetacherEquipement(ctx context.Context, db *sql.DB, equipement int64, qui string, maintenant time.Time) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // sans effet après Commit
	nomE, _, err := typeDe(ctx, tx, equipement)
	if err != nil {
		return err
	}
	var avant string
	if err := tx.QueryRowContext(ctx, `SELECT m.nom FROM equipements_collecteur e JOIN machines m ON m.id = e.collecteur_id WHERE e.machine_id = ?`,
		equipement).Scan(&avant); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%s n'est rattaché à aucun collecteur", nomE)
		}
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM equipements_collecteur WHERE machine_id = ?`, equipement); err != nil {
		return err
	}
	if err := audit(ctx, tx, maintenant.Unix(), qui, "equipement_detache", nomE, avant, "", ""); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	SignalerConfig()
	return nil
}

// EquipementsDe : équipements rattachés à un collecteur, par nom.
func EquipementsDe(ctx context.Context, db *sql.DB, collecteur int64) ([]Equipement, error) {
	rows, err := db.QueryContext(ctx, `SELECT m.id, m.nom, m.site, m.env, m.groupe, m.statut, m.vu_le FROM equipements_collecteur e
		JOIN machines m ON m.id = e.machine_id WHERE e.collecteur_id = ? ORDER BY m.nom`, collecteur)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Equipement
	for rows.Next() {
		var q Equipement
		if err := rows.Scan(&q.ID, &q.Nom, &q.Site, &q.Env, &q.Groupe, &q.Statut, &q.VuLe); err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

// CollecteurDe : collecteur qui relève un équipement (0 : aucun).
func CollecteurDe(ctx context.Context, db *sql.DB, equipement int64) (int64, string, error) {
	var id int64
	var nom string
	err := db.QueryRowContext(ctx, `SELECT m.id, m.nom FROM equipements_collecteur e JOIN machines m ON m.id = e.collecteur_id WHERE e.machine_id = ?`,
		equipement).Scan(&id, &nom)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", nil
	}
	return id, nom, err
}

// configCollecteur : clé « collecteur » de la configuration d'un collecteur :
// réglages faits à la main (s'il y en a) + liste des équipements posée ici.
func configCollecteur(ctx context.Context, db *sql.DB, collecteur int64, saisie json.RawMessage) (json.RawMessage, error) {
	m := map[string]json.RawMessage{}
	if len(saisie) > 0 {
		if err := json.Unmarshal(saisie, &m); err != nil {
			return nil, fmt.Errorf("réglage « collecteur » : %w", err)
		}
	}
	eqs, err := EquipementsDe(ctx, db, collecteur)
	if err != nil {
		return nil, err
	}
	noms := make([]string, 0, len(eqs))
	for _, q := range eqs {
		if q.Statut == "approuvee" {
			noms = append(noms, q.Nom)
		}
	}
	// Syslog réglé : certificat obtenu par le serveur (ACME), posé ici,
	// jamais saisi à la main.
	if sl, ok := m["syslog"]; ok {
		var sys map[string]json.RawMessage
		if err := json.Unmarshal(sl, &sys); err != nil {
			return nil, fmt.Errorf("réglage « syslog » : %w", err)
		}
		delete(sys, "certificat")
		// Émetteurs d'équipements détachés : retirés (le collecteur refuserait
		// sinon toute la configuration).
		if brut, ok := sys["emetteurs"]; ok {
			var ems []map[string]string
			if json.Unmarshal(brut, &ems) == nil {
				rattache := map[string]bool{}
				for _, n := range noms {
					rattache[n] = true
				}
				garde := ems[:0]
				for _, e := range ems {
					if rattache[e["machine"]] {
						garde = append(garde, e)
					}
				}
				sys["emetteurs"], _ = json.Marshal(garde)
			}
		}
		if e, err := certsyslog.LireEtat(ctx, db, collecteur); err == nil && e.Certificat != "" {
			var nom string
			_ = json.Unmarshal(sys["nom_certificat"], &nom)
			if e.Nom == nom {
				brut, _ := json.Marshal(e.Certificat)
				sys["certificat"] = brut
			}
		}
		if m["syslog"], err = json.Marshal(sys); err != nil {
			return nil, err
		}
	}
	delete(m, "equipements")
	if len(noms) > 0 {
		brut, err := json.Marshal(noms)
		if err != nil {
			return nil, err
		}
		m["equipements"] = brut
	}
	return json.Marshal(m)
}

// CreerEquipement : équipement relevé par un collecteur (pas de token : il
// n'envoie rien lui-même), approuvé tout de suite et rattaché. Refusé pour
// un nom existant.
func CreerEquipement(ctx context.Context, db *sql.DB, nom string, collecteur int64, qui string, maintenant time.Time) error {
	if !FormatNom.MatchString(nom) {
		return errors.New("nom de machine invalide")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // sans effet après Commit
	if err := nomLibre(ctx, tx, nom, 0); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO machines(nom, statut, cree_le) VALUES (?, 'approuvee', ?)`, nom, maintenant.Unix()); err != nil {
		return err
	}
	if err := audit(ctx, tx, maintenant.Unix(), qui, "equipement_cree", nom, "", "", ""); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	id, err := IDMachine(ctx, db, nom)
	if err != nil {
		return err
	}
	return RattacherEquipement(ctx, db, collecteur, id, qui, maintenant)
}

// Collecteurs : noms des collecteurs approuvés.
func Collecteurs(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT nom FROM machines WHERE type = 'collecteur' AND statut = 'approuvee' ORDER BY nom`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
