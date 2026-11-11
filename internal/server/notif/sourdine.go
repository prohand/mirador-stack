// SPDX-License-Identifier: GPL-3.0-or-later

// Package notif : ce qui se passe entre une alerte et un message (SPEC.md
// § 17.6 et suivants) : mise en sourdine (silences, maintenances,
// inhibitions, dépendances), regroupement, rappels, mode « ombre », puis
// envoi par les canaux (partie 3).
package notif

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	_ "time/tzdata" // fuseaux horaires embarqués : l'image Docker n'en a pas

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"

	"mirador-stack/internal/server/alertes"
)

// --- Cibles et sélecteurs ----------------------------------------------------------

// Une cible désigne des alertes :
//   - machine:NOM  → étiquette instance (ou host, ou src_ip) = NOM ;
//   - groupe:NOM   → machines du groupe NOM (réglé sur chaque machine) ;
//   - {…}          → sélecteur d'étiquettes PromQL, ex. {via="collecteur"}.

var analyseSelecteur = parser.NewParser(parser.Options{})

// TailleMaxSelecteur : longueur maximale d'un sélecteur.
const TailleMaxSelecteur = 1024

// LireSelecteur vérifie et lit un sélecteur d'étiquettes ({nom="valeur",…}).
func LireSelecteur(s string) ([]*labels.Matcher, error) {
	if len(s) > TailleMaxSelecteur {
		return nil, errors.New("sélecteur trop long")
	}
	if !strings.HasPrefix(strings.TrimSpace(s), "{") {
		return nil, fmt.Errorf("sélecteur %q : attendu {nom=\"valeur\", …}", s)
	}
	ms, err := analyseSelecteur.ParseMetricSelector(s)
	if err != nil {
		return nil, fmt.Errorf("sélecteur %q : %w", s, err)
	}
	// Au moins un filtre qui ne correspond pas à une valeur vide : sinon le
	// sélecteur viserait toutes les alertes (silence sur tout par erreur).
	for _, m := range ms {
		if !m.Matches("") {
			return ms, nil
		}
	}
	return nil, fmt.Errorf("sélecteur %q : il viserait toutes les alertes (au moins un filtre précis, ex. instance=\"pc1\")", s)
}

func correspond(ms []*labels.Matcher, etq labels.Labels) bool {
	for _, m := range ms {
		if !m.Matches(etq.Get(m.Name)) {
			return false
		}
	}
	return true
}

type cible struct {
	machine, groupe string
	filtres         []*labels.Matcher
}

// LireCible vérifie une cible (machine:NOM, groupe:NOM ou sélecteur).
func LireCible(s string) (cible, error) {
	if n, ok := strings.CutPrefix(s, "machine:"); ok && n != "" {
		return cible{machine: n}, nil
	}
	if n, ok := strings.CutPrefix(s, "groupe:"); ok && n != "" {
		return cible{groupe: n}, nil
	}
	ms, err := LireSelecteur(s)
	if err != nil {
		return cible{}, fmt.Errorf("cible : machine:NOM, groupe:NOM ou {sélecteur} (%w)", err)
	}
	return cible{filtres: ms}, nil
}

// touche : l'alerte est-elle visée ? groupes : machine → groupe.
func (c cible) touche(a *alertes.Alerte, groupes map[string]string) bool {
	switch {
	case c.machine != "":
		return a.Machine() == c.machine
	case c.groupe != "":
		return groupes[a.Machine()] == c.groupe
	}
	return correspond(c.filtres, a.Etiquettes)
}

// --- Éléments de sourdine ------------------------------------------------------------

// Silence : alertes tues pendant une période.
type Silence struct {
	ID                  int64
	Selecteur           string
	Debut, Fin          time.Time
	Auteur, Commentaire string
	filtres             []*labels.Matcher
}

// Recurrence : maintenance qui revient (ex. chaque dimanche à 03:00 pendant 2 h).
type Recurrence struct {
	Jours  []string      `json:"jours"` // lun, mar, mer, jeu, ven, sam, dim
	Heure  string        `json:"heure"` // HH:MM, heure locale du fuseau
	Duree  alertes.Duree `json:"duree"`
	Fuseau string        `json:"fuseau"` // défaut Europe/Paris
}

var joursSemaine = []string{"dim", "lun", "mar", "mer", "jeu", "ven", "sam"} // ordre de time.Weekday

