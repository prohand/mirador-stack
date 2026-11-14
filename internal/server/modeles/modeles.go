// SPDX-License-Identifier: GPL-3.0-or-later

// Package modeles : modèles de surveillance (SPEC.md § 24). Un modèle dit
// « cette machine est une machine Linux (ou Docker, Proxmox…), donc on y
// surveille ceci, avec ces seuils et ces alertes ». Il est attribué à une
// machine ou à un groupe depuis l'interface, et proposé quand l'agent
// détecte ce qu'il faut.
//
// Un modèle ne contient que des valeurs typées de la configuration des
// agents (vérifiées par le code de l'agent : jamais de commande) et des
// règles PromQL (même lecteur borné que les autres règles).
package modeles

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"mirador-stack/internal/agent/config"
	"mirador-stack/internal/agent/distant"
	"mirador-stack/internal/server/alertes"
)

//go:embed fournis/*.json
var fichiersFournis embed.FS

// Modele : un modèle de surveillance.
type Modele struct {
	Version     int             `json:"version"`
	Nom         string          `json:"nom"`
	Titre       string          `json:"titre"`
	Description string          `json:"description,omitempty"`
	Icone       string          `json:"icone,omitempty"`
	Detection   string          `json:"detection,omitempty"`
	Config      json.RawMessage `json:"config,omitempty"`
	Parametres  []Parametre     `json:"parametres,omitempty"`
	Regles      []RegleModele   `json:"regles,omitempty"`
	// Services : services systemd attendus sur les machines du modèle
	// (liste de départ ; modifiable dans l'interface, demande du 14/11).
	Services []string `json:"services,omitempty"`
	// Listes : adresses, ports, fichiers… à tester par l'agent (partie B,
	// SPEC § 24.10). Chaque ligne d'une liste devient un check.
	Listes []ListeParam `json:"listes,omitempty"`
}

// ListeParam : liste réglable par groupe puis par machine ; chaque ligne
// devient un check du type indiqué (config.TypesCheck).
type ListeParam struct {
	Nom     string `json:"nom"`
	Titre   string `json:"titre"`
	Type    string `json:"type"`
	Exemple string `json:"exemple,omitempty"`
	Aide    string `json:"aide,omitempty"`
}

// Parametre : seuil nommé, réglable par groupe puis par machine.
type Parametre struct {
	Nom    string  `json:"nom"`
	Titre  string  `json:"titre"`
	Unite  string  `json:"unite,omitempty"`
	Defaut float64 `json:"defaut"`
	Min    float64 `json:"min"`
	Max    float64 `json:"max"`
}

// RegleModele : règle d'alerte du modèle.
type RegleModele struct {
	Nom         string `json:"nom"`
	Remplace    string `json:"remplace,omitempty"` // règle (importée ou fournie) qu'elle remplace
	Expr        string `json:"expr"`
	Pour        string `json:"pour,omitempty"`
	Gravite     string `json:"gravite"`
	Resume      string `json:"resume,omitempty"`
	Description string `json:"description,omitempty"`
}

var (
	// ReService : nom d'unité systemd accepté dans un modèle.
	ReService      = regexp.MustCompile(`^[A-Za-z0-9:_.@\\-]{1,200}\.(service|timer)$`)
	reNomModele    = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	reNomParametre = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	reRefParametre = regexp.MustCompile(`\$([a-z][a-z0-9_]{0,31})`)
)

// Valider vérifie un modèle : noms, configuration (code de l'agent),
// paramètres bornés, règles (PromQL restreint, $p après une comparaison).
func (m *Modele) Valider() error {
	if m.Version != 1 {
		return fmt.Errorf("version %d inconnue", m.Version)
	}
	if !reNomModele.MatchString(m.Nom) || m.Titre == "" {
		return errors.New("nom (minuscules, chiffres, « - ») et titre obligatoires")
	}
	if len(m.Config) > 0 && string(m.Config) != "null" {
		if _, err := distant.Decoder(bytes.NewReader(m.Config)); err != nil {
			return fmt.Errorf("configuration : %w", err)
		}
	}
	vus := map[string]bool{}
	for _, p := range m.Parametres {
		if !reNomParametre.MatchString(p.Nom) || vus[p.Nom] || p.Titre == "" {
			return fmt.Errorf("paramètre %q invalide ou en double", p.Nom)
		}
		if math.IsNaN(p.Min) || math.IsNaN(p.Max) || p.Min > p.Max || p.Defaut < p.Min || p.Defaut > p.Max {
			return fmt.Errorf("paramètre %s : bornes invalides", p.Nom)
		}
		vus[p.Nom] = true
	}
	if err := ValiderServices(m.Services); err != nil {
		return err
	}
	for _, l := range m.Listes {
		if !reNomParametre.MatchString(l.Nom) || vus[l.Nom] || l.Titre == "" || !config.TypesCheck[l.Type] {
			return fmt.Errorf("liste %q invalide ou en double", l.Nom)
		}
		vus[l.Nom] = true
	}
	if m.Detection != "" {
		if _, err := alertes.AnalyserExpr(m.Detection); err != nil {
			return fmt.Errorf("détection : %w", err)
		}
	}
	for _, r := range m.Regles {
		if err := verifierRefs(r.Expr, vus); err != nil {
			return fmt.Errorf("règle %s : %w", r.Nom, err)
		}
		ar := m.RegleAlerte(r)
		if _, err := ar.Valider(); err != nil {
			return fmt.Errorf("règle %s : %w", r.Nom, err)
		}
	}
	return nil
}

