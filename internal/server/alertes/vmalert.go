// SPDX-License-Identifier: GPL-3.0-or-later

package alertes

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Comparaison avec vmalert (mode ombre, SPEC.md § 17.8) : pendant la période
// en parallèle, le serveur lit régulièrement les alertes actives de vmalert
// (API /api/v1/alerts, en lecture) et les compare aux siennes, machine par
// machine. Chaque écart (présente chez vmalert seulement = « manquante »,
// chez mirador seulement = « en trop ») est gardé avec son début et sa fin.

// ReglagesVmalert : clé « comparaison_vmalert » de la table reglages.
type ReglagesVmalert struct {
	Actif bool   `json:"actif"`
	URL   string `json:"url"` // ex. http://172.30.0.12:8880 (réseau Docker interne)
	// Nom chez mirador → nom chez vmalert ; à défaut, Suffixe est retiré
	// (srv-test-mirador → srv-test).
	Suffixe         string            `json:"suffixe"`
	Correspondances map[string]string `json:"correspondances"`
	Ignorer         []string          `json:"ignorer"` // règles non comparées (Watchdog…)
}

// ReglagesVmalertDefaut : désactivé ; suffixe « -mirador » (SPEC § 15.1).
func ReglagesVmalertDefaut() ReglagesVmalert {
	return ReglagesVmalert{Suffixe: "-mirador", Correspondances: map[string]string{}, Ignorer: []string{"Watchdog"}}
}

// Valider vérifie les réglages.
func (r *ReglagesVmalert) Valider() error {
	if r.URL != "" {
		u, err := url.Parse(r.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
			return errors.New("url : http(s)://hôte:port de vmalert")
		}
	}
	if r.Actif && r.URL == "" {
		return errors.New("url de vmalert manquante")
	}
	if len(r.Correspondances) > 500 || len(r.Ignorer) > 200 || len(r.Suffixe) > 64 {
		return errors.New("trop de correspondances ou de règles ignorées")
	}
	return nil
}

const cleVmalert = "comparaison_vmalert"

// LireReglagesVmalert : réglages en base, ou les défauts.
func LireReglagesVmalert(ctx context.Context, db *sql.DB) (ReglagesVmalert, error) {
	r := ReglagesVmalertDefaut()
	var v string
	err := db.QueryRowContext(ctx, `SELECT valeur FROM reglages WHERE cle = ?`, cleVmalert).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return r, nil
	}
	if err != nil {
		return r, err
	}
	dec := json.NewDecoder(strings.NewReader(v))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return r, fmt.Errorf("réglages de comparaison abîmés : %w", err)
	}
	return r, nil
}

// EcrireReglagesVmalert enregistre les réglages (audit).
func EcrireReglagesVmalert(ctx context.Context, db *sql.DB, r ReglagesVmalert, qui string, t time.Time) error {
	if err := r.Valider(); err != nil {
		return err
	}
	b, _ := json.Marshal(r)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // sans effet après Commit
	if _, err := tx.ExecContext(ctx, `INSERT INTO reglages(cle, valeur, modifie_le, modifie_par) VALUES (?, ?, ?, ?)
		ON CONFLICT(cle) DO UPDATE SET valeur = excluded.valeur, modifie_le = excluded.modifie_le, modifie_par = excluded.modifie_par`,
		cleVmalert, string(b), t.Unix(), qui); err != nil {
		return err
	}
	if err := audit(ctx, tx, t.Unix(), qui, "reglages_vmalert", cleVmalert, "", string(b)); err != nil {
		return err
	}
	return tx.Commit()
}

// AlerteVmalert : une alerte active de vmalert (champs utiles).
type AlerteVmalert struct {
	Nom        string            `json:"name"`
	Etat       string            `json:"state"`
	Etiquettes map[string]string `json:"labels"`
}

