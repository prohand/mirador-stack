// SPDX-License-Identifier: GPL-3.0-or-later

package flotte

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"mirador-stack/internal/server/certsyslog"
	"mirador-stack/internal/server/secrets"
)

// Collecteurs (SPEC § 23.13) : une machine de type « collecteur »
// (mirador-collector, sur le LAN) relève des équipements sans agent
// (Synology, UniFi, imprimante…). Chaque équipement est rattaché à un seul
// collecteur ; le serveur refuse les données d'un équipement non rattaché.

// Equipement : un équipement rattaché à un collecteur.
type Equipement struct {
	ID                     int64
	Nom, Site, Env, Groupe string
	Statut                 string
	VuLe                   int64 // dernière donnée reçue pour lui (0 : jamais)
}

// typeDe : type d'une machine (agent, collecteur).
func typeDe(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id int64) (nom, typ string, err error) {
	err = q.QueryRowContext(ctx, `SELECT nom, type FROM machines WHERE id = ?`, id).Scan(&nom, &typ)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", errors.New("machine inconnue")
	}
	return nom, typ, err
}

// RattacherEquipement : l'équipement est relevé par ce collecteur (déplacé
// s'il l'était par un autre). Audit dans la même transaction.
func RattacherEquipement(ctx context.Context, db *sql.DB, collecteur, equipement int64, qui string, maintenant time.Time) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // sans effet après Commit
	nomC, typC, err := typeDe(ctx, tx, collecteur)
	if err != nil {
		return err
	}
	if typC != "collecteur" {
		return fmt.Errorf("%s n'est pas un collecteur", nomC)
	}
	nomE, typE, err := typeDe(ctx, tx, equipement)
	if err != nil {
		return err
	}
	if typE == "collecteur" || equipement == collecteur {
		return errors.New("un collecteur ne peut pas être rattaché à un collecteur")
	}
	var avant string
	_ = tx.QueryRowContext(ctx, `SELECT m.nom FROM equipements_collecteur e JOIN machines m ON m.id = e.collecteur_id WHERE e.machine_id = ?`,
		equipement).Scan(&avant)
	if _, err := tx.ExecContext(ctx, `INSERT INTO equipements_collecteur(machine_id, collecteur_id, rattache_le, rattache_par) VALUES (?, ?, ?, ?)
		ON CONFLICT(machine_id) DO UPDATE SET collecteur_id = excluded.collecteur_id, rattache_le = excluded.rattache_le, rattache_par = excluded.rattache_par`,
		equipement, collecteur, maintenant.Unix(), qui); err != nil {
		return err
	}
	if err := audit(ctx, tx, maintenant.Unix(), qui, "equipement_rattache", nomE, avant, nomC, ""); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	SignalerConfig() // le collecteur reçoit sa nouvelle liste tout de suite
	return nil
}

