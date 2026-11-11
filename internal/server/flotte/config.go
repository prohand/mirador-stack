// SPDX-License-Identifier: GPL-3.0-or-later

package flotte

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"mirador-stack/internal/agent/config"
	"mirador-stack/internal/agent/distant"
)

// Portées de la configuration des agents (SPEC § 6.7) : défaut global, puis
// groupe, puis machine. Gardées dans la table « reglages ».
const prefixeConfig = "config_agent:"

var formatGroupe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,63}$`)

// ValiderPortee : « defaut », « groupe:NOM » ou « machine:ID ».
func ValiderPortee(p string) error {
	switch {
	case p == "defaut":
		return nil
	case strings.HasPrefix(p, "groupe:") && formatGroupe.MatchString(strings.TrimPrefix(p, "groupe:")):
		return nil
	case strings.HasPrefix(p, "machine:"):
		if id, err := strconv.ParseInt(strings.TrimPrefix(p, "machine:"), 10, 64); err == nil && id > 0 {
			return nil
		}
	}
	return fmt.Errorf("portée %q invalide (defaut, groupe:NOM ou machine:ID)", p)
}

// verifierDocument : même contrôle que l'agent (champs connus seulement,
// mêmes bornes), pour refuser une erreur dès l'enregistrement.
func verifierDocument(doc []byte) error {
	d, err := distant.Decoder(bytes.NewReader(doc))
	if err != nil {
		return err
	}
	base := config.Defaut()
	base.Nom, base.Site, base.Env = "verification", "site", "env"
	base.URLMetriques = "https://exemple.invalid/api/agent/v1/metrics"
	_, err = distant.Appliquer(base, d)
	return err
}

// DefinirConfig enregistre (ou retire, si doc est vide ou « {} ») la
// configuration d'une portée, avec une ligne d'audit.
func DefinirConfig(ctx context.Context, db *sql.DB, portee string, doc []byte, qui string, maintenant time.Time) error {
	if err := ValiderPortee(portee); err != nil {
		return err
	}
	vide := len(bytes.TrimSpace(doc)) == 0
	var normalise []byte
	if !vide {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(doc, &m); err != nil {
			return fmt.Errorf("document JSON illisible : %w", err)
		}
		vide = len(m) == 0
		if !vide {
			var err error
			if normalise, err = json.Marshal(m); err != nil { // clés triées, sans espaces
				return err
			}
			if err := verifierDocument(normalise); err != nil {
				return err
			}
		}
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // sans effet après Commit
	var avant string
	_ = tx.QueryRowContext(ctx, `SELECT valeur FROM reglages WHERE cle = ?`, prefixeConfig+portee).Scan(&avant)
	if vide {
		_, err = tx.ExecContext(ctx, `DELETE FROM reglages WHERE cle = ?`, prefixeConfig+portee)
	} else {
		_, err = tx.ExecContext(ctx, `INSERT INTO reglages(cle, valeur, modifie_le, modifie_par) VALUES (?, ?, ?, ?)
			ON CONFLICT(cle) DO UPDATE SET valeur = excluded.valeur, modifie_le = excluded.modifie_le, modifie_par = excluded.modifie_par`,
			prefixeConfig+portee, string(normalise), maintenant.Unix(), qui)
	}
	if err != nil {
		return err
	}
	if err := audit(ctx, tx, maintenant.Unix(), qui, "config_agent", portee, avant, string(normalise), ""); err != nil {
		return err
	}
	return tx.Commit()
}

// ConfigMachine assemble la configuration d'une machine (défaut, puis groupe,
// puis machine : la plus précise l'emporte, clé par clé) et son ETag.
func ConfigMachine(ctx context.Context, db *sql.DB, machine int64) (doc []byte, etag string, err error) {
	var groupe string
	if err := db.QueryRowContext(ctx, `SELECT groupe FROM machines WHERE id = ?`, machine).Scan(&groupe); err != nil {
		return nil, "", err
	}
	portees := []string{"defaut"}
	if groupe != "" {
		portees = append(portees, "groupe:"+groupe)
	}
	portees = append(portees, "machine:"+strconv.FormatInt(machine, 10))
	fusion := map[string]json.RawMessage{}
	for _, p := range portees {
		var valeur string
		err := db.QueryRowContext(ctx, `SELECT valeur FROM reglages WHERE cle = ?`, prefixeConfig+p).Scan(&valeur)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, "", err
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal([]byte(valeur), &m); err != nil {
			return nil, "", fmt.Errorf("configuration %s abîmée en base : %w", p, err)
		}
		for k, v := range m {
			fusion[k] = v
		}
	}
	if doc, err = json.Marshal(fusion); err != nil {
		return nil, "", err
	}
	// Défense en profondeur : un document qui ne passerait pas chez l'agent
	// n'est pas envoyé (l'agent le refuserait de toute façon).
	if err := verifierDocument(doc); err != nil {
		return nil, "", fmt.Errorf("configuration fusionnée invalide : %w", err)
	}
	h := sha256.Sum256(doc)
	return doc, `"` + hex.EncodeToString(h[:16]) + `"`, nil
}

func (f *Flotte) config(w http.ResponseWriter, r *http.Request) {
	m, liberer, ok := f.opt.Ingestion.Accueillir(w, r)
	if !ok {
		return
	}
	defer liberer()
	doc, etag, err := ConfigMachine(r.Context(), f.opt.DB, m.ID)
	if err != nil {
		f.opt.Journal.Error("configuration d'une machine", "machine", m.Nom, "erreur", err)
		erreur(w, http.StatusServiceUnavailable, "configuration indisponible")
		return
	}
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "no-store")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(doc)
}
