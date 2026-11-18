// SPDX-License-Identifier: GPL-3.0-or-later

// Package motifs : messages de logs regroupés en « motifs » (SPEC § 52,
// idée reprise de New Relic « log patterns », idée seulement). Deux étapes :
//  1. parties variables remplacées par un repère : UUID, IP, HEX, N
//     (nombres) — comme avant (SPEC § 33) ;
//  2. messages de même forme qui ne diffèrent que par quelques mots :
//     ces mots deviennent MOT (méthode « Drain » : même nombre de mots,
//     même premier mot, au moins 60 % de mots en commun).
//
// Utilisé par la page Logs (onglet « Messages regroupés », sur les lignes
// trouvées) et par le registre des motifs (toutes les lignes reçues, pour
// « vu la 1re fois » et « nouveau »).
package motifs

import (
	"fmt"
	"hash/fnv"
	"regexp"
	"sort"
	"strings"
)

// Segment : morceau de texte affiché ; Var : partie variable (repère).
type Segment struct {
	Texte string
	Var   bool
}

// Repères.
const (
	RepereMot = "MOT"
	marque    = "\x00" // encadre un repère dans un gabarit
)

// Parties variables remplacées, dans cet ordre (la première qui convient).
// besoin : test rapide fait avant l'expression (elle ne peut pas trouver
// sans lui) : la plupart des mots n'en essaient qu'une ou deux.
var variables = []struct {
	re     *regexp.Regexp
	nom    string
	besoin func(mot string) bool
}{
	{regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`), "UUID",
		func(m string) bool { return len(m) >= 36 && strings.Count(m, "-") >= 4 }},
	{regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`), "IP", func(m string) bool { return strings.Count(m, ".") >= 3 }},
	{regexp.MustCompile(`(?i)\b(?:[0-9a-f]{1,4}:){5,7}[0-9a-f]{1,4}\b`), "IP", func(m string) bool { return strings.Count(m, ":") >= 5 }},
	{regexp.MustCompile(`(?i)\b[0-9a-f]{1,4}(?::[0-9a-f]{1,4})*::(?:[0-9a-f]{1,4}(?::[0-9a-f]{1,4})*)?\b`), "IP",
		func(m string) bool { return strings.Contains(m, "::") }},
	{regexp.MustCompile(`(?i)\b0x[0-9a-f]+\b`), "HEX", func(m string) bool { return strings.Contains(m, "0x") || strings.Contains(m, "0X") }},
	{regexp.MustCompile(`(?i)\b[0-9a-f]*\d[0-9a-f]*[a-f][0-9a-f]*\b|(?i)\b[0-9a-f]*[a-f][0-9a-f]*\d[0-9a-f]*\b`), "HEX", // hexadécimal mêlé
		func(m string) bool { return strings.ContainsAny(m, "abcdefABCDEF") }},
	{regexp.MustCompile(`\b\d+(?:[.,]\d+)*\b`), "N", func(string) bool { return true }},
}

// TailleMax : au-delà, la fin d'un message ne compte pas.
const TailleMax = 2000

// Segments : message avec ses parties variables remplacées (étape 1).
// Les expressions ne sont essayées que sur les mots qui contiennent un
// chiffre (toutes en demandent un) : 10 à 20 fois moins de travail sur une
// ligne ordinaire.
func Segments(msg string) []Segment {
	if len(msg) > TailleMax {
		msg = msg[:TailleMax]
	}
	type trou struct {
		debut, fin int
		nom        string
	}
	var trous []trou
	for debut := 0; debut < len(msg); {
		// Mot suivant (séparé par des espaces).
		for debut < len(msg) && (msg[debut] == ' ' || msg[debut] == '\t') {
			debut++
		}
		fin := debut
		chiffre := false
		for fin < len(msg) && msg[fin] != ' ' && msg[fin] != '\t' {
			if msg[fin] >= '0' && msg[fin] <= '9' {
				chiffre = true
			}
			fin++
		}
		if chiffre {
			mot := msg[debut:fin]
			pris := make([]bool, len(mot))
			for _, v := range variables {
				if !v.besoin(mot) {
					continue
				}
				for _, m := range v.re.FindAllStringIndex(mot, -1) {
					libre := true
					for i := m[0]; i < m[1]; i++ {
						if pris[i] {
							libre = false
							break
						}
					}
					// Hexadécimal court : sans doute un mot (« cafe », « bed »).
					if !libre || (v.nom == "HEX" && m[1]-m[0] < 6 && !strings.HasPrefix(strings.ToLower(mot[m[0]:m[1]]), "0x")) {
						continue
					}
					for i := m[0]; i < m[1]; i++ {
						pris[i] = true
					}
					trous = append(trous, trou{debut + m[0], debut + m[1], v.nom})
				}
			}
		}
		debut = fin
	}
	sort.Slice(trous, func(i, j int) bool { return trous[i].debut < trous[j].debut })
	var out []Segment
	pos := 0
	for _, t := range trous {
		if t.debut > pos {
			out = append(out, Segment{Texte: msg[pos:t.debut]})
		}
		out = append(out, Segment{Texte: t.nom, Var: true})
		pos = t.fin
	}
	if pos < len(msg) {
		out = append(out, Segment{Texte: msg[pos:]})
	}
	return out
}

