// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"time"
)

// Rôles (brief : lecture seule et admin).
const (
	RoleAdmin   = "admin"
	RoleLecture = "lecture"
)

// Utilisateur : un compte de l'interface.
type Utilisateur struct {
	ID          int64
	Nom, Role   string
	MotDePasse  string // empreinte Argon2id
	TOTP        string // secret chiffré ; '' = 2FA pas encore créée
	TOTPDernier int64
	Generation  int64
	Desactive   bool
	CreeLe      int64
	ConnecteLe  int64
}

// Erreurs.
var (
	ErrUtilisateurInconnu = errors.New("utilisateur inconnu")
	ErrDernierAdmin       = errors.New("il doit rester au moins un administrateur actif")
)

var reNomUtilisateur = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{1,31}$`)

// VerifierNom : 2 à 32 caractères, minuscules, chiffres, « . _ - ».
func VerifierNom(nom string) error {
	if !reNomUtilisateur.MatchString(nom) {
		return errors.New("nom : 2 à 32 caractères (minuscules, chiffres, . _ -)")
	}
	return nil
}

func verifierRole(r string) error {
	if r != RoleAdmin && r != RoleLecture {
		return fmt.Errorf("rôle %q : admin ou lecture", r)
	}
	return nil
}

type execer interface {
	ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error)
}

// Auditer écrit une ligne d'audit (jamais de secret : l'appelant masque).
func Auditer(ctx context.Context, x execer, qui, action, objet, avant, apres, adresse string, t time.Time) error {
	_, err := x.ExecContext(ctx, `INSERT INTO audit(le, qui, action, objet, avant, apres, adresse) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		t.Unix(), qui, action, objet, avant, apres, adresse)
	return err
}

const colonnesUtilisateur = `id, nom, role, mot_de_passe, totp, totp_dernier, generation, desactive, cree_le, connecte_le`

func scannerUtilisateur(sc interface{ Scan(...any) error }) (Utilisateur, error) {
	var u Utilisateur
	var d int
	err := sc.Scan(&u.ID, &u.Nom, &u.Role, &u.MotDePasse, &u.TOTP, &u.TOTPDernier, &u.Generation, &d, &u.CreeLe, &u.ConnecteLe)
	u.Desactive = d != 0
	return u, err
}

// LireUtilisateur par son nom.
func LireUtilisateur(ctx context.Context, db *sql.DB, nom string) (Utilisateur, error) {
	u, err := scannerUtilisateur(db.QueryRowContext(ctx, `SELECT `+colonnesUtilisateur+` FROM utilisateurs WHERE nom = ?`, nom))
	if errors.Is(err, sql.ErrNoRows) {
		return u, ErrUtilisateurInconnu
	}
	return u, err
}

// LireUtilisateurID par son numéro.
func LireUtilisateurID(ctx context.Context, db *sql.DB, id int64) (Utilisateur, error) {
	u, err := scannerUtilisateur(db.QueryRowContext(ctx, `SELECT `+colonnesUtilisateur+` FROM utilisateurs WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return u, ErrUtilisateurInconnu
	}
	return u, err
}

// ListerUtilisateurs : tous les comptes (quelques-uns au plus).
func ListerUtilisateurs(ctx context.Context, db *sql.DB) ([]Utilisateur, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+colonnesUtilisateur+` FROM utilisateurs ORDER BY nom`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Utilisateur
	for rows.Next() {
		u, err := scannerUtilisateur(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// NombreUtilisateurs : 0 = le premier admin reste à créer.
func NombreUtilisateurs(ctx context.Context, db *sql.DB) (int, error) {
	var n int
	err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM utilisateurs`).Scan(&n)
	return n, err
}

// CreerUtilisateur ajoute un compte. Sa 2FA sera créée à sa première
// connexion (obligatoire avant tout accès).
func CreerUtilisateur(ctx context.Context, db *sql.DB, nom, role, mdp, qui, adresse string, t time.Time) error {
	if err := errors.Join(VerifierNom(nom), verifierRole(role), VerifierSimple(mdp, nom)); err != nil {
		return err
	}
	e, err := Empreinte(mdp)
	if err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // sans effet après Commit
	// Nom déjà pris : vérifié dans la transaction (la base n'a qu'une
	// connexion : une lecture hors de la transaction se bloquerait).
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM utilisateurs WHERE nom = ?`, nom).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return errors.New("ce nom existe déjà")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO utilisateurs(nom, role, mot_de_passe, cree_le) VALUES (?, ?, ?, ?)`,
		nom, role, e, t.Unix()); err != nil {
		return err
	}
	if err := Auditer(ctx, tx, qui, "utilisateur_creer", nom, "", "role="+role, adresse, t); err != nil {
		return err
	}
	return tx.Commit()
}

