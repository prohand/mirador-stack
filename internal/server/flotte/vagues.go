// SPDX-License-Identifier: GPL-3.0-or-later

package flotte

// Déploiement d'une version par vagues (SPEC § 141, maquette validée le
// 27/11) : machine d'essai, puis un groupe, puis un site, puis tout le parc.
// Chaque vague attend que la précédente soit saine : toutes ses machines
// (hors muettes) dans la version, aucun échec, pendant 30 minutes. Un échec
// met le déploiement en pause. Une vague = la version assignée à ses
// machines (version propre) ; la dernière, « tout le parc », assigne la
// version du parc et retire les versions propres posées par les vagues.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const cleVagues = "maj.vagues"

// SanteVague : durée sans échec, vague entière à jour, avant la suivante.
const SanteVague = 30 * time.Minute

// muetteVague : une machine muette depuis plus longtemps ne bloque pas.
const muetteVague = 10 * time.Minute

// Vague : une étape du déploiement.
type Vague struct {
	Genre  string `json:"genre"`  // machine, groupe, site, tout
	Valeur string `json:"valeur"` // nom de la machine, du groupe ou du site
	Lancee int64  `json:"lancee"` // version assignée (0 : pas encore)
	Saine  int64  `json:"saine"`  // à jour sans échec depuis (0 : pas encore)
}

// Titre : « Machine d'essai : par-042 », « Groupe « test » », « Tout le parc ».
func (v Vague) Titre() string {
	switch v.Genre {
	case "machine":
		return "Machine d'essai · " + v.Valeur
	case "groupe":
		return "Groupe « " + v.Valeur + " »"
	case "site":
		return "Site " + v.Valeur
	}
	return "Tout le parc"
}

// Deploiement : version déployée par vagues.
type Deploiement struct {
	Version string  `json:"version"`
	Vagues  []Vague `json:"vagues"`
	Pause   bool    `json:"pause"`
	Raison  string  `json:"raison,omitempty"` // pourquoi en pause
	Fini    bool    `json:"fini"`
	Par     string  `json:"par"`
	Debut   int64   `json:"debut"`
}

// EtatVague : avancement d'une vague (affichage).
type EtatVague struct {
	Vague
	Total, AJour, Echecs, Muettes int
	EnCours                       bool // vague lancée et pas encore saine
	Pct                           int
}

// LireDeploiement : déploiement enregistré (nil : aucun).
func LireDeploiement(ctx context.Context, db *sql.DB) (*Deploiement, error) {
	var v string
	err := db.QueryRowContext(ctx, `SELECT valeur FROM reglages WHERE cle = ?`, cleVagues).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var d Deploiement
	if err := json.Unmarshal([]byte(v), &d); err != nil {
		return nil, err
	}
	return &d, nil
}

