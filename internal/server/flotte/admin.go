// SPDX-License-Identifier: GPL-3.0-or-later

package flotte

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"mirador-stack/internal/protocole"
	"mirador-stack/internal/server/ingestion"
)

// Actions d'administration des machines. Utilisées par les commandes du
// serveur (partie 5) puis par l'interface (étape 5). Chaque action écrit une
// ligne d'audit ; aucun secret n'y figure.
//
// Effet sur un serveur en marche : immédiat pour un token tout neuf ; une
// révocation faite depuis une commande (autre processus) prend effet au plus
// tard après la durée du cache des tokens (60 s).

// PrefixeJeton : jetons d'enrôlement, repérables par les détecteurs de secrets.
const PrefixeJeton = "mirador_enrol_"

// ErrMachineInconnue : aucun machine de ce nom.
var ErrMachineInconnue = errors.New("machine inconnue")

// InfoMachine : ce que montre la liste des machines.
type InfoMachine struct {
	ID                                   int64
	Nom, Type, Statut, Site, Env, Groupe string
	Version, Architecture, VersionCible  string
	IPEnrolement, IPsAutorisees          string
	DerniereIP, MajEssai, MajEchec       string
	VuLe, BattementLe, CreeLe            int64
	TokensActifs                         int
	NomDuJeton                           bool   // nom choisi à la création du jeton
	Capacites                            string // annoncées par l'agent (« surveillance »…)
	DemarrageLe                          int64  // démarrage de l'agent
	// Empreinte de la machine (SPEC § 19.8) : dernière reçue, et celle à
	// laquelle le token est lié ('' : pas lié).
	EmpreinteVue, EmpreinteLiee string
}

// Surveillance : l'agent connaît les réglages de surveillance (SPEC § 19).
func (i InfoMachine) Surveillance() bool {
	return ACapacite(i.Capacites, protocole.CapaciteSurveillance)
}

// CreerJeton fabrique un jeton d'enrôlement à usage unique, valable duree.
// Le jeton n'est affiché qu'une fois ; seule son empreinte est gardée.
func CreerJeton(ctx context.Context, db *sql.DB, duree time.Duration, qui string, maintenant time.Time) (string, error) {
	return CreerJetonEtiquettes(ctx, db, duree, Etiquettes{}, qui, maintenant)
}

// Etiquettes : site, type (env) et groupe d'une machine ; Nom (jeton) :
// nom imposé à la future machine (” = celui qu'elle propose).
type Etiquettes struct{ Site, Env, Groupe, Nom string }

func (e Etiquettes) verifier() error {
	if e.Nom != "" && !FormatNom.MatchString(e.Nom) {
		return errors.New("nom de machine invalide (lettres, chiffres, . _ -)")
	}
	for _, v := range []string{e.Site, e.Env, e.Groupe} {
		if !formatEtiquette.MatchString(v) {
			return fmt.Errorf("valeur %q invalide (lettres, chiffres, . _ -)", v)
		}
	}
	return nil
}

