// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Renommer un site, un type ou un groupe (SPEC § 107) : tout ce qui porte
// le nom suit, en une seule transaction ; un nom déjà pris = fusion.
// Les données déjà enregistrées gardent l'ancien nom (elles ne sont jamais
// réécrites) ; les règles et tableaux qui l'écrivent en dur sont signalés,
// pas modifiés (une expression réécrite automatiquement pourrait changer de
// sens sans qu'on le voie).

type impactRenommage struct {
	Machines     int
	Jetons       int // jetons d'enrôlement pas encore utilisés
	Maintenances int
	Config       bool // réglages d'agent du groupe
	Modeles      int  // modèles attribués au groupe
	Regles       []string
	Tableaux     []string
	Motif        string // ce qui a été cherché (site="maison")
}

// requêtes par liste : écrites en entier (pas de nom de colonne assemblé).
var (
	majMachines = map[string]string{
		"sites":   `UPDATE machines SET site = ? WHERE site = ?`,
		"types":   `UPDATE machines SET env = ? WHERE env = ?`,
		"groupes": `UPDATE machines SET groupe = ? WHERE groupe = ?`,
	}
	majJetons = map[string]string{
		"sites":   `UPDATE jetons_enrolement SET site = ? WHERE site = ? AND utilise_le = 0`,
		"types":   `UPDATE jetons_enrolement SET env = ? WHERE env = ? AND utilise_le = 0`,
		"groupes": `UPDATE jetons_enrolement SET groupe = ? WHERE groupe = ? AND utilise_le = 0`,
	}
	compteMachines = map[string]string{
		"sites":   `SELECT COUNT(*) FROM machines WHERE site = ?`,
		"types":   `SELECT COUNT(*) FROM machines WHERE env = ?`,
		"groupes": `SELECT COUNT(*) FROM machines WHERE groupe = ?`,
	}
	compteJetons = map[string]string{
		"sites":   `SELECT COUNT(*) FROM jetons_enrolement WHERE site = ? AND utilise_le = 0`,
		"types":   `SELECT COUNT(*) FROM jetons_enrolement WHERE env = ? AND utilise_le = 0`,
		"groupes": `SELECT COUNT(*) FROM jetons_enrolement WHERE groupe = ? AND utilise_le = 0`,
	}
)

// motifsEtiquette : façons d'écrire « étiquette = valeur » dans une règle ou un
// tableau (JSON : guillemets échappés ou non).
func motifsEtiquette(etiquette, v string) []string {
	return []string{
		etiquette + `="` + v + `"`,
		etiquette + `=\"` + v + `\"`,
		`"` + etiquette + `":"` + v + `"`,
	}
}

func contientUn(texte string, ms []string) bool {
	for _, m := range ms {
		if strings.Contains(texte, m) {
			return true
		}
	}
	return false
}

