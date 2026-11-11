// SPDX-License-Identifier: GPL-3.0-or-later

package notif

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"mirador-stack/internal/server/alertes"
)

// Modes.
const (
	Ombre = "ombre" // tout est calculé et noté, rien n'est envoyé (période en parallèle)
	Actif = "actif"
)

// Route : réglages d'envoi d'une gravité (comme les routes d'Alertmanager).
type Route struct {
	Attente    alertes.Duree `json:"attente"`    // group_wait : regroupe ce qui arrive ensemble
	Intervalle alertes.Duree `json:"intervalle"` // group_interval : écart mini entre deux messages d'un groupe
	Rappel     alertes.Duree `json:"rappel"`     // repeat_interval ; 0 = jamais
	Son        bool          `json:"son"`        // notification avec son (Telegram)
}

// Reglages des notifications (clé « notifications » de la table reglages).
// La partie 3 y ajoute les canaux.
type Reglages struct {
	Mode        string           `json:"mode"`
	GrouperPar  []string         `json:"grouper_par"`
	Routes      map[string]Route `json:"routes"`       // par gravité : critical, warning, info
	ReglesPanne []string         `json:"regles_panne"` // règles qui veulent dire « machine en panne » (dépendances)
	Lien        string           `json:"lien"`         // adresse de l'interface, pour le lien direct (https://…)
	Fuseau      string           `json:"fuseau"`       // heures affichées dans les messages
	Canaux      Canaux           `json:"canaux"`
}

// ReglagesDefaut : repris d'alertmanager.yml (SPEC § 4.7) ; mode ombre tant
// que vmalert et Alertmanager tournent.
func ReglagesDefaut() Reglages {
	m := alertes.Duree(time.Minute)
	return Reglages{
		Mode:       Ombre,
		GrouperPar: []string{"alertname", "instance"},
		Routes: map[string]Route{
			alertes.Critique:      {Attente: alertes.Duree(10 * time.Second), Intervalle: 5 * m, Rappel: 120 * m, Son: true},
			alertes.Avertissement: {Attente: alertes.Duree(30 * time.Second), Intervalle: 5 * m, Rappel: 720 * m},
			alertes.Info:          {Attente: alertes.Duree(30 * time.Second), Intervalle: 5 * m, Rappel: 720 * m},
		},
		ReglesPanne: []string{"AgentMuet", "ProxmoxMuet", "OpnsenseMuet", "UnifiMuet", "SynologyInjoignable"},
		Fuseau:      "Europe/Paris",
		Canaux:      CanauxDefaut(),
	}
}

// Masques : copie sans secret (affichage, audit).
func (r Reglages) Masques() Reglages {
	m := r
	m.Canaux = r.Canaux.Masques()
	return m
}

var reNomEtiquette = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]{0,63}$`)

// Valider vérifie les réglages.
func (r *Reglages) Valider() error {
	var errs []error
	if r.Mode != Ombre && r.Mode != Actif {
		errs = append(errs, fmt.Errorf("mode %q : ombre ou actif", r.Mode))
	}
	if len(r.GrouperPar) > 8 {
		errs = append(errs, errors.New("grouper_par : 8 étiquettes au plus"))
	}
	for _, e := range r.GrouperPar {
		if !reNomEtiquette.MatchString(e) {
			errs = append(errs, fmt.Errorf("grouper_par : étiquette %q invalide", e))
		}
	}
	for _, g := range []string{alertes.Critique, alertes.Avertissement, alertes.Info} {
		ro, ok := r.Routes[g]
		if !ok {
			errs = append(errs, fmt.Errorf("routes : %s manquante", g))
			continue
		}
		if a := time.Duration(ro.Attente); a < 0 || a > 10*time.Minute {
			errs = append(errs, fmt.Errorf("route %s : attente entre 0 et 10 min", g))
		}
		if i := time.Duration(ro.Intervalle); i < time.Minute || i > time.Hour {
			errs = append(errs, fmt.Errorf("route %s : intervalle entre 1 min et 1 h", g))
		}
		if p := time.Duration(ro.Rappel); p != 0 && (p < 10*time.Minute || p > 7*24*time.Hour) {
			errs = append(errs, fmt.Errorf("route %s : rappel 0 (jamais) ou entre 10 min et 7 j", g))
		}
	}
	if len(r.Routes) != 3 {
		errs = append(errs, errors.New("routes : critical, warning et info seulement"))
	}
	if len(r.ReglesPanne) > 32 {
		errs = append(errs, errors.New("regles_panne : 32 au plus"))
	}
	if r.Lien != "" {
		u, err := url.Parse(r.Lien)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
			errs = append(errs, errors.New("lien : adresse https://… de l'interface, sans paramètre"))
		}
		r.Lien = strings.TrimSuffix(r.Lien, "/")
	}
	if _, err := time.LoadLocation(r.Fuseau); err != nil || r.Fuseau == "" {
		errs = append(errs, fmt.Errorf("fuseau %q inconnu", r.Fuseau))
	}
	// Chaque canal décrit est toujours vérifié. Le principal doit être
	// configuré dès qu'on passe en mode actif (ou qu'un secours est choisi).
	errs = append(errs, r.Canaux.Valider(r.Mode == Actif || r.Canaux.Secours != ""))
	return errors.Join(errs...)
}

const cleReglages = "notifications"

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
		return r, fmt.Errorf("réglages des notifications abîmés en base : %w", err)
	}
	return r, r.Valider()
}

// EcrireReglages enregistre des réglages vérifiés (audit avant / après,
// sans secret). Les secrets donnés en clair sont chiffrés avec cle ; « *** »
// garde le secret déjà enregistré.
func EcrireReglages(ctx context.Context, db *sql.DB, r Reglages, cle []byte, qui string, t time.Time) error {
	avant, err := LireReglages(ctx, db)
	if err != nil {
		return err
	}
	if r.Canaux, err = PreparerSecrets(r.Canaux, avant.Canaux, cle); err != nil {
		return err
	}
	if err := r.Valider(); err != nil {
		return err
	}
	brutAvant, _ := json.Marshal(avant.Masques())
	masque, _ := json.Marshal(r.Masques())
	brut, _ := json.Marshal(r)
	return ecrire(ctx, db, "notif", func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO reglages(cle, valeur, modifie_le, modifie_par) VALUES (?, ?, ?, ?)
			ON CONFLICT(cle) DO UPDATE SET valeur = excluded.valeur, modifie_le = excluded.modifie_le, modifie_par = excluded.modifie_par`,
			cleReglages, string(brut), t.Unix(), qui); err != nil {
			return err
		}
		return auditer(ctx, tx, qui, "reglages_notifications", cleReglages, string(brutAvant), string(masque), t)
	})
}