// Valider vérifie une récurrence.
func (r *Recurrence) Valider() error {
	if len(r.Jours) == 0 {
		return errors.New("récurrence : au moins un jour (lun…dim)")
	}
	for _, j := range r.Jours {
		if !slices.Contains(joursSemaine, j) {
			return fmt.Errorf("jour %q inconnu (lun, mar, mer, jeu, ven, sam, dim)", j)
		}
	}
	if _, err := time.Parse("15:04", r.Heure); err != nil {
		return fmt.Errorf("heure %q invalide (HH:MM)", r.Heure)
	}
	if d := time.Duration(r.Duree); d < time.Minute || d > 7*24*time.Hour {
		return errors.New("durée de maintenance entre 1 min et 7 j")
	}
	if r.Fuseau == "" {
		r.Fuseau = "Europe/Paris"
	}
	if _, err := time.LoadLocation(r.Fuseau); err != nil {
		return fmt.Errorf("fuseau %q inconnu", r.Fuseau)
	}
	return nil
}

// enCours : la récurrence couvre-t-elle l'instant t ?
func (r *Recurrence) enCours(t time.Time) bool {
	loc, err := time.LoadLocation(r.Fuseau)
	if err != nil {
		return false
	}
	h, _ := time.Parse("15:04", r.Heure)
	tl := t.In(loc)
	// Une fenêtre peut avoir commencé les jours précédents (durée ≤ 7 j).
	for k := 0; k <= 7; k++ {
		j := tl.AddDate(0, 0, -k)
		debut := time.Date(j.Year(), j.Month(), j.Day(), h.Hour(), h.Minute(), 0, 0, loc)
		if !slices.Contains(r.Jours, joursSemaine[debut.Weekday()]) {
			continue
		}
		if !t.Before(debut) && t.Before(debut.Add(time.Duration(r.Duree))) {
			return true
		}
	}
	return false
}

// Maintenance : fenêtre ponctuelle (Debut, Fin) ou récurrente.
type Maintenance struct {
	ID                  int64
	Cible               string
	Debut, Fin          time.Time
	Recurrence          *Recurrence
	Auteur, Commentaire string
	cible               cible
}

func (m *Maintenance) enCours(t time.Time) bool {
	if m.Recurrence != nil {
		return m.Recurrence.enCours(t)
	}
	return !t.Before(m.Debut) && t.Before(m.Fin)
}

// Inhibition : une alerte active « source » tue les alertes « cible » qui
// ont les mêmes valeurs pour les étiquettes Egales.
type Inhibition struct {
	ID              int64
	Source, Cible   string
	Egales          []string
	Commentaire     string
	fsource, fcible []*labels.Matcher
}

// Dependance : les alertes de l'enfant sont tues quand le parent est en panne.
type Dependance struct {
	ID             int64
	Enfant, Parent string
	Commentaire    string
	enfant         cible
}

// --- Sourdine : tout ce qui est chargé ---------------------------------------------

// Sourdine : éléments en vigueur, relus en base quand ils changent.
type Sourdine struct {
	silences     []Silence
	maintenances []Maintenance
	inhibitions  []Inhibition
	dependances  []Dependance
	groupes      map[string]string // machine → groupe
	reglesPanne  map[string]bool   // règles qui signifient « machine en panne »
}

// Raison renvoie pourquoi une alerte est en sourdine à l'instant t ("" si elle
// ne l'est pas). actives : alertes actives (sources d'inhibition, pannes des
// parents). Ordre : maintenance, silence, dépendance, inhibition.
func (s *Sourdine) Raison(a *alertes.Alerte, actives []alertes.Alerte, t time.Time) string {
	if s == nil {
		return ""
	}
	for i := range s.maintenances {
		m := &s.maintenances[i]
		if m.enCours(t) && m.cible.touche(a, s.groupes) {
			return fmt.Sprintf("maintenance %d (%s)", m.ID, m.Cible)
		}
	}
	for i := range s.silences {
		x := &s.silences[i]
		if !t.Before(x.Debut) && t.Before(x.Fin) && correspond(x.filtres, a.Etiquettes) {
			return fmt.Sprintf("silence %d", x.ID)
		}
	}
	if p := s.parentEnPanne(a, actives); p != "" {
		return "dépendance : " + p + " en panne"
	}
	for i := range s.inhibitions {
		in := &s.inhibitions[i]
		if !correspond(in.fcible, a.Etiquettes) {
			continue
		}
		for j := range actives {
			src := &actives[j]
			if src.Etiquettes.Hash() == a.Etiquettes.Hash() || !correspond(in.fsource, src.Etiquettes) {
				continue
			}
			egal := true
			for _, e := range in.Egales {
				egal = egal && src.Etiquettes.Get(e) == a.Etiquettes.Get(e)
			}
			if egal {
				return fmt.Sprintf("inhibée par %s (%s)", src.Regle, src.Machine())
			}
		}
	}
	return ""
}

