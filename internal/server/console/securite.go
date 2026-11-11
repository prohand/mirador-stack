// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"mirador-stack/internal/server/alertes"
	"mirador-stack/internal/server/ingestion"
)

// Securite : réglages de sécurité de l'interface (clé « console.securite »).
type Securite struct {
	Inactivite alertes.Duree `json:"inactivite"` // session fermée après ce temps sans requête
	DureeMax   alertes.Duree `json:"duree_max"`  // durée maximale d'une session
	Echecs     int           `json:"echecs"`     // échecs avant verrouillage (par adresse)
	Verrou     alertes.Duree `json:"verrou"`     // durée du verrouillage
	IPs        string        `json:"ips"`        // adresses ou préfixes autorisés ('' = toutes)
}

// SecuriteDefaut : sessions courtes (brief).
func SecuriteDefaut() Securite {
	return Securite{
		Inactivite: alertes.Duree(30 * time.Minute),
		DureeMax:   alertes.Duree(12 * time.Hour),
		Echecs:     5,
		Verrou:     alertes.Duree(15 * time.Minute),
	}
}

// Valider : bornes de chaque réglage.
func (s *Securite) Valider() error {
	var errs []error
	if d := time.Duration(s.Inactivite); d < 5*time.Minute || d > 8*time.Hour {
		errs = append(errs, errors.New("inactivité : entre 5 min et 8 h"))
	}
	if d := time.Duration(s.DureeMax); d < time.Hour || d > 7*24*time.Hour || d < time.Duration(s.Inactivite) {
		errs = append(errs, errors.New("durée maximale : entre 1 h et 7 j, et au moins l'inactivité"))
	}
	if s.Echecs < 3 || s.Echecs > 50 {
		errs = append(errs, errors.New("échecs avant verrouillage : entre 3 et 50"))
	}
	if d := time.Duration(s.Verrou); d < time.Minute || d > 24*time.Hour {
		errs = append(errs, errors.New("verrouillage : entre 1 min et 24 h"))
	}
	if len(s.IPs) > 4096 {
		errs = append(errs, errors.New("liste d'adresses : 4 Kio au plus"))
	} else if _, err := ingestion.LireIPs(s.IPs); err != nil {
		errs = append(errs, fmt.Errorf("liste d'adresses illisible : %v", err))
	}
	return errors.Join(errs...)
}

// prefixes : liste d'adresses analysée (déjà validée).
func (s *Securite) prefixes() []netip.Prefix {
	p, _ := ingestion.LireIPs(s.IPs)
	return p
}

// AdresseAutorisee : liste vide = toute adresse.
func (s *Securite) AdresseAutorisee(ip netip.Addr) bool {
	p := s.prefixes()
	if len(p) == 0 {
		return true
	}
	for _, x := range p {
		if x.Contains(ip.Unmap()) {
			return true
		}
	}
	return false
}

const cleSecurite = "console.securite"

// LireSecurite : réglages en base ou défauts.
func LireSecurite(ctx context.Context, db *sql.DB) (Securite, error) {
	s := SecuriteDefaut()
	var v string
	err := db.QueryRowContext(ctx, `SELECT valeur FROM reglages WHERE cle = ?`, cleSecurite).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	dec := json.NewDecoder(strings.NewReader(v))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return SecuriteDefaut(), fmt.Errorf("réglages de sécurité abîmés en base : %w", err)
	}
	return s, s.Valider()
}

// DelaiConfirmation : un changement qui peut enfermer dehors est annulé
// s'il n'est pas confirmé dans ce délai (brief : 2 minutes).
const DelaiConfirmation = 2 * time.Minute

// EcrireSecurite enregistre les réglages (audit avant / après). Si la liste
// d'adresses change, le changement est « à confirmer » : sans confirmation
// depuis la nouvelle configuration dans les 2 min, l'ancienne revient.
// Renvoie le numéro du changement à confirmer (0 si rien à confirmer).
func EcrireSecurite(ctx context.Context, db *sql.DB, s Securite, qui, adresse string, t time.Time) (int64, error) {
	if err := s.Valider(); err != nil {
		return 0, err
	}
	avant, err := LireSecurite(ctx, db)
	if err != nil {
		return 0, err
	}
	brutAvant, _ := json.Marshal(avant)
	brut, _ := json.Marshal(s)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck // sans effet après Commit
	if err := ecrireReglage(ctx, tx, cleSecurite, string(brut), qui, t); err != nil {
		return 0, err
	}
	if err := Auditer(ctx, tx, qui, "reglages_securite", cleSecurite, string(brutAvant), string(brut), adresse, t); err != nil {
		return 0, err
	}
	var id int64
	if normaliserIPs(avant.IPs) != normaliserIPs(s.IPs) {
		// Un seul changement en attente à la fois : on garde la valeur
		// d'origine du plus ancien (celle qui marchait).
		var origine string
		err := tx.QueryRowContext(ctx, `SELECT avant FROM a_confirmer WHERE quoi = 'securite'`).Scan(&origine)
		if errors.Is(err, sql.ErrNoRows) {
			origine = string(brutAvant)
		} else if err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM a_confirmer WHERE quoi = 'securite'`); err != nil {
			return 0, err
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO a_confirmer(quoi, avant, par, cree_le, expire_le) VALUES ('securite', ?, ?, ?, ?)`,
			origine, qui, t.Unix(), t.Add(DelaiConfirmation).Unix())
		if err != nil {
			return 0, err
		}
		id, _ = res.LastInsertId()
	}
	return id, tx.Commit()
}

