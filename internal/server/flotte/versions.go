// SPDX-License-Identifier: GPL-3.0-or-later

package flotte

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"mirador-stack/internal/protocole"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"time"

	"mirador-stack/internal/agent/majagent"
)

// Versions de l'agent gardées sur le serveur (mesure du 13/11, SPEC § 22.6) :
// chaque version importée prend ~16 Mo (amd64 + arm64) et chaque sauvegarde
// recopie tout le dossier (166 Mo et 51 Mo par archive après 12 versions).
//
// Sont gardées :
//   - les versions « en service » : cible globale, cibles propres des
//     machines, version qui tourne et version à l'essai sur chaque machine ;
//   - les N plus récentes (réglage, 2 par défaut : revenir en arrière vite) ;
//   - celles publiées depuis moins de 24 h (le temps de choisir une version
//     ancienne importée pour revenir en arrière, avant d'en faire la cible) ;
//   - seulement les architectures du parc (s'il y a au moins une machine).
//
// Une version supprimée se réimporte depuis GitHub (toujours signée). Une
// machine d'une nouvelle architecture s'installe avec le binaire intégré à
// l'image du serveur (binaireInstallation), puis on importe sa version.

// GarderVersions : versions récentes gardées en plus de celles en service
// (défaut ; réglable de 1 à 10 dans Mises à jour des agents).
const GarderVersions = 2

// cleGarderVersions : réglage du nombre de versions récentes gardées.
const cleGarderVersions = "maj.versions_gardees"

// LireGarderVersions : nombre de versions récentes gardées (défaut 2).
func LireGarderVersions(ctx context.Context, db *sql.DB) (int, error) {
	var v string
	err := db.QueryRowContext(ctx, `SELECT valeur FROM reglages WHERE cle = ?`, cleGarderVersions).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return GarderVersions, nil
	}
	if err != nil {
		return GarderVersions, err
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > 10 {
		return GarderVersions, nil
	}
	return n, nil
}

// DefinirGarderVersions : 1 à 10 (audit). Pris en compte au nettoyage
// suivant (au plus tard dans 24 h, ou au démarrage du serveur).
func DefinirGarderVersions(ctx context.Context, db *sql.DB, n int, qui string, maintenant time.Time) error {
	if n < 1 || n > 10 {
		return errors.New("versions gardées : de 1 à 10")
	}
	avant, err := LireGarderVersions(ctx, db)
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
		cleGarderVersions, strconv.Itoa(n), maintenant.Unix(), qui); err != nil {
		return err
	}
	if err := audit(ctx, tx, maintenant.Unix(), qui, "versions_gardees", "", strconv.Itoa(avant), strconv.Itoa(n), ""); err != nil {
		return err
	}
	return tx.Commit()
}

// reFichierVersion : binaires rangés par Publier (nomBinaire) et fichiers
// temporaires laissés par une écriture interrompue.
var reFichierVersion = regexp.MustCompile(`^mirador-(agent|collector)-([0-9]{1,5}\.[0-9]{1,5}\.[0-9]{1,5})-linux-(amd64|arm64)(\.tmp)?$`)

// publiee : un binaire publié.
type publiee struct{ Produit, Version, Arch string }

