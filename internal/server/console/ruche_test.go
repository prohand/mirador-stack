// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"strings"
	"testing"
)

// Vue « ruche » (SPEC § 51) : onglet de la vue d'ensemble, filtres, fragment.
func TestRuche(t *testing.T) {
	b := nouveauBanc(t)
	b.metriquesSimulees(t, []serieTest{
		{[]string{"__name__", "node_docker_container_running", "instance", "web1", "name", "postgres", "state", "running", "restart_policy", "always"}, constante(1)},
		{[]string{"__name__", "node_docker_container_running", "instance", "web1", "name", "cache", "state", "exited", "restart_policy", "always"}, constante(0)},
	})
	b.machinesApprouvees(t, "web1", "nas")
	_, _ = b.c.db.Exec(`UPDATE machines SET site = 'maison'`)
	cl, _ := b.premierAdmin("admin", mdpAdmin)
	_, corps, _ := b.get(cl, "/")
	if !strings.Contains(corps, `data-vue-accueil="ruche"`) || strings.Contains(corps, `class="ruche"`) || !strings.Contains(corps, `class="kpis o-0 t-0" data-bloc="tuiles"`) {
		t.Fatal("par défaut : résumé, avec l'onglet Ruche")
	}
	_, corps, _ = b.get(cl, "/?vue=ruche")
	for _, attendu := range []string{`class="ruche"`, `href="/machines/web1"`, `href="/machines/nas"`, "<h2>maison", "2 éléments",
		`data-rafraichir="/fragments/accueil?vue=ruche&amp;ruche=machines"`} {
		if !strings.Contains(corps, attendu) {
			t.Errorf("ruche des machines : %q absent", attendu)
		}
	}
	if strings.Contains(corps, `data-bloc="tuiles"`) {
		t.Error("la ruche remplace le résumé")
	}
	_, corps, _ = b.get(cl, "/?vue=ruche&ruche=conteneurs")
	for _, attendu := range []string{"Conteneurs · web1", `class="h cr"`, `class="h ok"`, "1 critique"} {
		if !strings.Contains(corps, attendu) {
			t.Errorf("ruche des conteneurs : %q absent", attendu)
		}
	}
	_, corps, _ = b.get(cl, "/fragments/accueil?vue=ruche&ruche=tout")
	if !strings.Contains(corps, "Machines · maison") || !strings.Contains(corps, "Conteneurs · web1") {
		t.Error("fragment « tout »")
	}
}
