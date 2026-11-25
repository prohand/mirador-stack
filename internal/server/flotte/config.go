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
	"mirador-stack/internal/protocole"
	"mirador-stack/internal/server/modeles"
	"mirador-stack/internal/server/secrets"
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
	// Audit sans secret : les mots de passe chiffrés (SNMP, UniFi) sont
	// remplacés par « *** » (audit du 16/11, SPEC § 34).
	if err := audit(ctx, tx, maintenant.Unix(), qui, "config_agent", portee, secrets.MasquerJSON(avant), secrets.MasquerJSON(string(normalise)), ""); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	SignalerConfig() // agents en attente : nouvelle configuration tout de suite
	return nil
}

// ConfigMachine assemble la configuration d'une machine (défaut, modèles de
// surveillance, groupe, puis machine : la plus précise l'emporte, clé par
// clé) et son ETag.
func ConfigMachine(ctx context.Context, db *sql.DB, machine int64) (doc []byte, etag string, err error) {
	var groupe, typeMachine string
	if err := db.QueryRowContext(ctx, `SELECT groupe, type FROM machines WHERE id = ?`, machine).Scan(&groupe, &typeMachine); err != nil {
		return nil, "", err
	}
	portees := []string{"defaut", "modeles"}
	if groupe != "" {
		portees = append(portees, "groupe:"+groupe)
	}
	portees = append(portees, "machine:"+strconv.FormatInt(machine, 10))
	fusion := map[string]json.RawMessage{}
	for _, p := range portees {
		// Modèles de surveillance (SPEC § 24.2) : après le défaut, avant les
		// réglages faits à la main pour le groupe ou la machine.
		if p == "modeles" {
			effs, err := modeles.Effectifs(ctx, db, machine, groupe)
			if err != nil {
				return nil, "", err
			}
			cm, err := modeles.ConfigEffective(effs)
			if err != nil {
				return nil, "", err
			}
			for k, v := range cm {
				fusion[k] = v
			}
			continue
		}
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
	// Collecteur (SPEC § 23.13) : la liste de ses équipements est posée par
	// le serveur (rattachements de l'interface), jamais saisie à la main.
	if typeMachine == "collecteur" {
		if fusion["collecteur"], err = configCollecteur(ctx, db, machine, fusion["collecteur"]); err != nil {
			return nil, "", err
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
	libere := false
	defer func() {
		if !libere {
			liberer()
		}
	}()
	charger := func() ([]byte, string, error) {
		doc, etag, err := ConfigMachine(r.Context(), f.opt.DB, m.ID)
		if err != nil {
			return nil, "", err
		}
		// Agent d'avant la surveillance (SPEC § 19) ou les éléments (§ 21) :
		// il refuserait tout le document à cause d'une clé qu'il ne connaît
		// pas. On les lui retire.
		if retirer := clesInconnues(capacitesDe(r)); len(retirer) > 0 {
			if doc, etag, err = sansCles(doc, retirer); err != nil {
				return nil, "", err
			}
		}
		// Collecteur d'avant les sondes de services (SPEC § 48) : il
		// refuserait la clé collecteur.services ; on la lui retire.
		if m.Type == "collecteur" && !ACapacite(capacitesDe(r), protocole.CapaciteSondes) {
			if doc, etag, err = sansServices(doc); err != nil {
				return nil, "", err
			}
		}
		// Collecteur : mots de passe SNMP déchiffrés pour lui seul, à
		// l'envoi (ils restent chiffrés en base ; même ETag).
		if m.Type == "collecteur" {
			if doc, err = dechiffrerSecretsCollecteur(doc, f.opt.Cle); err != nil {
				return nil, "", err
			}
		}
		return doc, etag, nil
	}
	signal := attenteSignal() // avant la lecture : aucun changement manqué
	// Direct (SPEC § 97) : une page qui commence à regarder la machine
	// réveille son agent ; un direct déjà demandé mais pas encore commencé
	// par l'agent est répondu tout de suite.
	var signalDirect <-chan struct{}
	directEnAttente := false
	if f.opt.Direct != nil {
		signalDirect = f.opt.Direct.Signal(m.ID)
		directEnAttente = f.opt.Direct.Reste(m.ID) > 0 && r.Header.Get(protocole.EnteteDirectActif) != "1"
	}
	doc, etag, err := charger()
	if err != nil {
		f.opt.Journal.Error("configuration d'une machine", "machine", m.Nom, "erreur", err)
		erreur(w, http.StatusServiceUnavailable, "configuration indisponible")
		return
	}
	// Demande gardée ouverte (SPEC § 22) : rien de neuf pour cet agent,
	// on attend un changement. La place de décodage de l'ingestion est
	// rendue pendant l'attente (elle ne sert qu'aux envois de données).
	actu := actualisationDe(m.ID)
	vu, _ := strconv.ParseInt(r.Header.Get(protocole.EnteteActualise), 10, 64)
	// « Vérifier maintenant » (SPEC § 127) : seulement pour un agent qui
	// envoie EnteteVerifie (il sait le faire) ; un ancien agent n'est jamais
	// réveillé pour rien.
	_, avecVerif := r.Header[http.CanonicalHeaderKey(protocole.EnteteVerifie)]
	verif := verificationDe(m.ID)
	vuVerif, _ := strconv.ParseInt(r.Header.Get(protocole.EnteteVerifie), 10, 64)
	verifEnAttente := avecVerif && verif > vuVerif
	// Essais de sondes demandés (SPEC § 108) : seulement pour un collecteur
	// qui envoie EnteteSonde (il sait les faire).
	_, avecSonde := r.Header[http.CanonicalHeaderKey(protocole.EnteteSonde)]
	vuSonde, _ := strconv.ParseInt(r.Header.Get(protocole.EnteteSonde), 10, 64)
	essaisEnAttente := func() (int64, []string) {
		if !avecSonde {
			return 0, nil
		}
		return essaisDe(m.ID, vuSonde, time.Now())
	}
	dernierEssai, nomsEssai := essaisEnAttente()
	if attente := lireAttente(r.Header.Get(protocole.EnteteAttendre)); attente > 0 &&
		r.Header.Get("If-None-Match") == etag && actu <= vu && !directEnAttente && len(nomsEssai) == 0 && !verifEnAttente {
		liberer()
		libere = true
		minuterie := time.NewTimer(attente)
		select {
		case <-signal:
		case <-signalDirect:
		case <-minuterie.C:
		case <-r.Context().Done():
			minuterie.Stop()
			return
		}
		minuterie.Stop()
		if doc, etag, err = charger(); err != nil {
			erreur(w, http.StatusServiceUnavailable, "configuration indisponible")
			return
		}
		actu = actualisationDe(m.ID)
		dernierEssai, nomsEssai = essaisEnAttente()
		verif = verificationDe(m.ID)
	}
	if v := protocole.FormerSonder(dernierEssai, nomsEssai); v != "" {
		w.Header().Set(protocole.EnteteSonder, v)
	}
	w.Header().Set(protocole.EnteteAttente, "1") // ce serveur sait garder la demande ouverte
	if f.opt.Direct != nil {
		if reste := f.opt.Direct.Reste(m.ID); reste > 0 {
			w.Header().Set(protocole.EnteteDirect, strconv.Itoa(reste))
		}
	}
	if actu > 0 {
		w.Header().Set(protocole.EnteteActualiser, strconv.FormatInt(actu, 10))
	}
	if avecVerif && verif > 0 {
		w.Header().Set(protocole.EnteteVerifier, strconv.FormatInt(verif, 10))
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