// Jetons : mots du message après l'étape 1 ; un repère est encadré par
// marque (« \x00IP\x00 »), collé ou non à du texte (« port=\x00N\x00 »).
func Jetons(msg string) []string {
	var b strings.Builder
	for _, s := range Segments(msg) {
		if s.Var {
			b.WriteString(marque + s.Texte + marque)
		} else {
			b.WriteString(s.Texte)
		}
	}
	js := strings.Fields(b.String())
	if len(js) > 64 { // très longues lignes : la fin compte peu
		js = js[:64]
	}
	return js
}

// variable : jeton qui n'est qu'un repère (IP, N, MOT…).
func variable(j string) bool {
	return len(j) > 2 && strings.HasPrefix(j, marque) && strings.HasSuffix(j, marque) && strings.Count(j, marque) == 2
}

// Gabarit : texte d'un motif (jetons séparés par une espace).
func Gabarit(js []string) string { return strings.Join(js, " ") }

// SegmentsGabarit : gabarit → segments à afficher.
func SegmentsGabarit(g string) []Segment {
	var out []Segment
	parts := strings.Split(g, marque)
	for i, p := range parts {
		if p == "" {
			continue
		}
		out = append(out, Segment{Texte: p, Var: i%2 == 1})
	}
	return out
}

// Texte : gabarit lisible (« Invalid user ‹MOT› from ‹IP› »).
func Texte(g string) string {
	var b strings.Builder
	for _, s := range SegmentsGabarit(g) {
		if s.Var {
			b.WriteString("‹" + s.Texte + "›")
		} else {
			b.WriteString(s.Texte)
		}
	}
	return b.String()
}

// Cle : empreinte courte d'un gabarit (lien « ces lignes »).
func Cle(g string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(g))
	return fmt.Sprintf("%012x", h.Sum64()&0xffffffffffff)
}

// Seuil : part minimale de jetons identiques pour fusionner deux messages.
const Seuil = 0.6

// seau : messages de même forme (nombre de jetons, premier jeton).
type seau struct {
	n    int
	tete string
}

func seauDe(js []string) seau {
	s := seau{n: len(js)}
	if len(js) > 0 && !variable(js[0]) {
		s.tete = js[0]
	}
	return s
}

// ressemblance : part de jetons égaux (ou repère du gabarit).
func ressemblance(gabarit, js []string) float64 {
	if len(gabarit) == 0 {
		return 1
	}
	egaux := 0
	for i, g := range gabarit {
		if g == js[i] || variable(g) && g == js[i] {
			egaux++
		}
	}
	return float64(egaux) / float64(len(gabarit))
}

// fusionner : positions différentes → MOT (ou repère commun).
func fusionner(gabarit, js []string) {
	for i, g := range gabarit {
		if g != js[i] {
			gabarit[i] = marque + RepereMot + marque
		}
	}
}

// Couvre : le gabarit (plus général) couvre ces jetons (même forme ; chaque
// jeton égal, ou repère MOT dans le gabarit).
func Couvre(gabarit, js []string) bool {
	if len(gabarit) != len(js) {
		return false
	}
	for i, g := range gabarit {
		if g != js[i] && g != marque+RepereMot+marque {
			return false
		}
	}
	return true
}

// Regroupeur : regroupement d'un ensemble de messages (une recherche).
type Regroupeur struct {
	seaux map[seau][]*Groupe
}

// Groupe : un motif et les messages rangés dedans (indices).
type Groupe struct {
	Jetons  []string
	Indices []int
}

// NouveauRegroupeur : vide.
func NouveauRegroupeur() *Regroupeur { return &Regroupeur{seaux: map[seau][]*Groupe{}} }

// Ajouter : message i ; renvoie son groupe.
func (r *Regroupeur) Ajouter(i int, msg string) *Groupe {
	js := Jetons(msg)
	k := seauDe(js)
	var meilleur *Groupe
	mieux := -1.0
	for _, g := range r.seaux[k] {
		if s := ressemblance(g.Jetons, js); s > mieux {
			mieux, meilleur = s, g
		}
	}
	if meilleur != nil && mieux >= Seuil {
		fusionner(meilleur.Jetons, js)
		meilleur.Indices = append(meilleur.Indices, i)
		return meilleur
	}
	g := &Groupe{Jetons: append([]string(nil), js...), Indices: []int{i}}
	r.seaux[k] = append(r.seaux[k], g)
	return g
}

// Groupes : tous les groupes formés.
func (r *Regroupeur) Groupes() []*Groupe {
	var out []*Groupe
	for _, gs := range r.seaux {
		out = append(out, gs...)
	}
	return out
}
