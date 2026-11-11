// SPDX-License-Identifier: GPL-3.0-or-later

package sauvegarde

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"mirador-stack/internal/server/secrets"
)

// --- Réglages (modifiables dans l'interface ; commande en attendant) ------------

const cleReglages = "sauvegarde"

// Reglages des sauvegardes.
type Reglages struct {
	Actif     bool   `json:"actif"`     // sauvegarde planifiée chaque jour
	Heure     string `json:"heure"`     // HH:MM, heure UTC
	Conserver int    `json:"conserver"` // nombre d'archives gardées dans le dossier local
	Metriques bool   `json:"metriques"` // inclure les métriques (volumineux)
	Logs      bool   `json:"logs"`      // inclure les logs (volumineux)
	SFTP      *SFTP  `json:"sftp,omitempty"`
	S3        *S3    `json:"s3,omitempty"`
}

// ReglagesDefaut : chaque nuit, configuration seule, 14 archives gardées.
func ReglagesDefaut() Reglages {
	return Reglages{Actif: true, Heure: "02:43", Conserver: 14}
}

var reHeure = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)

// Valider vérifie les réglages.
func (r Reglages) Valider() error {
	var errs []error
	if !reHeure.MatchString(r.Heure) {
		errs = append(errs, fmt.Errorf("heure %q invalide (HH:MM, UTC)", r.Heure))
	}
	if r.Conserver < 1 || r.Conserver > 365 {
		errs = append(errs, errors.New("conserver : entre 1 et 365 archives"))
	}
	if r.SFTP != nil {
		errs = append(errs, r.SFTP.valider())
	}
	if r.S3 != nil {
		errs = append(errs, r.S3.valider())
	}
	return errors.Join(errs...)
}

// LireReglages : réglages en base, ou les défauts.
func LireReglages(ctx context.Context, db *sql.DB) (Reglages, error) {
	r := ReglagesDefaut()
	var v string
	err := db.QueryRowContext(ctx, `SELECT valeur FROM reglages WHERE cle = ?`, cleReglages).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return r, nil
	}
	if err != nil {
		return r, err
	}
	dec := json.NewDecoder(strings.NewReader(v))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return r, fmt.Errorf("réglages de sauvegarde abîmés en base : %w", err)
	}
	return r, nil
}

// EcrireReglages enregistre des réglages déjà vérifiés et dont les secrets
// sont chiffrés (voir ChiffrerSecrets), avec une ligne d'audit sans secret.
func EcrireReglages(ctx context.Context, db *sql.DB, r Reglages, qui string, maintenant time.Time) error {
	if err := r.Valider(); err != nil {
		return err
	}
	brut, err := json.Marshal(r)
	if err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // sans effet après Commit
	if _, err := tx.ExecContext(ctx, `INSERT INTO reglages(cle, valeur, modifie_le, modifie_par) VALUES (?, ?, ?, ?)
		ON CONFLICT(cle) DO UPDATE SET valeur = excluded.valeur, modifie_le = excluded.modifie_le, modifie_par = excluded.modifie_par`,
		cleReglages, string(brut), maintenant.Unix(), qui); err != nil {
		return err
	}
	masque, _ := json.Marshal(r.Masques())
	if _, err := tx.ExecContext(ctx, `INSERT INTO audit(le, qui, action, objet, apres) VALUES (?, ?, 'reglages_sauvegarde', 'sauvegarde', ?)`,
		maintenant.Unix(), qui, string(masque)); err != nil {
		return err
	}
	return tx.Commit()
}

// Masques : copie sans aucun secret (affichage, audit).
func (r Reglages) Masques() Reglages {
	m := r
	if r.SFTP != nil {
		s := *r.SFTP
		if s.CleSSH != "" {
			s.CleSSH = "***"
		}
		m.SFTP = &s
	}
	if r.S3 != nil {
		s := *r.S3
		if s.Secret != "" {
			s.Secret = "***"
		}
		m.S3 = &s
	}
	return m
}

// --- Historique (sert aussi à l'alerte) --------------------------------------------

// Execution : une ligne de la table sauvegardes.
type Execution struct {
	ID                                     int64
	DemandeeLe, Debut, Fin, Taille         int64
	Statut, Origine, Fichier, Destinations string
	Erreur                                 string
	Metriques, Logs                        bool
}