// DetacherEquipement : l'équipement n'est plus relevé par aucun collecteur.
func DetacherEquipement(ctx context.Context, db *sql.DB, equipement int64, qui string, maintenant time.Time) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // sans effet après Commit
	nomE, _, err := typeDe(ctx, tx, equipement)
	if err != nil {
		return err
	}
	var avant string
	if err := tx.QueryRowContext(ctx, `SELECT m.nom FROM equipements_collecteur e JOIN machines m ON m.id = e.collecteur_id WHERE e.machine_id = ?`,
		equipement).Scan(&avant); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%s n'est rattaché à aucun collecteur", nomE)
		}
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM equipements_collecteur WHERE machine_id = ?`, equipement); err != nil {
		return err
	}
	if err := audit(ctx, tx, maintenant.Unix(), qui, "equipement_detache", nomE, avant, "", ""); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	SignalerConfig()
	return nil
}

// EquipementsDe : équipements rattachés à un collecteur, par nom.
func EquipementsDe(ctx context.Context, db *sql.DB, collecteur int64) ([]Equipement, error) {
	rows, err := db.QueryContext(ctx, `SELECT m.id, m.nom, m.site, m.env, m.groupe, m.statut, m.vu_le FROM equipements_collecteur e
		JOIN machines m ON m.id = e.machine_id WHERE e.collecteur_id = ? ORDER BY m.nom`, collecteur)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Equipement
	for rows.Next() {
		var q Equipement
		if err := rows.Scan(&q.ID, &q.Nom, &q.Site, &q.Env, &q.Groupe, &q.Statut, &q.VuLe); err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

// CollecteurDe : collecteur qui relève un équipement (0 : aucun).
func CollecteurDe(ctx context.Context, db *sql.DB, equipement int64) (int64, string, error) {
	var id int64
	var nom string
	err := db.QueryRowContext(ctx, `SELECT m.id, m.nom FROM equipements_collecteur e JOIN machines m ON m.id = e.collecteur_id WHERE e.machine_id = ?`,
		equipement).Scan(&id, &nom)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", nil
	}
	return id, nom, err
}

// configCollecteur : clé « collecteur » de la configuration d'un collecteur :
// réglages faits à la main (s'il y en a) + liste des équipements posée ici.
func configCollecteur(ctx context.Context, db *sql.DB, collecteur int64, saisie json.RawMessage) (json.RawMessage, error) {
	m := map[string]json.RawMessage{}
	if len(saisie) > 0 {
		if err := json.Unmarshal(saisie, &m); err != nil {
			return nil, fmt.Errorf("réglage « collecteur » : %w", err)
		}
	}
	eqs, err := EquipementsDe(ctx, db, collecteur)
	if err != nil {
		return nil, err
	}
	noms := make([]string, 0, len(eqs))
	for _, q := range eqs {
		if q.Statut == "approuvee" {
			noms = append(noms, q.Nom)
		}
	}
	// Syslog réglé : certificat obtenu par le serveur (ACME), posé ici,
	// jamais saisi à la main.
	if sl, ok := m["syslog"]; ok {
		var sys map[string]json.RawMessage
		if err := json.Unmarshal(sl, &sys); err != nil {
			return nil, fmt.Errorf("réglage « syslog » : %w", err)
		}
		delete(sys, "certificat")
		// Émetteurs d'équipements détachés : retirés (le collecteur refuserait
		// sinon toute la configuration).
		if brut, ok := sys["emetteurs"]; ok {
			var ems []map[string]string
			if json.Unmarshal(brut, &ems) == nil {
				rattache := map[string]bool{}
				for _, n := range noms {
					rattache[n] = true
				}
				garde := ems[:0]
				for _, e := range ems {
					if rattache[e["machine"]] {
						garde = append(garde, e)
					}
				}
				sys["emetteurs"], _ = json.Marshal(garde)
			}
		}
		if e, err := certsyslog.LireEtat(ctx, db, collecteur); err == nil && e.Certificat != "" {
			var nom string
			_ = json.Unmarshal(sys["nom_certificat"], &nom)
			if e.Nom == nom {
				brut, _ := json.Marshal(e.Certificat)
				sys["certificat"] = brut
			}
		}
		if m["syslog"], err = json.Marshal(sys); err != nil {
			return nil, err
		}
	}
	// Contrôleur UniFi d'un équipement détaché : retiré.
	if brut, ok := m["unifi"]; ok {
		var u map[string]any
		if json.Unmarshal(brut, &u) == nil {
			if n, _ := u["machine"].(string); !contient(noms, n) {
				delete(m, "unifi")
			}
		}
	}
	// Cibles SNMP et sondes d'équipements détachés : retirées.
	rattache := map[string]bool{}
	for _, n := range noms {
		rattache[n] = true
	}
	for _, cle := range []string{"snmp", "sondes"} {
		if brut, ok := m[cle]; ok {
			var liste []map[string]any
			if json.Unmarshal(brut, &liste) == nil {
				garde := liste[:0]
				for _, e := range liste {
					if n, _ := e["machine"].(string); rattache[n] {
						garde = append(garde, e)
					}
				}
				if len(garde) == 0 {
					delete(m, cle)
				} else if m[cle], err = json.Marshal(garde); err != nil {
					return nil, err
				}
			}
		}
	}
	delete(m, "equipements")
	if len(noms) > 0 {
		brut, err := json.Marshal(noms)
		if err != nil {
			return nil, err
		}
		m["equipements"] = brut
	}
	return json.Marshal(m)
}

// CreerEquipement : équipement relevé par un collecteur (pas de token : il
// n'envoie rien lui-même), approuvé tout de suite et rattaché. Refusé pour
// un nom existant.
func CreerEquipement(ctx context.Context, db *sql.DB, nom string, collecteur int64, qui string, maintenant time.Time) error {
	if !FormatNom.MatchString(nom) {
		return errors.New("nom de machine invalide")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // sans effet après Commit
	if err := nomLibre(ctx, tx, nom, 0); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO machines(nom, statut, cree_le) VALUES (?, 'approuvee', ?)`, nom, maintenant.Unix()); err != nil {
		return err
	}
	if err := audit(ctx, tx, maintenant.Unix(), qui, "equipement_cree", nom, "", "", ""); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	id, err := IDMachine(ctx, db, nom)
	if err != nil {
		return err
	}
	return RattacherEquipement(ctx, db, collecteur, id, qui, maintenant)
}

