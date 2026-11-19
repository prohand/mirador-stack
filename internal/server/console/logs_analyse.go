// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"fmt"
	"html/template"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"mirador-stack/internal/server/motifs"
	"mirador-stack/internal/server/stockage"
)

// Calculs de la page Logs (SPEC § 33) faits sur les lignes déjà trouvées,
// sans nouvelle requête : messages regroupés, histogramme, surlignage.

// segment : morceau de texte affiché, variable (« IP », « N ») ou surligné.
type segment struct {
	Texte  string
	Var    bool   // partie variable d'un message type
	Marque bool   // mot cherché
	Genre  string // « ip » ou « err » : couleur discrète (SPEC § 77)
}

// Couleurs discrètes dans les messages (SPEC § 77) : adresses IP (avec le
// port) et mots qui annoncent un échec.
var reColorer = regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}(?::\d{1,5})?\b|(?i)\b(?:failed|failure|error|errors|fatal|panic|denied|refused|emerg|critical|timed out|timeout|unreachable)\b`)

// colorer : découpe les morceaux non surlignés autour des adresses IP et
// des mots d'échec (Genre « ip » ou « err »). Message trop long : inchangé.
func colorer(segs []segment) []segment {
	var out []segment
	for _, s := range segs {
		if s.Marque || len(s.Texte) > surlignageMax {
			out = append(out, s)
			continue
		}
		debut := 0
		for _, m := range reColorer.FindAllStringIndex(s.Texte, -1) {
			if m[0] > debut {
				out = append(out, segment{Texte: s.Texte[debut:m[0]]})
			}
			g := "err"
			if c := s.Texte[m[0]]; c >= '0' && c <= '9' {
				g = "ip"
			}
			out = append(out, segment{Texte: s.Texte[m[0]:m[1]], Genre: g})
			debut = m[1]
		}
		if debut < len(s.Texte) {
			out = append(out, segment{Texte: s.Texte[debut:]})
		}
	}
	return out
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

// Regroupement (SPEC § 33, refait au § 52) : module motifs (parties
// variables remplacées, puis messages de même forme fusionnés : les mots
// qui changent deviennent MOT). « Vu la 1re fois » et « nouveau » viennent
// du registre des motifs (toutes les lignes reçues par le serveur).

// groupeLogs : un message type et ses lignes.
type groupeLogs struct {
	Cle       string
	Motif     []segment
	N         int
	Niveau    string // le plus grave vu
	Classe    string
	Machines  []string
	Derniere  int64 // millisecondes
	Largeur   int   // barre : part du groupe le plus fréquent (0 à 100)
	Evolution template.HTML
	PremierVu string // « 12/10 », « il y a 40 min » ; vide si inconnu
	Nouveau   bool
	premier   int64
}

// TexteMachines : « pve1, srv-test » ou « 5 machines ».
func (g groupeLogs) TexteMachines() string {
	if len(g.Machines) > 2 {
		return fmt.Sprintf("%d machines", len(g.Machines))
	}
	return strings.Join(g.Machines, ", ")
}

const groupesMax = 200

// segmentsMotif : segments d'affichage d'un gabarit.
func segmentsMotif(g string) []segment {
	var out []segment
	for _, s := range motifs.SegmentsGabarit(g) {
		out = append(out, segment{Texte: s.Texte, Var: s.Var})
	}
	return out
}

// groupesBruts : lignes regroupées (gabarit et indices des lignes).
func groupesBruts(ls []stockage.Ligne) []*motifs.Groupe {
	r := motifs.NouveauRegroupeur()
	for i, l := range ls {
		r.Ajouter(i, l.Msg)
	}
	return r.Groupes()
}

// regrouper : messages semblables comptés ensemble ; tri : « frequents »,
// « rares », « erreurs » ou « nouveaux ». debut, fin : période (ms) pour la
// petite courbe ; reg : registre (nil : pas de « vu la 1re fois »).
func regrouper(ls []stockage.Ligne, tri string, debut, fin int64, reg *motifs.Registre, t time.Time) []groupeLogs {
	var out []groupeLogs
	loc := fuseau.Load()
	for _, gb := range groupesBruts(ls) {
		gab := motifs.Gabarit(gb.Jetons)
		g := groupeLogs{Cle: motifs.Cle(gab), Motif: segmentsMotif(gab), N: len(gb.Indices)}
		vues := map[string]bool{}
		var ts []int64
		for _, i := range gb.Indices {
			l := ls[i]
			if n := l.Champs["level"]; g.Niveau == "" || rangNiveau(n) > rangNiveau(g.Niveau) {
				g.Niveau = n
			}
			if h := l.Champs["host"]; h != "" && !vues[h] {
				vues[h] = true
				g.Machines = append(g.Machines, h)
			}
			if l.T > g.Derniere {
				g.Derniere = l.T
			}
			ts = append(ts, l.T)
		}
		g.Classe = classeNiveau(g.Niveau)
		sort.Strings(g.Machines)
		g.Evolution = svgEvolution(ts, debut, fin, g.Classe == "err")
		if reg != nil {
			if p, ok := reg.Vu(gb.Jetons); ok {
				g.premier, g.Nouveau = p, reg.Nouveau(p, t)
				vu := time.Unix(p, 0)
				if t.Sub(vu) < 24*time.Hour {
					g.PremierVu = depuis(vu.UnixMilli())
				} else {
					g.PremierVu = vu.In(loc).Format("02/01")
				}
			}
		}
		out = append(out, g)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		switch tri {
		case "nouveaux":
			if a.Nouveau != b.Nouveau {
				return a.Nouveau
			}
			if a.premier != b.premier {
				return a.premier > b.premier
			}
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

// svgEvolution : lignes du groupe sur la période, 30 barres.
func svgEvolution(ts []int64, debut, fin int64, erreur bool) template.HTML {
	const nb, l, h = 30, 120.0, 22.0
	if fin <= debut || len(ts) == 0 {
		return ""
	}
	var c [nb]int
	mx := 1
	for _, t := range ts {
		k := int(float64(t-debut) / float64(fin-debut) * nb)
		k = min(max(k, 0), nb-1)
		c[k]++
		mx = max(mx, c[k])
	}
	couleur := "var(--accent)"
	if erreur {
		couleur = "var(--erreur)"
	}
	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="evolution-motif" width="%.0f" height="%.0f" viewBox="0 0 %.0f %.0f" role="img" aria-label="Évolution sur la période">`, l, h, l, h)
	for k, n := range c {
		if n == 0 {
			continue
		}
		hb := max(1.5, h*float64(n)/float64(mx))
		fmt.Fprintf(&b, `<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" fill="%s"/>`, float64(k)*l/nb, h-hb, l/nb-1, hb, couleur)
	}
	b.WriteString("</svg>")
	return template.HTML(b.String()) // #nosec G203 -- SVG fait de nombres formatés et de couleurs fixes
}

// lignesDuMotif : lignes d'un message type (clic sur un groupe).
func lignesDuMotif(ls []stockage.Ligne, cle string) []stockage.Ligne {
	for _, gb := range groupesBruts(ls) {
		if motifs.Cle(motifs.Gabarit(gb.Jetons)) != cle {
			continue
		}
		out := make([]stockage.Ligne, 0, len(gb.Indices))
		for _, i := range gb.Indices {
			out = append(out, ls[i])
		}
		return out
	}
	return nil
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
		b.Titre = fmt.Sprintf("%s – %s : ", t0.Format("02/01 15:04"), t1.Format("15:04")) + accorderTexte(fmt.Sprintf("%d ligne(s)", b.N))
		if b.Erreurs > 0 {
			b.Titre += accorderTexte(fmt.Sprintf(", %d erreur(s)", b.Erreurs))
		}
		if b.Avertissements > 0 {
			b.Titre += accorderTexte(fmt.Sprintf(", %d avertissement(s)", b.Avertissements))
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
