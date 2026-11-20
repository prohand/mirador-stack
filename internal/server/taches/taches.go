// SPDX-License-Identifier: GPL-3.0-or-later

// Package taches : tâches planifiées suivies (SPEC § 80).
//
// Une tâche (sauvegarde, cron, timer…) doit tourner régulièrement et réussir.
// Deux façons de le savoir :
//   - l'agent le lit lui-même : timers systemd, conteneurs Docker ponctuels,
//     sauvegardes Proxmox VE / Backup Server (séries mirador_tache_*) ;
//   - la tâche le signale : un appel à /ping/<jeton> (curl) à la fin.
//
// Le serveur garde la liste des tâches suivies, calcule chaque minute
// l'échéance de chacune (passée = « en retard ») et écrit des séries que
// les règles TacheEnEchec / TacheEnRetard lisent.
package taches

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Sources.
const (
	SourcePing = "ping"
)

// Tache : une tâche suivie.
type Tache struct {
	ID             int64
	Cle            string
	Nom, Source    string
	Machine        string
	Periode, Marge time.Duration // Periode 0 : d'après le timer (systemd)
	Gravite        string        // warning ou critical
	CreeLe         time.Time
	Releve         Releve // ping : dernier signal (sinon rempli d'après les métriques)
}

// Releve : dernière exécution connue.
type Releve struct {
	Debut, Fin, Prochaine time.Time
	Code                  int
	Message               string
}

// Etat : ce que l'interface affiche.
type Etat struct {
	Classe   string // ok, erreur, attention, inconnu
	Texte    string // « OK », « Échec · code 1 », « En retard de 2 j », « Jamais vue »
	Probleme bool
	Echeance time.Time // zéro : pas de vérification du retard
}

// CleAgent : clé d'une tâche lue par l'agent.
func CleAgent(machine, source, nom string) string { return machine + "|" + source + "|" + nom }

// Echeance : date au-delà de laquelle la tâche est en retard (zéro : pas de
// limite connue). Période réglée : dernière fin (ou suivi depuis) + période
// + marge ; sinon (timer) prochaine exécution prévue + marge.
func Echeance(t Tache, r Releve) time.Time {
	switch {
	case t.Periode > 0:
		base := r.Fin
		if base.IsZero() { // jamais vue : délai compté depuis le début du suivi
			base = t.CreeLe
		}
		return base.Add(t.Periode + t.Marge)
	case !r.Prochaine.IsZero():
		return r.Prochaine.Add(t.Marge)
	}
	return time.Time{}
}

// Calculer : état d'une tâche à l'instant donné.
func Calculer(t Tache, r Releve, maintenant time.Time) Etat {
	e := Etat{Echeance: Echeance(t, r)}
	switch {
	case !e.Echeance.IsZero() && maintenant.After(e.Echeance):
		e.Classe, e.Probleme = "erreur", true
		e.Texte = "En retard de " + dureeLisible(maintenant.Sub(e.Echeance))
		if r.Fin.IsZero() {
			e.Texte = "Jamais reçue"
		}
	case r.Fin.IsZero():
		e.Classe, e.Texte = "inconnu", "Pas encore vue"
	case r.Code != 0:
		e.Classe, e.Probleme = "erreur", true
		e.Texte = "Échec · code " + strconv.Itoa(r.Code)
	default:
		e.Classe, e.Texte = "ok", "OK"
	}
	return e
}

func dureeLisible(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%d j", d/(24*time.Hour))
	case d >= time.Hour:
		return fmt.Sprintf("%d h", d/time.Hour)
	}
	return fmt.Sprintf("%d min", max(1, d/time.Minute))
}

const colonnes = `id, cle, nom, source, machine, periode, marge, gravite, cree_le, debut, fin, code, message`

func scanner(sc interface{ Scan(...any) error }) (Tache, error) {
	var t Tache
	var per, marge, cree, debut, fin int64
	err := sc.Scan(&t.ID, &t.Cle, &t.Nom, &t.Source, &t.Machine, &per, &marge, &t.Gravite, &cree, &debut, &fin, &t.Releve.Code, &t.Releve.Message)
	t.Periode, t.Marge, t.CreeLe = time.Duration(per)*time.Second, time.Duration(marge)*time.Second, time.Unix(cree, 0)
	if debut > 0 {
		t.Releve.Debut = time.Unix(debut, 0)
	}
	if fin > 0 {
		t.Releve.Fin = time.Unix(fin, 0)
	}
	return t, err
}

