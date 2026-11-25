// SPDX-License-Identifier: GPL-3.0-or-later

package alertes

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// VM ou conteneur Proxmox exclu de la règle VMSansSauvegarde (retour du
// 25/11 : la VM du PBS lui-même n'a pas à être dans une sauvegarde PBS).
// La liste est en base (réglage) ; le serveur écrit une série
// MetriqueInviteExclue{instance, id} par invité exclu, et la règle les
// écarte (« unless on (instance, id) … »). L'agent ne change pas.

// MetriqueInviteExclue : série synthétique (1) d'un invité exclu.
const MetriqueInviteExclue = "mirador_pve_invite_exclue"

// ExprVMSansSauvegarde : expression de la règle VMSansSauvegarde ;
// exprVMSansSauvegardeAvant : celle d'avant les exclusions (mise à jour au
// démarrage si l'utilisateur ne l'a pas changée).
const (
	ExprVMSansSauvegarde      = `mirador_pve_invite_sans_sauvegarde == 1 unless on (instance, id) ` + MetriqueInviteExclue
	exprVMSansSauvegardeAvant = `mirador_pve_invite_sans_sauvegarde == 1`
)

const cleInvitesExclus = "taches.invites_exclus"

// InviteExclu : un invité (VM, CT) d'un nœud exclu de la règle.
type InviteExclu struct {
	Machine string `json:"machine"`
	ID      string `json:"id"`
	Nom     string `json:"nom,omitempty"`
	Motif   string `json:"motif,omitempty"`
	Qui     string `json:"qui"`
	Le      int64  `json:"le"`
}

var (
	reIDInvite    = regexp.MustCompile(`^[0-9]{1,9}$`)
	reNomMachine  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)
	maxExclusions = 500
)

// LireInvitesExclus : liste en base (vide si aucune).
func LireInvitesExclus(ctx context.Context, db *sql.DB) ([]InviteExclu, error) {
	var v string
	err := db.QueryRowContext(ctx, `SELECT valeur FROM reglages WHERE cle = ?`, cleInvitesExclus).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var l []InviteExclu
	if err := json.Unmarshal([]byte(v), &l); err != nil {
		return nil, fmt.Errorf("liste des invités exclus abîmée : %w", err)
	}
	return l, nil
}

// ExclureInvite ajoute (ou met à jour) un invité exclu ; audit.
func ExclureInvite(ctx context.Context, db *sql.DB, e InviteExclu, t time.Time) error {
	if !reNomMachine.MatchString(e.Machine) || !reIDInvite.MatchString(e.ID) {
		return errors.New("machine ou numéro de VM invalide")
	}
	e.Nom, e.Motif = borner(e.Nom, 100), borner(e.Motif, 200)
	e.Le = t.Unix()
	l, err := LireInvitesExclus(ctx, db)
	if err != nil {
		return err
	}
	garde := l[:0]
	for _, x := range l {
		if x.Machine != e.Machine || x.ID != e.ID {
			garde = append(garde, x)
		}
	}
	if len(garde) >= maxExclusions {
		return fmt.Errorf("%d exclusions au plus", maxExclusions)
	}
	return ecrireInvitesExclus(ctx, db, append(garde, e), e.Qui, "invite_exclu", e.Machine+" "+e.ID, t)
}

// ReintegrerInvite retire un invité de la liste ; audit.
func ReintegrerInvite(ctx context.Context, db *sql.DB, machine, id, qui string, t time.Time) error {
	l, err := LireInvitesExclus(ctx, db)
	if err != nil {
		return err
	}
	garde := l[:0]
	trouve := false
	for _, x := range l {
		if x.Machine == machine && x.ID == id {
			trouve = true
			continue
		}
		garde = append(garde, x)
	}
	if !trouve {
		return errors.New("cette VM n'est pas exclue")
	}
	return ecrireInvitesExclus(ctx, db, garde, qui, "invite_reintegre", machine+" "+id, t)
}

func ecrireInvitesExclus(ctx context.Context, db *sql.DB, l []InviteExclu, qui, action, objet string, t time.Time) error {
	if l == nil {
		l = []InviteExclu{}
	}
	b, _ := json.Marshal(l)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // sans effet après Commit
	if _, err := tx.ExecContext(ctx, `INSERT INTO reglages(cle, valeur, modifie_le, modifie_par) VALUES (?, ?, ?, ?)
		ON CONFLICT(cle) DO UPDATE SET valeur = excluded.valeur, modifie_le = excluded.modifie_le, modifie_par = excluded.modifie_par`,
		cleInvitesExclus, string(b), t.Unix(), qui); err != nil {
		return err
	}
	if err := audit(ctx, tx, t.Unix(), qui, action, objet, "", ""); err != nil {
		return err
	}
	return tx.Commit()
}

// borner : texte d'une ligne, n caractères au plus.
func borner(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// MettreAJourVMSansSauvegarde : la règle posée avant les exclusions reçoit
// la nouvelle expression, si l'utilisateur ne l'a pas changée (sinon elle
// est laissée telle quelle). Renvoie vrai si elle a été mise à jour.
func MettreAJourVMSansSauvegarde(ctx context.Context, db *sql.DB, t time.Time) (bool, error) {
	rs, err := ListerRegles(ctx, db)
	if err != nil {
		return false, err
	}
	for _, r := range rs {
		if r.Nom == "VMSansSauvegarde" && strings.TrimSpace(r.Expr) == exprVMSansSauvegardeAvant {
			r.Expr = ExprVMSansSauvegarde
			_, err := Enregistrer(ctx, db, []Regle{r}, "mirador", t)
			return err == nil, err
		}
	}
	return false, nil
}
