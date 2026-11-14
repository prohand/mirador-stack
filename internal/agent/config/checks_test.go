// SPDX-License-Identifier: GPL-3.0-or-later

package config

import (
	"strings"
	"testing"
	"time"
)

func TestValiderChecks(t *testing.T) {
	bons := []Check{
		{Type: "http", Nom: "site", Cible: "https://exemple.org/sante", Contient: "ok"},
		{Type: "tcp", Nom: "pg", Cible: "127.0.0.1:5432"},
		{Type: "tcp", Nom: "v6", Cible: "[::1]:22"},
		{Type: "processus", Nom: "nginx", Cible: "nginx"},
		{Type: "fichier", Nom: "sauv", Cible: "/srv/sauvegardes/*.tar.gz"},
		{Type: "dossier", Nom: "d", Cible: "/srv/sauvegardes"},
		{Type: "dns", Nom: "dns", Cible: "exemple.org"},
		{Type: "ping", Nom: "routeur", Cible: "192.0.2.1", Delai: Duree(5 * time.Second)},
	}
	if err := ValiderChecks(bons); err != nil {
		t.Fatal(err)
	}
	for _, mauvais := range []Check{
		{Type: "commande", Nom: "x", Cible: "rm -rf /"},
		{Type: "http", Nom: "x", Cible: "file:///etc/passwd"},
		{Type: "http", Nom: "x", Cible: "https://moi:secret@exemple.org/"},
		{Type: "tcp", Nom: "x", Cible: "exemple.org"},
		{Type: "tcp", Nom: "x", Cible: "h:70000"},
		{Type: "processus", Nom: "x", Cible: "un nom trop long pour comm"},
		{Type: "fichier", Nom: "x", Cible: "relatif/a.txt"},
		{Type: "fichier", Nom: "x", Cible: "/srv/../etc/shadow"},
		{Type: "dossier", Nom: "x", Cible: "/srv/*"},
		{Type: "dns", Nom: "x", Cible: "a b"},
		{Type: "ping", Nom: "x y", Cible: "h"},
		{Type: "tcp", Nom: "x", Cible: "h:1", Contient: "a"},
		{Type: "http", Nom: "x", Cible: "https://h/", Delai: Duree(time.Minute)},
	} {
		if err := mauvais.Valider(); err == nil {
			t.Errorf("accepté : %+v", mauvais)
		}
	}
	trop := make([]Check, MaxChecks+1)
	for i := range trop {
		trop[i] = Check{Type: "dns", Nom: "n" + strings.Repeat("x", i%10) + string(rune('a'+i%26)), Cible: "h"}
	}
	if ValiderChecks(trop) == nil {
		t.Error("plus de 50 checks acceptés")
	}
	if ValiderChecks([]Check{bons[0], bons[0]}) == nil {
		t.Error("check en double accepté")
	}
}
