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
		v.Set("csrf", b.csrf(cl, "/applications"))
		v.Set("code", b.code(secret))
		b.post(cl, "/applications", v)
		b.h.avancer(31 * time.Second) // code TOTP suivant
	}
	poster(url.Values{"action": {"enregistrer"}, "nom": {"Domotique"}, "elements": {"clé sonde HA\nconteneur docker01/ha\nmachine docker01"}})
	_, corps, _ := b.get(cl, "/applications")
	for _, attendu := range []string{"<h1>Applications</h1>", "Domotique", `<span class="pastille attention">Dégradée</span>`,
		"OK · 41 ms", "Non sain", `<span class="cle">clé</span>`, "<b>100 %</b> sur 30 j", `href="/applications"`} {
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

// Refonte du 23/11 (SPEC § 122) : bandeau de la panne et sa cause,
// propositions Docker Compose (dès 1 conteneur), éditeur prérempli, éléments
// cochés (« el », « cle »), page de statut publique.
func TestApplicationsRefonte(t *testing.T) {
	b := nouveauBanc(t)
	info := func(nom, projet string) serieTest {
		return serieTest{[]string{"__name__", "node_docker_container_running", "instance", "docker01", "name", nom, "project", projet}, constante(1)}
	}
	b.metriquesSimulees(t, []serieTest{
		info("immich-server", "immich"), info("immich-db", "immich"), info("ha", ""),
		{[]string{"__name__", "mirador_sonde_succes", "instance", "mirador-server", "sonde", "NC"}, constante(0)},
	})
	b.machinesApprouvees(t, "docker01")
	cl, secret := b.premierAdmin("admin", mdpAdmin)
	poster := func(v url.Values) {
		v.Set("csrf", b.csrf(cl, "/applications"))
		v.Set("code", b.code(secret))
		b.post(cl, "/applications", v)
		b.h.avancer(31 * time.Second)
	}
	poster(url.Values{"action": {"enregistrer"}, "nom": {"Nextcloud"}, "elements": {"clé sonde NC\nmachine docker01"}})
	_, corps, _ := b.get(cl, "/applications")
	for _, attendu := range []string{
		"Nextcloud est en panne", "Sonde NC : ", // bandeau et cause
		`class="frise-app"`, "Disponibilité 30 j",
		"1 application proposée d'après Docker Compose", "machine=docker01&amp;proposer=immich",
		`id="editeur"`, `name="el" value="conteneur|docker01/ha"`, // éditeur (fermé)
	} {
		if !strings.Contains(corps, attendu) {
			t.Errorf("page : %q absent", attendu)
		}
	}
	// Fiche : pourquoi, frise, éléments avec leur état, lien « Modifier ».
	_, fiche, _ := b.get(cl, "/applications/Nextcloud")
	for _, attendu := range []string{"<h2>Sonde NC : ", `class="frise-app grande"`, `class="elements-app"`, "/applications?modifier=Nextcloud#editeur"} {
		if !strings.Contains(fiche, attendu) {
			t.Errorf("fiche : %q absent", attendu)
		}
	}
	// Proposition : formulaire ouvert, conteneurs du projet et machine hôte cochés.
	_, corps, _ = b.get(cl, "/applications?proposer=immich&machine=docker01")
	for _, attendu := range []string{
		`id="editeur" open`, `value="Immich"`,
		`name="el" value="conteneur|docker01/immich-server" checked`, `name="el" value="conteneur|docker01/immich-db" checked`,
		`name="el" value="machine|docker01" checked`,
	} {
		if !strings.Contains(corps, attendu) {
			t.Errorf("proposition : %q absent", attendu)
		}
	}
	if strings.Contains(corps, `name="el" value="conteneur|docker01/ha" checked`) {
		t.Error("conteneur d'un autre projet coché")
	}
	// Création par cases à cocher, montrée sur la page de statut.
	poster(url.Values{"action": {"enregistrer"}, "nom": {"Immich"}, "statut_present": {"1"}, "statut": {"1"},
		"el":  {"conteneur|docker01/immich-server", "conteneur|docker01/immich-db", "machine|docker01"},
		"cle": {"conteneur|docker01/immich-server"}})
	l, _ := servicesmetier.Lire(t.Context(), b.c.db)
	var immich *servicesmetier.Service
	for i := range l {
		if l[i].Nom == "Immich" {
			immich = &l[i]
		}
	}
	if immich == nil || len(immich.Composants) != 3 || !immich.Composants[0].Cle || immich.Composants[1].Cle {
		t.Fatalf("Immich : %+v", immich)
	}
	if st := lireStatut(t.Context(), b.c.db); len(st.Applications) != 1 || st.Applications[0] != "Immich" {
		t.Errorf("page de statut : %+v", st.Applications)
	}
	// Plus proposée une fois créée ; modification préremplie.
	_, corps, _ = b.get(cl, "/applications?modifier=Immich")
	if strings.Contains(corps, "proposer=immich") {
		t.Error("projet encore proposé")
	}
	for _, attendu := range []string{`Modifier « Immich »`, `name="cle" value="conteneur|docker01/immich-server" checked`, `name="statut" value="1" checked`} {
		if !strings.Contains(corps, attendu) {
			t.Errorf("modification : %q absent", attendu)
		}
	}
	// Suppression : retirée aussi de la page de statut.
	poster(url.Values{"action": {"supprimer"}, "ancien": {"Immich"}})
	if st := lireStatut(t.Context(), b.c.db); len(st.Applications) != 0 {
		t.Errorf("page de statut après suppression : %+v", st.Applications)
	}
}
