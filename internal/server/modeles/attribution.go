// SPDX-License-Identifier: GPL-3.0-or-later

package modeles

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Attribution des modèles (SPEC § 24.2) : table modeles_attribues.

// États d'une attribution.
const (
	EtatActif  = "actif"
	EtatIgnore = "ignore" // proposition refusée, ou modèle du groupe retiré pour cette machine
)

var rePortee = regexp.MustCompile(`^(machine:[0-9]{1,18}|groupe:[A-Za-z0-9][A-Za-z0-9._-]{0,62})$`)

// Attribution : une ligne de la table.
type Attribution struct {
	Portee, Modele, Etat string
	Parametres           map[string]float64
	Listes               map[string][]string // listes réglées pour cette portée (partie B)
	ModifiePar           string              // « auto » : appliqué automatiquement (détection)
}

// PorteeMachine, PorteeGroupe : noms des portées.
func PorteeMachine(id int64) string  { return "machine:" + strconv.FormatInt(id, 10) }
func PorteeGroupe(nom string) string { return "groupe:" + nom }

// Lister : attributions d'une portée.
func Lister(ctx context.Context, db *sql.DB, portee string) ([]Attribution, error) {
	rows, err := db.QueryContext(ctx, `SELECT portee, modele, etat, parametres, listes, modifie_par FROM modeles_attribues WHERE portee = ? ORDER BY modele`, portee)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Attribution
	for rows.Next() {
		var a Attribution
		var p, l string
		if err := rows.Scan(&a.Portee, &a.Modele, &a.Etat, &p, &l, &a.ModifiePar); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(p), &a.Parametres); err != nil {
			a.Parametres = nil // ligne abîmée : défauts du modèle
		}
		if err := json.Unmarshal([]byte(l), &a.Listes); err != nil {
			a.Listes = nil
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Definir : attribue (actif), ignore, ou retire (etat vide) un modèle pour
// une portée, avec ses paramètres (vérifiés : bornes du modèle) et ses
// listes (vérifiées : format de l'agent ; absente = héritée). Audit dans la
// même transaction.
func Definir(ctx context.Context, db *sql.DB, portee, modele, etat string, parametres map[string]float64, listes map[string][]string, qui string, maintenant time.Time) error {
	if !rePortee.MatchString(portee) {
		return errors.New("portée invalide")
	}
	m := Trouver(modele)
	if m == nil {
		return fmt.Errorf("modèle %q inconnu", modele)
	}
	if etat != "" && etat != EtatActif && etat != EtatIgnore {
		return errors.New("état invalide")
	}
	for nom, v := range parametres {
		if _, err := m.LireValeur(nom, strconv.FormatFloat(v, 'g', -1, 64)); err != nil {
			return err
		}
	}
	for nom, lignes := range listes {
		lp := m.Liste(nom)
		if lp == nil {
			return fmt.Errorf("liste %q inconnue", nom)
		}
		if _, err := lp.LireLignes(strings.Join(lignes, "\n")); err != nil {
			return err
		}
	}
	brut, err := json.Marshal(parametres)
	if err != nil {
		return err
	}
	if parametres == nil {
		brut = []byte("{}")
	}
	brutListes, err := json.Marshal(listes)
	if err != nil {
		return err
	}
	if listes == nil {
		brutListes = []byte("{}")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // sans effet après Commit
	action := "modele_" + etat
	if etat == "" {
		action = "modele_retire"
		if _, err := tx.ExecContext(ctx, `DELETE FROM modeles_attribues WHERE portee = ? AND modele = ?`, portee, modele); err != nil {
			return err
		}
	} else if _, err := tx.ExecContext(ctx, `INSERT INTO modeles_attribues(portee, modele, etat, parametres, listes, modifie_le, modifie_par)
		VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT(portee, modele) DO UPDATE SET etat = excluded.etat, parametres = excluded.parametres,
		listes = excluded.listes, modifie_le = excluded.modifie_le, modifie_par = excluded.modifie_par`,
		portee, modele, etat, string(brut), string(brutListes), maintenant.Unix(), qui); err != nil {
		return err
	}
	apres := string(brut)
	if listes != nil {
		apres += " " + string(brutListes)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO audit(le, qui, action, objet, avant, apres, adresse) VALUES (?, ?, ?, ?, '', ?, '')`,
		maintenant.Unix(), qui, action, portee+" "+modele, apres); err != nil {
		return err
	}
	return tx.Commit()
}

// Effectif : un modèle appliqué à une machine, avec ses seuils effectifs
// (machine > groupe > défaut) et d'où il vient.
type Effectif struct {
	Modele     *Modele
	Parametres map[string]float64  // tous les paramètres du modèle
	DuGroupe   bool                // attribué au groupe (pas à la machine elle-même)
	Surcharges map[string]bool     // paramètres réglés pour cette machine
	Services   []string            // services systemd attendus (liste du modèle)
	Listes     map[string][]string // listes effectives (machine > groupe)
	ListesMach map[string]bool     // listes réglées pour cette machine
	Auto       bool                // appliqué automatiquement (détection)
}

// Effectifs : modèles d'une machine = ceux de son groupe + les siens, moins
// ceux qu'elle ignore ; triés par nom (ordre de la configuration).
func Effectifs(ctx context.Context, db *sql.DB, machine int64, groupe string) ([]Effectif, error) {
	var deGroupe []Attribution
	var err error
	if groupe != "" {
		if deGroupe, err = Lister(ctx, db, PorteeGroupe(groupe)); err != nil {
			return nil, err
		}
	}
	propres, err := Lister(ctx, db, PorteeMachine(machine))
	if err != nil {
		return nil, err
	}
	type cumul struct {
		groupe, machine *Attribution
	}
	tous := map[string]*cumul{}
	for i := range deGroupe {
		tous[deGroupe[i].Modele] = &cumul{groupe: &deGroupe[i]}
	}
	for i := range propres {
		c := tous[propres[i].Modele]
		if c == nil {
			c = &cumul{}
			tous[propres[i].Modele] = c
		}
		c.machine = &propres[i]
	}
	var out []Effectif
	for nom, c := range tous {
		m := Trouver(nom)
		if m == nil {
			continue // modèle disparu d'une version de mirador
		}
		actif := c.groupe != nil && c.groupe.Etat == EtatActif
		if c.machine != nil {
			actif = c.machine.Etat == EtatActif
		}
		if !actif {
			continue
		}
		svc, err := Services(ctx, db, m)
		if err != nil {
			return nil, err
		}
		e := Effectif{Modele: m, Parametres: map[string]float64{}, Surcharges: map[string]bool{},
			Listes: map[string][]string{}, ListesMach: map[string]bool{},
			DuGroupe: c.machine == nil, Services: svc, Auto: c.machine != nil && c.machine.ModifiePar == QuiAuto}
		for _, p := range m.Parametres {
			e.Parametres[p.Nom] = p.Defaut
			if c.groupe != nil && c.groupe.Etat == EtatActif {
				if v, ok := c.groupe.Parametres[p.Nom]; ok {
					e.Parametres[p.Nom] = v
				}
			}
			if c.machine != nil {
				if v, ok := c.machine.Parametres[p.Nom]; ok {
					e.Parametres[p.Nom] = v
					e.Surcharges[p.Nom] = true
				}
			}
		}
		for _, l := range m.Listes {
			if c.groupe != nil && c.groupe.Etat == EtatActif {
				if v, ok := c.groupe.Listes[l.Nom]; ok {
					e.Listes[l.Nom] = v
				}
			}
			if c.machine != nil {
				if v, ok := c.machine.Listes[l.Nom]; ok {
					e.Listes[l.Nom] = v
					e.ListesMach[l.Nom] = true
				}
			}
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Modele.Nom < out[j].Modele.Nom })
	return out, nil
}

// QuiAuto : auteur noté quand un modèle détecté est appliqué automatiquement.
const QuiAuto = "auto"

// cleAuto : réglage « appliquer automatiquement les modèles détectés ».
const cleAuto = "modeles.auto"

// LireAuto : application automatique activée ? (non par défaut).
func LireAuto(ctx context.Context, db *sql.DB) bool {
	var v string
	err := db.QueryRowContext(ctx, `SELECT valeur FROM reglages WHERE cle = ?`, cleAuto).Scan(&v)
	return err == nil && v == "oui"
}

// DefinirAuto : active ou coupe l'application automatique (audit).
func DefinirAuto(ctx context.Context, db *sql.DB, actif bool, qui string, maintenant time.Time) error {
	v := "non"
	if actif {
		v = "oui"
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // sans effet après Commit
	if _, err := tx.ExecContext(ctx, `INSERT INTO reglages(cle, valeur, modifie_le, modifie_par) VALUES (?, ?, ?, ?)
		ON CONFLICT(cle) DO UPDATE SET valeur = excluded.valeur, modifie_le = excluded.modifie_le, modifie_par = excluded.modifie_par`,
		cleAuto, v, maintenant.Unix(), qui); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO audit(le, qui, action, objet, avant, apres, adresse) VALUES (?, ?, 'modeles_auto', '', '', ?, '')`,
		maintenant.Unix(), qui, v); err != nil {
		return err
	}
	return tx.Commit()
}

// Ignores : modèles qu'une machine a ignorés (pour ne plus les proposer).
func Ignores(ctx context.Context, db *sql.DB, machine int64) (map[string]bool, error) {
	as, err := Lister(ctx, db, PorteeMachine(machine))
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, a := range as {
		if a.Etat == EtatIgnore {
			out[a.Modele] = true
		}
	}
	return out, nil
}

// ConfigEffective : configuration apportée par les modèles d'une machine
// (clé par clé, dans l'ordre des noms ; listes réunies ; checks tirés des
// listes des modèles).
func ConfigEffective(effectifs []Effectif) (map[string]json.RawMessage, error) {
	out := map[string]json.RawMessage{}
	if checks, _ := checksEffectifs(effectifs); len(checks) > 0 {
		brut, err := json.Marshal(checks)
		if err != nil {
			return nil, err
		}
		out["checks"] = brut
	}
	for _, e := range effectifs {
		// Services du modèle : toujours surveillés (même si la machine ne
		// surveille que les services validés un par un).
		if len(e.Services) > 0 {
			if brut, err := json.Marshal(e.Services); err == nil {
				out["systemd_surveiller"] = reunir(out["systemd_surveiller"], brut)
			}
		}
		if len(e.Modele.Config) == 0 {
			continue
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(e.Modele.Config, &m); err != nil {
			return nil, fmt.Errorf("modèle %s : %w", e.Modele.Nom, err)
		}
		for k, v := range m {
			out[k] = reunir(out[k], v)
		}
	}
	return out, nil
}

// reunir : deux listes de textes sont réunies (sans doublon) ; sinon la
// nouvelle valeur l'emporte.
func reunir(avant, apres json.RawMessage) json.RawMessage {
	var a, b []string
	if avant == nil || json.Unmarshal(avant, &a) != nil || json.Unmarshal(apres, &b) != nil {
		return apres
	}
	vu := map[string]bool{}
	var out []string
	for _, x := range append(a, b...) {
		if !vu[x] {
			vu[x] = true
			out = append(out, x)
		}
	}
	r, err := json.Marshal(out)
	if err != nil {
		return apres
	}
	return r
}

// --- Services des modèles -----------------------------------------------------------

// cleServices : réglage qui remplace la liste de départ d'un modèle.
func cleServices(modele string) string { return "modeles.services." + modele }

// Services : services attendus d'un modèle (liste modifiée dans
// l'interface, sinon celle du modèle).
func Services(ctx context.Context, db *sql.DB, m *Modele) ([]string, error) {
	var v string
	err := db.QueryRowContext(ctx, `SELECT valeur FROM reglages WHERE cle = ?`, cleServices(m.Nom)).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return m.Services, nil
	}
	if err != nil {
		return nil, err
	}
	var l []string
	if err := json.Unmarshal([]byte(v), &l); err != nil || ValiderServices(l) != nil {
		return m.Services, nil // valeur abîmée : liste du modèle
	}
	return l, nil
}

// DefinirServices : nouvelle liste de services d'un modèle (vérifiée, triée,
// audit). Valable pour toutes les machines qui portent le modèle.
func DefinirServices(ctx context.Context, db *sql.DB, modele string, services []string, qui string, maintenant time.Time) error {
	m := Trouver(modele)
	if m == nil {
		return fmt.Errorf("modèle %q inconnu", modele)
	}
	if err := ValiderServices(services); err != nil {
		return err
	}
	l := append([]string{}, services...)
	sort.Strings(l)
	brut, err := json.Marshal(l)
	if err != nil {
		return err
	}
	avant, err := Services(ctx, db, m)
	if err != nil {
		return err
	}
	avantBrut, _ := json.Marshal(avant)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // sans effet après Commit
	if _, err := tx.ExecContext(ctx, `INSERT INTO reglages(cle, valeur, modifie_le, modifie_par) VALUES (?, ?, ?, ?)
		ON CONFLICT(cle) DO UPDATE SET valeur = excluded.valeur, modifie_le = excluded.modifie_le, modifie_par = excluded.modifie_par`,
		cleServices(modele), string(brut), maintenant.Unix(), qui); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO audit(le, qui, action, objet, avant, apres, adresse) VALUES (?, ?, 'modele_services', ?, ?, ?, '')`,
		maintenant.Unix(), qui, modele, string(avantBrut), string(brut)); err != nil {
		return err
	}
	return tx.Commit()
}
