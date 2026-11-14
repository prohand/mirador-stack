// SPDX-License-Identifier: GPL-3.0-or-later

package modeles

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"mirador-stack/internal/server/alertes"
)

// Modèles personnels (partie C, SPEC § 24.11) : créés, copiés ou importés
// dans l'interface, rangés dans la table modeles_perso. Même format et mêmes
// contrôles que les modèles livrés ; leurs règles sont posées (ou mises à
// jour, ou retirées) à chaque enregistrement.

// Bornes des modèles personnels.
const (
	MaxPerso       = 200
	TailleMaxPerso = 64 << 10 // JSON d'un modèle
)

// reFiltreModele : filtre modele="…" dans une expression de règle.
var reFiltreModele = regexp.MustCompile(`\bmodele\s*=\s*"([^"]*)"`)

var (
	muPerso sync.RWMutex
	perso   = map[string]*Modele{}
	// remplacees : règles remplacées par au moins un modèle (cache relu à
	// chaque chargement, consulté par le moteur d'alertes à chaque règle).
	remplacees = map[string]bool{}
)

func init() { majRemplacees() }

// majRemplacees recalcule le cache des règles remplacées.
func majRemplacees() {
	r := map[string]bool{}
	for nom := range Remplacees() {
		r[nom] = true
	}
	muPerso.Lock()
	remplacees = r
	muPerso.Unlock()
}

// EstRemplacee : une règle est-elle remplacée par un modèle (livré ou
// personnel) ? Utilisé par le moteur d'alertes (SPEC § 24.2).
func EstRemplacee(nom string) bool {
	muPerso.RLock()
	defer muPerso.RUnlock()
	return remplacees[nom]
}

// DecoderModele lit un modèle JSON (champs inconnus refusés, taille bornée)
// et le vérifie.
func DecoderModele(brut []byte) (*Modele, error) {
	if len(brut) > TailleMaxPerso {
		return nil, fmt.Errorf("modèle trop grand (%d Kio au plus)", TailleMaxPerso>>10)
	}
	var m Modele
	dec := json.NewDecoder(bytes.NewReader(brut))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("JSON du modèle : %w", err)
	}
	if dec.More() {
		return nil, errors.New("JSON du modèle : texte en trop après le modèle")
	}
	if err := m.Valider(); err != nil {
		return nil, err
	}
	return &m, nil
}

// ChargerPerso relit les modèles personnels. Un modèle abîmé, ou qui porte
// le nom d'un modèle livré depuis (nouvelle version de mirador), est laissé
// de côté et cité dans l'erreur renvoyée (le reste est chargé).
func ChargerPerso(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `SELECT nom, definition FROM modeles_perso`)
	if err != nil {
		return err
	}
	defer rows.Close()
	lus := map[string]*Modele{}
	var errs []error
	for rows.Next() {
		var nom, def string
		if err := rows.Scan(&nom, &def); err != nil {
			return err
		}
		if fournis[nom] != nil {
			errs = append(errs, fmt.Errorf("modèle personnel %s ignoré : un modèle livré porte ce nom", nom))
			continue
		}
		m, err := DecoderModele([]byte(def))
		if err != nil || m.Nom != nom {
			errs = append(errs, fmt.Errorf("modèle personnel %s abîmé : %v", nom, err))
			continue
		}
		m.Perso = true
		lus[nom] = m
	}
	if err := rows.Err(); err != nil {
		return err
	}
	muPerso.Lock()
	perso = lus
	muPerso.Unlock()
	majRemplacees()
	return errors.Join(errs...)
}

