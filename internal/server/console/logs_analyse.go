// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"fmt"
	"hash/fnv"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"mirador-stack/internal/server/stockage"
)

// Calculs de la page Logs (SPEC § 33) faits sur les lignes déjà trouvées,
// sans nouvelle requête : messages regroupés, histogramme, surlignage.

// segment : morceau de texte affiché, variable (« IP », « N ») ou surligné.
type segment struct {
	Texte  string
	Var    bool // partie variable d'un message type
	Marque bool // mot cherché
}

// --- Gravité ------------------------------------------------------------------------

// Niveaux rangés par gravité : « err et plus grave », « warning et plus grave ».
var (
	niveauxErreur        = []string{"err", "error", "crit", "critical", "alert", "emerg", "fatal", "panic"}
	niveauxAvertissement = []string{"warning", "warn"}
)

// classeNiveau : erreur, avertissement ou rien (couleur de la ligne).
func classeNiveau(niveau string) string {
	n := strings.ToLower(niveau)
	for _, x := range niveauxErreur {
		if n == x {
			return "err"
		}
	}
	for _, x := range niveauxAvertissement {
		if n == x {
			return "warn"
		}
	}
	return ""
}

// rangNiveau : 2 erreur, 1 avertissement, 0 autre (le plus grave d'un groupe).
func rangNiveau(niveau string) int {
	switch classeNiveau(niveau) {
	case "err":
		return 2
	case "warn":
		return 1
	}
	return 0
}

// --- Messages regroupés -------------------------------------------------------------

// Parties variables remplacées, dans cet ordre (la première qui convient).
var variablesMessage = []struct {
	re  *regexp.Regexp
	nom string
}{
	{regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`), "UUID"},
	{regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`), "IP"},
	{regexp.MustCompile(`(?i)\b(?:[0-9a-f]{1,4}:){5,7}[0-9a-f]{1,4}\b`), "IP"},
	{regexp.MustCompile(`(?i)\b[0-9a-f]{1,4}(?::[0-9a-f]{1,4})*::(?:[0-9a-f]{1,4}(?::[0-9a-f]{1,4})*)?\b`), "IP"},
	{regexp.MustCompile(`(?i)\b0x[0-9a-f]+\b`), "HEX"},
	{regexp.MustCompile(`(?i)\b[0-9a-f]*\d[0-9a-f]*[a-f][0-9a-f]*\b|(?i)\b[0-9a-f]*[a-f][0-9a-f]*\d[0-9a-f]*\b`), "HEX"}, // hexadécimal mêlé
	{regexp.MustCompile(`\b\d+(?:[.,]\d+)*\b`), "N"},
}

// motifMessage : message type (parties variables remplacées), en segments.
func motifMessage(msg string) []segment {
	if len(msg) > 2000 {
		msg = msg[:2000]
	}
	type trou struct {
		debut, fin int
		nom        string
	}
	var trous []trou
	pris := make([]bool, len(msg))
	for _, v := range variablesMessage {
		for _, m := range v.re.FindAllStringIndex(msg, -1) {
			libre := true
			for i := m[0]; i < m[1]; i++ {
				if pris[i] {
					libre = false
					break
				}
			}
			// Hexadécimal court : sans doute un mot (« cafe », « bed »).
			if !libre || (v.nom == "HEX" && m[1]-m[0] < 6 && !strings.HasPrefix(strings.ToLower(msg[m[0]:m[1]]), "0x")) {
				continue
			}
			for i := m[0]; i < m[1]; i++ {
				pris[i] = true
			}
			trous = append(trous, trou{m[0], m[1], v.nom})
		}
	}
	sort.Slice(trous, func(i, j int) bool { return trous[i].debut < trous[j].debut })
	var out []segment
	pos := 0
	for _, t := range trous {
		if t.debut > pos {
			out = append(out, segment{Texte: msg[pos:t.debut]})
		}
		out = append(out, segment{Texte: t.nom, Var: true})
		pos = t.fin
	}
	if pos < len(msg) {
		out = append(out, segment{Texte: msg[pos:]})
	}
	return out
}