// Demander dépose une demande de sauvegarde ; le serveur l'exécute.
func Demander(ctx context.Context, db *sql.DB, origine string, metriques, logs bool, maintenant time.Time) (int64, error) {
	res, err := db.ExecContext(ctx, `INSERT INTO sauvegardes(demandee_le, origine, metriques, logs) VALUES (?, ?, ?, ?)`,
		maintenant.Unix(), origine, metriques, logs)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

const colonnes = `id, demandee_le, debut, fin, taille, statut, origine, fichier, destinations, erreur, metriques, logs`

func scanner(s interface{ Scan(...any) error }) (Execution, error) {
	var e Execution
	err := s.Scan(&e.ID, &e.DemandeeLe, &e.Debut, &e.Fin, &e.Taille, &e.Statut, &e.Origine, &e.Fichier,
		&e.Destinations, &e.Erreur, &e.Metriques, &e.Logs)
	return e, err
}

// Etat d'une sauvegarde.
func Etat(ctx context.Context, db *sql.DB, id int64) (Execution, error) {
	return scanner(db.QueryRowContext(ctx, `SELECT `+colonnes+` FROM sauvegardes WHERE id = ?`, id))
}

// Historique : les n dernières sauvegardes, la plus récente d'abord.
func Historique(ctx context.Context, db *sql.DB, n int) ([]Execution, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+colonnes+` FROM sauvegardes ORDER BY id DESC LIMIT ?`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Execution
	for rows.Next() {
		e, err := scanner(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// DerniereReussie : la plus récente sauvegarde réussie (ok = false s'il n'y
// en a aucune). Sert à l'alerte « sauvegarde trop ancienne » (étape 4).
func DerniereReussie(ctx context.Context, db *sql.DB) (Execution, bool, error) {
	e, err := scanner(db.QueryRowContext(ctx, `SELECT `+colonnes+` FROM sauvegardes WHERE statut = 'ok' ORDER BY fin DESC LIMIT 1`))
	if errors.Is(err, sql.ErrNoRows) {
		return e, false, nil
	}
	return e, err == nil, err
}

// --- Planificateur (dans le serveur) -------------------------------------------------

// Planificateur exécute les demandes et la sauvegarde quotidienne.
type Planificateur struct {
	Sources    Sources
	Cle        []byte
	Version    string
	Journal    *slog.Logger
	Maintenant func() time.Time

	mu sync.Mutex // une sauvegarde à la fois
}

// RythmePlanificateur : intervalle entre deux tours (modifiable dans les tests).
var RythmePlanificateur = 15 * time.Second

// Boucle : un tour toutes les 15 s jusqu'à l'arrêt. Au démarrage, une
// sauvegarde restée « en cours » (arrêt brutal) est notée en échec.
func (p *Planificateur) Boucle(ctx context.Context) {
	if p.Maintenant == nil {
		p.Maintenant = time.Now
	}
	if _, err := p.Sources.DB.ExecContext(ctx, `UPDATE sauvegardes SET statut = 'echec', fin = ?, erreur = 'interrompue (arrêt du serveur)'
		WHERE statut = 'en_cours'`, p.Maintenant().Unix()); err != nil {
		p.Journal.Error("sauvegardes interrompues", "erreur", err)
	}
	t := time.NewTicker(RythmePlanificateur)
	defer t.Stop()
	for {
		if err := p.Tour(ctx); err != nil {
			p.Journal.Error("sauvegarde", "erreur", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Tour : ajoute la sauvegarde quotidienne si elle est due, puis exécute la
// plus ancienne demande en attente.
func (p *Planificateur) Tour(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	db := p.Sources.DB
	r, err := LireReglages(ctx, db)
	if err != nil {
		return err
	}
	maintenant := p.Maintenant().UTC()
	if r.Actif {
		h, _ := time.Parse("15:04", r.Heure)
		prevue := time.Date(maintenant.Year(), maintenant.Month(), maintenant.Day(), h.Hour(), h.Minute(), 0, 0, time.UTC)
		if !maintenant.Before(prevue) {
			var n int
			if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sauvegardes WHERE origine = 'planifiee' AND demandee_le >= ?`,
				prevue.Unix()).Scan(&n); err != nil {
				return err
			}
			if n == 0 {
				if _, err := Demander(ctx, db, "planifiee", r.Metriques, r.Logs, maintenant); err != nil {
					return err
				}
			}
		}
	}
	e, err := scanner(db.QueryRowContext(ctx, `SELECT `+colonnes+` FROM sauvegardes WHERE statut = 'demandee' ORDER BY id LIMIT 1`))
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `UPDATE sauvegardes SET statut = 'en_cours', debut = ? WHERE id = ?`, maintenant.Unix(), e.ID); err != nil {
		return err
	}
	chemin, taille, destinations, errS := p.executer(ctx, e, r)
	statut, msg := "ok", ""
	if errS != nil {
		statut, msg = "echec", tronquer(errS.Error(), 500)
		p.Journal.Error("sauvegarde en échec", "id", e.ID, "erreur", errS)
	} else {
		p.Journal.Info("sauvegarde faite", "id", e.ID, "fichier", chemin, "taille", FormatTaille(taille), "destinations", destinations)
	}
	_, err = db.ExecContext(ctx, `UPDATE sauvegardes SET statut = ?, fin = ?, fichier = ?, taille = ?, destinations = ?, erreur = ? WHERE id = ?`,
		statut, p.Maintenant().Unix(), chemin, taille, destinations, msg, e.ID)
	return err
}

// executer : archive locale, copies vers les destinations distantes, rétention.
// Une copie distante en échec fait échouer la sauvegarde (l'alerte le dira),
// mais l'archive locale est gardée.
func (p *Planificateur) executer(ctx context.Context, e Execution, r Reglages) (string, int64, string, error) {
	chemin, man, err := Sauvegarder(ctx, p.Sources, Options{Cle: p.Cle, Metriques: e.Metriques, Logs: e.Logs,
		Version: p.Version, Maintenant: p.Maintenant})
	if err != nil {
		return "", 0, "", err
	}
	_ = man
	var taille int64
	if a, err := Lister(DossierArchives(p.Sources.Dossier)); err == nil {
		for _, x := range a {
			if x.Chemin == chemin {
				taille = x.Taille
			}
		}
	}
	destinations := []string{"locale"}
	var errs []error
	for _, d := range p.distantes(r) {
		if err := d.Envoyer(ctx, chemin, r.Conserver); err != nil {
			errs = append(errs, fmt.Errorf("%s : %w", d.Nom(), err))
			continue
		}
		destinations = append(destinations, d.Nom())
	}
	if _, err := Retention(DossierArchives(p.Sources.Dossier), r.Conserver); err != nil {
		errs = append(errs, fmt.Errorf("rétention locale : %w", err))
	}
	return chemin, taille, strings.Join(destinations, ", "), errors.Join(errs...)
}

// distantes : destinations configurées (SFTP, S3), secrets déchiffrés ici.
func (p *Planificateur) distantes(r Reglages) []Destination {
	var out []Destination
	if r.SFTP != nil {
		out = append(out, &destinationSFTP{r: *r.SFTP, cle: p.Cle})
	}
	if r.S3 != nil {
		out = append(out, &destinationS3{r: *r.S3, cle: p.Cle})
	}
	return out
}

// Destination distante d'une archive.
type Destination interface {
	Nom() string
	Envoyer(ctx context.Context, chemin string, conserver int) error
}

func tronquer(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// PreparerSecrets chiffre les secrets donnés en clair dans de nouveaux
// réglages. « *** » ou vide = garder la valeur déjà enregistrée (on peut donc
// renvoyer les réglages affichés, masqués, sans retaper les secrets).
func PreparerSecrets(nouveaux, anciens Reglages, cle []byte) (Reglages, error) {
	r := nouveaux
	garder := func(v, ancien string) (string, bool) {
		if v == "" || v == "***" {
			return ancien, true
		}
		return v, secrets.EstChiffre(v)
	}
	if r.SFTP != nil {
		s := *r.SFTP
		ancien := ""
		if anciens.SFTP != nil {
			ancien = anciens.SFTP.CleSSH
		}
		v, deja := garder(s.CleSSH, ancien)
		if !deja {
			if _, err := ssh.ParsePrivateKey([]byte(v)); err != nil {
				return r, errors.New("sftp.cle_ssh : clé privée OpenSSH sans phrase de passe attendue")
			}
			var err error
			if v, err = secrets.Chiffrer(cle, UsageCleSSH, v); err != nil {
				return r, err
			}
		}
		s.CleSSH = v
		r.SFTP = &s
	}
	if r.S3 != nil {
		s := *r.S3
		ancien := ""
		if anciens.S3 != nil {
			ancien = anciens.S3.Secret
		}
		v, deja := garder(s.Secret, ancien)
		if !deja {
			var err error
			if v, err = secrets.Chiffrer(cle, UsageSecretS3, v); err != nil {
				return r, err
			}
		}
		s.Secret = v
		r.S3 = &s
	}
	return r, nil
}
