// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Conversion d'un tableau Grafana (JSON exporté) vers le format mirador.
// Sert une fois, pour les 8 tableaux actuels (reference/grafana/), et
// pourra resservir pour un nouveau tableau fait dans Grafana :
//
//	CONVERTIR_GRAFANA=1 go test ./internal/server/console -run TestConvertirGrafana
//
// Repris : rangées, panneaux stat / table / bargauge / timeseries, requêtes
// PromQL, légendes, unités, décimales, seuils, correspondances de valeurs,
// colonnes des tableaux (transformation « organize » et surcharges par nom).
// Non repris (listés dans « ignores ») : panneaux de logs (LogsQL), carte du
// monde, et toute requête qui n'est pas du PromQL.

type gDashboard struct {
	Title      string                `json:"title"`
	Time       struct{ From string } `json:"time"`
	Panels     []gPanel              `json:"panels"`
	Templating struct {
		List []struct {
			Name  string          `json:"name"`
			Label string          `json:"label"`
			Type  string          `json:"type"`
			Query json.RawMessage `json:"query"`
		} `json:"list"`
	} `json:"templating"`
}

type gPanel struct {
	Type        string     `json:"type"`
	Title       string     `json:"title"`
	Description string     `json:"description"`
	Collapsed   bool       `json:"collapsed"`
	Panels      []gPanel   `json:"panels"`
	Datasource  any        `json:"datasource"`
	Targets     []gTarget  `json:"targets"`
	FieldConfig gFieldConf `json:"fieldConfig"`
	Transforms  []struct {
		ID      string `json:"id"`
		Options struct {
			Exclude map[string]bool   `json:"excludeByName"`
			Index   map[string]int    `json:"indexByName"`
			Rename  map[string]string `json:"renameByName"`
		} `json:"options"`
	} `json:"transformations"`
}

type gTarget struct {
	RefID      string `json:"refId"`
	Expr       string `json:"expr"`
	Legend     string `json:"legendFormat"`
	Datasource any    `json:"datasource"`
}

type gDefaults struct {
	Unit       string     `json:"unit"`
	Decimals   *int       `json:"decimals"`
	Thresholds *gSeuils   `json:"thresholds"`
	Mappings   []gMapping `json:"mappings"`
}

type gFieldConf struct {
	Defaults  gDefaults `json:"defaults"`
	Overrides []struct {
		Matcher struct {
			ID      string `json:"id"`
			Options any    `json:"options"`
		} `json:"matcher"`
		Properties []struct {
			ID    string          `json:"id"`
			Value json.RawMessage `json:"value"`
		} `json:"properties"`
	} `json:"overrides"`
}

type gSeuils struct {
	Steps []struct {
		Color string   `json:"color"`
		Value *float64 `json:"value"`
	} `json:"steps"`
}

type gMapping struct {
	Type    string          `json:"type"`
	Options json.RawMessage `json:"options"`
}

type gTexte struct {
	Text  string `json:"text"`
	Color string `json:"color"`
}

func couleurGrafana(c string) string {
	switch {
	case strings.Contains(c, "green"):
		return "ok"
	case strings.Contains(c, "red"):
		return "erreur"
	case strings.Contains(c, "orange"), strings.Contains(c, "yellow"):
		return "attention"
	case strings.Contains(c, "blue"):
		return "info"
	}
	return ""
}

func seuilsGrafana(s *gSeuils) []Seuil {
	if s == nil {
		return nil
	}
	var out []Seuil
	for _, st := range s.Steps {
		out = append(out, Seuil{Valeur: st.Value, Couleur: couleurGrafana(st.Color)})
	}
	// Un seul seuil « vert dès 0 » n'apporte rien : on l'enlève.
	if len(out) == 1 {
		return nil
	}
	return out
}

