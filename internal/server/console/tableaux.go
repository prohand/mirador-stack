// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"mirador-stack/internal/server/alertes"
)

// Tableaux de bord (SPEC § 18.4) : des données, pas du code. Chaque tableau
// est un fichier JSON (format mirador, versionné) : variables (filtres),
// rangées, panneaux, et pour chaque panneau des requêtes PromQL évaluées par
// le lecteur restreint (alertes.Lecteur). Les 8 tableaux de départ sont
// convertis une fois depuis les tableaux Grafana actuels (grafana.go) ; les
// panneaux de logs (LogsQL) sont remplacés par la page « Logs ».

//go:embed tableaux/*.json
var fichiersTableaux embed.FS

// Tableau : un tableau de bord.
type Tableau struct {
	Version     int        `json:"version"`
	Nom         string     `json:"nom"`
	Titre       string     `json:"titre"`
	Description string     `json:"description,omitempty"`
	Periode     string     `json:"periode"` // période affichée par défaut (6h, 30d…)
	Variables   []Variable `json:"variables,omitempty"`
	Rangees     []Rangee   `json:"rangees"`
	Ignores     []string   `json:"ignores,omitempty"` // panneaux non repris (logs, carte) et pourquoi
	// Icone et Teinte : pastille à côté du titre (liste et page du tableau).
	Icone  string `json:"icone,omitempty"`
	Teinte string `json:"teinte,omitempty"`
}

// Variable : un filtre (liste de valeurs d'une étiquette).
type Variable struct {
	Nom     string `json:"nom"`
	Titre   string `json:"titre"`
	Requete string `json:"requete"` // label_values(SELECTEUR, ETIQUETTE)
}

// Rangee : un groupe de panneaux.
type Rangee struct {
	Titre     string    `json:"titre"`
	Panneaux  []Panneau `json:"panneaux"`
	Enroulee  bool      `json:"enroulee,omitempty"`
	Seulement string    `json:"seulement,omitempty"`
}

// Panneau : chiffre, tableau, barres ou courbe.
type Panneau struct {
	Type            string           `json:"type"`
	Titre           string           `json:"titre"`
	Description     string           `json:"description,omitempty"`
	Unite           string           `json:"unite,omitempty"`
	Decimales       *int             `json:"decimales,omitempty"`
	Seuils          []Seuil          `json:"seuils,omitempty"`
	Correspondances []Correspondance `json:"correspondances,omitempty"`
	Requetes        []Requete        `json:"requetes"`
	Colonnes        []Colonne        `json:"colonnes,omitempty"`
	Masquer         []string         `json:"masquer,omitempty"`
	// Largeur en douzièmes de la rangée (3 = ¼, 4 = ⅓, 6 = ½, 8 = ⅔, 9 = ¾,
	// 12 = toute la largeur) ; 0 = selon le type (comme avant).
	Largeur int `json:"largeur,omitempty"`
	// Icone et Teinte : petite pastille dans l'en-tête (comme les cartes de
	// la vue d'ensemble) ; vide = pas d'icône.
	Icone  string `json:"icone,omitempty"`
	Teinte string `json:"teinte,omitempty"`
}

// iconesPanneaux : icônes proposées (dessinées dans morceaux.html).
var iconesPanneaux = []string{"machines", "terminal", "carte-mere", "cpu", "memoire", "disque", "stockage", "base",
	"reseau", "routeur", "wifi", "globe", "nuage", "mur", "bouclier", "cadenas", "cle", "temperature", "ventilateur",
	"eclair", "batterie", "jauge", "vitesse", "activite", "graphique", "horloge", "calendrier", "conteneur", "sauvegarde",
	"telechargement", "alertes", "attention", "ok", "envoi", "courrier", "telephone", "logs", "fichier", "dossier",
	"utilisateurs", "utilisateur", "maison", "camera", "imprimante", "ampoule", "etoile", "puce-sim", "tableaux"}

// teintesPanneaux : couleurs de la pastille.
var teintesPanneaux = []string{"bleu", "vert", "rouge", "orange", "violet", "gris"}

// largeursPermises : choix proposés dans l'éditeur.
var largeursPermises = []int{0, 3, 4, 6, 8, 9, 12}

// largeurChoisie : au moins un panneau de la rangée a une largeur choisie.
func largeurChoisie(r *Rangee) bool {
	for _, p := range r.Panneaux {
		if p.Largeur != 0 {
			return true
		}
	}
	return false
}

