// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// Thème par utilisateur : choisi dans le menu, retenu dans le compte,
// prioritaire sur le réglage du serveur.
func TestThemeUtilisateur(t *testing.T) {
	b := nouveauBanc(t)
	c, _ := b.premierAdmin("admin", mdpAdmin)
	csrf := b.csrf(c, "/")
	_, corps, _ := b.get(c, "/")
	if !strings.Contains(corps, `action="/compte/theme"`) || !strings.Contains(corps, `value="auto" role="radio" aria-checked="true"`) || strings.Contains(corps, "data-theme=") {
		t.Fatal("sélecteur absent ou mauvais état par défaut")
	}
	// Formulaire sans JavaScript : enregistré, retour à la page.
	code, _, ent := b.post(c, "/compte/theme", url.Values{"csrf": {csrf}, "theme": {"sombre"}, "retour": {"/machines?groupe=lan"}})
	if code != http.StatusSeeOther || ent.Get("Location") != "/machines?groupe=lan" {
		t.Fatalf("retour : %d %q", code, ent.Get("Location"))
	}
	if _, corps, _ = b.get(c, "/"); !strings.Contains(corps, `data-theme="sombre"`) || !strings.Contains(corps, `value="sombre" role="radio" aria-checked="true"`) {
		t.Error("thème sombre non appliqué")
	}
	// Depuis le JavaScript : 204, jeton dans l'en-tête.
	code, _, _ = b.post(c, "/compte/theme", url.Values{"theme": {"clair"}}, "X-Requested-With", "mirador", "X-CSRF", csrf)
	if code != http.StatusNoContent {
		t.Fatalf("appel JavaScript : %d", code)
	}
	if _, corps, _ = b.get(c, "/"); !strings.Contains(corps, `data-theme="clair"`) {
		t.Error("thème clair non appliqué")
	}
	// Auto : plus d'attribut (suit le système).
	b.post(c, "/compte/theme", url.Values{"csrf": {csrf}, "theme": {"auto"}})
	if _, corps, _ = b.get(c, "/"); strings.Contains(corps, "data-theme=") {
		t.Error("auto : data-theme encore présent")
	}
	// Refus : thème inconnu, jeton absent ; retour hors du site ramené à « / ».
	if code, _, _ = b.post(c, "/compte/theme", url.Values{"csrf": {csrf}, "theme": {"rose"}}); code != http.StatusBadRequest {
		t.Errorf("thème inconnu : %d", code)
	}
	if code, _, _ = b.post(c, "/compte/theme", url.Values{"theme": {"sombre"}}); code != http.StatusForbidden {
		t.Errorf("sans jeton CSRF : %d", code)
	}
	for _, r := range []string{"//exemple.org/", "https://exemple.org/", "javascript:alert(1)", `/\exemple.org`} {
		if _, _, ent = b.post(c, "/compte/theme", url.Values{"csrf": {csrf}, "theme": {"auto"}, "retour": {r}}); ent.Get("Location") != "/" {
			t.Errorf("retour %q → %q", r, ent.Get("Location"))
		}
	}
}