func correspondancesGrafana(ms []gMapping) []Correspondance {
	var out []Correspondance
	for _, m := range ms {
		switch m.Type {
		case "value":
			var o map[string]gTexte
			if json.Unmarshal(m.Options, &o) != nil {
				continue
			}
			cles := make([]string, 0, len(o))
			for k := range o {
				cles = append(cles, k)
			}
			sort.Strings(cles)
			for _, k := range cles {
				out = append(out, Correspondance{Valeur: k, Texte: o[k].Text, Couleur: couleurGrafana(o[k].Color)})
			}
		case "range":
			var o struct {
				From, To *float64
				Result   gTexte
			}
			if json.Unmarshal(m.Options, &o) == nil {
				out = append(out, Correspondance{De: o.From, A: o.To, Texte: o.Result.Text, Couleur: couleurGrafana(o.Result.Color)})
			}
		}
	}
	return out
}

// estLogs : la source de données est VictoriaLogs (ou autre que Prometheus).
func estLogs(ds any) bool {
	s := fmt.Sprint(ds)
	return strings.Contains(s, "logs") || strings.Contains(s, "DSL")
}

var rePeriodeGrafana = regexp.MustCompile(`^now-([0-9]+[mhdw])$`)

// DepuisGrafana convertit un tableau Grafana.
func DepuisGrafana(nom string, brut []byte) (*Tableau, error) {
	var g gDashboard
	if err := json.Unmarshal(brut, &g); err != nil {
		return nil, err
	}
	t := &Tableau{Version: 1, Nom: nom, Titre: g.Title, Periode: "6h"}
	if m := rePeriodeGrafana.FindStringSubmatch(g.Time.From); m != nil {
		t.Periode = strings.Replace(m[1], "w", "d", 1)
		if strings.HasSuffix(m[1], "w") {
			t.Periode = fmt.Sprint(7*atoiSur(strings.TrimSuffix(m[1], "w"))) + "d"
		}
	}
	for _, v := range g.Templating.List {
		if v.Type != "query" {
			continue
		}
		var q string
		if json.Unmarshal(v.Query, &q) != nil {
			var o struct{ Query string }
			if json.Unmarshal(v.Query, &o) != nil || o.Query == "" {
				continue // variable LogsQL : ignorée
			}
			q = o.Query
		}
		if !strings.HasPrefix(strings.TrimSpace(q), "label_values(") {
			continue
		}
		titre := v.Label
		if titre == "" {
			titre = v.Name
		}
		t.Variables = append(t.Variables, Variable{Nom: v.Name, Titre: titre, Requete: q})
	}
	courante := &Rangee{Titre: ""}
	ajouterRangee := func() {
		if len(courante.Panneaux) > 0 {
			t.Rangees = append(t.Rangees, *courante)
		}
	}
	var convertir func(ps []gPanel)
	convertir = func(ps []gPanel) {
		for _, p := range ps {
			if p.Type == "row" {
				ajouterRangee()
				courante = &Rangee{Titre: p.Title, Enroulee: p.Collapsed}
				convertir(p.Panels)
				continue
			}
			pn, raison := panneauGrafana(p)
			if raison != "" {
				t.Ignores = append(t.Ignores, p.Title+" : "+raison)
				continue
			}
			courante.Panneaux = append(courante.Panneaux, pn)
		}
	}
	convertir(g.Panels)
	ajouterRangee()
	return t, t.Valider()
}

func atoiSur(s string) int {
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}

