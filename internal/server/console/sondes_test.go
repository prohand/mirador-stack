// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"mirador-stack/internal/server/flotte"
)

// Page Sondes (SPEC § 48) : ajout (code 2FA), état, retrait.
func TestPageSondes(t *testing.T) {
	b := nouveauBanc(t)
	s := func(sonde, nom string, v float64, extra ...string) serieTest {
		return serieTest{append([]string{"__name__", nom, "instance", "mirador-server", "job", "sondes", "sonde", sonde}, extra...), constante(v)}
	}
	b.metriquesSimulees(t, []serieTest{
		s("Nextcloud", "mirador_sonde_succes", 0), s("Nextcloud", "mirador_sonde_duree_secondes", 0.9),
		s("Nextcloud", "mirador_sonde_erreur", 1, "erreur", "code 502"),
		s("Vaultwarden", "mirador_sonde_succes", 1), s("Vaultwarden", "mirador_sonde_duree_secondes", 0.041),
		s("Vaultwarden", "mirador_sonde_lente", 0),
	})
	cl, secret := b.premierAdmin("admin", mdpAdmin)
	ajouter := func(champs url.Values) string {
		champs.Set("csrf", b.csrf(cl, "/sondes"))
		champs.Set("action", "ajouter")
		champs.Set("code", b.code(secret))
		_, corps, _ := b.post(cl, "/sondes", champs)
		_, corps, _ = b.get(cl, "/sondes")
		return corps
	}
	ajouter(url.Values{"nom": {"Nextcloud"}, "type": {"http"}, "cible": {"https://cloud.exemple.fr/status.php"}, "depuis": {"serveur"},
		"code_http": {"200"}, "contenu": {"installed"}})
	b.h.avancer(31 * time.Second) // code TOTP suivant (anti-rejeu)
	corps := ajouter(url.Values{"nom": {"Vaultwarden"}, "type": {"http"}, "cible": {"https://pass.exemple.fr/alive"}, "depuis": {"serveur"}})
	for _, attendu := range []string{"<h1>Sondes</h1>", "Échec : code 502", "OK · 41 ms", "code 200 · contient « installed »",
		`class="tuile critique"`, "Temps de réponse médian", `name="nom" value="Nextcloud"`} {
		if !strings.Contains(corps, attendu) {
			t.Errorf("page des sondes : %q absent", attendu)
		}
	}
	if strings.Index(corps, "Nextcloud</strong>") > strings.Index(corps, "Vaultwarden</strong>") {
		t.Error("la sonde en échec doit venir en premier")
	}
	// Refus : collecteur inconnu, cible invalide.
	b.h.avancer(31 * time.Second)
	ajouter(url.Values{"nom": {"X"}, "type": {"tcp"}, "cible": {"a:22"}, "depuis": {"inconnu"}})
	b.h.avancer(31 * time.Second)
	ajouter(url.Values{"nom": {"Y"}, "type": {"http"}, "cible": {"ftp://a"}, "depuis": {"serveur"}})
	liste, _ := flotte.LireSondes(t.Context(), b.c.db)
	if len(liste) != 2 {
		t.Fatalf("sondes enregistrées : %+v", liste)
	}
	// Retrait.
	b.h.avancer(31 * time.Second)
	b.post(cl, "/sondes", url.Values{"csrf": {b.csrf(cl, "/sondes")}, "action": {"supprimer"}, "nom": {"Nextcloud"}, "code": {b.code(secret)}})
	if liste, _ := flotte.LireSondes(t.Context(), b.c.db); len(liste) != 1 || liste[0].Nom != "Vaultwarden" {
		t.Fatalf("après retrait : %+v", liste)
	}
	// Menu : compteur des sondes en échec.
	_, corps, _ = b.get(cl, "/")
	if !strings.Contains(corps, `href="/sondes"`) {
		t.Error("entrée Sondes absente du menu")
	}
}