// Requete : une expression PromQL et sa légende ({{etiquette}}).
type Requete struct {
	ID      string `json:"id"`
	Expr    string `json:"expr"`
	Legende string `json:"legende,omitempty"`
}

// Seuil : à partir de Valeur (nil = tout en bas), la couleur.
type Seuil struct {
	Valeur  *float64 `json:"valeur"`
	Couleur string   `json:"couleur"` // ok, attention, erreur, info, ''
}

// Correspondance : valeur (ou plage De..A) affichée comme un texte (ex. 1 → « OK »).
type Correspondance struct {
	Valeur  string   `json:"valeur,omitempty"`
	De      *float64 `json:"de,omitempty"`
	A       *float64 `json:"a,omitempty"`
	Texte   string   `json:"texte"`
	Couleur string   `json:"couleur,omitempty"`
}

// Colonne d'un panneau « tableau » : une étiquette ou une requête (par son ID).
type Colonne struct {
	Champ           string           `json:"champ"`
	Titre           string           `json:"titre"`
	Unite           string           `json:"unite,omitempty"`
	Decimales       *int             `json:"decimales,omitempty"`
	Seuils          []Seuil          `json:"seuils,omitempty"`
	Correspondances []Correspondance `json:"correspondances,omitempty"`
}

// Types de panneaux.
const (
	PanneauChiffre = "chiffre"
	PanneauTableau = "tableau"
	PanneauBarres  = "barres"
	PanneauCourbe  = "courbe"
)

var reNomTableau = regexp.MustCompile(`^[a-z0-9-]{1,40}$`)

// Valider vérifie un tableau : structure, et chaque expression dans le
// langage restreint (avec des valeurs de variables fictives).
func (t *Tableau) Valider() error {
	var errs []error
	if t.Version != 1 {
		errs = append(errs, fmt.Errorf("version %d inconnue (1 attendue)", t.Version))
	}
	if !reNomTableau.MatchString(t.Nom) {
		errs = append(errs, errors.New("nom : minuscules, chiffres, tirets"))
	}
	if _, err := lirePeriode(t.Periode); err != nil {
		errs = append(errs, err)
	}
	if t.Icone != "" && !slices.Contains(iconesPanneaux, t.Icone) {
		errs = append(errs, fmt.Errorf("icône %q inconnue", t.Icone))
	}
	if t.Teinte != "" && !slices.Contains(teintesPanneaux, t.Teinte) {
		errs = append(errs, fmt.Errorf("teinte %q inconnue", t.Teinte))
	}
	vals := map[string]string{}
	for _, v := range t.Variables {
		if !reNomEtiquette.MatchString(v.Nom) {
			errs = append(errs, fmt.Errorf("variable %q invalide", v.Nom))
		}
		vals[v.Nom] = ""
		if _, err := requeteVariable(v.Requete, vals); err != nil {
			errs = append(errs, fmt.Errorf("variable %s : %w", v.Nom, err))
		}
	}
	n := 0
	for _, r := range t.Rangees {
		for _, p := range r.Panneaux {
			n++
			switch p.Type {
			case PanneauChiffre, PanneauTableau, PanneauBarres, PanneauCourbe:
			default:
				errs = append(errs, fmt.Errorf("%s : type %q inconnu", p.Titre, p.Type))
			}
			if p.Icone != "" && !slices.Contains(iconesPanneaux, p.Icone) {
				errs = append(errs, fmt.Errorf("%s : icône %q inconnue", p.Titre, p.Icone))
			}
			if p.Teinte != "" && !slices.Contains(teintesPanneaux, p.Teinte) {
				errs = append(errs, fmt.Errorf("%s : teinte %q inconnue", p.Titre, p.Teinte))
			}
			if !slices.Contains(largeursPermises, p.Largeur) {
				errs = append(errs, fmt.Errorf("%s : largeur %d non permise", p.Titre, p.Largeur))
			}
			if len(p.Requetes) == 0 || len(p.Requetes) > 12 {
				errs = append(errs, fmt.Errorf("%s : 1 à 12 requêtes", p.Titre))
			}
			for _, q := range p.Requetes {
				if _, err := alertes.AnalyserExpr(substituer(q.Expr, vals, time.Minute, 6*time.Hour)); err != nil {
					errs = append(errs, fmt.Errorf("%s (%s) : %w", p.Titre, q.ID, err))
				}
			}
		}
	}
	if n > 80 {
		errs = append(errs, errors.New("80 panneaux au plus"))
	}
	return errors.Join(errs...)
}

var reNomEtiquette = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]{0,63}$`)

// tableauxDepart : les tableaux embarqués, par nom.
var tableauxDepart = func() map[string]*Tableau {
	out := map[string]*Tableau{}
	entrees, _ := fs.ReadDir(fichiersTableaux, "tableaux")
	for _, e := range entrees {
		b, err := fichiersTableaux.ReadFile("tableaux/" + e.Name())
		if err != nil {
			panic(err)
		}
		var t Tableau
		if err := json.Unmarshal(b, &t); err != nil {
			panic(fmt.Sprintf("tableau %s : %v", e.Name(), err))
		}
		out[t.Nom] = &t
	}
	return out
}()

// --- Variables et substitution ------------------------------------------------------

// reValeurVariable : valeurs permises pour un filtre (aucun guillemet ni
// barre oblique inverse : rien ne peut sortir de la chaîne PromQL).
var reValeurVariable = regexp.MustCompile(`^[A-Za-z0-9 ._:/@+-]{1,128}$`)

var reVariable = regexp.MustCompile(`(=~|!~|=|!=)"\$\{?([a-zA-Z_][a-zA-Z0-9_]*)\}?"`)

// substituer remplace les variables dans une expression :
//   - etiq=~"$var" : vide (toutes) → =~".*" ; sinon → ="valeur" (égalité
//     exacte, pas d'expression régulière fournie par l'utilisateur) ;
//   - $__rate_interval, $__interval, $__range : calculés depuis le pas.
func substituer(expr string, vals map[string]string, pas, periode time.Duration) string {
	expr = reVariable.ReplaceAllStringFunc(expr, func(m string) string {
		s := reVariable.FindStringSubmatch(m)
		op, nom := s[1], s[2]
		v, connue := vals[nom]
		if !connue {
			return m
		}
		if v == "" {
			if op == "!~" || op == "!=" {
				return `!~"a^"` // expression qui ne correspond à rien : rien n'est exclu
			}
			return `=~".*"`
		}
		switch op {
		case "=~", "=":
			return `="` + v + `"`
		default:
			return `!="` + v + `"`
		}
	})
	ri := 4 * pas
	if ri < 2*time.Minute {
		ri = 2 * time.Minute
	}
	r := strings.NewReplacer("$__rate_interval", secondes(ri), "${__rate_interval}", secondes(ri),
		"$__interval", secondes(pas), "${__interval}", secondes(pas), "$__range", secondes(periode))
	return r.Replace(expr)
}

func secondes(d time.Duration) string { return strconv.Itoa(int(d.Seconds())) + "s" }

var reLabelValues = regexp.MustCompile(`^\s*label_values\(\s*(.+)\s*,\s*([a-zA-Z_][a-zA-Z0-9_]*)\s*\)\s*$`)

// requeteVariable : label_values(SEL, etiq) → expression PromQL qui donne
// une série par valeur de l'étiquette vue sur le dernier jour.
func requeteVariable(lv string, vals map[string]string) (string, error) {
	m := reLabelValues.FindStringSubmatch(lv)
	if m == nil {
		return "", errors.New("label_values(SÉLECTEUR, ÉTIQUETTE) attendu")
	}
	expr := "group by (" + m[2] + ") (last_over_time(" + substituer(m[1], vals, time.Minute, time.Hour) + "[1d]))"
	if _, err := alertes.AnalyserExpr(expr); err != nil {
		return "", err
	}
	return expr, nil
}

// lirePeriode : 15m, 6h, 7d, 30d…
func lirePeriode(s string) (time.Duration, error) {
	d, err := alertes.LireDuree(s)
	if err != nil || time.Duration(d) < 5*time.Minute || time.Duration(d) > 31*24*time.Hour {
		return 0, fmt.Errorf("période %q : entre 5 min et 31 j (ex. 6h, 7d)", s)
	}
	return time.Duration(d), nil
}

// --- Mise en forme des valeurs --------------------------------------------------------

// formater affiche une valeur selon l'unité (unités de Grafana reprises).
func formater(v float64, unite string, decimales *int) string {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return "—"
	}
	dec := func(def int) int {
		if decimales != nil {
			return *decimales
		}
		return def
	}
	nombre := func(x float64, d int) string {
		s := strconv.FormatFloat(x, 'f', d, 64)
		return strings.Replace(s, ".", ",", 1)
	}
	echelle := func(x float64, base float64, suffixes []string, d int) string {
		i := 0
		for math.Abs(x) >= base && i < len(suffixes)-1 {
			x /= base
			i++
		}
		if suffixes[i] == "" {
			return nombre(x, d)
		}
		return nombre(x, d) + " " + suffixes[i]
	}
	switch unite {
	case "percent":
		return nombre(v, dec(1)) + " %"
	case "percentunit":
		return nombre(v*100, dec(1)) + " %"
	case "bytes", "decbytes":
		return echelle(v, 1024, []string{"o", "Kio", "Mio", "Gio", "Tio", "Pio"}, dec(1))
	case "Bps", "binBps":
		return echelle(v, 1024, []string{"o/s", "Kio/s", "Mio/s", "Gio/s"}, dec(1))
	case "bps":
		return echelle(v, 1000, []string{"bit/s", "kbit/s", "Mbit/s", "Gbit/s", "Tbit/s"}, dec(1))
	case "pps":
		return echelle(v, 1000, []string{"p/s", "kp/s", "Mp/s"}, dec(1))
	case "ops", "reqps":
		return echelle(v, 1000, []string{"/s", "k/s", "M/s"}, dec(1))
	case "s", "dtdurations":
		return dureeLisible(time.Duration(v * float64(time.Second)))
	case "ms":
		return nombre(v, dec(0)) + " ms"
	case "d":
		return nombre(v, dec(0)) + " j"
	case "lignes/min":
		return echelle(v, 1000, []string{"", "k", "M"}, dec(1)) + " lignes/min"
	case "celsius":
		return nombre(v, dec(0)) + " °C"
	case "dBm":
		return nombre(v, dec(0)) + " dBm"
	case "watt":
		return nombre(v, dec(0)) + " W"
	case "dateTimeAsIso", "dateTimeAsLocal", "dateTimeAsUS", "dateTimeAsSystem", "dateTimeFromNow":
		// Grafana attend des millisecondes (« … * 1000 ») ; une valeur en
		// secondes reste acceptée : au-delà de 1e11, ce sont des ms (1e11 s =
		// an 5138, 1e11 ms = 1973).
		if math.Abs(v) >= 1e11 {
			v /= 1000
		}
		return time.Unix(int64(v), 0).In(fuseau.Load()).Format("02/01/2006 15:04")
	default: // short, none, ''
		if v == math.Trunc(v) && decimales == nil && math.Abs(v) < 1e15 {
			return echelle(v, 1000, []string{"", "k", "M", "G", "T"}, 0)
		}
		return echelle(v, 1000, []string{"", "k", "M", "G", "T"}, dec(2))
	}
}

// couleur : classe CSS de la valeur selon les seuils.
func couleur(v float64, seuils []Seuil) string {
	c := ""
	for _, s := range seuils {
		if s.Valeur == nil || v >= *s.Valeur {
			c = s.Couleur
		}
	}
	return c
}

// correspondance : texte et couleur pour une valeur connue (ex. 1 → OK).
func correspondance(v float64, cs []Correspondance) (string, string, bool) {
	for _, c := range cs {
		if c.Valeur != "" {
			if f, err := strconv.ParseFloat(c.Valeur, 64); err == nil && f == v {
				return c.Texte, c.Couleur, true
			}
			continue
		}
		if (c.De == nil || v >= *c.De) && (c.A == nil || v <= *c.A) && (c.De != nil || c.A != nil) {
			return c.Texte, c.Couleur, true
		}
	}
	return "", "", false
}

// legende : remplace {{etiquette}} par sa valeur.
var reLegende = regexp.MustCompile(`\{\{\s*([a-zA-Z_][a-zA-Z0-9_]*)\s*\}\}`)

func legende(modele string, etiq func(string) string, defaut string) string {
	if modele == "" || modele == "__auto" {
		return defaut
	}
	return reLegende.ReplaceAllStringFunc(modele, func(m string) string {
		return etiq(reLegende.FindStringSubmatch(m)[1])
	})
}