func panneauGrafana(p gPanel) (Panneau, string) {
	pn := Panneau{Titre: p.Title, Description: p.Description, Unite: p.FieldConfig.Defaults.Unit,
		Decimales: p.FieldConfig.Defaults.Decimals, Seuils: seuilsGrafana(p.FieldConfig.Defaults.Thresholds),
		Correspondances: correspondancesGrafana(p.FieldConfig.Defaults.Mappings)}
	switch p.Type {
	case "stat":
		pn.Type = PanneauChiffre
	case "table":
		pn.Type = PanneauTableau
	case "bargauge":
		pn.Type = PanneauBarres
	case "timeseries":
		pn.Type = PanneauCourbe
	case "logs":
		return pn, "panneau de logs : voir la page Logs"
	case "geomap":
		return pn, "carte du monde : page « Flux bloqués »"
	default:
		return pn, "type " + p.Type + " non repris"
	}
	for _, tg := range p.Targets {
		if tg.Expr == "" || estLogs(tg.Datasource) || (tg.Datasource == nil && estLogs(p.Datasource)) {
			return pn, "requête LogsQL : voir la page Logs"
		}
		pn.Requetes = append(pn.Requetes, Requete{ID: tg.RefID, Expr: tg.Expr, Legende: tg.Legend})
	}
	if len(pn.Requetes) == 0 {
		return pn, "aucune requête"
	}
	if pn.Type == PanneauTableau {
		pn.Colonnes, pn.Masquer = colonnesGrafana(p, pn.Requetes)
	}
	return pn, ""
}

// colonnesGrafana : ordre, titres et réglages des colonnes d'un tableau.
// Grafana nomme les valeurs « Value #A » (plusieurs requêtes) ou « Value ».
func colonnesGrafana(p gPanel, rs []Requete) ([]Colonne, []string) {
	champ := func(nomG string) string {
		if strings.HasPrefix(nomG, "Value #") {
			return strings.TrimPrefix(nomG, "Value #")
		}
		if nomG == "Value" && len(rs) > 0 {
			return rs[0].ID
		}
		return nomG
	}
	var masquer []string
	index := map[string]int{}
	rename := map[string]string{}
	for _, tr := range p.Transforms {
		if tr.ID != "organize" {
			continue
		}
		for k, v := range tr.Options.Exclude {
			if v {
				masquer = append(masquer, champ(k))
			}
		}
		for k, v := range tr.Options.Index {
			index[champ(k)] = v
		}
		for k, v := range tr.Options.Rename {
			rename[champ(k)] = v
		}
	}
	sort.Strings(masquer)
	var cols []Colonne
	for c, i := range index {
		_ = i
		cols = append(cols, Colonne{Champ: c, Titre: rename[c]})
	}
	// Requêtes sans place dans l'ordre : à la fin.
	for _, r := range rs {
		if _, ok := index[r.ID]; !ok {
			cols = append(cols, Colonne{Champ: r.ID, Titre: rename[r.ID]})
			index[r.ID] = 1000 + len(index)
		}
	}
	sort.Slice(cols, func(i, j int) bool { return index[cols[i].Champ] < index[cols[j].Champ] })
	// Surcharges « par nom » (le nom affiché après renommage).
	for i := range cols {
		if cols[i].Titre == "" {
			cols[i].Titre = cols[i].Champ
		}
		for _, o := range p.FieldConfig.Overrides {
			nomO := fmt.Sprint(o.Matcher.Options)
			if o.Matcher.ID != "byName" || (nomO != cols[i].Titre && champ(nomO) != cols[i].Champ) {
				continue
			}
			for _, pr := range o.Properties {
				switch pr.ID {
				case "unit":
					_ = json.Unmarshal(pr.Value, &cols[i].Unite)
				case "decimals":
					var d int
					if json.Unmarshal(pr.Value, &d) == nil {
						cols[i].Decimales = &d
					}
				case "thresholds":
					var s gSeuils
					if json.Unmarshal(pr.Value, &s) == nil {
						cols[i].Seuils = seuilsGrafana(&s)
					}
				case "mappings":
					var ms []gMapping
					if json.Unmarshal(pr.Value, &ms) == nil {
						cols[i].Correspondances = correspondancesGrafana(ms)
					}
				case "displayName":
					_ = json.Unmarshal(pr.Value, &cols[i].Titre)
				}
			}
		}
	}
	return cols, masquer
}
