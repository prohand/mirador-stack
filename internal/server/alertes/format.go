// SPDX-License-Identifier: GPL-3.0-or-later

package alertes

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Formats d'échange des règles (SPEC.md § 17.2) :
//   - YAML mirador, versionné (« version: 1 »), pour l'export, la sauvegarde
//     et le suivi dans Git ;
//   - YAML vmalert (system.yml, equipements.yml) en import seulement, avec un
//     rapport règle par règle.

// VersionFormat : version du format YAML mirador.
const VersionFormat = 1

// TailleMaxFichier : taille maximale d'un fichier de règles importé.
const TailleMaxFichier = 2 << 20

// FichierMirador : format YAML mirador.
type FichierMirador struct {
	Version int     `yaml:"version"`
	Regles  []Regle `yaml:"regles"`
}

// Exporter écrit les règles au format YAML mirador.
func Exporter(w io.Writer, regles []Regle) error {
	enc := yaml.NewEncoder(w)
	enc.SetIndent(2)
	if _, err := io.WriteString(w, "# Règles d'alerte de mirador-server (format version 1).\n"); err != nil {
		return err
	}
	if err := enc.Encode(FichierMirador{Version: VersionFormat, Regles: regles}); err != nil {
		return err
	}
	return enc.Close()
}

// Classements du rapport d'import.
const (
	Exact      = "exact"      // même expression, même comportement
	Approche   = "approché"   // importée avec une adaptation (voir la note)
	Impossible = "impossible" // non importée, ou importée désactivée
)

// LigneRapport : une règle du fichier importé.
type LigneRapport struct {
	Nom, Groupe, Classement, Note string
	Importee, Active              bool
}

// Rapport d'import.
type Rapport struct {
	Format string // « mirador » ou « vmalert »
	Lignes []LigneRapport
	Regles []Regle // règles à enregistrer
}

// Compter : nombre de lignes par classement.
func (r *Rapport) Compter() map[string]int {
	n := map[string]int{}
	for _, l := range r.Lignes {
		n[l.Classement]++
	}
	return n
}

// Ecrire : rapport lisible (commande regles-importer, interface plus tard).
func (r *Rapport) Ecrire(w io.Writer) {
	n := r.Compter()
	fmt.Fprintf(w, "Format %s : %d règle(s) — %d exacte(s), %d approchée(s), %d impossible(s)\n",
		r.Format, len(r.Lignes), n[Exact], n[Approche], n[Impossible])
	for _, l := range r.Lignes {
		etat := "importée"
		switch {
		case !l.Importee:
			etat = "non importée"
		case !l.Active:
			etat = "importée désactivée"
		}
		fmt.Fprintf(w, "  %-36s %-10s %-20s %s\n", l.Nom, l.Classement, etat, l.Note)
	}
}

// Importer lit un fichier de règles (YAML mirador ou vmalert, reconnu tout
// seul) et prépare les règles et le rapport. Rien n'est enregistré ici.
func Importer(r io.Reader, nomFichier string) (*Rapport, error) {
	brut, err := io.ReadAll(io.LimitReader(r, TailleMaxFichier+1))
	if err != nil {
		return nil, err
	}
	if len(brut) > TailleMaxFichier {
		return nil, fmt.Errorf("fichier trop gros (%d Mio au plus)", TailleMaxFichier>>20)
	}
	var tete struct {
		Version *int `yaml:"version"`
		Groups  any  `yaml:"groups"`
	}
	if err := yaml.Unmarshal(brut, &tete); err != nil {
		return nil, fmt.Errorf("YAML illisible : %w", err)
	}
	switch {
	case tete.Version != nil:
		return importerMirador(brut)
	case tete.Groups != nil:
		return importerVmalert(brut, nomFichier)
	}
	return nil, errors.New("format inconnu : ni « version: » (mirador) ni « groups: » (vmalert)")
}

func importerMirador(brut []byte) (*Rapport, error) {
	var f FichierMirador
	dec := yaml.NewDecoder(bytes.NewReader(brut))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("YAML mirador : %w", err)
	}
	if f.Version != VersionFormat {
		return nil, fmt.Errorf("version %d du format non gérée (attendue : %d)", f.Version, VersionFormat)
	}
	rap := &Rapport{Format: "mirador"}
	for _, r := range f.Regles {
		r.Defauts()
		if _, err := r.Valider(); err != nil {
			return nil, err // format mirador : tout ou rien
		}
		rap.Regles = append(rap.Regles, r)
		rap.Lignes = append(rap.Lignes, LigneRapport{Nom: r.Nom, Groupe: r.Groupe, Classement: Exact,
			Importee: true, Active: r.Active})
	}
	return rap, nil
}

// Format vmalert (sous-ensemble utilisé par les règles actuelles).
type fichierVmalert struct {
	Groups []struct {
		Name     string         `yaml:"name"`
		Interval string         `yaml:"interval"`
		Rules    []regleVmalert `yaml:"rules"`
	} `yaml:"groups"`
}

