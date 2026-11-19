// SPDX-License-Identifier: GPL-3.0-or-later

package console

import "testing"

// Couleurs discrètes : adresse IP (avec le port) et mots d'échec (SPEC § 77).
func TestColorer(t *testing.T) {
	segs := colorer([]segment{{Texte: "bind() to 0.0.0.0:443 failed (98)"}})
	var ip, err string
	for _, s := range segs {
		switch s.Genre {
		case "ip":
			ip = s.Texte
		case "err":
			err = s.Texte
		}
	}
	if ip != "0.0.0.0:443" || err != "failed" || joindre(segs) != "bind() to 0.0.0.0:443 failed (98)" {
		t.Fatalf("segments : %+v", segs)
	}
	// Mot cherché (surligné) : laissé tel quel.
	if s := colorer([]segment{{Texte: "error", Marque: true}}); len(s) != 1 || s[0].Genre != "" {
		t.Fatalf("mot cherché recoloré : %+v", s)
	}
}

// Repli : 3 lignes ou plus, même origine, même message aux chiffres près,
// le même jour ; la première porte le groupe.
func TestReplierSemblables(t *testing.T) {
	l := func(host, msg, jour string) ligneVue {
		return ligneVue{Host: host, Origine: "ssh.service", Heure: "00:0" + msg[len(msg)-1:], Jour: jour, Segments: []segment{{Texte: msg}}}
	}
	ls := []ligneVue{l("a", "port 1", "Aujourd'hui"), l("b", "port 2", ""), l("c", "port 3", ""), l("a", "port 4", ""),
		{Host: "x", Origine: "nginx", Segments: []segment{{Texte: "autre"}}},
		l("a", "port 5", ""), l("b", "port 6", "Hier"), l("c", "port 7", "")}
	replierSemblables(ls)
	if ls[0].Groupe != 1 || ls[0].GroupeN != 3 || ls[0].GroupeJusqua != "00:04" || ls[0].GroupeMachines != "a, b, c" {
		t.Fatalf("tête du groupe : %+v", ls[0])
	}
	if ls[1].Semblable != 1 || ls[3].Semblable != 1 || ls[4].Semblable != 0 {
		t.Fatal("lignes du groupe mal marquées")
	}
	// 2 lignes seulement (le jour change entre 5 et 6) : pas de repli.
	if ls[5].Groupe != 0 || ls[6].Groupe != 0 || ls[7].Semblable != 0 {
		t.Fatal("repli de moins de 3 lignes ou par-dessus un changement de jour")
	}
}
