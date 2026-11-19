// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"mirador-stack/internal/server/flotte"
	"mirador-stack/internal/server/servicesmetier"
	essais "mirador-stack/internal/sondes"
)

// Registre des éléments, fiches et étiquettes (SPEC § 55, § 56, § 59).
func TestElementsFichesEtiquettes(t *testing.T) {
	b := nouveauBanc(t)
	m := b.h.maintenant()
	b.metriquesSimulees(t, []serieTest{
		{[]string{"__name__", "node_docker_container_running", "instance", "web1", "name", "ha", "state", "running", "restart_policy", "always"}, constante(1)},
		{[]string{"__name__", "mirador_sonde_succes", "instance", "mirador-server", "sonde", "Nextcloud"}, constante(0)},
	})
	b.machinesApprouvees(t, "web1")
	ctx := t.Context()
	if err := flotte.EnregistrerSondes(ctx, b.c.db, []essais.Sonde{{Nom: "Nextcloud", Type: essais.HTTP, Cible: "https://cloud.exemple.fr/", Depuis: "serveur"}}, "test", m); err != nil {
		t.Fatal(err)
	}
	if err := servicesmetier.Enregistrer(ctx, b.c.db, []servicesmetier.Service{{Nom: "Cloud", Composants: []servicesmetier.Composant{
		{Sorte: "sonde", Nom: "Nextcloud", Cle: true}, {Sorte: "conteneur", Nom: "web1/ha"}}}}, "test", "", m); err != nil {
		t.Fatal(err)
	}
	cl, secret := b.premierAdmin("admin", mdpAdmin)
	_ = secret
	// Étiquettes (refus d'une clé invalide, puis enregistrement).
	b.post(cl, "/etiquettes", url.Values{"csrf": {b.csrf(cl, "/elements")}, "element": {"m:web1"}, "etiquettes": {"Critique=oui"}, "retour": {"/machines/web1"}})
	if len(b.c.lireEtiquettes(ctx)) != 0 {
		t.Fatal("clé en majuscules acceptée")
	}
	b.post(cl, "/etiquettes", url.Values{"csrf": {b.csrf(cl, "/elements")}, "element": {"m:web1"}, "etiquettes": {"critique=oui, role=web"}, "retour": {"/machines/web1"}})
	if b.c.lireEtiquettes(ctx)["m:web1"]["role"] != "web" {
		t.Fatalf("étiquettes : %+v", b.c.lireEtiquettes(ctx))
	}
	_, corps, _ := b.get(cl, "/elements")
	for _, attendu := range []string{"<h1>Éléments</h1>", `href="/machines/web1"`, `href="/conteneurs/web1/ha"`, `href="/sondes/Nextcloud"`,
		`href="/services/Cloud"`, "fait partie de", "<b>critique</b>"} {
		if !strings.Contains(corps, attendu) {
			t.Errorf("éléments : %q absent", attendu)
		}
	}
	_, corps, _ = b.get(cl, "/elements?etiquette=critique=oui")
	if strings.Contains(corps, `href="/sondes/Nextcloud"`) || !strings.Contains(corps, `href="/machines/web1"`) {
		t.Error("filtre par étiquette")
	}
	for chemin, attendus := range map[string][]string{
		"/sondes/Nextcloud":   {"Disponibilité 30 j", "Échec", "fait partie de", "Page Sondes"},
		"/services/Cloud":     {"En panne", "comprend", "Page Services"},
		"/conteneurs/web1/ha": {"Redémarrages 24 h", "sur la machine", "Logs du conteneur"},
		"/machines/web1":      {`id="etiquettes"`, "<b>role</b>", "fait partie de"},
	} {
		_, corps, _ := b.get(cl, chemin)
		for _, a := range attendus {
			if !strings.Contains(corps, a) {
				t.Errorf("%s : %q absent", chemin, a)
			}
		}
	}
	if code, _, _ := b.get(cl, "/sondes/inconnue"); code != 404 {
		t.Errorf("fiche inconnue : %d", code)
	}
	_ = time.Second
}