// EnregistrerPerso crée ou remplace un modèle personnel : vérifié, nom
// différent des modèles livrés, règles au nom libre (pas prises par une
// autre règle), puis règles posées (une règle désactivée dans « Règles » le
// reste), règles disparues retirées, audit.
func EnregistrerPerso(ctx context.Context, db *sql.DB, m *Modele, qui string, maintenant time.Time) error {
	if err := m.Valider(); err != nil {
		return err
	}
	if fournis[m.Nom] != nil {
		return fmt.Errorf("« %s » est un modèle livré : choisir un autre nom (ou le dupliquer)", m.Nom)
	}
	// Erreur courante (copie, import renommé) : la règle filtre les tests
	// d'un autre modèle et ne sonnerait jamais pour celui-ci.
	for _, r := range m.Regles {
		for _, x := range reFiltreModele.FindAllStringSubmatch(r.Expr, -1) {
			if x[1] != m.Nom {
				return fmt.Errorf("règle %s : elle filtre modele=%q, mais les tests de ce modèle sont étiquetés modele=%q", r.Nom, x[1], m.Nom)
			}
		}
	}
	brut, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if len(brut) > TailleMaxPerso {
		return fmt.Errorf("modèle trop grand (%d Kio au plus)", TailleMaxPerso>>10)
	}
	origine := "modele:" + m.Nom
	existantes, err := alertes.ListerRegles(ctx, db)
	if err != nil {
		return err
	}
	parNom := map[string]alertes.Regle{}
	for _, r := range existantes {
		parNom[r.Nom] = r
	}
	nouvelles := make([]alertes.Regle, 0, len(m.Regles))
	garder := map[string]bool{}
	for _, rm := range m.Regles {
		if r, ok := parNom[rm.Nom]; ok && r.Origine != origine {
			return fmt.Errorf("règle %s : ce nom est déjà pris par une autre règle", rm.Nom)
		}
		for _, autre := range Tous() {
			if autre.Nom == m.Nom {
				continue
			}
			for _, x := range autre.Regles {
				if x.Nom == rm.Nom {
					return fmt.Errorf("règle %s : ce nom est déjà pris par le modèle « %s »", rm.Nom, autre.Titre)
				}
			}
		}
		r := m.RegleAlerte(rm)
		if avant, ok := parNom[rm.Nom]; ok {
			r.Active = avant.Active
		}
		nouvelles = append(nouvelles, r)
		garder[rm.Nom] = true
	}

	var avant string
	err = db.QueryRowContext(ctx, `SELECT definition FROM modeles_perso WHERE nom = ?`, m.Nom).Scan(&avant)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if errors.Is(err, sql.ErrNoRows) {
		var n int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM modeles_perso`).Scan(&n); err != nil {
			return err
		}
		if n >= MaxPerso {
			return fmt.Errorf("%d modèles personnels au plus", MaxPerso)
		}
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // sans effet après Commit
	if _, err := tx.ExecContext(ctx, `INSERT INTO modeles_perso(nom, definition, modifie_le, modifie_par) VALUES (?, ?, ?, ?)
		ON CONFLICT(nom) DO UPDATE SET definition = excluded.definition, modifie_le = excluded.modifie_le, modifie_par = excluded.modifie_par`,
		m.Nom, string(brut), maintenant.Unix(), qui); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO audit(le, qui, action, objet, avant, apres, adresse) VALUES (?, ?, 'modele_perso', ?, ?, ?, '')`,
		maintenant.Unix(), qui, m.Nom, avant, string(brut)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	// Règles : posées après le modèle (vérifiées plus haut, l'échec est
	// improbable ; un nouvel enregistrement les repose).
	if _, err := alertes.Enregistrer(ctx, db, nouvelles, qui, maintenant); err != nil {
		return fmt.Errorf("modèle enregistré, mais règles refusées : %w", err)
	}
	for _, r := range existantes {
		if r.Origine == origine && !garder[r.Nom] {
			if err := alertes.Supprimer(ctx, db, r.Nom, qui, maintenant); err != nil {
				return err
			}
		}
	}
	return ChargerPerso(ctx, db)
}

// Attributions : portées où un modèle est attribué (actif).
func Attributions(ctx context.Context, db *sql.DB, modele string) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT portee FROM modeles_attribues WHERE modele = ? AND etat = ? ORDER BY portee`, modele, EtatActif)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// SupprimerPerso retire un modèle personnel qui n'est plus attribué (lignes
// « ignoré », liste de services et règles retirées avec lui ; audit).
func SupprimerPerso(ctx context.Context, db *sql.DB, nom, qui string, maintenant time.Time) error {
	m := Trouver(nom)
	if m == nil || !m.Perso {
		return fmt.Errorf("modèle personnel %q inconnu", nom)
	}
	ps, err := Attributions(ctx, db, nom)
	if err != nil {
		return err
	}
	if len(ps) > 0 {
		return fmt.Errorf("« %s » est encore attribué (%s) : le retirer d'abord", m.Titre, strings.Join(ps, ", "))
	}
	existantes, err := alertes.ListerRegles(ctx, db)
	if err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // sans effet après Commit
	var avant string
	if err := tx.QueryRowContext(ctx, `SELECT definition FROM modeles_perso WHERE nom = ?`, nom).Scan(&avant); err != nil {
		return err
	}
	for _, q := range []string{`DELETE FROM modeles_perso WHERE nom = ?`, `DELETE FROM modeles_attribues WHERE modele = ?`} {
		if _, err := tx.ExecContext(ctx, q, nom); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM reglages WHERE cle = ?`, cleServices(nom)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO audit(le, qui, action, objet, avant, apres, adresse) VALUES (?, ?, 'modele_perso_supprime', ?, ?, '', '')`,
		maintenant.Unix(), qui, nom, avant); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	for _, r := range existantes {
		if r.Origine == "modele:"+nom {
			if err := alertes.Supprimer(ctx, db, r.Nom, qui, maintenant); err != nil {
				return err
			}
		}
	}
	return ChargerPerso(ctx, db)
}

// Copie : copie d'un modèle sous un nouveau nom, prête à modifier (règles
// renommées avec le suffixe « Perso » : un nom de règle est unique ; filtre
// modele="ancien" → modele="nouveau", l'étiquette des checks de la copie).
func Copie(m *Modele, nom, titre string) *Modele {
	c := *m
	c.Nom, c.Titre, c.Perso = nom, titre, false
	c.Parametres = append([]Parametre(nil), m.Parametres...)
	c.Listes = append([]ListeParam(nil), m.Listes...)
	c.Services = append([]string(nil), m.Services...)
	c.Config = append(json.RawMessage(nil), m.Config...)
	c.Regles = nil
	for _, r := range m.Regles {
		r.Nom = suffixeRegle(r.Nom)
		r.Expr = strings.ReplaceAll(r.Expr, `modele="`+m.Nom+`"`, `modele="`+nom+`"`)
		c.Regles = append(c.Regles, r)
	}
	return &c
}

// suffixeRegle : nom de règle + « Perso » (64 caractères au plus).
func suffixeRegle(nom string) string {
	if len(nom) > 59 {
		nom = nom[:59]
	}
	return nom + "Perso"
}
