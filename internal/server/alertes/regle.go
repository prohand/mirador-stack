// SPDX-License-Identifier: GPL-3.0-or-later

// Package alertes : moteur d'alertes de mirador-server (SPEC.md § 9 et § 17).
//
//   - règles en base (table regles), rechargées sans redémarrage ;
//   - expressions PromQL restreintes (langage.go), évaluées sur la base brute
//     des métriques ;
//   - état des alertes en mémoire, écrit en base seulement quand il change
//     (début, passage en alerte, fin) ;
//   - import des règles vmalert actuelles et format YAML mirador (format.go).
package alertes

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"text/template"
	"time"

	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/promql/parser"
)

// Gravités (valeurs de l'étiquette severity, comme aujourd'hui).
const (
	Critique      = "critical"
	Avertissement = "warning"
	Info          = "info"
)

// Duree : durée lue et écrite comme dans vmalert (« 30s », « 10m », « 1h »,
// « 1d », « 1w »).
type Duree time.Duration

func (d Duree) String() string {
	if d == 0 {
		return "0s"
	}
	return model.Duration(d).String()
}

// LireDuree lit « 30s », « 5m », « 1d »… (« 0 » accepté).
func LireDuree(s string) (Duree, error) {
	if s == "" || s == "0" {
		return 0, nil
	}
	d, err := model.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("durée %q invalide (ex. 30s, 5m, 1h, 1d)", s)
	}
	return Duree(d), nil
}

// MarshalJSON / UnmarshalJSON : texte lisible en base et dans l'export.
func (d Duree) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

func (d *Duree) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := LireDuree(s)
	*d = v
	return err
}

// MarshalYAML / UnmarshalYAML (export et import YAML).
func (d Duree) MarshalYAML() (any, error) { return d.String(), nil }

func (d *Duree) UnmarshalYAML(lire func(any) error) error {
	var s string
	if err := lire(&s); err != nil {
		return err
	}
	v, err := LireDuree(s)
	*d = v
	return err
}

// Regle : une règle d'alerte. Les champs reprennent ceux de vmalert, en
// français ; Groupe et Intervalle remplacent les groupes de vmalert.
type Regle struct {
	Nom         string            `json:"nom" yaml:"nom"`                                   // devient l'étiquette alertname
	Groupe      string            `json:"groupe" yaml:"groupe"`                             // classement et intervalle commun
	Intervalle  Duree             `json:"intervalle" yaml:"intervalle"`                     // fréquence d'évaluation
	Expr        string            `json:"expr" yaml:"expr"`                                 // PromQL restreint
	Pour        Duree             `json:"pour,omitempty" yaml:"pour,omitempty"`             // vrai pendant au moins…
	Garder      Duree             `json:"garder,omitempty" yaml:"garder,omitempty"`         // reste active … après la fin (anti-clignotement)
	Gravite     string            `json:"gravite" yaml:"gravite"`                           // critical, warning, info
	Etiquettes  map[string]string `json:"etiquettes,omitempty" yaml:"etiquettes,omitempty"` // ajoutées à l'alerte
	Resume      string            `json:"resume,omitempty" yaml:"resume,omitempty"`         // modèle (summary)
	Description string            `json:"description,omitempty" yaml:"description,omitempty"`
	Active      bool              `json:"active" yaml:"active"`
	Origine     string            `json:"origine,omitempty" yaml:"origine,omitempty"` // vmalert, mirador, modèle…
	Note        string            `json:"note,omitempty" yaml:"note,omitempty"`       // commentaire libre
}

var (
	reNom       = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,63}$`)
	reEtiquette = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]{0,63}$`)
)

// Bornes.
const (
	IntervalleMin = 15 * time.Second
	IntervalleMax = time.Hour
	PourMax       = 7 * 24 * time.Hour
	FenetreMax    = 7 * 24 * time.Hour // rétention brute par défaut
	TailleMaxTxt  = 1024               // résumé, description
	EtiquettesMax = 16
)

// Defauts complète les champs vides (intervalle 1 min, groupe « general »).
func (r *Regle) Defauts() {
	if r.Intervalle == 0 {
		r.Intervalle = Duree(time.Minute)
	}
	if r.Groupe == "" {
		r.Groupe = "general"
	}
}