// versionsEnService : versions dont une machine a besoin, à ne jamais supprimer.
func versionsEnService(ctx context.Context, db *sql.DB) (map[string]bool, error) {
	garder := map[string]bool{}
	cible, err := CibleGlobale(ctx, db)
	if err != nil {
		return nil, err
	}
	garder[cible] = true
	rows, err := db.QueryContext(ctx, `SELECT version_cible, version_agent, maj_essai FROM machines WHERE statut != 'refusee'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var c, v, e string
		if err := rows.Scan(&c, &v, &e); err != nil {
			return nil, err
		}
		garder[c], garder[v], garder[e] = true, true, true
	}
	delete(garder, "")
	return garder, rows.Err()
}

// ArchitecturesParc : architectures des machines connues (vide : aucune
// machine, toutes les architectures sont alors utiles).
func ArchitecturesParc(ctx context.Context, db *sql.DB) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT DISTINCT architecture FROM machines WHERE statut != 'refusee' AND architecture != ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, err
		}
		if architectures[a] {
			out[a] = true
		}
	}
	return out, rows.Err()
}

// DelaiNouvelleVersion : une version publiée depuis moins longtemps est gardée.
const DelaiNouvelleVersion = 24 * time.Hour

// versionsPubliees : binaires (produit, version, architecture) en base.
func versionsPubliees(ctx context.Context, db *sql.DB) ([]publiee, error) {
	return versionsPublieesDepuis(ctx, db, 0)
}

// versionsPublieesDepuis : celles publiées à partir de « depuis » (secondes Unix).
func versionsPublieesDepuis(ctx context.Context, db *sql.DB, depuis int64) ([]publiee, error) {
	rows, err := db.QueryContext(ctx, `SELECT produit, version, architecture FROM versions_binaires WHERE publiee_le >= ?`, depuis)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []publiee
	for rows.Next() {
		var p publiee
		if err := rows.Scan(&p.Produit, &p.Version, &p.Arch); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// recentes : les n versions les plus récentes (numéro de version, pas date
// d'import : réimporter une vieille version ne la rend pas « récente »).
func recentes(publiees []publiee, n int) map[string]bool {
	vu := map[string]bool{}
	var vs []string
	for _, p := range publiees {
		if !vu[p.Version] {
			vu[p.Version] = true
			vs = append(vs, p.Version)
		}
	}
	sort.Slice(vs, func(i, j int) bool {
		c, err := majagent.ComparerVersions(vs[i], vs[j])
		return err == nil && c > 0
	})
	out := map[string]bool{}
	for i := 0; i < len(vs) && i < n; i++ {
		out[vs[i]] = true
	}
	return out
}

// NettoyerVersions supprime les versions inutiles (voir en tête) et les
// fichiers orphelins du dossier. Renvoie ce qui a été supprimé.
func NettoyerVersions(ctx context.Context, db *sql.DB, dossier, qui string, maintenant time.Time) ([]string, error) {
	enService, err := versionsEnService(ctx, db)
	if err != nil {
		return nil, err
	}
	parc, err := ArchitecturesParc(ctx, db)
	if err != nil {
		return nil, err
	}
	publiees, err := versionsPubliees(ctx, db)
	if err != nil {
		return nil, err
	}
	garder, err := LireGarderVersions(ctx, db)
	if err != nil {
		return nil, err
	}
	recent := recentes(publiees, garder)
	nouvelles, err := versionsPublieesDepuis(ctx, db, maintenant.Add(-DelaiNouvelleVersion).Unix())
	if err != nil {
		return nil, err
	}
	for _, p := range nouvelles {
		recent[p.Version] = true
	}
	var faites []string
	var errs []error
	for _, p := range publiees {
		utile := enService[p.Version] || recent[p.Version]
		if utile && (len(parc) == 0 || parc[p.Arch]) {
			continue
		}
		if err := supprimer(ctx, db, dossier, p, qui, maintenant); err != nil {
			errs = append(errs, err)
			continue
		}
		faites = append(faites, p.nom())
	}
	// Fichiers sans ligne en base (écriture interrompue, base restaurée…).
	connus := map[string]bool{}
	if publiees, err = versionsPubliees(ctx, db); err == nil {
		for _, p := range publiees {
			connus[nomBinaire(p.Produit, p.Version, p.Arch)] = true
		}
		entrees, _ := os.ReadDir(dossier)
		for _, e := range entrees {
			if reFichierVersion.MatchString(e.Name()) && !connus[e.Name()] && e.Type().IsRegular() {
				if err := os.Remove(filepath.Join(dossier, e.Name())); err == nil {
					faites = append(faites, e.Name())
				}
			}
		}
	}
	return faites, errors.Join(errs...)
}

// SupprimerVersion : bouton « Supprimer » de l'interface (toutes les
// architectures). Refusé pour une version dont une machine a besoin.
func SupprimerVersion(ctx context.Context, db *sql.DB, dossier, version, qui string, maintenant time.Time) error {
	if !formatVersionPubliee.MatchString(version) {
		return fmt.Errorf("version %q invalide", version)
	}
	enService, err := versionsEnService(ctx, db)
	if err != nil {
		return err
	}
	if enService[version] {
		return fmt.Errorf("version %s en service (cible ou installée sur une machine) : changer la cible d'abord", version)
	}
	publiees, err := versionsPubliees(ctx, db)
	if err != nil {
		return err
	}
	trouvee := false
	for _, p := range publiees {
		if p.Version == version {
			trouvee = true
			if err := supprimer(ctx, db, dossier, p, qui, maintenant); err != nil {
				return err
			}
		}
	}
	if !trouvee {
		return fmt.Errorf("version %s non publiée", version)
	}
	return nil
}

// supprimer : ligne en base (avec audit) puis fichier. Si le fichier ne
// peut pas être effacé, il sera repris comme orphelin au passage suivant.
func supprimer(ctx context.Context, db *sql.DB, dossier string, p publiee, qui string, maintenant time.Time) error {
	if !formatVersionPubliee.MatchString(p.Version) || !architectures[p.Arch] || !produits[p.Produit] {
		return fmt.Errorf("binaire %q invalide", p.nom())
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // sans effet après Commit
	if _, err := tx.ExecContext(ctx, `DELETE FROM versions_binaires WHERE produit = ? AND version = ? AND architecture = ?`, p.Produit, p.Version, p.Arch); err != nil {
		return err
	}
	if err := audit(ctx, tx, maintenant.Unix(), qui, "version_supprimee", p.nom(), "", "", ""); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(dossier, nomBinaire(p.Produit, p.Version, p.Arch))); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// nom : « 1.2.3/amd64 » pour l'agent, « mirador-collector 1.2.3/amd64 » sinon.
func (p publiee) nom() string {
	if p.Produit == protocole.ProduitAgent {
		return p.Version + "/" + p.Arch
	}
	return p.Produit + " " + p.Version + "/" + p.Arch
}
