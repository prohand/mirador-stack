// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"net/url"
	"strings"
	"testing"

	"mirador-stack/internal/server/flotte"
)

// « Vérifier maintenant » (SPEC § 127) : bouton sur la ligne en échec
// seulement, demande enregistrée, une par minute au plus, attente affichée ;
// agent ancien : bouton remplacé par la raison.
func TestVerifierMaintenant(t *testing.T) {
	b := nouveauBanc(t)
	u := func(m, nom string) []string {
		return []string{"__name__", "node_systemd_unit_state", "instance", m, "name", nom, "state", "failed"}
	}
	b.metriquesSimulees(t, []serieTest{
		{u("web1", "nginx.service"), constante(1)}, {u("web1", "cron.service"), constante(0)},
		{u("vieux", "nginx.service"), constante(1)},
	})
	b.machinesApprouvees(t, "web1", "vieux")
	if _, err := b.c.db.Exec(`UPDATE machines SET version_agent = '0.6.19', capacites = 'surveillance,verifier' WHERE nom = 'web1'`); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.db.Exec(`UPDATE machines SET version_agent = '0.6.10', capacites = 'surveillance' WHERE nom = 'vieux'`); err != nil {
		t.Fatal(err)
	}
	cl, _ := b.premierAdmin("admin", mdpAdmin)
	_, corps, _ := b.get(cl, "/machines/web1")
	svc := blocDetails(corps, `id="services"`)
	if strings.Count(svc, `value="verifier"`) != 1 || !strings.Contains(svc, `value="/machines/web1#services"`) {
		t.Fatalf("bouton « Vérifier » : %d (une ligne en échec)", strings.Count(svc, `value="verifier"`))
	}
	_, vieux, _ := b.get(cl, "/machines/vieux")
	if s := blocDetails(vieux, `id="services"`); strings.Contains(s, `value="verifier"`) || !strings.Contains(s, "agent à mettre à jour (0.6.19 ou plus)") {
		t.Error("agent ancien : bouton présent ou raison absente")
	}

	csrf := b.csrf(cl, "/machines/web1")
	envoyer := func() string {
		_, _, h := b.post(cl, "/machines/web1", url.Values{"csrf": {csrf}, "action": {"verifier"}, "retour": {"/machines/web1#services"}})
		if !strings.HasPrefix(h.Get("Location"), "/machines/web1") { // sans l'ancre en cas d'erreur
			t.Errorf("retour : %q", h.Get("Location"))
		}
		_, page, _ := b.get(cl, "/machines/web1")
		return page
	}
	page := envoyer()
	if !strings.Contains(page, "résultat dans quelques secondes") {
		t.Errorf("message après la demande absent")
	}
	id, _ := flotte.IDMachine(t.Context(), b.c.db, "web1")
	if flotte.VerificationDe(id).IsZero() {
		t.Fatal("demande non enregistrée")
	}
	if s := blocDetails(page, `id="services"`); !strings.Contains(s, "Vérification demandée") || !strings.Contains(s, `data-recharger="5"`) {
		t.Error("attente non affichée (avec rechargement)")
	}
	if page = envoyer(); !strings.Contains(page, "une par minute au plus") {
		t.Error("deuxième demande dans la minute acceptée")
	}
	// Retour vers une page d'une autre origine : ignoré.
	if retourVerifierSur("//exemple.org/x") || retourVerifierSur("https://exemple.org") || !retourVerifierSur("/alertes/12") {
		t.Error("contrôle de la page de retour")
	}
}

// Résultats affichés (SPEC § 127).
func TestResultatsVerification(t *testing.T) {
	fait := &vueVerif{Machine: "web1", Possible: true, Etat: "fait", Il: "il y a 9 s", Recu: "10:41:05"}
	if r := resultatServices(fait, nil); r.Classe != "ok" || !strings.Contains(r.Titre, "plus aucun service en échec") {
		t.Errorf("services réparés : %+v", r)
	}
	if r := resultatServices(fait, []string{"nginx.service"}); r.Classe != "attention" || !strings.Contains(r.Titre, "nginx.service toujours en échec") {
		t.Errorf("service encore en échec : %+v", r)
	}
	if r := resultatAlerte(fait, false, "x"); r.Titre != "Réparé : alerte fermée" {
		t.Errorf("alerte fermée : %+v", r)
	}
	if r := resultatAlerte(&vueVerif{Etat: "sans-reponse", Il: "il y a 40 s"}, true, "x"); !strings.Contains(r.Titre, "Pas de réponse") {
		t.Errorf("sans réponse : %+v", r)
	}
	if resultatServices(&vueVerif{Possible: true}, nil) != nil || resultatAlerte(nil, true, "") != nil {
		t.Error("résultat sans vérification récente")
	}
}

// Bloc Paquets sans liste (retour du 25/11) : le motif de l'échec envoyé
// par l'agent remplace le message « agent trop ancien ».
func TestPaquetsSansListe(t *testing.T) {
	b := nouveauBanc(t)
	b.metriquesSimulees(t, []serieTest{
		{[]string{"__name__", "apt_upgrades_pending", "instance", "web1"}, constante(76)},
		{[]string{"__name__", "apt_upgrades_calcule_le", "instance", "web1"}, constante(1)},
		{[]string{"__name__", "mirador_agent_mises_a_jour_echec", "instance", "web1", "raison", "apt-get : code de sortie 100"}, constante(1)},
		{[]string{"__name__", "apt_upgrades_pending", "instance", "vieux"}, constante(39)},
	})
	b.machinesApprouvees(t, "web1", "vieux")
	cl, _ := b.premierAdmin("admin", mdpAdmin)
	_, corps, _ := b.get(cl, "/machines/web1")
	if p := blocDetails(corps, `id="paquets"`); !strings.Contains(p, "le dernier calcul a échoué") || !strings.Contains(p, "apt-get : code de sortie 100") ||
		strings.Contains(p, "arrive avec") {
		t.Error("motif de l'échec absent")
	}
	_, vieux, _ := b.get(cl, "/machines/vieux")
	if p := blocDetails(vieux, `id="paquets"`); !strings.Contains(p, "n'envoie que le nombre de paquets (39)") {
		t.Error("agent ancien : message absent")
	}
}
