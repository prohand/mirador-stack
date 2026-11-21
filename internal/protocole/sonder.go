// SPDX-License-Identifier: GPL-3.0-or-later

package protocole

import (
	"net/url"
	"strconv"
	"strings"
)

// Valeur de EnteteSonder (SPEC § 108) : « N:nom1,nom2 ».

// MaxSonder : noms lus au plus dans un en-tête.
const MaxSonder = 100

// FormerSonder : valeur de l'en-tête (vide : rien à demander).
func FormerSonder(dernier int64, noms []string) string {
	if len(noms) == 0 || dernier <= 0 {
		return ""
	}
	e := make([]string, 0, len(noms))
	for _, n := range noms {
		if len(e) == MaxSonder {
			break
		}
		e = append(e, url.QueryEscape(n))
	}
	return strconv.FormatInt(dernier, 10) + ":" + strings.Join(e, ",")
}

// LireSonder : décode l'en-tête. Valeur mal formée : rien.
func LireSonder(v string) (dernier int64, noms []string) {
	num, liste, ok := strings.Cut(v, ":")
	if !ok {
		return 0, nil
	}
	dernier, err := strconv.ParseInt(num, 10, 64)
	if err != nil || dernier <= 0 {
		return 0, nil
	}
	for _, e := range strings.Split(liste, ",") {
		if n, err := url.QueryUnescape(e); err == nil && n != "" && len(noms) < MaxSonder {
			noms = append(noms, n)
		}
	}
	return dernier, noms
}