// impact : ce que changerait le renommage de « ancien » (affiché avant).
func (c *Console) impact(ctx context.Context, liste listeMachines, ancien string) impactRenommage {
	im := impactRenommage{Motif: liste.Etiquette + `="` + ancien + `"`}
	_ = c.db.QueryRowContext(ctx, compteMachines[liste.Cle], ancien).Scan(&im.Machines)
	_ = c.db.QueryRowContext(ctx, compteJetons[liste.Cle], ancien).Scan(&im.Jetons)
	if liste.Cle != "types" {
		_ = c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM maintenances WHERE cible = ?`, liste.Un+":"+ancien).Scan(&im.Maintenances)
	}
	if liste.Cle == "groupes" {
		var n int
		_ = c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM reglages WHERE cle = ?`, "config_agent:groupe:"+ancien).Scan(&n)
		im.Config = n > 0
		_ = c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM modeles_attribues WHERE portee = ?`, "groupe:"+ancien).Scan(&im.Modeles)
	}
	ms := motifsEtiquette(liste.Etiquette, ancien)
	if rows, err := c.db.QueryContext(ctx, `SELECT nom, definition FROM regles`); err == nil {
		for rows.Next() {
			var nom, def string
			if rows.Scan(&nom, &def) == nil && contientUn(def, ms) {
				im.Regles = append(im.Regles, nom)
			}
		}
		c.lectureIncomplete(rows.Err(), "impact")
		_ = rows.Close()
	}
	if rows, err := c.db.QueryContext(ctx, `SELECT nom, contenu FROM tableaux WHERE supprime = 0`); err == nil {
		for rows.Next() {
			var nom, contenu string
			if rows.Scan(&nom, &contenu) == nil && contientUn(contenu, ms) {
				im.Tableaux = append(im.Tableaux, nom)
			}
		}
		c.lectureIncomplete(rows.Err(), "impact")
		_ = rows.Close()
	}
	return im
}

// renommerValeur : renomme (ou fusionne dans une valeur existante) et
// renvoie le nombre de machines changées.
func (c *Console) renommerValeur(ctx context.Context, liste listeMachines, ancien, nouveau string, q *req) (int, error) {
	if !reValeurListe.MatchString(nouveau) {
		return 0, fmt.Errorf("nouveau nom %q : lettres, chiffres, . _ - (63 au plus, sans espace)", nouveau)
	}
	if nouveau == ancien {
		return 0, errors.New("le nouveau nom est le même que l'ancien")
	}
	l := c.lireListes(ctx)
	vals := l.valeurs(liste.Cle)
	i := slices.Index(*vals, ancien)
	if i < 0 {
		return 0, fmt.Errorf("%s %q inconnu", liste.Un, ancien)
	}
	fusion := slices.Contains(*vals, nouveau)
	*vals = slices.Delete(*vals, i, i+1)
	if !fusion {
		*vals = append(*vals, nouveau)
	}
	avantListes := c.lireListes(ctx)
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck // sans effet après Commit
	n, err := renommerDans(ctx, tx, liste, ancien, nouveau)
	if err != nil {
		return 0, err
	}
	if err := ecrireListesAvec(ctx, tx, avantListes, l, q); err != nil {
		return 0, err
	}
	action := "renommer_" + liste.Un
	if fusion {
		action = "fusionner_" + liste.Un
	}
	if err := Auditer(ctx, tx, q.qui(), action, ancien, ancien, nouveau, q.ip.String(), q.t); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	c.oublierEtiquettes()
	return n, nil
}

// renommerDans : toutes les mises à jour, dans la transaction donnée.
func renommerDans(ctx context.Context, tx *sql.Tx, liste listeMachines, ancien, nouveau string) (int, error) {
	res, err := tx.ExecContext(ctx, majMachines[liste.Cle], nouveau, ancien)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if _, err := tx.ExecContext(ctx, majJetons[liste.Cle], nouveau, ancien); err != nil {
		return 0, err
	}
	if liste.Cle != "types" {
		if _, err := tx.ExecContext(ctx, `UPDATE maintenances SET cible = ? WHERE cible = ?`, liste.Un+":"+nouveau, liste.Un+":"+ancien); err != nil {
			return 0, err
		}
	}
	if liste.Cle == "groupes" {
		// Fusion : les réglages et modèles du groupe gardé l'emportent ;
		// ceux de l'ancien groupe qui feraient doublon sont retirés.
		if _, err := tx.ExecContext(ctx, `UPDATE OR IGNORE reglages SET cle = ? WHERE cle = ?`,
			"config_agent:groupe:"+nouveau, "config_agent:groupe:"+ancien); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM reglages WHERE cle = ?`, "config_agent:groupe:"+ancien); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE OR IGNORE modeles_attribues SET portee = ? WHERE portee = ?`, "groupe:"+nouveau, "groupe:"+ancien); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM modeles_attribues WHERE portee = ?`, "groupe:"+ancien); err != nil {
			return 0, err
		}
	}
	return int(n), nil
}