// CreerJetonEtiquettes : jeton d'enrôlement qui porte déjà le site, le type
// et le groupe de la future machine (copiés sur elle à sa demande).
func CreerJetonEtiquettes(ctx context.Context, db *sql.DB, duree time.Duration, e Etiquettes, qui string, maintenant time.Time) (string, error) {
	if err := e.verifier(); err != nil {
		return "", err
	}
	if duree < time.Minute || duree > 7*24*time.Hour {
		return "", errors.New("durée de validité entre 1 min et 7 jours")
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	jeton := PrefixeJeton + base64.RawURLEncoding.EncodeToString(b)
	em := ingestion.Empreinte(jeton)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback() //nolint:errcheck // sans effet après Commit
	if e.Nom != "" {
		if err := nomLibre(ctx, tx, e.Nom, 0); err != nil {
			return "", err
		}
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM jetons_enrolement WHERE nom = ? AND utilise_le = 0 AND expire_le > ?`,
			e.Nom, maintenant.Unix()).Scan(&n); err != nil {
			return "", err
		}
		if n > 0 {
			return "", errors.New("un jeton valable existe déjà pour ce nom")
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO jetons_enrolement(empreinte, expire_le, cree_par, cree_le, site, env, groupe, nom) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		em[:], maintenant.Add(duree).Unix(), qui, maintenant.Unix(), e.Site, e.Env, e.Groupe, e.Nom); err != nil {
		return "", err
	}
	if err := audit(ctx, tx, maintenant.Unix(), qui, "jeton_cree", "", "", "*** expire "+maintenant.Add(duree).UTC().Format(time.RFC3339), ""); err != nil {
		return "", err
	}
	return jeton, tx.Commit()
}

// ListerMachines : toutes les machines, par nom.
func ListerMachines(ctx context.Context, db *sql.DB) ([]InfoMachine, error) {
	rows, err := db.QueryContext(ctx, `SELECT m.id, m.nom, m.type, m.statut, m.site, m.env, m.groupe, m.version_agent, m.architecture,
		m.version_cible, m.ip_enrolement, m.ips_autorisees, m.derniere_ip, m.maj_essai, m.maj_echec, m.vu_le, m.battement_le, m.cree_le,
		(SELECT count(*) FROM tokens t WHERE t.machine_id = m.id AND t.revoque_le = 0),
		EXISTS(SELECT 1 FROM jetons_enrolement j WHERE j.machine_id = m.id AND j.nom != ''),
		m.capacites, m.demarrage_le, m.empreinte_vue, m.empreinte_liee
		FROM machines m ORDER BY m.nom`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []InfoMachine
	for rows.Next() {
		var i InfoMachine
		if err := rows.Scan(&i.ID, &i.Nom, &i.Type, &i.Statut, &i.Site, &i.Env, &i.Groupe, &i.Version, &i.Architecture,
			&i.VersionCible, &i.IPEnrolement, &i.IPsAutorisees, &i.DerniereIP, &i.MajEssai, &i.MajEchec, &i.VuLe, &i.BattementLe,
			&i.CreeLe, &i.TokensActifs, &i.NomDuJeton, &i.Capacites, &i.DemarrageLe,
			&i.EmpreinteVue, &i.EmpreinteLiee); err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// IDMachine renvoie l'identifiant d'une machine par son nom.
func IDMachine(ctx context.Context, db *sql.DB, nom string) (int64, error) {
	var id int64
	err := db.QueryRowContext(ctx, `SELECT id FROM machines WHERE nom = ?`, nom).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrMachineInconnue
	}
	return id, err
}

// changer : modification d'une machine + audit, dans une transaction.
func changer(ctx context.Context, db *sql.DB, nom, qui, action, apres string, maintenant time.Time,
	f func(tx *sql.Tx, id int64) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // sans effet après Commit
	var id int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM machines WHERE nom = ?`, nom).Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrMachineInconnue
		}
		return err
	}
	if err := f(tx, id); err != nil {
		return err
	}
	if err := audit(ctx, tx, maintenant.Unix(), qui, action, nom, "", apres, ""); err != nil {
		return err
	}
	return tx.Commit()
}

// Approuver une machine en attente. avecIP : l'adresse de la demande
// d'enrôlement devient la liste des adresses autorisées (si elle est vide).
func Approuver(ctx context.Context, db *sql.DB, nom string, avecIP bool, qui string, maintenant time.Time) error {
	return changer(ctx, db, nom, qui, "approuver", "approuvee", maintenant, func(tx *sql.Tx, id int64) error {
		if _, err := tx.ExecContext(ctx, `UPDATE machines SET statut = 'approuvee' WHERE id = ?`, id); err != nil {
			return err
		}
		if avecIP {
			_, err := tx.ExecContext(ctx, `UPDATE machines SET ips_autorisees = ip_enrolement WHERE id = ? AND ips_autorisees = ''`, id)
			return err
		}
		return nil
	})
}