// modifier : une mise à jour d'un compte + son audit, en une transaction.
// fermer = true ferme toutes les sessions du compte (generation + 1).
func modifier(ctx context.Context, db *sql.DB, nom, action, avant, apres, qui, adresse string, t time.Time,
	fermer bool, req string, args ...any) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // sans effet après Commit
	res, err := tx.ExecContext(ctx, req, append(args, nom)...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrUtilisateurInconnu
	}
	if fermer {
		if _, err := tx.ExecContext(ctx, `UPDATE utilisateurs SET generation = generation + 1 WHERE nom = ?`, nom); err != nil {
			return err
		}
	}
	if err := Auditer(ctx, tx, qui, action, nom, avant, apres, adresse, t); err != nil {
		return err
	}
	if err := verifierUnAdmin(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

// verifierUnAdmin : refuse toute modification qui laisserait zéro admin
// actif avec sa 2FA (on ne peut pas s'enfermer dehors par erreur).
func verifierUnAdmin(ctx context.Context, tx *sql.Tx) error {
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM utilisateurs WHERE role = 'admin' AND desactive = 0`).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return ErrDernierAdmin
	}
	return nil
}

// DefinirMotDePasse change le mot de passe et ferme les sessions du compte.
func DefinirMotDePasse(ctx context.Context, db *sql.DB, nom, mdp, qui, adresse string, t time.Time) error {
	if err := VerifierSimple(mdp, nom); err != nil {
		return err
	}
	e, err := Empreinte(mdp)
	if err != nil {
		return err
	}
	return modifier(ctx, db, nom, "utilisateur_mot_de_passe", "", "***", qui, adresse, t, true,
		`UPDATE utilisateurs SET mot_de_passe = ? WHERE nom = ?`, e)
}

// DefinirTOTP enregistre le secret (déjà chiffré) et le pas du code qui l'a
// confirmé ; les autres sessions du compte sont fermées.
func DefinirTOTP(ctx context.Context, db *sql.DB, nom, secretChiffre string, pas int64, qui, adresse string, t time.Time) error {
	return modifier(ctx, db, nom, "utilisateur_2fa", "", "nouvelle 2FA", qui, adresse, t, true,
		`UPDATE utilisateurs SET totp = ?, totp_dernier = ? WHERE nom = ?`, secretChiffre, pas)
}

// ReinitialiserTOTP efface la 2FA : le compte devra la recréer à sa
// prochaine connexion (après son mot de passe).
func ReinitialiserTOTP(ctx context.Context, db *sql.DB, nom, qui, adresse string, t time.Time) error {
	return modifier(ctx, db, nom, "utilisateur_2fa_reinitialiser", "", "", qui, adresse, t, true,
		`UPDATE utilisateurs SET totp = '', totp_dernier = 0 WHERE nom = ?`)
}

// DefinirRole change le rôle (refusé s'il ne reste plus d'admin).
func DefinirRole(ctx context.Context, db *sql.DB, nom, role, qui, adresse string, t time.Time) error {
	if err := verifierRole(role); err != nil {
		return err
	}
	u, err := LireUtilisateur(ctx, db, nom)
	if err != nil {
		return err
	}
	return modifier(ctx, db, nom, "utilisateur_role", u.Role, role, qui, adresse, t, false,
		`UPDATE utilisateurs SET role = ? WHERE nom = ?`, role)
}

// DefinirDesactive active ou désactive un compte (sessions fermées).
func DefinirDesactive(ctx context.Context, db *sql.DB, nom string, desactive bool, qui, adresse string, t time.Time) error {
	v, act := 0, "utilisateur_activer"
	if desactive {
		v, act = 1, "utilisateur_desactiver"
	}
	return modifier(ctx, db, nom, act, "", "", qui, adresse, t, true, `UPDATE utilisateurs SET desactive = ? WHERE nom = ?`, v)
}

// SupprimerUtilisateur efface un compte (jamais le dernier admin).
func SupprimerUtilisateur(ctx context.Context, db *sql.DB, nom, qui, adresse string, t time.Time) error {
	return modifier(ctx, db, nom, "utilisateur_supprimer", "", "", qui, adresse, t, false,
		`DELETE FROM utilisateurs WHERE nom = ?`)
}

// NoterPasTOTP enregistre le pas d'un code accepté. La condition
// « totp_dernier < pas » rend le rejeu impossible même avec deux requêtes
// simultanées : une seule réussit.
func NoterPasTOTP(ctx context.Context, db *sql.DB, id, pas int64, t time.Time) (bool, error) {
	res, err := db.ExecContext(ctx, `UPDATE utilisateurs SET totp_dernier = ?, connecte_le = ? WHERE id = ? AND totp_dernier < ?`,
		pas, t.Unix(), id, pas)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}
