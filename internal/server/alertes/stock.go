// SPDX-License-Identifier: GPL-3.0-or-later

package alertes

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/prometheus/prometheus/model/labels"
)

// Accès à la base : règles, compteurs de modification, historique.

// execer : *sql.DB ou *sql.Tx.
type execer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

// LireGeneration renvoie le compteur de modification d'une table (0 si jamais
// modifiée).
func LireGeneration(ctx context.Context, db *sql.DB, nom string) (int64, error) {
	var v int64
	err := db.QueryRowContext(ctx, `SELECT valeur FROM generations WHERE nom = ?`, nom).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return v, err
}

// Incrementer augmente le compteur (dans la transaction de la modification).
func Incrementer(ctx context.Context, x execer, nom string) error {
	_, err := x.ExecContext(ctx, `INSERT INTO generations(nom, valeur) VALUES (?, 1)
		ON CONFLICT(nom) DO UPDATE SET valeur = valeur + 1`, nom)
	return err
}

func audit(ctx context.Context, x execer, le int64, qui, action, objet, avant, apres string) error {
	_, err := x.ExecContext(ctx, `INSERT INTO audit(le, qui, action, objet, avant, apres) VALUES (?, ?, ?, ?, ?, ?)`,
		le, qui, action, objet, avant, apres)
	return err
}