func normaliserIPs(s string) string {
	p, _ := ingestion.LireIPs(s)
	out := make([]string, len(p))
	for i, x := range p {
		out[i] = x.String()
	}
	return strings.Join(out, ",")
}

func ecrireReglage(ctx context.Context, x execer, cle, valeur, qui string, t time.Time) error {
	_, err := x.ExecContext(ctx, `INSERT INTO reglages(cle, valeur, modifie_le, modifie_par) VALUES (?, ?, ?, ?)
		ON CONFLICT(cle) DO UPDATE SET valeur = excluded.valeur, modifie_le = excluded.modifie_le, modifie_par = excluded.modifie_par`,
		cle, valeur, t.Unix(), qui)
	return err
}

// AConfirmer : un changement en attente de confirmation.
type AConfirmer struct {
	ID       int64
	Quoi     string
	Par      string
	ExpireLe int64
}

// ListerAConfirmer : changements en attente.
func ListerAConfirmer(ctx context.Context, db *sql.DB) ([]AConfirmer, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, quoi, par, expire_le FROM a_confirmer ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AConfirmer
	for rows.Next() {
		var a AConfirmer
		if err := rows.Scan(&a.ID, &a.Quoi, &a.Par, &a.ExpireLe); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Confirmer garde le changement (la requête qui confirme a passé la
// nouvelle configuration : c'est la preuve qu'on n'est pas enfermé dehors).
func Confirmer(ctx context.Context, db *sql.DB, id int64, qui, adresse string, t time.Time) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // sans effet après Commit
	var quoi string
	var expire int64
	err = tx.QueryRowContext(ctx, `SELECT quoi, expire_le FROM a_confirmer WHERE id = ?`, id).Scan(&quoi, &expire)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && t.Unix() > expire) {
		return errors.New("rien à confirmer : délai dépassé, l'ancienne valeur est revenue")
	}
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM a_confirmer WHERE id = ?`, id); err != nil {
		return err
	}
	if err := Auditer(ctx, tx, qui, "confirmer", quoi, "", "", adresse, t); err != nil {
		return err
	}
	return tx.Commit()
}

// AnnulerExpires remet l'ancienne valeur des changements non confirmés à
// temps. Renvoie le nombre de changements annulés.
func AnnulerExpires(ctx context.Context, db *sql.DB, t time.Time) (int, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, quoi, avant FROM a_confirmer WHERE expire_le < ?`, t.Unix())
	if err != nil {
		return 0, err
	}
	type ligne struct {
		id          int64
		quoi, avant string
	}
	var aFaire []ligne
	for rows.Next() {
		var l ligne
		if err := rows.Scan(&l.id, &l.quoi, &l.avant); err != nil {
			_ = rows.Close() // lecture seule : une erreur de fermeture est sans effet
			return 0, err
		}
		aFaire = append(aFaire, l)
	}
	_ = rows.Close() // lecture seule : une erreur de fermeture est sans effet
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, l := range aFaire {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return 0, err
		}
		switch {
		case l.quoi == "securite":
			err = ecrireReglage(ctx, tx, cleSecurite, l.avant, "annulation automatique", t)
		case strings.HasPrefix(l.quoi, "role:"):
			_, err = tx.ExecContext(ctx, `UPDATE utilisateurs SET role = ? WHERE nom = ?`, l.avant, strings.TrimPrefix(l.quoi, "role:"))
		}
		if err == nil {
			_, err = tx.ExecContext(ctx, `DELETE FROM a_confirmer WHERE id = ?`, l.id)
		}
		if err == nil {
			err = Auditer(ctx, tx, "annulation automatique", "annuler_non_confirme", l.quoi, "", l.avant, "", t)
		}
		if err != nil {
			_ = tx.Rollback()
			return 0, err
		}
		if err := tx.Commit(); err != nil {
			return 0, err
		}
	}
	return len(aFaire), nil
}

// DemanderConfirmationRole : un changement de son PROPRE rôle peut enfermer
// dehors ; il est appliqué puis annulé sans confirmation sous 2 min.
func DemanderConfirmationRole(ctx context.Context, db *sql.DB, nom, ancien, qui string, t time.Time) (int64, error) {
	if _, err := db.ExecContext(ctx, `DELETE FROM a_confirmer WHERE quoi = ?`, "role:"+nom); err != nil {
		return 0, err
	}
	res, err := db.ExecContext(ctx, `INSERT INTO a_confirmer(quoi, avant, par, cree_le, expire_le) VALUES (?, ?, ?, ?, ?)`,
		"role:"+nom, ancien, qui, t.Unix(), t.Add(DelaiConfirmation).Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}