// Refuser une machine : statut « refusée » et tokens révoqués.
func Refuser(ctx context.Context, db *sql.DB, nom, qui string, maintenant time.Time) error {
	return changer(ctx, db, nom, qui, "refuser", "refusee", maintenant, func(tx *sql.Tx, id int64) error {
		if _, err := tx.ExecContext(ctx, `UPDATE machines SET statut = 'refusee' WHERE id = ?`, id); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE tokens SET revoque_le = ? WHERE machine_id = ? AND revoque_le = 0`, maintenant.Unix(), id)
		return err
	})
}

// Revoquer tous les tokens d'une machine (elle reste approuvée : un nouveau
// token ou un nouvel enrôlement la remet en service).
func Revoquer(ctx context.Context, db *sql.DB, nom, qui string, maintenant time.Time) error {
	return changer(ctx, db, nom, qui, "revoquer", "tokens révoqués", maintenant, func(tx *sql.Tx, id int64) error {
		_, err := tx.ExecContext(ctx, `UPDATE tokens SET revoque_le = ? WHERE machine_id = ? AND revoque_le = 0`, maintenant.Unix(), id)
		return err
	})
}

// TokenManuel : installation sans enrôlement (comme le script de l'étape 2).
// Crée la machine approuvée si elle n'existe pas, révoque ses anciens tokens
// et renvoie un nouveau token, affiché une seule fois.
func TokenManuel(ctx context.Context, db *sql.DB, nom, qui string, maintenant time.Time) (string, error) {
	if !FormatNom.MatchString(nom) {
		return "", errors.New("nom de machine invalide")
	}
	token, err := NouveauToken()
	if err != nil {
		return "", err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback() //nolint:errcheck // sans effet après Commit
	var id int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM machines WHERE nom = ?`, nom).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		res, err := tx.ExecContext(ctx, `INSERT INTO machines(nom, statut, cree_le) VALUES (?, 'approuvee', ?)`, nom, maintenant.Unix())
		if err != nil {
			return "", err
		}
		if id, err = res.LastInsertId(); err != nil {
			return "", err
		}
	} else if err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tokens SET revoque_le = ? WHERE machine_id = ? AND revoque_le = 0`, maintenant.Unix(), id); err != nil {
		return "", err
	}
	e := ingestion.Empreinte(token)
	if _, err := tx.ExecContext(ctx, `INSERT INTO tokens(machine_id, empreinte, cree_le) VALUES (?, ?, ?)`, id, e[:], maintenant.Unix()); err != nil {
		return "", err
	}
	if err := audit(ctx, tx, maintenant.Unix(), qui, "token_manuel", nom, "", "***", ""); err != nil {
		return "", err
	}
	return token, tx.Commit()
}

// DefinirIPs : adresses ou préfixes autorisés pour le token de la machine
// (vide = toutes celles admises par nginx).
func DefinirIPs(ctx context.Context, db *sql.DB, nom, liste, qui string, maintenant time.Time) error {
	prefixes, err := ingestion.LireIPs(liste)
	if err != nil {
		return fmt.Errorf("liste d'adresses invalide : %w", err)
	}
	var morceaux []string
	for _, p := range prefixes {
		morceaux = append(morceaux, p.String())
	}
	norme := strings.Join(morceaux, ", ")
	return changer(ctx, db, nom, qui, "ips_autorisees", norme, maintenant, func(tx *sql.Tx, id int64) error {
		_, err := tx.ExecContext(ctx, `UPDATE machines SET ips_autorisees = ? WHERE id = ?`, norme, id)
		return err
	})
}

var formatEtiquette = regexp.MustCompile(`^[A-Za-z0-9._-]{0,63}$`)

// LierEmpreinte lie le token de la machine à l'empreinte qu'elle envoie en ce
// moment (lier = true), ou retire ce lien. Lié, le token est refusé s'il est
// présenté par une autre machine, ou par un agent trop ancien pour envoyer
// son empreinte.
func LierEmpreinte(ctx context.Context, db *sql.DB, nom string, lier bool, qui string, maintenant time.Time) error {
	if !lier {
		return changer(ctx, db, nom, qui, "machine_empreinte", "non liée", maintenant,
			func(tx *sql.Tx, id int64) error {
				_, err := tx.ExecContext(ctx, `UPDATE machines SET empreinte_liee = '' WHERE id = ?`, id)
				return err
			})
	}
	var vue string
	if err := db.QueryRowContext(ctx, `SELECT empreinte_vue FROM machines WHERE nom = ?`, nom).Scan(&vue); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrMachineInconnue
		}
		return err
	}
	if !ingestion.EmpreinteValide(vue) {
		return errors.New("cette machine n'a pas encore envoyé d'empreinte (agent trop ancien, ou /etc/machine-id absent)")
	}
	return changer(ctx, db, nom, qui, "machine_empreinte", "liée à "+vue[:8]+"…", maintenant,
		func(tx *sql.Tx, id int64) error {
			_, err := tx.ExecContext(ctx, `UPDATE machines SET empreinte_liee = ? WHERE id = ?`, vue, id)
			return err
		})
}

// DefinirEtiquettes : site, env et groupe (groupe = portée de configuration).
func DefinirEtiquettes(ctx context.Context, db *sql.DB, nom, site, env, groupe, qui string, maintenant time.Time) error {
	if err := (Etiquettes{Site: site, Env: env, Groupe: groupe}).verifier(); err != nil {
		return err
	}
	return changer(ctx, db, nom, qui, "etiquettes", "site="+site+" env="+env+" groupe="+groupe, maintenant,
		func(tx *sql.Tx, id int64) error {
			_, err := tx.ExecContext(ctx, `UPDATE machines SET site = ?, env = ?, groupe = ? WHERE id = ?`, site, env, groupe, id)
			return err
		})
}

// Renommer change le nom d'une machine (étiquette instance / host des
// prochaines données ; l'historique garde l'ancien nom). Appeler ensuite
// Oublier sur le cache des tokens pour un effet immédiat.
func Renommer(ctx context.Context, db *sql.DB, nom, nouveau, qui string, maintenant time.Time) error {
	if !FormatNom.MatchString(nouveau) {
		return errors.New("nouveau nom invalide (lettres, chiffres, . _ -)")
	}
	return changer(ctx, db, nom, qui, "renommer", nouveau, maintenant, func(tx *sql.Tx, id int64) error {
		if err := nomLibre(ctx, tx, nouveau, id); err != nil {
			return err
		}
		// L'ancien nom est gardé (historique rattaché au nouveau nom à la
		// lecture), sauf pour une machine encore en attente (aucune donnée).
		// Revenir à un ancien nom l'enlève de la liste.
		var statut string
		if err := tx.QueryRowContext(ctx, `SELECT statut FROM machines WHERE id = ?`, id).Scan(&statut); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM noms_anciens WHERE machine_id = ? AND nom = ?`, id, nouveau); err != nil {
			return err
		}
		if statut != "en_attente" {
			if _, err := tx.ExecContext(ctx, `INSERT INTO noms_anciens(machine_id, nom, jusqu_a) VALUES (?, ?, ?)
				ON CONFLICT(nom) DO UPDATE SET machine_id = excluded.machine_id, jusqu_a = excluded.jusqu_a`, id, nom, maintenant.Unix()); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, `UPDATE machines SET nom = ? WHERE id = ?`, nouveau, id)
		return err
	})
}

