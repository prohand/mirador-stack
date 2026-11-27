// SPDX-License-Identifier: GPL-3.0-or-later

package logs

import (
	"bytes"
	"regexp"
	"regexp/syntax"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Filtre « bruit » (lignes du journal jetées) avec une voie rapide (revue
// perf du 27/11) : l'expression par défaut, en « (?i) », coûtait ~75 % du
// traitement d'une ligne. Quand l'expression n'est qu'une liste de textes
// fixes sans différence majuscules/minuscules (cas du réglage par défaut)
// et que la ligne est en ASCII, une recherche de texte sur la ligne en
// minuscules donne exactement le même résultat. Sinon (expression libre,
// ligne avec accents), l'expression complète sert, comme avant.

// maxTextesBruit : au-delà, pas de voie rapide (expression trop riche).
const maxTextesBruit = 64

type filtreBruit struct {
	re     *regexp.Regexp
	textes [][]byte // en minuscules ; nil : pas de voie rapide
	tampon []byte   // ligne en minuscules (réutilisé ; appelé sous verrou)
}

func nouveauFiltreBruit(expr string) *filtreBruit {
	f := &filtreBruit{re: regexp.MustCompile(expr)} // validée par la configuration
	if arbre, err := syntax.Parse(expr, syntax.Perl); err == nil {
		// Un texte vide correspond à toute ligne : on laisse l'expression.
		if ts, ok := textesFixes(arbre.Simplify()); ok && len(ts) > 0 && !contientTexte(ts, "") {
			for _, t := range ts {
				f.textes = append(f.textes, []byte(t))
			}
		}
	}
	return f
}

// Correspond : la ligne est-elle du bruit ?
func (f *filtreBruit) Correspond(ligne string) bool {
	if f.textes == nil {
		return f.re.MatchString(ligne)
	}
	b := f.tampon[:0]
	for i := 0; i < len(ligne); i++ {
		c := ligne[i]
		if c >= utf8.RuneSelf {
			return f.re.MatchString(ligne) // accents : règles Unicode de l'expression
		}
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		b = append(b, c)
	}
	if cap(b) <= 64<<10 {
		f.tampon = b
	}
	for _, t := range f.textes {
		if bytes.Contains(b, t) {
			return true
		}
	}
	return false
}

// textesFixes : tous les textes que l'expression reconnaît, en minuscules,
// si c'est une liste finie de textes ASCII où chaque lettre accepte ses
// deux casses. ok=false sinon (répétition, ancre, classe large, lettre
// sensible à la casse…).
func textesFixes(r *syntax.Regexp) ([]string, bool) {
	switch r.Op {
	case syntax.OpEmptyMatch:
		return []string{""}, true
	case syntax.OpLiteral:
		var sb strings.Builder
		for _, c := range r.Rune {
			if c >= utf8.RuneSelf || (unicode.IsLetter(c) && r.Flags&syntax.FoldCase == 0) {
				return nil, false
			}
			sb.WriteRune(unicode.ToLower(c))
		}
		return []string{sb.String()}, true
	case syntax.OpCharClass:
		// Petite classe ([Ss], [ab]) : chaque lettre doit y être dans ses
		// deux casses.
		var dans []rune
		for i := 0; i+1 < len(r.Rune); i += 2 {
			for c := r.Rune[i]; c <= r.Rune[i+1]; c++ {
				if c >= utf8.RuneSelf || len(dans) >= 16 {
					return nil, false
				}
				dans = append(dans, c)
			}
		}
		var out []string
		for _, c := range dans {
			if unicode.IsLetter(c) && !(contient(dans, unicode.ToLower(c)) && contient(dans, unicode.ToUpper(c))) {
				return nil, false
			}
			if s := string(unicode.ToLower(c)); !contientTexte(out, s) {
				out = append(out, s)
			}
		}
		return out, true
	case syntax.OpCapture:
		return textesFixes(r.Sub[0])
	case syntax.OpAlternate:
		var out []string
		for _, s := range r.Sub {
			ts, ok := textesFixes(s)
			if !ok || len(out)+len(ts) > maxTextesBruit {
				return nil, false
			}
			out = append(out, ts...)
		}
		return out, true
	case syntax.OpConcat:
		out := []string{""}
		for _, s := range r.Sub {
			ts, ok := textesFixes(s)
			if !ok || len(out)*len(ts) > maxTextesBruit {
				return nil, false
			}
			var suite []string
			for _, a := range out {
				for _, b := range ts {
					suite = append(suite, a+b)
				}
			}
			out = suite
		}
		return out, true
	}
	return nil, false
}

func contient(rs []rune, c rune) bool {
	for _, x := range rs {
		if x == c {
			return true
		}
	}
	return false
}

func contientTexte(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