// ListerRegles renvoie toutes les règles (actives ou non), triées par groupe
// puis nom. Une définition abîmée en base est une erreur (jamais ignorée en
// silence).
func ListerRegles(ctx context.Context, db *sql.DB) ([]Regle, error) {
	rows, err := db.QueryContext(ctx, `SELECT nom, definition FROM regles`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Regle
	for rows.Next() {
		var nom, def string
		if err := rows.Scan(&nom, &def); err != nil {
			return nil, err
		}
		r, err := lireDefinition(def)
		if err != nil {
			return nil, fmt.Errorf("règle %s abîmée en base : %w", nom, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Groupe != out[j].Groupe {
			return out[i].Groupe < out[j].Groupe
		}
		return out[i].Nom < out[j].Nom
	})
	return out, nil
}

func lireDefinition(def string) (Regle, error) {
	var r Regle
	dec := json.NewDecoder(strings.NewReader(def))
	dec.DisallowUnknownFields()
	err := dec.Decode(&r)
	return r, err
}

// Enregistrer ajoute ou remplace des règles (vérifiées d'abord, toutes ou
// aucune), avec une ligne d'audit par règle changée. Renvoie le nombre de
// règles réellement changées.
func Enregistrer(ctx context.Context, db *sql.DB, regles []Regle, qui string, maintenant time.Time) (int, error) {
	vus := map[string]bool{}
	for i := range regles {
		regles[i].Defauts()
		if _, err := regles[i].Valider(); err != nil {
			return 0, err
		}
		if vus[regles[i].Nom] {
			return 0, fmt.Errorf("règle %s en double", regles[i].Nom)
		}
		vus[regles[i].Nom] = true
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck // sans effet après Commit
	n := 0
	for _, r := range regles {
		def, err := json.Marshal(r)
		if err != nil {
			return 0, err
		}
		var avant string
		err = tx.QueryRowContext(ctx, `SELECT definition FROM regles WHERE nom = ?`, r.Nom).Scan(&avant)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
		if avant == string(def) {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO regles(nom, definition, modifie_le, modifie_par) VALUES (?, ?, ?, ?)
			ON CONFLICT(nom) DO UPDATE SET definition = excluded.definition, modifie_le = excluded.modifie_le,
			modifie_par = excluded.modifie_par`, r.Nom, string(def), maintenant.Unix(), qui); err != nil {
			return 0, err
		}
		if err := audit(ctx, tx, maintenant.Unix(), qui, "regle", r.Nom, avant, string(def)); err != nil {
			return 0, err
		}
		n++
	}
	if n > 0 {
		if err := Incrementer(ctx, tx, "regles"); err != nil {
			return 0, err
		}
	}
	return n, tx.Commit()
}

// Supprimer retire une règle (ses alertes en cours sont résolues au
// rechargement).
func Supprimer(ctx context.Context, db *sql.DB, nom, qui string, maintenant time.Time) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // sans effet après Commit
	var avant string
	if err := tx.QueryRowContext(ctx, `SELECT definition FROM regles WHERE nom = ?`, nom).Scan(&avant); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("règle %s inconnue", nom)
		}
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM regles WHERE nom = ?`, nom); err != nil {
		return err
	}
	if err := audit(ctx, tx, maintenant.Unix(), qui, "regle_supprimee", nom, avant, ""); err != nil {
		return err
	}
	if err := Incrementer(ctx, tx, "regles"); err != nil {
		return err
	}
	return tx.Commit()
}

// Activer active ou désactive une règle.
func Activer(ctx context.Context, db *sql.DB, nom string, active bool, qui string, maintenant time.Time) error {
	rs, err := ListerRegles(ctx, db)
	if err != nil {
		return err
	}
	for _, r := range rs {
		if r.Nom == nom {
			r.Active = active
			_, err := Enregistrer(ctx, db, []Regle{r}, qui, maintenant)
			return err
		}
	}
	return fmt.Errorf("règle %s inconnue", nom)
}

// Historique : dernières alertes (en cours et résolues), plus récentes d'abord.
func Historique(ctx context.Context, db *sql.DB, limite int) ([]Alerte, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, regle, etiquettes, gravite, etat, debut, active_le, fin,
		valeur_debut, valeur_max, valeur, resume, acquittee_le, acquittee_par, commentaire
		FROM alertes ORDER BY id DESC LIMIT ?`, limite)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Alerte
	for rows.Next() {
		var a Alerte
		var etq string
		var debut, activeLe, fin, ackLe int64
		if err := rows.Scan(&a.ID, &a.Regle, &etq, &a.Gravite, &a.Etat, &debut, &activeLe, &fin,
			&a.ValeurDebut, &a.ValeurMax, &a.Valeur, &a.Resume, &ackLe, &a.AcquitteePar, &a.Commentaire); err != nil {
			return nil, err
		}
		a.Etiquettes = etiquettesJSON(etq)
		a.Debut, a.ActiveLe, a.Fin, a.AcquitteeLe = depuisMs(debut), depuisMs(activeLe), depuisMs(fin), depuisMs(ackLe)
		out = append(out, a)
	}
	return out, rows.Err()
}

// PurgerHistorique efface les alertes résolues avant la date donnée.
func PurgerHistorique(ctx context.Context, db *sql.DB, avant time.Time) (int64, error) {
	res, err := db.ExecContext(ctx, `DELETE FROM alertes WHERE etat = 'resolue' AND fin < ?`, avant.UnixMilli())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// etiquettesJSON lit des étiquettes gardées en JSON (vide si illisible).
func etiquettesJSON(s string) labels.Labels {
	var mp map[string]string
	if err := json.Unmarshal([]byte(s), &mp); err != nil {
		return labels.EmptyLabels()
	}
	return labels.FromMap(mp)
}

// Filtre de l'historique (interface) : pagination par numéro décroissant.
type Filtre struct {
	Avant   int64  // numéros plus petits que celui-ci (0 = depuis le plus récent)
	Regle   string // nom exact
	Machine string // étiquette instance exacte
	Gravite string
	Limite  int // 200 au plus
}

// HistoriqueFiltre : alertes (toutes, en cours comprises), plus récentes d'abord.
func HistoriqueFiltre(ctx context.Context, db *sql.DB, f Filtre) ([]Alerte, error) {
	if f.Limite <= 0 || f.Limite > 200 {
		f.Limite = 200
	}
	q := `SELECT id, regle, etiquettes, gravite, etat, debut, active_le, fin, valeur_debut, valeur_max, valeur, resume,
		acquittee_le, acquittee_par, commentaire, description FROM alertes WHERE 1 = 1`
	var args []any
	if f.Avant > 0 {
		q += ` AND id < ?`
		args = append(args, f.Avant)
	}
	if f.Regle != "" {
		q += ` AND regle = ?`
		args = append(args, f.Regle)
	}
	if f.Machine != "" {
		q += ` AND json_extract(etiquettes, '$.instance') = ?`
		args = append(args, f.Machine)
	}
	if f.Gravite != "" {
		q += ` AND gravite = ?`
		args = append(args, f.Gravite)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, f.Limite)
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Alerte
	for rows.Next() {
		var a Alerte
		var etq string
		var debut, activeLe, fin, ackLe int64
		if err := rows.Scan(&a.ID, &a.Regle, &etq, &a.Gravite, &a.Etat, &debut, &activeLe, &fin,
			&a.ValeurDebut, &a.ValeurMax, &a.Valeur, &a.Resume, &ackLe, &a.AcquitteePar, &a.Commentaire, &a.Description); err != nil {
			return nil, err
		}
		a.Etiquettes = etiquettesJSON(etq)
		a.Debut, a.ActiveLe, a.Fin, a.AcquitteeLe = depuisMs(debut), depuisMs(activeLe), depuisMs(fin), depuisMs(ackLe)
		out = append(out, a)
	}
	return out, rows.Err()
}

// LireAlerte : une alerte par son numéro.
func LireAlerte(ctx context.Context, db *sql.DB, id int64) (Alerte, error) {
	q := Filtre{Avant: id + 1, Limite: 1}
	as, err := HistoriqueFiltre(ctx, db, q)
	if err != nil {
		return Alerte{}, err
	}
	if len(as) == 0 || as[0].ID != id {
		return Alerte{}, fmt.Errorf("alerte %d inconnue", id)
	}
	return as[0], nil
}