// Lister : tâches suivies, par nom.
func Lister(ctx context.Context, db *sql.DB) ([]Tache, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+colonnes+` FROM taches ORDER BY nom COLLATE NOCASE, id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }() // lecture seule
	var out []Tache
	for rows.Next() {
		t, err := scanner(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Verifier : bornes des réglages d'une tâche.
func Verifier(t Tache) error {
	var errs []error
	if n := len([]rune(strings.TrimSpace(t.Nom))); n == 0 || n > 100 {
		errs = append(errs, errors.New("nom : 1 à 100 caractères"))
	}
	if t.Periode != 0 && (t.Periode < time.Minute || t.Periode > 62*24*time.Hour) {
		errs = append(errs, errors.New("fréquence : entre 1 min et 62 jours"))
	}
	if t.Periode == 0 && t.Source != "systemd" {
		errs = append(errs, errors.New("fréquence attendue obligatoire (seuls les timers systemd la donnent eux-mêmes)"))
	}
	if t.Marge < 0 || t.Marge > 7*24*time.Hour {
		errs = append(errs, errors.New("marge : entre 0 et 7 jours"))
	}
	if t.Gravite != "warning" && t.Gravite != "critical" {
		errs = append(errs, errors.New("gravité : avertissement ou critique"))
	}
	return errors.Join(errs...)
}

// Suivre : commence à suivre une tâche lue par l'agent.
func Suivre(ctx context.Context, db *sql.DB, t Tache, maintenant time.Time) error {
	if t.Source == SourcePing || t.Machine == "" {
		return errors.New("tâche d'agent : machine et source attendues")
	}
	if err := Verifier(t); err != nil {
		return err
	}
	_, err := db.ExecContext(ctx, `INSERT INTO taches(cle, nom, source, machine, periode, marge, gravite, cree_le) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		CleAgent(t.Machine, t.Source, t.Nom), t.Nom, t.Source, t.Machine, int64(t.Periode/time.Second), int64(t.Marge/time.Second), t.Gravite, maintenant.Unix())
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return errors.New("tâche déjà suivie")
	}
	return err
}

// CreerPing : nouvelle tâche signalée par curl. Renvoie le jeton en clair
// (montré une seule fois ; seul son SHA-256 est gardé).
func CreerPing(ctx context.Context, db *sql.DB, t Tache, maintenant time.Time) (string, error) {
	t.Source = SourcePing
	if err := Verifier(t); err != nil {
		return "", err
	}
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	jeton := base64.RawURLEncoding.EncodeToString(b)
	_, err := db.ExecContext(ctx, `INSERT INTO taches(cle, nom, source, machine, periode, marge, gravite, jeton, cree_le)
		VALUES ('ping|' || hex(randomblob(8)), ?, 'ping', ?, ?, ?, ?, ?, ?)`,
		strings.TrimSpace(t.Nom), t.Machine, int64(t.Periode/time.Second), int64(t.Marge/time.Second), t.Gravite, empreinte(jeton), maintenant.Unix())
	return jeton, err
}

// NouveauJeton : remplace le jeton d'une tâche ping (l'ancien ne sert plus).
func NouveauJeton(ctx context.Context, db *sql.DB, id int64) (string, error) {
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	jeton := base64.RawURLEncoding.EncodeToString(b)
	r, err := db.ExecContext(ctx, `UPDATE taches SET jeton = ? WHERE id = ? AND source = 'ping'`, empreinte(jeton), id)
	if err != nil {
		return "", err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return "", errors.New("tâche introuvable")
	}
	return jeton, nil
}

// Modifier : nom (ping seulement), fréquence, marge, gravité.
func Modifier(ctx context.Context, db *sql.DB, t Tache) error {
	if err := Verifier(t); err != nil {
		return err
	}
	_, err := db.ExecContext(ctx, `UPDATE taches SET nom = CASE WHEN source = 'ping' THEN ? ELSE nom END, periode = ?, marge = ?, gravite = ? WHERE id = ?`,
		strings.TrimSpace(t.Nom), int64(t.Periode/time.Second), int64(t.Marge/time.Second), t.Gravite, t.ID)
	return err
}

// Retirer : la tâche n'est plus suivie.
func Retirer(ctx context.Context, db *sql.DB, id int64) error {
	_, err := db.ExecContext(ctx, `DELETE FROM taches WHERE id = ?`, id)
	return err
}

func empreinte(jeton string) string {
	e := sha256.Sum256([]byte(jeton))
	return hex.EncodeToString(e[:])
}

// ErrJetonInconnu : aucun ping avec ce jeton.
var ErrJetonInconnu = errors.New("jeton inconnu")

// Signaler : une tâche « ping » annonce sa fin (code 0 = réussie) ou son
// début (debut = true : seulement noté, la fin reste attendue).
func Signaler(ctx context.Context, db *sql.DB, jeton string, debut bool, code int, duree time.Duration, message string, maintenant time.Time) error {
	if len(jeton) < 16 || len(jeton) > 64 {
		return ErrJetonInconnu
	}
	if len(message) > 200 { // coupé sans casser un caractère accentué
		message = strings.ToValidUTF8(message[:200], "")
	}
	var r sql.Result
	var err error
	if debut {
		r, err = db.ExecContext(ctx, `UPDATE taches SET debut = ? WHERE jeton = ? AND source = 'ping'`, maintenant.Unix(), empreinte(jeton))
	} else {
		d := maintenant.Add(-duree).Unix()
		r, err = db.ExecContext(ctx, `UPDATE taches SET debut = CASE WHEN ? > 0 THEN ? WHEN debut > fin THEN debut ELSE ? END,
			fin = ?, code = ?, message = ? WHERE jeton = ? AND source = 'ping'`,
			int64(duree), d, maintenant.Unix(), maintenant.Unix(), code, message, empreinte(jeton))
	}
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return ErrJetonInconnu
	}
	return nil
}
