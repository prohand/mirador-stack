// SPDX-License-Identifier: GPL-3.0-or-later

package console

import "testing"

func TestAccord(t *testing.T) {
	for _, c := range []struct {
		n    any
		t, v string
	}{
		{0, "alerte(s)", "alerte"}, {1, "alerte(s) présente(s)", "alerte présente"}, {2, "alerte(s) présente(s)", "alertes présentes"},
		{int64(7), "panneau(x)", "panneaux"}, {1, "panneau(x)", "panneau"}, {"12", "règle(s)", "règles"}, {"abc", "règle(s)", "règle"},
	} {
		if g := accorder(c.n, c.t); g != c.v {
			t.Errorf("accorder(%v, %q) = %q, attendu %q", c.n, c.t, g, c.v)
		}
	}
	for texte, voulu := range map[string]string{
		"3 règle(s) enregistrée(s) ou changée(s)":           "3 règles enregistrées ou changée(s)", // « ou » coupe la suite : laissé tel quel
		"1 modèle(s) détecté(s) appliqué(s) : retirables":   "1 modèle détecté appliqué : retirables",
		"12 certificat(s) suivi(s), 1 fichier(s) écarté(s)": "12 certificats suivis, 1 fichier écarté",
		"aucun (s) ici":        "aucun (s) ici",
		"sans nombre règle(s)": "sans nombre règle(s)",
	} {
		if g := accorderTexte(texte); g != voulu {
			t.Errorf("accorderTexte(%q) = %q, attendu %q", texte, g, voulu)
		}
	}
}