type regleVmalert struct {
	Alert         string            `yaml:"alert"`
	Record        string            `yaml:"record"`
	Expr          string            `yaml:"expr"`
	For           string            `yaml:"for"`
	KeepFiringFor string            `yaml:"keep_firing_for"`
	Labels        map[string]string `yaml:"labels"`
	Annotations   map[string]string `yaml:"annotations"`
}

// Métriques qui sont le résultat d'une règle d'enregistrement de vmalert sur
// les logs : n'existent pas dans mirador.
var metriquesEnregistrees = map[string]string{
	"syslog_lignes_24h": "remplacée à l'étape 6 par « dernière ligne reçue » tenu à l'ingestion syslog",
}

// prefixesAgent : métriques produites par mirador-agent (et son assistant root).
var prefixesAgent = []string{"node_", "ssl_", "smartctl_", "mirador_"}

func importerVmalert(brut []byte, nomFichier string) (*Rapport, error) {
	var f fichierVmalert
	dec := yaml.NewDecoder(bytes.NewReader(brut))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("YAML vmalert : %w", err)
	}
	rap := &Rapport{Format: "vmalert"}
	vus := map[string]bool{}
	for _, g := range f.Groups {
		groupe := strings.ReplaceAll(g.Name, "-", "_")
		iv := Duree(time.Minute)
		if g.Interval != "" {
			d, err := LireDuree(g.Interval)
			if err != nil {
				return nil, fmt.Errorf("groupe %s : %w", g.Name, err)
			}
			iv = d
		}
		for _, v := range g.Rules {
			ligne, regle := convertir(v, groupe, iv, nomFichier)
			if regle != nil {
				if vus[regle.Nom] {
					ligne.Classement, ligne.Importee, ligne.Note = Impossible, false, "nom en double dans le fichier"
					regle = nil
				} else {
					vus[regle.Nom] = true
					rap.Regles = append(rap.Regles, *regle)
				}
			}
			rap.Lignes = append(rap.Lignes, ligne)
		}
	}
	sort.SliceStable(rap.Lignes, func(i, j int) bool { return rap.Lignes[i].Groupe < rap.Lignes[j].Groupe })
	return rap, nil
}

// convertir une règle vmalert ; regle == nil si elle n'est pas importée.
func convertir(v regleVmalert, groupe string, iv Duree, fichier string) (LigneRapport, *Regle) {
	l := LigneRapport{Nom: v.Alert, Groupe: groupe, Classement: Exact}
	if v.Record != "" {
		l.Nom, l.Classement, l.Note = v.Record, Impossible, "règle d'enregistrement : non gérée (calcul fait par mirador quand c'est utile)"
		return l, nil
	}
	if v.Alert == "Watchdog" {
		l.Classement, l.Note = Impossible, "remplacée par le heartbeat HetrixTools du serveur (indépendant des règles)"
		return l, nil
	}
	var notes []string
	r := &Regle{Nom: v.Alert, Groupe: groupe, Intervalle: iv, Expr: strings.TrimSpace(v.Expr), Active: true,
		Origine: "vmalert:" + fichier, Etiquettes: map[string]string{}}
	var err error
	if r.Pour, err = LireDuree(v.For); err != nil {
		l.Classement, l.Note = Impossible, err.Error()
		return l, nil
	}
	if r.Garder, err = LireDuree(v.KeepFiringFor); err != nil {
		l.Classement, l.Note = Impossible, err.Error()
		return l, nil
	}
	for k, val := range v.Labels {
		if k == "severity" {
			r.Gravite = val
			continue
		}
		r.Etiquettes[k] = val
	}
	switch r.Gravite {
	case Critique, Avertissement, Info:
	case "":
		r.Gravite = Avertissement
		notes = append(notes, "sans gravité : warning")
		l.Classement = Approche
	default:
		notes = append(notes, fmt.Sprintf("gravité %q remplacée par warning", r.Gravite))
		r.Gravite = Avertissement
		l.Classement = Approche
	}
	for k, val := range v.Annotations {
		switch k {
		case "summary":
			r.Resume = val
		case "description":
			r.Description = val
		default:
			notes = append(notes, "annotation "+k+" ignorée")
			l.Classement = Approche
		}
	}
	e, err := r.Valider()
	if err != nil {
		l.Classement, l.Note = Impossible, strings.TrimPrefix(err.Error(), "règle "+r.Nom+" : ")
		return l, nil
	}
	sansAgent := false
	for _, nom := range metriquesLues(e) {
		if raison, ok := metriquesEnregistrees[nom]; ok {
			r.Active = false
			r.Note = "désactivée à l'import : " + raison
			l.Classement, l.Note = Impossible, "lit "+nom+" (règle d'enregistrement de vmalert-logs) ; "+raison
			l.Importee, l.Active = true, false
			return l, r
		}
		agent := false
		for _, p := range prefixesAgent {
			agent = agent || strings.HasPrefix(nom, p)
		}
		sansAgent = sansAgent || !agent
	}
	if sansAgent {
		notes = append(notes, "source sans agent : données attendues à l'étape 6")
	}
	if strings.Contains(r.Expr, "lag(") {
		notes = append(notes, "lag() : fonction MetricsQL reprise par mirador")
	}
	l.Note = strings.Join(notes, " ; ")
	l.Importee, l.Active = true, true
	return l, r
}