func ecrireDeploiement(ctx context.Context, db *sql.DB, d *Deploiement, qui, action string, t time.Time) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // sans effet après Commit
	if d == nil {
		_, err = tx.ExecContext(ctx, `DELETE FROM reglages WHERE cle = ?`, cleVagues)
	} else {
		b, _ := json.Marshal(d)
		_, err = tx.ExecContext(ctx, `INSERT INTO reglages(cle, valeur, modifie_le, modifie_par) VALUES (?, ?, ?, ?)
			ON CONFLICT(cle) DO UPDATE SET valeur = excluded.valeur, modifie_le = excluded.modifie_le, modifie_par = excluded.modifie_par`,
			cleVagues, string(b), t.Unix(), qui)
	}
	if err != nil {
		return err
	}
	if action != "" {
		version := ""
		if d != nil {
			version = d.Version
		}
		if err := audit(ctx, tx, t.Unix(), qui, action, "vagues", "", version, ""); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// LancerDeploiement : nouvelle version par vagues (la première est lancée
// tout de suite). vagues : dans l'ordre ; « tout le parc » est ajouté à la
// fin s'il manque.
func LancerDeploiement(ctx context.Context, db *sql.DB, version string, vagues []Vague, qui string, t time.Time) error {
	if !formatVersionPubliee.MatchString(version) {
		return fmt.Errorf("version %q invalide", version)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM versions_binaires WHERE version = ?`, version).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("version %s non publiée", version)
	}
	var garde []Vague
	for _, v := range vagues {
		v.Valeur = strings.TrimSpace(v.Valeur)
		switch {
		case v.Genre == "tout":
		case (v.Genre == "machine" || v.Genre == "groupe" || v.Genre == "site") && v.Valeur != "" && len(v.Valeur) <= 100:
		default:
			continue
		}
		v.Lancee, v.Saine = 0, 0
		if v.Genre != "tout" {
			garde = append(garde, v)
		}
	}
	garde = append(garde, Vague{Genre: "tout"})
	d := &Deploiement{Version: version, Vagues: garde, Par: qui, Debut: t.Unix()}
	if err := lancerVague(ctx, db, d, 0, qui, t); err != nil {
		return err
	}
	return ecrireDeploiement(ctx, db, d, qui, "vagues_lancees", t)
}

// membre : la machine fait-elle partie de la vague ?
func membre(v Vague, m InfoMachine) bool {
	switch v.Genre {
	case "machine":
		return m.Nom == v.Valeur
	case "groupe":
		return strings.EqualFold(m.Groupe, v.Valeur)
	case "site":
		return strings.EqualFold(m.Site, v.Valeur)
	}
	return true
}

// concernee : machine approuvée qui reçoit des mises à jour (agent ou
// collecteur ; pas les équipements sans version).
func concernee(m InfoMachine) bool { return m.Statut == "approuvee" && m.Version != "" }

func lancerVague(ctx context.Context, db *sql.DB, d *Deploiement, i int, qui string, t time.Time) error {
	v := &d.Vagues[i]
	if v.Genre == "tout" {
		if err := DefinirCible(ctx, db, 0, d.Version, qui, t); err != nil {
			return err
		}
		// Versions propres posées par les vagues : retirées (les machines
		// suivent de nouveau la version du parc).
		if _, err := db.ExecContext(ctx, `UPDATE machines SET version_cible = '' WHERE version_cible = ?`, d.Version); err != nil {
			return err
		}
	} else {
		ms, err := ListerMachines(ctx, db)
		if err != nil {
			return err
		}
		for _, m := range ms {
			if concernee(m) && membre(*v, m) && m.Version != d.Version {
				if err := DefinirCible(ctx, db, m.ID, d.Version, qui, t); err != nil {
					return err
				}
			}
		}
	}
	v.Lancee, v.Saine = t.Unix(), 0
	return nil
}

// EtatsVagues : avancement de chaque vague.
func EtatsVagues(d *Deploiement, ms []InfoMachine, t time.Time) []EtatVague {
	out := make([]EtatVague, len(d.Vagues))
	for i, v := range d.Vagues {
		e := EtatVague{Vague: v}
		for _, m := range ms {
			if !concernee(m) || !membre(v, m) {
				continue
			}
			e.Total++
			switch {
			case m.Version == d.Version:
				e.AJour++
			case m.MajEchec != "":
				e.Echecs++
			case t.Sub(time.Unix(m.BattementLe, 0)) > muetteVague:
				e.Muettes++
			}
		}
		if e.Total > 0 {
			e.Pct = e.AJour * 100 / e.Total
		}
		e.EnCours = v.Lancee > 0 && v.Saine == 0
		out[i] = e
	}
	return out
}

// AvancerDeploiement : appelé chaque minute. Met en pause sur un échec,
// note la santé de la vague en cours, lance la suivante après SanteVague.
// Renvoie un message à journaliser ("" : rien de nouveau).
func AvancerDeploiement(ctx context.Context, db *sql.DB, t time.Time) (string, error) {
	d, err := LireDeploiement(ctx, db)
	if err != nil || d == nil || d.Pause || d.Fini {
		return "", err
	}
	ms, err := ListerMachines(ctx, db)
	if err != nil {
		return "", err
	}
	etats := EtatsVagues(d, ms, t)
	cour := -1
	for i, v := range d.Vagues {
		if v.Lancee > 0 {
			cour = i
		}
	}
	if cour < 0 {
		return "", nil
	}
	e := etats[cour]
	switch {
	case e.Echecs > 0:
		d.Pause, d.Raison = true, fmt.Sprintf("%d %s dans la vague « %s »", e.Echecs, map[bool]string{true: "échec", false: "échecs"}[e.Echecs == 1], e.Titre())
		return "déploiement " + d.Version + " en pause : " + d.Raison, ecrireDeploiement(ctx, db, d, "mirador", "vagues_pause", t)
	case e.AJour+e.Muettes < e.Total:
		return "", nil // pas encore tout à jour
	case d.Vagues[cour].Saine == 0:
		d.Vagues[cour].Saine = t.Unix()
		return "", ecrireDeploiement(ctx, db, d, "mirador", "", t)
	case t.Sub(time.Unix(d.Vagues[cour].Saine, 0)) < SanteVague:
		return "", nil
	case cour == len(d.Vagues)-1:
		d.Fini = true
		return "déploiement " + d.Version + " terminé", ecrireDeploiement(ctx, db, d, "mirador", "vagues_fini", t)
	}
	if err := lancerVague(ctx, db, d, cour+1, "mirador", t); err != nil {
		return "", err
	}
	return "déploiement " + d.Version + " : vague « " + d.Vagues[cour+1].Titre() + " » lancée", ecrireDeploiement(ctx, db, d, "mirador", "vagues_suivante", t)
}

// PauseDeploiement : pause (vrai) ou reprise (faux) par l'administrateur.
func PauseDeploiement(ctx context.Context, db *sql.DB, pause bool, qui string, t time.Time) error {
	d, err := LireDeploiement(ctx, db)
	if err != nil {
		return err
	}
	if d == nil || d.Fini {
		return errors.New("aucun déploiement en cours")
	}
	d.Pause, d.Raison = pause, ""
	action := "vagues_reprise"
	if pause {
		action, d.Raison = "vagues_pause", "mis en pause par "+qui
	}
	return ecrireDeploiement(ctx, db, d, qui, action, t)
}

// ArreterDeploiement : oublie le déploiement (les versions déjà assignées
// restent : la page des mises à jour permet de les changer).
func ArreterDeploiement(ctx context.Context, db *sql.DB, qui string, t time.Time) error {
	return ecrireDeploiement(ctx, db, nil, qui, "vagues_arret", t)
}
