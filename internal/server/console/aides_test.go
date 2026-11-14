// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// Aides (SPEC § 26) : bouton « Avec aides / Sans aides » retenu dans le
// compte ; les avertissements restent ; textes courts avec « En savoir plus ».
func TestAidesUtilisateur(t *testing.T) {
	b := nouveauBanc(t)
	c, _ := b.premierAdmin("admin", mdpAdmin)
	csrf := b.csrf(c, "/")
	_, corps, _ := b.get(c, "/securite")
	if !strings.Contains(corps, `action="/compte/aides"`) || !strings.Contains(corps, `value="avec" role="radio" aria-checked="true"`) ||
		strings.Contains(corps, `class="sans-aides"`) {
		t.Fatal("bouton absent ou mauvais état par défaut")
	}
	// Aide courte, durée lisible, lien vers la doc ; avertissement sans classe « aide ».
	for _, attendu := range []string{`<small class="aide">5 min à 8 h (défaut 30 min)</small>`, `aide.md#securite-mode-admin`, `<small>Votre adresse`} {
		if !strings.Contains(corps, attendu) {
			t.Errorf("absent de la page Sécurité : %s", attendu)
		}
	}
	// Formulaire sans JavaScript, puis appel JavaScript.
	code, _, ent := b.post(c, "/compte/aides", url.Values{"csrf": {csrf}, "aides": {"sans"}, "retour": {"/securite"}})
	if code != http.StatusSeeOther || ent.Get("Location") != "/securite" {
		t.Fatalf("retour : %d %q", code, ent.Get("Location"))
	}
	if _, corps, _ = b.get(c, "/securite"); !strings.Contains(corps, `class="sans-aides"`) || !strings.Contains(corps, `value="sans" role="radio" aria-checked="true"`) {
		t.Error("« Sans aides » non appliqué")
	}
	if code, _, _ = b.post(c, "/compte/aides", url.Values{"aides": {"avec"}}, "X-Requested-With", "mirador", "X-CSRF", csrf); code != http.StatusNoContent {
		t.Fatalf("appel JavaScript : %d", code)
	}
	if _, corps, _ = b.get(c, "/"); strings.Contains(corps, `class="sans-aides"`) {
		t.Error("« Avec aides » non appliqué")
	}
	if code, _, _ = b.post(c, "/compte/aides", url.Values{"csrf": {csrf}, "aides": {"peut-etre"}}); code != http.StatusBadRequest {
		t.Errorf("choix inconnu : %d", code)
	}
	if code, _, _ = b.post(c, "/compte/aides", url.Values{"aides": {"sans"}}); code != http.StatusForbidden {
		t.Errorf("sans jeton CSRF : %d", code)
	}
}

func TestDureeCourte(t *testing.T) {
	type duree time.Duration // comme alertes.Duree
	for v, attendu := range map[any]string{
		30 * time.Minute: "30 min", 12 * time.Hour: "12 h", duree(7 * 24 * time.Hour): "7 j",
		duree(365 * 24 * time.Hour): "1 an", 730 * 24 * time.Hour: "2 ans", 90 * time.Second: "1m30s", "x": "x",
	} {
		if r := dureeCourte(v); r != attendu {
			t.Errorf("%v → %q (attendu %q)", v, r, attendu)
		}
	}
}