// cleMotif : empreinte courte d'un message type (lien « ces lignes »).
func cleMotif(segs []segment) string {
	h := fnv.New64a()
	for _, s := range segs {
		if s.Var {
			_, _ = h.Write([]byte{0})
		}
		_, _ = h.Write([]byte(s.Texte))
	}
	return fmt.Sprintf("%012x", h.Sum64()&0xffffffffffff)
}

// groupeLogs : un message type et ses lignes.
type groupeLogs struct {
	Cle      string
	Motif    []segment
	N        int
	Niveau   string // le plus grave vu
	Classe   string
	Machines []string
	Derniere int64 // millisecondes
	Largeur  int   // barre : part du groupe le plus fréquent (0 à 100)
}

// TexteMachines : « pve1, srv-test » ou « 5 machines ».
func (g groupeLogs) TexteMachines() string {
	if len(g.Machines) > 2 {
		return fmt.Sprintf("%d machines", len(g.Machines))
	}
	return strings.Join(g.Machines, ", ")
}

const groupesMax = 200

// regrouper : messages semblables comptés ensemble ; tri : « frequents »,
// « rares » ou « erreurs ».
func regrouper(ls []stockage.Ligne, tri string) []groupeLogs {
	parCle := map[string]*groupeLogs{}
	machines := map[string]map[string]bool{}
	var ordre []string
	for _, l := range ls {
		segs := motifMessage(l.Msg)
		k := cleMotif(segs)
		g := parCle[k]
		if g == nil {
			g = &groupeLogs{Cle: k, Motif: segs}
			parCle[k] = g
			machines[k] = map[string]bool{}
			ordre = append(ordre, k)
		}
		g.N++
		if n := l.Champs["level"]; g.Niveau == "" || rangNiveau(n) > rangNiveau(g.Niveau) {
			g.Niveau = n
		}
		if h := l.Champs["host"]; h != "" && !machines[k][h] {
			machines[k][h] = true
			g.Machines = append(g.Machines, h)
		}
		if l.T > g.Derniere {
			g.Derniere = l.T
		}
	}
	out := make([]groupeLogs, 0, len(ordre))
	for _, k := range ordre {
		g := parCle[k]
		g.Classe = classeNiveau(g.Niveau)
		sort.Strings(g.Machines)
		out = append(out, *g)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		switch tri {
		case "rares":
			if a.N != b.N {
				return a.N < b.N
			}
		case "erreurs":
			if ra, rb := rangNiveau(a.Niveau), rangNiveau(b.Niveau); ra != rb {
				return ra > rb
			}
			if a.N != b.N {
				return a.N < b.N
			}
		default:
			if a.N != b.N {
				return a.N > b.N
			}
		}
		return a.Derniere > b.Derniere
	})
	if len(out) > groupesMax {
		out = out[:groupesMax]
	}
	maxN := 1
	for _, g := range out {
		maxN = max(maxN, g.N)
	}
	for i := range out {
		out[i].Largeur = max(1, out[i].N*100/maxN)
	}
	return out
}

// lignesDuMotif : lignes d'un message type (clic sur un groupe).
func lignesDuMotif(ls []stockage.Ligne, cle string) []stockage.Ligne {
	var out []stockage.Ligne
	for _, l := range ls {
		if cleMotif(motifMessage(l.Msg)) == cle {
			out = append(out, l)
		}
	}
	return out
}

// --- Histogramme --------------------------------------------------------------------

// barreHisto : une tranche de temps (hauteurs en unités du dessin SVG).
type barreHisto struct {
	X, L                               float64 // position et largeur
	YAutre, HAutre, YAvert, HAvert     float64
	YErreur, HErreur                   float64
	Titre                              string
	Autres, Avertissements, Erreurs, N int
}

// histogramme : lignes trouvées par tranche, empilées par gravité.
type histogramme struct {
	Barres      []barreHisto
	Debut, Fin  int64 // millisecondes (zoom par glisser)
	Graduations []graduation
}

type graduation struct {
	X     float64
	Texte string
}

const (
	histoLargeur = 1000.0
	histoHauteur = 90.0
	histoBarres  = 60
)