// verifierRefs : chaque $p est un paramètre du modèle, placé juste après un
// opérateur de comparaison (il y devient une série à comparer).
func verifierRefs(expr string, params map[string]bool) error {
	for _, loc := range reRefParametre.FindAllStringSubmatchIndex(expr, -1) {
		nom := expr[loc[2]:loc[3]]
		if !params[nom] {
			return fmt.Errorf("paramètre $%s inconnu", nom)
		}
		avant := strings.TrimRight(expr[:loc[0]], " ")
		ok := false
		for _, op := range []string{"<=", ">=", "==", "!=", "<", ">"} {
			if strings.HasSuffix(avant, op) {
				ok = true
				break
			}
		}
		if !ok {
			return fmt.Errorf("$%s doit suivre une comparaison (<, >, <=, >=, ==, !=)", nom)
		}
	}
	return nil
}

// ExprRegle : expression d'une règle, chaque $p remplacé par la série du
// seuil de chaque machine (écrite par le serveur, SPEC § 24.2).
func (m *Modele) ExprRegle(r RegleModele) string {
	return reRefParametre.ReplaceAllStringFunc(r.Expr, func(ref string) string {
		return `on (instance) group_left () mirador_modele_parametre{modele="` + m.Nom + `", parametre="` + ref[1:] + `"}`
	})
}

// RegleAlerte : la règle telle qu'enregistrée dans la base des règles.
func (m *Modele) RegleAlerte(r RegleModele) alertes.Regle {
	var pour alertes.Duree
	if r.Pour != "" {
		pour, _ = alertes.LireDuree(r.Pour)
	}
	return alertes.Regle{Nom: r.Nom, Groupe: "modele_" + strings.ReplaceAll(m.Nom, "-", "_"), Intervalle: alertes.Duree(time.Minute),
		Expr: m.ExprRegle(r), Pour: pour, Gravite: r.Gravite, Resume: r.Resume, Description: r.Description,
		Active: true, Origine: "modele:" + m.Nom,
		Note: "Modèle « " + m.Titre + " » : seuils réglables par groupe et par machine (page de la machine)"}
}

// Defaut : valeur par défaut d'un paramètre (0 s'il n'existe pas).
func (m *Modele) Defaut(nom string) (float64, bool) {
	for _, p := range m.Parametres {
		if p.Nom == nom {
			return p.Defaut, true
		}
	}
	return 0, false
}

// LireValeur : valeur saisie pour un paramètre, vérifiée (nombre, bornes).
func (m *Modele) LireValeur(nom, texte string) (float64, error) {
	for _, p := range m.Parametres {
		if p.Nom != nom {
			continue
		}
		v, err := strconv.ParseFloat(strings.ReplaceAll(strings.TrimSpace(texte), ",", "."), 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
			return 0, fmt.Errorf("%s : nombre attendu", p.Titre)
		}
		if v < p.Min || v > p.Max {
			return 0, fmt.Errorf("%s : entre %g et %g", p.Titre, p.Min, p.Max)
		}
		return v, nil
	}
	return 0, fmt.Errorf("paramètre %q inconnu", nom)
}

// fournis : modèles livrés avec mirador, chargés une fois.
var fournis = func() map[string]*Modele {
	out := map[string]*Modele{}
	entrees, err := fichiersFournis.ReadDir("fournis")
	if err != nil {
		panic(err)
	}
	for _, e := range entrees {
		brut, err := fichiersFournis.ReadFile(path.Join("fournis", e.Name()))
		if err != nil {
			panic(err)
		}
		var m Modele
		dec := json.NewDecoder(bytes.NewReader(brut))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&m); err != nil {
			panic(fmt.Sprintf("modèle %s : %v", e.Name(), err))
		}
		out[m.Nom] = &m
	}
	return out
}()

// Fournis : modèles livrés, triés par titre.
func Fournis() []*Modele {
	out := make([]*Modele, 0, len(fournis))
	for _, m := range fournis {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Titre < out[j].Titre })
	return out
}

// Trouver : modèle par son nom (nil s'il n'existe pas).
func Trouver(nom string) *Modele { return fournis[nom] }

// Regles : règles de tous les modèles, à poser au démarrage.
func Regles() []alertes.Regle {
	var out []alertes.Regle
	for _, m := range Fournis() {
		for _, r := range m.Regles {
			out = append(out, m.RegleAlerte(r))
		}
	}
	return out
}

// Remplacees : règle remplacée → modèles qui la remplacent.
func Remplacees() map[string][]string {
	out := map[string][]string{}
	for _, m := range Fournis() {
		for _, r := range m.Regles {
			if r.Remplace != "" {
				out[r.Remplace] = append(out[r.Remplace], m.Nom)
			}
		}
	}
	return out
}

// maxServices : services par modèle.
const maxServices = 100

// ValiderServices : noms d'unités systemd (.service, .timer), sans doublon.
func ValiderServices(l []string) error {
	if len(l) > maxServices {
		return fmt.Errorf("%d services au plus par modèle", maxServices)
	}
	vus := map[string]bool{}
	for _, n := range l {
		if !ReService.MatchString(n) {
			return fmt.Errorf("service %q invalide (nom d'unité en .service ou .timer)", n)
		}
		if vus[n] {
			return fmt.Errorf("service %q en double", n)
		}
		vus[n] = true
	}
	return nil
}