// nomLibre : ni une autre machine, ni l'ancien nom d'une autre machine
// (son historique y est rattaché). sauf = la machine elle-même (0 = aucune).
func nomLibre(ctx context.Context, tx *sql.Tx, nom string, sauf int64) error {
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM machines WHERE nom = ? AND id != ?`, nom, sauf).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return errors.New("ce nom est déjà pris")
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM noms_anciens WHERE nom = ? AND machine_id != ?`, nom, sauf).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return errors.New("ce nom est l'ancien nom d'une autre machine (son historique y est rattaché)")
	}
	return nil
}

// AnciensNoms : anciens noms d'une machine (du plus récent au plus ancien).
func AnciensNoms(ctx context.Context, db *sql.DB, id int64) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT nom FROM noms_anciens WHERE machine_id = ? ORDER BY jusqu_a DESC`, id)
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

// NomsAnciens : ancien nom → nom actuel (rattachement de l'historique).
func NomsAnciens(ctx context.Context, db *sql.DB) (map[string]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT a.nom, m.nom FROM noms_anciens a JOIN machines m ON m.id = a.machine_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var ancien, actuel string
		if err := rows.Scan(&ancien, &actuel); err != nil {
			return nil, err
		}
		out[ancien] = actuel
	}
	return out, rows.Err()
}

// Supprimer efface une machine et ses tokens (l'historique des métriques
// est effacé à part, sur demande ; les logs partent avec la rétention).
func Supprimer(ctx context.Context, db *sql.DB, nom, qui string, maintenant time.Time) error {
	return changer(ctx, db, nom, qui, "supprimer_machine", "", maintenant, func(tx *sql.Tx, id int64) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM machines WHERE id = ?`, id)
		return err
	})
}