func calculerHistogramme(ls []stockage.Ligne, debut, fin int64, loc *time.Location) histogramme {
	h := histogramme{Debut: debut, Fin: fin}
	if fin <= debut {
		return h
	}
	pas := float64(fin-debut) / histoBarres
	barres := make([]barreHisto, histoBarres)
	for _, l := range ls {
		i := int(float64(l.T-debut) / pas)
		if i < 0 || i >= histoBarres {
			continue
		}
		switch classeNiveau(l.Champs["level"]) {
		case "err":
			barres[i].Erreurs++
		case "warn":
			barres[i].Avertissements++
		default:
			barres[i].Autres++
		}
		barres[i].N++
	}
	maxN := 1
	for _, b := range barres {
		maxN = max(maxN, b.N)
	}
	l := histoLargeur / histoBarres
	for i := range barres {
		b := &barres[i]
		b.X, b.L = float64(i)*l+1, l-2
		echelle := (histoHauteur - 2) / float64(maxN)
		b.HAutre = float64(b.Autres) * echelle
		b.HAvert = float64(b.Avertissements) * echelle
		b.HErreur = float64(b.Erreurs) * echelle
		b.YAutre = histoHauteur - b.HAutre
		b.YAvert = b.YAutre - b.HAvert
		b.YErreur = b.YAvert - b.HErreur
		t0 := time.UnixMilli(debut + int64(float64(i)*pas)).In(loc)
		t1 := time.UnixMilli(debut + int64(float64(i+1)*pas)).In(loc)
		b.Titre = fmt.Sprintf("%s – %s : %d ligne(s)", t0.Format("02/01 15:04"), t1.Format("15:04"), b.N)
		if b.Erreurs > 0 {
			b.Titre += fmt.Sprintf(", %d erreur(s)", b.Erreurs)
		}
		if b.Avertissements > 0 {
			b.Titre += fmt.Sprintf(", %d avertissement(s)", b.Avertissements)
		}
	}
	h.Barres = barres
	// 5 graduations : début, trois intermédiaires, fin.
	for k := 0; k <= 4; k++ {
		t := time.UnixMilli(debut + (fin-debut)*int64(k)/4).In(loc)
		texte := t.Format("15:04")
		if fin-debut > 2*24*3600*1000 {
			texte = t.Format("02/01")
		} else if fin-debut > 20*3600*1000 && k == 0 {
			texte = t.Format("02/01 15:04")
		}
		h.Graduations = append(h.Graduations, graduation{X: histoLargeur * float64(k) / 4, Texte: texte})
	}
	return h
}

// --- Surlignage ---------------------------------------------------------------------

const surlignageMax = 4000 // octets de message traités

// surligner : mots entiers cherchés et texte contenu, casse ignorée.
func surligner(msg string, mots []string, contient string) []segment {
	if len(msg) > surlignageMax || (len(mots) == 0 && contient == "") {
		return []segment{{Texte: msg}}
	}
	bas := strings.ToLower(msg)
	if len(bas) != len(msg) {
		// Lettres dont la minuscule change de taille : pas de surlignage.
		return []segment{{Texte: msg}}
	}
	marque := make([]bool, len(msg))
	poser := func(i, n int) {
		for k := i; k < i+n && k < len(marque); k++ {
			marque[k] = true
		}
	}
	for _, w := range mots {
		w = strings.ToLower(w)
		for i := 0; w != "" && i < len(bas); {
			j := strings.Index(bas[i:], w)
			if j < 0 {
				break
			}
			d := i + j
			avant, _ := utf8.DecodeLastRuneInString(bas[:d])
			apres, _ := utf8.DecodeRuneInString(bas[d+len(w):])
			if (d == 0 || !estLettreOuChiffre(avant)) && (d+len(w) == len(bas) || !estLettreOuChiffre(apres)) {
				poser(d, len(w))
			}
			i = d + len(w)
		}
	}
	if c := strings.ToLower(contient); c != "" {
		for i := 0; i < len(bas); {
			j := strings.Index(bas[i:], c)
			if j < 0 {
				break
			}
			poser(i+j, len(c))
			i += j + len(c)
		}
	}
	var out []segment
	debut := 0
	for i := 1; i <= len(msg); i++ {
		if i == len(msg) || marque[i] != marque[debut] {
			out = append(out, segment{Texte: msg[debut:i], Marque: marque[debut]})
			debut = i
		}
	}
	return out
}

func estLettreOuChiffre(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }
