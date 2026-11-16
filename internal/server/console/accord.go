// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Accord du nombre (audit du 16/11, SPEC § 34) : « 1 machine », « 3 machines »
// au lieu de « machine(s) ». Les textes gardent leur forme « mot(s) » dans le
// code et les gabarits ; l'accord est fait à l'affichage :
//   - gabarits : {{.N}} {{accord .N "alerte(s) présente(s)"}} ;
//   - messages construits en Go : accorderTexte("3 règle(s) retirée(s)").
// Règle du français : 0 et 1 au singulier, 2 et plus au pluriel.

var reMarquePluriel = strings.NewReplacer("(s)", "s", "(x)", "x")
var reMarqueSingulier = strings.NewReplacer("(s)", "", "(x)", "")

// accorder : texte avec ses « (s) » ou « (x) » mis au pluriel si n >= 2,
// retirés sinon. n : n'importe quel nombre entier (int, int64, len…).
func accorder(n any, texte string) string {
	v, err := strconv.ParseFloat(fmt.Sprint(n), 64)
	if err == nil && v >= 2 {
		return reMarquePluriel.Replace(texte)
	}
	return reMarqueSingulier.Replace(texte)
}

// reNombreMots : un nombre entier, puis un ou plusieurs mots marqués
// « (s) » ou « (x) » (« 3 règle(s) importée(s) »).
var reNombreMots = regexp.MustCompile(`(\d+)((?: [\p{L}'’-]+\((?:s|x)\))+)`)

// accorderTexte : accorde chaque « nombre mot(s)… » d'un message écrit par
// le serveur (messages de retour, résumés). À ne pas appliquer à un texte
// venu d'ailleurs (ligne de log, nom donné par un utilisateur).
func accorderTexte(s string) string {
	if !strings.Contains(s, "(s)") && !strings.Contains(s, "(x)") {
		return s
	}
	return reNombreMots.ReplaceAllStringFunc(s, func(m string) string {
		p := reNombreMots.FindStringSubmatch(m)
		return p[1] + accorder(p[1], p[2])
	})
}