// parentEnPanne : nom du parent en panne dont dépend l'alerte ("" sinon).
// Les alertes du parent lui-même ne sont jamais tues par sa propre panne.
func (s *Sourdine) parentEnPanne(a *alertes.Alerte, actives []alertes.Alerte) string {
	for i := range s.dependances {
		d := &s.dependances[i]
		if a.Machine() == d.Parent || !d.enfant.touche(a, s.groupes) {
			continue
		}
		for j := range actives {
			if p := &actives[j]; s.reglesPanne[p.Regle] && p.Machine() == d.Parent {
				return d.Parent
			}
		}
	}
	return ""
}

// Charger lit les éléments en vigueur (les silences finis ou annulés sont
// laissés en base pour l'historique, mais pas chargés).
func Charger(ctx context.Context, db *sql.DB, reglesPanne []string, t time.Time) (*Sourdine, error) {
	s := &Sourdine{groupes: map[string]string{}, reglesPanne: map[string]bool{}}
	for _, r := range reglesPanne {
		s.reglesPanne[r] = true
	}
	var err error
	if s.silences, err = ListerSilences(ctx, db, false, t); err != nil {
		return nil, err
	}
	if s.maintenances, err = ListerMaintenances(ctx, db); err != nil {
		return nil, err
	}
	if s.inhibitions, err = ListerInhibitions(ctx, db); err != nil {
		return nil, err
	}
	if s.dependances, err = ListerDependances(ctx, db); err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `SELECT nom, groupe FROM machines WHERE groupe != ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var n, g string
		if err := rows.Scan(&n, &g); err != nil {
			return nil, err
		}
		s.groupes[n] = g
	}
	return s, rows.Err()
}

// --- Base : lecture -------------------------------------------------------------------

func ms(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func depuisMs(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.UnixMilli(v)
}

// ListerSilences : silences en vigueur ou à venir (tous = avec les finis et
// annulés, pour l'historique), plus récents d'abord.
func ListerSilences(ctx context.Context, db *sql.DB, tous bool, t time.Time) ([]Silence, error) {
	q := `SELECT id, selecteur, debut, fin, auteur, commentaire FROM silences`
	var args []any
	if !tous {
		q += ` WHERE annule_le = 0 AND fin > ?`
		args = append(args, ms(t))
	}
	rows, err := db.QueryContext(ctx, q+` ORDER BY id DESC LIMIT 1000`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Silence
	for rows.Next() {
		var x Silence
		var d, f int64
		if err := rows.Scan(&x.ID, &x.Selecteur, &d, &f, &x.Auteur, &x.Commentaire); err != nil {
			return nil, err
		}
		x.Debut, x.Fin = depuisMs(d), depuisMs(f)
		if x.filtres, err = LireSelecteur(x.Selecteur); err != nil {
			return nil, fmt.Errorf("silence %d abîmé en base : %w", x.ID, err)
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// ListerMaintenances : toutes les fenêtres.
func ListerMaintenances(ctx context.Context, db *sql.DB) ([]Maintenance, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, cible, debut, fin, recurrence, auteur, commentaire FROM maintenances ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Maintenance
	for rows.Next() {
		var m Maintenance
		var d, f int64
		var rec string
		if err := rows.Scan(&m.ID, &m.Cible, &d, &f, &rec, &m.Auteur, &m.Commentaire); err != nil {
			return nil, err
		}
		m.Debut, m.Fin = depuisMs(d), depuisMs(f)
		if rec != "" {
			m.Recurrence = &Recurrence{}
			if err := json.Unmarshal([]byte(rec), m.Recurrence); err != nil {
				return nil, fmt.Errorf("maintenance %d abîmée en base : %w", m.ID, err)
			}
		}
		if m.cible, err = LireCible(m.Cible); err != nil {
			return nil, fmt.Errorf("maintenance %d abîmée en base : %w", m.ID, err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ListerInhibitions : toutes les inhibitions.
func ListerInhibitions(ctx context.Context, db *sql.DB) ([]Inhibition, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, source, cible, egales, commentaire FROM inhibitions ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Inhibition
	for rows.Next() {
		var in Inhibition
		var eg string
		if err := rows.Scan(&in.ID, &in.Source, &in.Cible, &eg, &in.Commentaire); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(eg), &in.Egales); err != nil {
			return nil, err
		}
		if in.fsource, err = LireSelecteur(in.Source); err != nil {
			return nil, err
		}
		if in.fcible, err = LireSelecteur(in.Cible); err != nil {
			return nil, err
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

// ListerDependances : toutes les dépendances.
func ListerDependances(ctx context.Context, db *sql.DB) ([]Dependance, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, enfant, parent, commentaire FROM dependances ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Dependance
	for rows.Next() {
		var d Dependance
		if err := rows.Scan(&d.ID, &d.Enfant, &d.Parent, &d.Commentaire); err != nil {
			return nil, err
		}
		if d.enfant, err = LireCible(d.Enfant); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