// LireVmalert : alertes actives de vmalert (4 Mio et 10 s au plus, pas de
// redirection suivie).
func LireVmalert(ctx context.Context, adresse string) ([]AlerteVmalert, error) {
	ctx, annuler := context.WithTimeout(ctx, 10*time.Second)
	defer annuler()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(adresse, "/")+"/api/v1/alerts", nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("vmalert injoignable : %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("vmalert : HTTP %d", resp.StatusCode)
	}
	var r struct {
		Data struct {
			Alerts []AlerteVmalert `json:"alerts"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&r); err != nil {
		return nil, fmt.Errorf("réponse de vmalert illisible : %w", err)
	}
	return r.Data.Alerts, nil
}

// Ecart : une règle sur une machine, comparée des deux côtés.
type Ecart struct {
	Regle, Machine   string
	Vmalert, Mirador int    // nombre d'alertes actives de chaque côté
	Type             string // commune, manquante, en_trop
}

// Comparer : alertes actives des deux côtés, pour les machines connues de
// mirador seulement (pendant la période en parallèle, vmalert surveille tout
// le parc et mirador une partie). Noms ramenés à ceux de vmalert.
func Comparer(r ReglagesVmalert, vm []AlerteVmalert, mi []Alerte, machines map[string]bool) []Ecart {
	ignorer := map[string]bool{}
	for _, n := range r.Ignorer {
		ignorer[n] = true
	}
	nomVm := func(m string) string {
		if c, ok := r.Correspondances[m]; ok {
			return c
		}
		return strings.TrimSuffix(m, r.Suffixe)
	}
	suivies := map[string]bool{}
	for m := range machines {
		suivies[nomVm(m)] = true
	}
	type cle struct{ regle, machine string }
	comptes := map[cle]*Ecart{}
	compter := func(regle, machine string) *Ecart {
		k := cle{regle, machine}
		if comptes[k] == nil {
			comptes[k] = &Ecart{Regle: regle, Machine: machine}
		}
		return comptes[k]
	}
	for _, a := range vm {
		m := a.Etiquettes["instance"]
		if m == "" {
			m = a.Etiquettes["host"]
		}
		if a.Etat != "firing" || ignorer[a.Nom] || !suivies[m] {
			continue
		}
		compter(a.Nom, m).Vmalert++
	}
	for _, a := range mi {
		if a.Etat != Active || ignorer[a.Regle] {
			continue
		}
		m := nomVm(a.Machine())
		if !suivies[m] {
			continue
		}
		compter(a.Regle, m).Mirador++
	}
	out := make([]Ecart, 0, len(comptes))
	for _, e := range comptes {
		switch {
		case e.Vmalert > 0 && e.Mirador > 0:
			e.Type = "commune"
		case e.Vmalert > 0:
			e.Type = "manquante"
		default:
			e.Type = "en_trop"
		}
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Type != out[j].Type {
			return out[i].Type > out[j].Type
		}
		if out[i].Machine != out[j].Machine {
			return out[i].Machine < out[j].Machine
		}
		return out[i].Regle < out[j].Regle
	})
	return out
}

// NoterEcarts met à jour les épisodes d'écart (table ecarts_vmalert) : un
// écart qui apparaît ouvre un épisode, un écart qui disparaît le ferme.
func NoterEcarts(ctx context.Context, db *sql.DB, ecarts []Ecart, t time.Time) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // sans effet après Commit
	ouverts := map[string]int64{}
	rows, err := tx.QueryContext(ctx, `SELECT id, regle || '|' || machine || '|' || type FROM ecarts_vmalert WHERE fin = 0`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id int64
		var k string
		if err := rows.Scan(&id, &k); err != nil {
			_ = rows.Close()
			return err
		}
		ouverts[k] = id
	}
	_ = rows.Close()
	for _, e := range ecarts {
		if e.Type == "commune" {
			continue
		}
		k := e.Regle + "|" + e.Machine + "|" + e.Type
		if _, ok := ouverts[k]; ok {
			delete(ouverts, k)
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO ecarts_vmalert(regle, machine, type, debut) VALUES (?, ?, ?, ?)`,
			e.Regle, e.Machine, e.Type, t.UnixMilli()); err != nil {
			return err
		}
	}
	for _, id := range ouverts {
		if _, err := tx.ExecContext(ctx, `UPDATE ecarts_vmalert SET fin = ? WHERE id = ?`, t.UnixMilli(), id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Episode : un écart et sa durée.
type Episode struct {
	Regle, Machine, Type string
	Debut, Fin           time.Time
}

// ListerEpisodes : écarts depuis la date donnée, plus récents d'abord.
func ListerEpisodes(ctx context.Context, db *sql.DB, depuis time.Time) ([]Episode, error) {
	rows, err := db.QueryContext(ctx, `SELECT regle, machine, type, debut, fin FROM ecarts_vmalert
		WHERE fin = 0 OR fin >= ? ORDER BY debut DESC LIMIT 10000`, depuis.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Episode
	for rows.Next() {
		var e Episode
		var d, f int64
		if err := rows.Scan(&e.Regle, &e.Machine, &e.Type, &d, &f); err != nil {
			return nil, err
		}
		e.Debut, e.Fin = depuisMs(d), depuisMs(f)
		out = append(out, e)
	}
	return out, rows.Err()
}
