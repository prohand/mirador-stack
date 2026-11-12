// SPDX-License-Identifier: GPL-3.0-or-later

package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Codes de secours de la 2FA (SPEC.md § 20) : si le téléphone est perdu, un
// code remplace le code TOTP, une seule fois. Dix codes par compte, affichés
// une seule fois à leur création ; en créer de nouveaux annule les anciens.

// NombreCodesSecours : codes créés à chaque fois.
const NombreCodesSecours = 10

// Alphabet sans les caractères les plus ambigus (ni 0 ni O, ni 1 ni I) :
// 32 signes, soit 5 bits par signe.
const alphabetSecours = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// nouveauCodeSecours : 12 signes (60 bits d'aléa), écrits « XXXX-XXXX-XXXX ».
func nouveauCodeSecours() (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	var s strings.Builder
	for i, o := range b {
		if i > 0 && i%4 == 0 {
			s.WriteByte('-')
		}
		s.WriteByte(alphabetSecours[o&31])
	}
	return s.String(), nil
}

// normaliserSecours : majuscules, sans espaces ni tirets ; "" si la forme
// n'est pas celle d'un code de secours.
func normaliserSecours(code string) string {
	var s strings.Builder
	for _, r := range strings.ToUpper(code) {
		switch {
		case r == ' ' || r == '-':
		case strings.ContainsRune(alphabetSecours, r):
			s.WriteRune(r)
		default:
			return ""
		}
	}
	if s.Len() != 12 {
		return ""
	}
	return s.String()
}

func empreinteSecours(normalise string) []byte {
	h := sha256.Sum256([]byte("mirador-secours:" + normalise))
	return h[:]
}

// EstCodeSecours dit si le texte a la forme d'un code de secours (et pas
// celle d'un code TOTP à 6 chiffres).
func EstCodeSecours(code string) bool { return normaliserSecours(code) != "" }

// CreerCodesSecours remplace les codes du compte par dix nouveaux et les
// renvoie en clair : c'est la seule fois où ils sont visibles.
func CreerCodesSecours(ctx context.Context, db *sql.DB, u Utilisateur, qui, adresse string, t time.Time) ([]string, error) {
	codes := make([]string, 0, NombreCodesSecours)
	for len(codes) < NombreCodesSecours {
		c, err := nouveauCodeSecours()
		if err != nil {
			return nil, err
		}
		codes = append(codes, c)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck // sans effet après Commit
	if _, err := tx.ExecContext(ctx, `DELETE FROM codes_secours WHERE utilisateur_id = ?`, u.ID); err != nil {
		return nil, err
	}
	for _, c := range codes {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO codes_secours(utilisateur_id, empreinte, cree_le) VALUES (?, ?, ?)`,
			u.ID, empreinteSecours(normaliserSecours(c)), t.Unix()); err != nil {
			return nil, err
		}
	}
	if err := auditer(ctx, tx, qui, "utilisateur_codes_secours", u.Nom, "", "10 nouveaux codes", adresse, t); err != nil {
		return nil, err
	}
	return codes, tx.Commit()
}

// CodesSecoursRestants : nombre de codes pas encore utilisés.
func CodesSecoursRestants(ctx context.Context, db *sql.DB, utilisateur int64) (int, error) {
	var n int
	err := db.QueryRowContext(ctx, `SELECT count(*) FROM codes_secours WHERE utilisateur_id = ? AND utilise_le = 0`, utilisateur).Scan(&n)
	return n, err
}

// UtiliserCodeSecours consomme un code : vrai une seule fois par code. La
// mise à jour « WHERE utilise_le = 0 » empêche deux requêtes simultanées de
// se servir du même code.
func UtiliserCodeSecours(ctx context.Context, db *sql.DB, u Utilisateur, code, adresse string, t time.Time) (bool, error) {
	n := normaliserSecours(code)
	if n == "" {
		return false, nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback() //nolint:errcheck // sans effet après Commit
	res, err := tx.ExecContext(ctx, `UPDATE codes_secours SET utilise_le = ? WHERE utilisateur_id = ? AND empreinte = ? AND utilise_le = 0`,
		t.Unix(), u.ID, empreinteSecours(n))
	if err != nil {
		return false, err
	}
	if lignes, _ := res.RowsAffected(); lignes != 1 {
		return false, nil
	}
	if err := auditer(ctx, tx, "web:"+u.Nom, "utilisateur_code_secours_utilise", u.Nom, "", "", adresse, t); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// EffacerSecoursEtPasskeys retire les codes de secours et les passkeys d'un
// compte : fait avec la remise à zéro de sa 2FA (téléphone perdu ou volé,
// compte repris en main par un administrateur).
func EffacerSecoursEtPasskeys(ctx context.Context, db *sql.DB, nom string) error {
	var id int64
	err := db.QueryRowContext(ctx, `SELECT id FROM utilisateurs WHERE nom = ?`, nom).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrUtilisateurInconnu
	}
	if err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM codes_secours WHERE utilisateur_id = ?`, id); err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `DELETE FROM passkeys WHERE utilisateur_id = ?`, id)
	return err
}
