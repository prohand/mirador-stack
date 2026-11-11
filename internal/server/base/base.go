// SPDX-License-Identifier: GPL-3.0-or-later

// Package base : base SQLite de mirador-server (configuration, machines,
// tokens, audit…). Les métriques et les logs ont leurs propres moteurs
// (SPEC.md § 16.1).
//
// Choix (SPEC.md § 7.2) : modernc.org/sqlite (pur Go, pas de cgo), mode WAL,
// écritures groupées en transactions, une seule connexion d'écriture.
//
// Migrations : fichiers SQL numérotés, embarqués dans le binaire, appliqués
// au démarrage dans une transaction chacun. Avant toute migration d'une base
// existante, une copie cohérente est faite (VACUUM INTO) : revenir en arrière
// = reprendre cette copie (SPEC.md, « Compatibilité et évolution »).
package base

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pilote SQLite pur Go
)

//go:embed migrations/*.sql
var fichiersMigrations embed.FS

// Ouvrir ouvre (ou crée) la base avec les réglages de sécurité et de
// performance : WAL, clés étrangères, attente en cas de verrou.
func Ouvrir(chemin string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(chemin), 0o750); err != nil {
		return nil, err
	}
	// temp_store(2) : tables temporaires en mémoire. L'image Docker est en
	// lecture seule et sans /tmp : SQLite ne doit jamais y écrire (VACUUM
	// INTO des sauvegardes, tris). La base est petite : coût négligeable.
	dsn := "file:" + chemin + "?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)" +
		"&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_pragma=temp_store(2)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// SQLite n'écrit qu'une transaction à la fois : une connexion suffit et
	// évite les conflits de verrou.
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		return nil, errors.Join(err, db.Close())
	}
	if err := os.Chmod(chemin, 0o600); err != nil {
		return nil, errors.Join(err, db.Close())
	}
	return db, nil
}

// migration : un fichier NNNN_nom.sql.
type migration struct {
	version int
	nom     string
	sql     string
}

func lireMigrations() ([]migration, error) {
	entrees, err := fs.ReadDir(fichiersMigrations, "migrations")
	if err != nil {
		return nil, err
	}
	var out []migration
	for _, e := range entrees {
		num, _, ok := strings.Cut(e.Name(), "_")
		v, err := strconv.Atoi(num)
		if !ok || err != nil || v <= 0 {
			return nil, fmt.Errorf("nom de migration invalide %q (NNNN_nom.sql)", e.Name())
		}
		contenu, err := fichiersMigrations.ReadFile("migrations/" + e.Name())
		if err != nil {
			return nil, err
		}
		out = append(out, migration{version: v, nom: e.Name(), sql: string(contenu)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	for i, m := range out {
		if m.version != i+1 {
			return nil, fmt.Errorf("migrations non consécutives : %s", m.nom)
		}
	}
	return out, nil
}

// VersionAttendue : version du schéma de ce binaire (nombre de migrations).
func VersionAttendue() int {
	migs, err := lireMigrations()
	if err != nil {
		return -1
	}
	return len(migs)
}

// Version renvoie la version du schéma (0 = base vide).
func Version(ctx context.Context, db *sql.DB) (int, error) {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_version (
		version INTEGER PRIMARY KEY, nom TEXT NOT NULL, appliquee_le INTEGER NOT NULL)`); err != nil {
		return 0, err
	}
	var v sql.NullInt64
	err := db.QueryRowContext(ctx, `SELECT max(version) FROM schema_version`).Scan(&v)
	return int(v.Int64), err
}

// Migrer applique les migrations manquantes. Si la base contient déjà des
// données, une copie est faite avant dans dossierSauvegardes. Renvoie le
// chemin de la copie (vide si aucune n'a été nécessaire).
func Migrer(ctx context.Context, db *sql.DB, dossierSauvegardes string) (string, error) {
	return migrerJusqua(ctx, db, dossierSauvegardes, 0)
}

// migrerJusqua : Migrer, en s'arrêtant à la version cible (0 = dernière).
// La cible sert aux tests (fabriquer une vraie base ancienne).
func migrerJusqua(ctx context.Context, db *sql.DB, dossierSauvegardes string, cible int) (string, error) {
	migs, err := lireMigrations()
	if err != nil {
		return "", err
	}
	if cible > 0 && cible < len(migs) {
		migs = migs[:cible]
	}
	actuelle, err := Version(ctx, db)
	if err != nil {
		return "", err
	}
	if actuelle > len(migs) {
		return "", fmt.Errorf("base en version %d, plus récente que ce binaire (%d) : mettez le serveur à jour", actuelle, len(migs))
	}
	if actuelle == len(migs) {
		return "", nil
	}
	copie := ""
	if actuelle > 0 {
		if copie, err = Copier(ctx, db, dossierSauvegardes,
			fmt.Sprintf("avant-migration-v%d-%s.db", actuelle, time.Now().UTC().Format("20060102-150405"))); err != nil {
			return "", fmt.Errorf("copie avant migration impossible, migration annulée : %w", err)
		}
	}
	for _, m := range migs[actuelle:] {
		if err := appliquer(ctx, db, m); err != nil {
			return copie, fmt.Errorf("migration %s : %w (copie de la base avant migration : %s)", m.nom, err, copie)
		}
	}
	return copie, nil
}

// appliquer exécute une migration et note sa version, dans une transaction :
// en cas d'erreur, rien n'est modifié.
func appliquer(ctx context.Context, db *sql.DB, m migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // sans effet après Commit
	if _, err := tx.ExecContext(ctx, m.sql); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_version(version, nom, appliquee_le) VALUES (?, ?, ?)`,
		m.version, m.nom, time.Now().Unix()); err != nil {
		return err
	}
	return tx.Commit()
}

// Copier fait une copie cohérente de la base pendant qu'elle tourne
// (VACUUM INTO : compacte et ne bloque pas les lecteurs). Sert aux migrations
// et aux sauvegardes.
func Copier(ctx context.Context, db *sql.DB, dossier, nom string) (string, error) {
	if strings.ContainsAny(nom, `/\'`) {
		return "", errors.New("nom de copie invalide")
	}
	if err := os.MkdirAll(dossier, 0o700); err != nil {
		return "", err
	}
	chemin := filepath.Join(dossier, nom)
	if _, err := os.Stat(chemin); err == nil {
		return "", fmt.Errorf("%s existe déjà", chemin)
	}
	// Le chemin ne contient ni apostrophe ni séparateur dangereux (vérifié
	// ci-dessus et par le dossier venant de la configuration) ; VACUUM INTO
	// n'accepte pas de paramètre lié.
	if _, err := db.ExecContext(ctx, `VACUUM INTO '`+chemin+`'`); err != nil { // #nosec G202 -- chemin contrôlé, pas d'entrée utilisateur
		return "", err
	}
	return chemin, os.Chmod(chemin, 0o600)
}
