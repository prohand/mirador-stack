// SPDX-License-Identifier: GPL-3.0-or-later

package notif

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"mirador-stack/internal/server/alertes"
)

// Écritures (commandes du serveur, interface à l'étape 5). Chacune : une
// transaction, une ligne d'audit, et le compteur « sourdine » augmenté pour
// que le serveur en marche relise (10 s au plus).

// Bornes.
const (
	DureeSilenceMax = 90 * 24 * time.Hour
	TailleMaxTexte  = 1024
)

func ecrire(ctx context.Context, db *sql.DB, generation string, f func(tx *sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // sans effet après Commit
	if err := f(tx); err != nil {
		return err
	}
	if err := alertes.Incrementer(ctx, tx, generation); err != nil {
		return err
	}
	return tx.Commit()
}

func auditer(ctx context.Context, tx *sql.Tx, qui, action, objet, avant, apres string, t time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO audit(le, qui, action, objet, avant, apres) VALUES (?, ?, ?, ?, ?, ?)`,
		t.Unix(), qui, action, objet, avant, apres)
	return err
}

func verifierTexte(s string) error {
	if len(s) > TailleMaxTexte {
		return fmt.Errorf("texte trop long (%d octets au plus)", TailleMaxTexte)
	}
	return nil
}

// AjouterSilence crée un silence [debut, debut+duree) ; renvoie son numéro.
func AjouterSilence(ctx context.Context, db *sql.DB, selecteur string, debut time.Time, duree time.Duration,
	qui, commentaire string, t time.Time) (int64, error) {
	if _, err := LireSelecteur(selecteur); err != nil {
		return 0, err
	}
	if duree < time.Minute || duree > DureeSilenceMax {
		return 0, errors.New("durée d'un silence entre 1 min et 90 j")
	}
	if err := verifierTexte(commentaire); err != nil {
		return 0, err
	}
	var id int64
	err := ecrire(ctx, db, "sourdine", func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO silences(selecteur, debut, fin, auteur, commentaire, cree_le) VALUES (?, ?, ?, ?, ?, ?)`,
			selecteur, ms(debut), ms(debut.Add(duree)), qui, commentaire, ms(t))
		if err != nil {
			return err
		}
		if id, err = res.LastInsertId(); err != nil {
			return err
		}
		return auditer(ctx, tx, qui, "silence", strconv.FormatInt(id, 10), "",
			fmt.Sprintf("%s du %s au %s : %s", selecteur, debut.UTC().Format(time.RFC3339),
				debut.Add(duree).UTC().Format(time.RFC3339), commentaire), t)
	})
	return id, err
}