// Collecteurs : noms des collecteurs approuvés.
func Collecteurs(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT nom FROM machines WHERE type = 'collecteur' AND statut = 'approuvee' ORDER BY nom`)
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

// UsageSecretSNMP : usage du chiffrement des mots de passe SNMP.
const UsageSecretSNMP = "collecteur.snmp" // #nosec G101 -- nom d'usage du chiffrement, pas un secret

// champsSecretsSNMP : champs chiffrés d'une cible SNMP.
var champsSecretsSNMP = []string{"mot_de_passe", "cle_chiffrement"}

// ChiffrerSecretSNMP : mot de passe SNMP chiffré pour la base.
func ChiffrerSecretSNMP(cle []byte, clair string) (string, error) {
	return secrets.Chiffrer(cle, UsageSecretSNMP, clair)
}

// dechiffrerSecretsCollecteur : document envoyé au collecteur, avec les mots
// de passe SNMP en clair (HTTPS). Un secret illisible fait échouer l'envoi
// (le collecteur garde sa configuration).
func dechiffrerSecretsCollecteur(doc []byte, cle []byte) ([]byte, error) {
	var d map[string]json.RawMessage
	if err := json.Unmarshal(doc, &d); err != nil {
		return nil, err
	}
	brutC, ok := d["collecteur"]
	if !ok {
		return doc, nil
	}
	var c map[string]json.RawMessage
	if err := json.Unmarshal(brutC, &c); err != nil {
		return nil, err
	}
	dechiffrer := func(obj map[string]any, champs []string) error {
		for _, champ := range champs {
			v, _ := obj[champ].(string)
			if secrets.EstChiffre(v) {
				clair, err := secrets.Dechiffrer(cle, UsageSecretSNMP, v)
				if err != nil {
					return fmt.Errorf("mot de passe d'un relevé illisible : %w", err)
				}
				obj[champ] = clair
			}
		}
		return nil
	}
	var err error
	if brutS, ok := c["snmp"]; ok {
		var cibles []map[string]any
		if err := json.Unmarshal(brutS, &cibles); err != nil {
			return nil, err
		}
		for _, cible := range cibles {
			if err := dechiffrer(cible, champsSecretsSNMP); err != nil {
				return nil, err
			}
		}
		if c["snmp"], err = json.Marshal(cibles); err != nil {
			return nil, err
		}
	}
	if brutU, ok := c["unifi"]; ok {
		var u map[string]any
		if err := json.Unmarshal(brutU, &u); err != nil {
			return nil, err
		}
		if err := dechiffrer(u, []string{"mot_de_passe", "cle_api"}); err != nil {
			return nil, err
		}
		if c["unifi"], err = json.Marshal(u); err != nil {
			return nil, err
		}
	}
	if d["collecteur"], err = json.Marshal(c); err != nil {
		return nil, err
	}
	return json.Marshal(d)
}

func contient(l []string, v string) bool {
	for _, x := range l {
		if x == v {
			return true
		}
	}
	return false
}

// RelevesParCollecteur : pour chaque équipement rattaché, le nom de son
// collecteur (une seule requête, pour la liste des machines).
func RelevesParCollecteur(ctx context.Context, db *sql.DB) (map[string]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT m.nom, c.nom FROM equipements_collecteur e
		JOIN machines m ON m.id = e.machine_id JOIN machines c ON c.id = e.collecteur_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var eq, col string
		if err := rows.Scan(&eq, &col); err != nil {
			return nil, err
		}
		out[eq] = col
	}
	return out, rows.Err()
}
