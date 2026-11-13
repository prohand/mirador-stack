// SPDX-License-Identifier: GPL-3.0-or-later

package flotte

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"mirador-stack/internal/agent/majagent"
	"mirador-stack/internal/protocole"
)

// Versions signées par la CI de GitHub (choix du 11/11, SPEC § 18.26) : à
// chaque tag « vX.Y.Z », la CI compile l'agent, le signe avec la clé gardée
// dans les secrets GitHub et ajoute à la Release :
//   mirador-agent-X.Y.Z-linux-ARCH et mirador-agent-X.Y.Z-linux-ARCH.manifeste.json
// L'interface importe ces fichiers ; le serveur vérifie la signature avec
// les clés publiques réglées (il n'a jamais la clé privée). Le dépôt source
// est un réglage : un fork règle le sien (et sa propre clé).

// DepotDefaut : dépôt officiel. Ses versions ne sont acceptées que si sa clé
// publique est réglée : un fork qui garde ce défaut avec sa propre clé voit
// simplement l'import refusé (signature).
const DepotDefaut = "prohand/mirador-stack"

const cleDepot = "maj.depot_versions"

var (
	formatDepot = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}/[A-Za-z0-9._-]{1,100}$`)
	formatTag   = regexp.MustCompile(`^v?([0-9]{1,5}\.[0-9]{1,5}\.[0-9]{1,5})$`)
)

// LireDepot : dépôt GitHub d'où importer les versions.
func LireDepot(ctx context.Context, db *sql.DB) (string, error) {
	var v string
	err := db.QueryRowContext(ctx, `SELECT valeur FROM reglages WHERE cle = ?`, cleDepot).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return DepotDefaut, nil
	}
	return v, err
}

// DefinirDepot : « propriétaire/dépôt » (audit).
func DefinirDepot(ctx context.Context, db *sql.DB, depot, qui string, maintenant time.Time) error {
	depot = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(depot), "https://github.com/"))
	depot = strings.TrimSuffix(depot, "/")
	if !formatDepot.MatchString(depot) {
		return errors.New("dépôt : « propriétaire/nom » attendu (ex. " + DepotDefaut + ")")
	}
	avant, err := LireDepot(ctx, db)
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
		cleDepot, depot, maintenant.Unix(), qui); err != nil {
		return err
	}
	if err := audit(ctx, tx, maintenant.Unix(), qui, "depot_versions", "", avant, depot, ""); err != nil {
		return err
	}
	return tx.Commit()
}

// SourceGitHub : où et comment télécharger (adresse remplaçable dans les tests).
type SourceGitHub struct {
	Base   string       // défaut https://github.com
	Client *http.Client // défaut : clientGitHub()
	// Architectures : seules celles-ci sont importées (vide = toutes). Par
	// défaut, l'interface passe celles du parc (SPEC § 22.6).
	Architectures map[string]bool
}

// clientGitHub : HTTPS seulement ; redirections suivies seulement vers
// GitHub (les fichiers des Releases sont servis par *.githubusercontent.com).
func clientGitHub() *http.Client {
	return &http.Client{Timeout: 3 * time.Minute,
		CheckRedirect: func(r *http.Request, via []*http.Request) error {
			h := r.URL.Hostname()
			if len(via) > 3 || r.URL.Scheme != "https" ||
				(h != "github.com" && !strings.HasSuffix(h, ".githubusercontent.com")) {
				return errors.New("redirection refusée : " + r.URL.Redacted())
			}
			return nil
		}}
}

// ImporterGitHub : importe la version « tag » (vX.Y.Z) du dépôt : pour chaque
// architecture, manifeste signé + binaire, vérifiés puis publiés (Publier).
// Une architecture absente de la Release est sautée ; au moins une exigée.
// Renvoie les architectures importées.
func ImporterGitHub(ctx context.Context, db *sql.DB, dossier string, src SourceGitHub, depot, tag, qui string, maintenant time.Time) ([]string, error) {
	if !formatDepot.MatchString(depot) {
		return nil, errors.New("dépôt invalide")
	}
	m := formatTag.FindStringSubmatch(strings.TrimSpace(tag))
	if m == nil {
		return nil, errors.New("version : vX.Y.Z attendu (ex. v0.2.9)")
	}
	version := m[1]
	if src.Base == "" {
		src.Base = "https://github.com"
	}
	if src.Client == nil {
		src.Client = clientGitHub()
	}
	var faites []string
	var erreurs []error
	for _, arch := range []string{"amd64", "arm64"} {
		if len(src.Architectures) > 0 && !src.Architectures[arch] {
			continue // architecture absente du parc
		}
		nom := "mirador-agent-" + version + "-linux-" + arch
		base := src.Base + "/" + depot + "/releases/download/" + url.PathEscape("v"+version) + "/"
		brutM, err := telechargerGitHub(ctx, src.Client, base+nom+".manifeste.json", 64<<10)
		if errors.Is(err, errAbsent) {
			continue // architecture non publiée
		}
		if err != nil {
			erreurs = append(erreurs, fmt.Errorf("%s : manifeste : %w", arch, err))
			continue
		}
		var man protocole.Manifeste
		dec := json.NewDecoder(bytes.NewReader(brutM))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&man); err != nil {
			erreurs = append(erreurs, fmt.Errorf("%s : manifeste illisible : %w", arch, err))
			continue
		}
		if man.Version != version || man.Architecture != arch {
			erreurs = append(erreurs, fmt.Errorf("%s : manifeste pour %s/%s", arch, man.Version, man.Architecture))
			continue
		}
		// Signature vérifiée AVANT de télécharger le binaire (comme l'agent).
		cles, err := clesVerification(ctx, db)
		if err != nil {
			return faites, err
		}
		if cles == "" {
			return faites, errors.New("aucune clé publique réglée : la saisir d'abord (Clé publique de signature)")
		}
		if err := majagent.VerifierSignature(&man, cles); err != nil {
			erreurs = append(erreurs, fmt.Errorf("%s : %w (version signée par une autre clé ?)", arch, err))
			continue
		}
		bin, err := telechargerGitHub(ctx, src.Client, base+nom, majagent.TailleMax)
		if err != nil {
			erreurs = append(erreurs, fmt.Errorf("%s : binaire : %w", arch, err))
			continue
		}
		if err := Publier(ctx, db, dossier, &man, bin, qui, maintenant); err != nil {
			erreurs = append(erreurs, fmt.Errorf("%s : %w", arch, err))
			continue
		}
		faites = append(faites, arch)
	}
	if len(faites) == 0 && len(erreurs) == 0 {
		return nil, fmt.Errorf("aucune version signée v%s dans les Releases de %s (CI pas finie, ou clé de signature absente des secrets GitHub ?)", version, depot)
	}
	return faites, errors.Join(erreurs...)
}

var errAbsent = errors.New("absent")

func telechargerGitHub(ctx context.Context, client *http.Client, adresse string, max int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, adresse, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "mirador-server")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, errAbsent
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("réponse %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, errors.New("fichier trop gros")
	}
	return b, nil
}