// AnnulerSilence met fin à un silence (gardé dans l'historique).
func AnnulerSilence(ctx context.Context, db *sql.DB, id int64, qui string, t time.Time) error {
	return ecrire(ctx, db, "sourdine", func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE silences SET annule_le = ? WHERE id = ? AND annule_le = 0`, ms(t), id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("silence %d inconnu ou déjà annulé", id)
		}
		return auditer(ctx, tx, qui, "silence_annule", strconv.FormatInt(id, 10), "", "", t)
	})
}

// AjouterMaintenance crée une fenêtre ponctuelle (rec == nil : debut, fin)
// ou récurrente.
func AjouterMaintenance(ctx context.Context, db *sql.DB, c string, debut, fin time.Time, rec *Recurrence,
	qui, commentaire string, t time.Time) (int64, error) {
	if _, err := LireCible(c); err != nil {
		return 0, err
	}
	if err := verifierTexte(commentaire); err != nil {
		return 0, err
	}
	recTxt := ""
	if rec != nil {
		if err := rec.Valider(); err != nil {
			return 0, err
		}
		b, _ := json.Marshal(rec)
		recTxt, debut, fin = string(b), time.Time{}, time.Time{}
	} else if !fin.After(debut) || fin.Sub(debut) > DureeSilenceMax {
		return 0, errors.New("maintenance ponctuelle : fin après le début, 90 j au plus")
	}
	var id int64
	err := ecrire(ctx, db, "sourdine", func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO maintenances(cible, debut, fin, recurrence, auteur, commentaire, cree_le)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, c, ms(debut), ms(fin), recTxt, qui, commentaire, ms(t))
		if err != nil {
			return err
		}
		if id, err = res.LastInsertId(); err != nil {
			return err
		}
		return auditer(ctx, tx, qui, "maintenance", strconv.FormatInt(id, 10), "", c+" "+recTxt+" "+commentaire, t)
	})
	return id, err
}

// AjouterInhibition : source et cible sont des sélecteurs.
func AjouterInhibition(ctx context.Context, db *sql.DB, source, cibleSel string, egales []string, qui, commentaire string, t time.Time) (int64, error) {
	if _, err := LireSelecteur(source); err != nil {
		return 0, err
	}
	if _, err := LireSelecteur(cibleSel); err != nil {
		return 0, err
	}
	if len(egales) > 16 {
		return 0, errors.New("16 étiquettes égales au plus")
	}
	if err := verifierTexte(commentaire); err != nil {
		return 0, err
	}
	if egales == nil {
		egales = []string{}
	}
	eg, _ := json.Marshal(egales)
	var id int64
	err := ecrire(ctx, db, "sourdine", func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO inhibitions(source, cible, egales, commentaire, cree_le) VALUES (?, ?, ?, ?, ?)`,
			source, cibleSel, string(eg), commentaire, ms(t))
		if err != nil {
			return err
		}
		if id, err = res.LastInsertId(); err != nil {
			return err
		}
		return auditer(ctx, tx, qui, "inhibition", strconv.FormatInt(id, 10), "", source+" → "+cibleSel+" "+string(eg), t)
	})
	return id, err
}

// AjouterDependance : les alertes de enfant sont tues quand parent est en panne.
func AjouterDependance(ctx context.Context, db *sql.DB, enfant, parent, qui, commentaire string, t time.Time) (int64, error) {
	c, err := LireCible(enfant)
	if err != nil {
		return 0, err
	}
	if parent == "" || len(parent) > 255 || c.machine == parent {
		return 0, errors.New("parent : nom de machine, différent de l'enfant")
	}
	if err := verifierTexte(commentaire); err != nil {
		return 0, err
	}
	var id int64
	err = ecrire(ctx, db, "sourdine", func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO dependances(enfant, parent, commentaire, cree_le) VALUES (?, ?, ?, ?)`,
			enfant, parent, commentaire, ms(t))
		if err != nil {
			return err
		}
		if id, err = res.LastInsertId(); err != nil {
			return err
		}
		return auditer(ctx, tx, qui, "dependance", strconv.FormatInt(id, 10), "", enfant+" dépend de "+parent, t)
	})
	return id, err
}

// SupprimerElement retire une maintenance, une inhibition ou une dépendance.
func SupprimerElement(ctx context.Context, db *sql.DB, table string, id int64, qui string, t time.Time) error {
	switch table {
	case "maintenances", "inhibitions", "dependances":
	default:
		return fmt.Errorf("table %q inconnue", table)
	}
	return ecrire(ctx, db, "sourdine", func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE id = ?`, id) // #nosec G202 -- nom de table vérifié ci-dessus
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("%d inconnu", id)
		}
		return auditer(ctx, tx, qui, table+"_supprime", strconv.FormatInt(id, 10), "", "", t)
	})
}