// Valider vérifie une règle et renvoie son expression analysée.
func (r *Regle) Valider() (parser.Expr, error) {
	var errs []error
	if !reNom.MatchString(r.Nom) {
		errs = append(errs, fmt.Errorf("nom %q invalide (lettres, chiffres, _ ; 64 au plus)", r.Nom))
	}
	if !reNom.MatchString(r.Groupe) {
		errs = append(errs, fmt.Errorf("groupe %q invalide", r.Groupe))
	}
	if iv := time.Duration(r.Intervalle); iv < IntervalleMin || iv > IntervalleMax {
		errs = append(errs, fmt.Errorf("intervalle %s hors bornes (%s à %s)", r.Intervalle, IntervalleMin, IntervalleMax))
	}
	if p := time.Duration(r.Pour); p < 0 || p > PourMax {
		errs = append(errs, fmt.Errorf("durée « pour » %s hors bornes (0 à 7 j)", r.Pour))
	}
	if g := time.Duration(r.Garder); g < 0 || g > 24*time.Hour {
		errs = append(errs, fmt.Errorf("durée « garder » %s hors bornes (0 à 24 h)", r.Garder))
	}
	switch r.Gravite {
	case Critique, Avertissement, Info:
	default:
		errs = append(errs, fmt.Errorf("gravité %q invalide (critical, warning, info)", r.Gravite))
	}
	if len(r.Etiquettes) > EtiquettesMax {
		errs = append(errs, fmt.Errorf("%d étiquettes au plus", EtiquettesMax))
	}
	for k, v := range r.Etiquettes {
		if !reEtiquette.MatchString(k) || strings.HasPrefix(k, "__") {
			errs = append(errs, fmt.Errorf("étiquette %q invalide", k))
		}
		if k == "alertname" || k == "severity" {
			errs = append(errs, fmt.Errorf("étiquette %q réservée (nom et gravité de la règle)", k))
		}
		if len(v) > 256 {
			errs = append(errs, fmt.Errorf("valeur de l'étiquette %q trop longue", k))
		}
	}
	for nom, txt := range map[string]string{"résumé": r.Resume, "description": r.Description} {
		if len(txt) > TailleMaxTxt {
			errs = append(errs, fmt.Errorf("%s trop long (%d octets au plus)", nom, TailleMaxTxt))
		} else if _, err := compilerModele(txt); err != nil {
			errs = append(errs, fmt.Errorf("%s : %w", nom, err))
		}
	}
	if len(r.Note) > TailleMaxTxt {
		errs = append(errs, errors.New("note trop longue"))
	}
	e, err := AnalyserExpr(r.Expr)
	if err != nil {
		errs = append(errs, err)
	} else if f := fenetreMax(e); time.Duration(f)*time.Millisecond > FenetreMax {
		errs = append(errs, fmt.Errorf("l'expression lit %s en arrière, au-delà des 7 jours gardés en brut",
			Duree(time.Duration(f)*time.Millisecond)))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("règle %s : %w", r.Nom, err)
	}
	return e, nil
}

// --- Modèles de texte (résumé, description) -------------------------------------

// Comme vmalert : {{ $labels.instance }}, {{ $value }}, {{ $value | humanize }}.
// Fonctions limitées à la mise en forme (aucune requête, aucun accès externe) ;
// texte limité à TailleMaxTxt.

var fonctionsModele = template.FuncMap{
	"humanize":           humaniser,
	"humanize1024":       humaniser1024,
	"humanizePercentage": func(v float64) string { return humaniser(v*100) + "%" },
	"humanizeDuration":   humaniserDuree,
	"toUpper":            strings.ToUpper,
	"toLower":            strings.ToLower,
}

const enteteModele = "{{$labels := .Labels}}{{$value := .Value}}"

func compilerModele(txt string) (*template.Template, error) {
	if txt == "" {
		return nil, nil
	}
	return template.New("").Option("missingkey=zero").Funcs(fonctionsModele).Parse(enteteModele + txt)
}

type donneesModele struct {
	Labels map[string]string
	Value  float64
}

// limiteEcriture coupe le texte produit (protection mémoire).
type limiteEcriture struct {
	b   strings.Builder
	max int
}

func (l *limiteEcriture) Write(p []byte) (int, error) {
	if l.b.Len()+len(p) > l.max {
		return 0, errors.New("texte trop long")
	}
	return l.b.Write(p)
}

// remplir exécute un modèle ; en cas d'erreur, le texte brut est rendu (une
// alerte ne doit jamais être perdue à cause de son texte).
func remplir(t *template.Template, brut string, etiquettes map[string]string, valeur float64) string {
	if t == nil {
		return ""
	}
	w := &limiteEcriture{max: 2 * TailleMaxTxt}
	if err := t.Execute(w, donneesModele{Labels: etiquettes, Value: valeur}); err != nil {
		return brut
	}
	return w.b.String()
}

func humaniser(v float64) string {
	if v == 0 || v != v {
		return fmt.Sprintf("%.4g", v)
	}
	a := v
	if a < 0 {
		a = -a
	}
	switch {
	case a >= 1:
		suffixes := []string{"", "k", "M", "G", "T", "P", "E"}
		i := 0
		for a >= 1000 && i < len(suffixes)-1 {
			a /= 1000
			v /= 1000
			i++
		}
		return fmt.Sprintf("%.4g%s", v, suffixes[i])
	default:
		suffixes := []string{"", "m", "u", "n", "p", "f", "a"}
		i := 0
		for a < 1 && i < len(suffixes)-1 {
			a *= 1000
			v *= 1000
			i++
		}
		return fmt.Sprintf("%.4g%s", v, suffixes[i])
	}
}

func humaniser1024(v float64) string {
	a := v
	if a < 0 {
		a = -a
	}
	if a < 1 {
		return fmt.Sprintf("%.4g", v)
	}
	suffixes := []string{"", "ki", "Mi", "Gi", "Ti", "Pi", "Ei"}
	i := 0
	for a >= 1024 && i < len(suffixes)-1 {
		a /= 1024
		v /= 1024
		i++
	}
	return fmt.Sprintf("%.4g%s", v, suffixes[i])
}

// humaniserDuree : secondes → « 1d 2h 3m 4s » (comme Prometheus).
func humaniserDuree(v float64) string {
	if v != v || v < 0 {
		return fmt.Sprintf("%.4g", v)
	}
	if v < 1 {
		return humaniser(v) + "s"
	}
	s := int64(v)
	j, h, m := s/86400, s/3600%24, s/60%60
	s %= 60
	switch {
	case j > 0:
		return fmt.Sprintf("%dd %dh %dm %ds", j, h, m, s)
	case h > 0:
		return fmt.Sprintf("%dh %dm %ds", h, m, s)
	case m > 0:
		return fmt.Sprintf("%dm %ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}
