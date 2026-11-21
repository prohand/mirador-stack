// SPDX-License-Identifier: GPL-3.0-or-later

// Package motif : contrôle d'un texte par une expression régulière simple
// plus une longueur bornée.
//
// Mesure du 21/11 (SPEC § 115) : une expression comme
// `^[\p{L}\p{N} ._-]{1,200}$` recopie 200 fois la classe Unicode dans le
// programme compilé, soit 1 à 4 Mo de mémoire chacune, gardés tant que le
// serveur tourne. `^[\p{L}\p{N} ._-]+$` avec la longueur vérifiée à part
// fait le même contrôle pour ~0,05 Mo.
package motif

import (
	"regexp"
	"unicode/utf8"
)

// Borne : expression (sans répétition bornée) et longueur en caractères.
type Borne struct {
	re       *regexp.Regexp
	min, max int
}

// Doit compile l'expression (panique si elle est fausse, comme
// regexp.MustCompile) ; min et max comptent des caractères, pas des octets.
func Doit(expr string, min, max int) Borne {
	return Borne{re: regexp.MustCompile(expr), min: min, max: max}
}

// MatchString : longueur dans les bornes et expression vérifiée.
func (b Borne) MatchString(s string) bool {
	n := utf8.RuneCountInString(s)
	return n >= b.min && n <= b.max && b.re.MatchString(s)
}