// Acquitter : accusé de réception d'une alerte en cours (arrête les rappels).
func Acquitter(ctx context.Context, db *sql.DB, id int64, qui, commentaire string, jusqua, t time.Time) error {
	if err := verifierTexte(commentaire); err != nil {
		return err
	}
	fin := int64(0) // jusqu'à la fin de l'alerte
	if !jusqua.IsZero() {
		if !jusqua.After(t) {
			return errors.New("fin de la prise en charge déjà passée")
		}
		fin = ms(jusqua)
	}
	return ecrire(ctx, db, "acquittements", func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE alertes SET acquittee_le = ?, acquittee_par = ?, commentaire = ?, acquittee_jusqua = ?
			WHERE id = ? AND etat = 'active'`, ms(t), qui, commentaire, fin, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("alerte %d inconnue ou pas en cours", id)
		}
		apres := commentaire
		if fin > 0 {
			apres = strings.TrimSpace("jusqu'au " + jusqua.Format("02/01 15:04") + " " + commentaire)
		}
		return auditer(ctx, tx, qui, "acquittement", strconv.FormatInt(id, 10), "", apres, t)
	})
}

// DureeAcquittement : durée par défaut d'une prise en charge (bouton des
// messages, choix proposé en premier dans l'interface), SPEC § 30.
const DureeAcquittement = 4 * time.Hour

// Reprendre : l'alerte n'est plus prise en charge (de nouveau à traiter).
func Reprendre(ctx context.Context, db *sql.DB, id int64, qui string, t time.Time) error {
	return ecrire(ctx, db, "acquittements", func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE alertes SET acquittee_le = 0, acquittee_par = '', commentaire = '', acquittee_jusqua = 0
			WHERE id = ? AND etat != 'resolue' AND acquittee_le > 0`, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("alerte %d pas prise en charge", id)
		}
		return auditer(ctx, tx, qui, "acquittement_repris", strconv.FormatInt(id, 10), "", "", t)
	})
}

// expirerAcquittements : prises en charge dont la durée est passée, levées
// (l'alerte redevient à traiter) ; renvoie leurs numéros (rappel immédiat).
func expirerAcquittements(ctx context.Context, db *sql.DB, t time.Time) ([]int64, error) {
	rows, err := db.QueryContext(ctx, `SELECT id FROM alertes WHERE etat != 'resolue' AND acquittee_le > 0
		AND acquittee_jusqua > 0 AND acquittee_jusqua <= ?`, ms(t))
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(ids) == 0 {
		return nil, err
	}
	err = ecrire(ctx, db, "acquittements", func(tx *sql.Tx) error {
		for _, id := range ids {
			if _, err := tx.ExecContext(ctx, `UPDATE alertes SET acquittee_le = 0, acquittee_par = '', commentaire = '', acquittee_jusqua = 0 WHERE id = ?`, id); err != nil {
				return err
			}
			if err := auditer(ctx, tx, "mirador", "acquittement_expire", strconv.FormatInt(id, 10), "", "", t); err != nil {
				return err
			}
		}
		return nil
	})
	return ids, err
}

// acquittees : alertes en cours acquittées (numéros).
func acquittees(ctx context.Context, db *sql.DB) (map[int64]bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT id FROM alertes WHERE etat != 'resolue' AND acquittee_le > 0`) // les expirées sont levées avant
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

const cleDefautsPoses = "notif.defauts_poses"

// PoserDefauts : inhibitions et dépendance d'Alertmanager (SPEC § 4.7),
// posées une seule fois.
func PoserDefauts(ctx context.Context, db *sql.DB, t time.Time) (bool, error) {
	var v string
	err := db.QueryRowContext(ctx, `SELECT valeur FROM reglages WHERE cle = ?`, cleDefautsPoses).Scan(&v)
	if err == nil {
		return false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if _, err := AjouterInhibition(ctx, db, `{alertname="AgentMuet"}`, `{severity=~"warning|info"}`, []string{"instance"},
		"mirador", "machine muette : inutile de signaler ses métriques manquantes", t); err != nil {
		return false, err
	}
	if _, err := AjouterInhibition(ctx, db, `{alertname="SynologyVolumeDegrade"}`, `{alertname="SynologyVolumeEnReparation"}`,
		[]string{"instance"}, "mirador", "volume dégradé : la reconstruction fait doublon", t); err != nil {
		return false, err
	}
	if _, err := AjouterDependance(ctx, db, `{via="collecteur"}`, "miradorcollector", "mirador",
		"collecteur LAN muet : une seule alerte au lieu d'une par équipement", t); err != nil {
		return false, err
	}
	_, err = db.ExecContext(ctx, `INSERT INTO reglages(cle, valeur, modifie_le, modifie_par) VALUES (?, 'true', ?, 'mirador')`,
		cleDefautsPoses, t.Unix())
	return err == nil, err
}
