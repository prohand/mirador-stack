// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"mirador-stack/internal/server/servicesmetier"
)

// Page Services (SPEC § 50) : création, état d'après les métriques,
// modification (renommer), suppression.
func TestPageServices(t *testing.T) {
	b := nouveauBanc(t)
	ct := func(nom string, v float64, extra ...string) serieTest {
		return serieTest{append([]string{"__name__", nom, "instance", "docker01", "name", "ha"}, extra...), constante(v)}
	}
	b.metriquesSimulees(t, []serieTest{
		ct("node_docker_container_running", 1, "state", "running", "restart_policy", "always"),
		ct("node_docker_container_healthy", 0),
		{[]string{"__name__", "mirador_sonde_succes", "instance", "lan", "sonde", "HA"}, constante(1)},
		{[]string{"__name__", "mirador_sonde_duree_secondes", "instance", "lan", "sonde", "HA"}, constante(0.041)},
	})
	b.machinesApprouvees(t, "docker01")
	cl, secret := b.premierAdmin("admin", mdpAdmin)
	poster := func(v url.Values) {
		v.Set("csrf", b.csrf(cl, "/services"))
		v.Set("code", b.code(secret))
		b.post(cl, "/services", v)
		b.h.avancer(31 * time.Second) // code TOTP suivant
	}
	poster(url.Values{"action": {"enregistrer"}, "nom": {"Domotique"}, "elements": {"clé sonde HA\nconteneur docker01/ha\nmachine docker01"}})
	_, corps, _ := b.get(cl, "/services")
	for _, attendu := range []string{"<h1>Services</h1>", "Domotique", `<span class="pastille attention">Dégradé</span>`,
		"OK · 41 ms", "Non sain", `<span class="cle">clé</span>`, "30 j : 100 %", `href="/services"`} {
		if !strings.Contains(corps, attendu) {
			t.Errorf("page : %q absent", attendu)
		}
	}
	// Renommer, puis supprimer.
	poster(url.Values{"action": {"enregistrer"}, "ancien": {"Domotique"}, "nom": {"Maison"}, "elements": {"clé sonde HA"}})
	if l, _ := servicesmetier.Lire(t.Context(), b.c.db); len(l) != 1 || l[0].Nom != "Maison" || len(l[0].Composants) != 1 {
		t.Fatalf("après modification : %+v", l)
	}
	poster(url.Values{"action": {"enregistrer"}, "nom": {"X"}, "elements": {"routeur r1"}}) // refusé
	poster(url.Values{"action": {"supprimer"}, "ancien": {"Maison"}})
	if l, _ := servicesmetier.Lire(t.Context(), b.c.db); len(l) != 0 {
		t.Fatalf("après suppression : %+v", l)
	}
}
