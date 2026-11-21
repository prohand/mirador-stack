// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"net/url"
	"strings"
	"testing"
	"time"
)

// Mode administration (retour du 21/11) : rouvrable depuis n'importe quelle
// page ; la fin du mode est donnée au navigateur.
func TestModeAdminBandeaux(t *testing.T) {
	b := nouveauBanc(t)
	c, secret := b.premierAdmin("admin", mdpAdmin)
	// Ouvert (juste après le code de connexion) : bandeau vert, secondes
	// restantes, bandeau « fermé » présent mais caché.
	_, corps, _ := b.get(c, "/sondes")
	if !strings.Contains(corps, `data-mode-admin-reste="600"`) || !strings.Contains(corps, `data-bandeau-admin-ferme hidden`) {
		t.Fatal("mode ouvert : reste ou bandeau caché absent")
	}
	// Fermé : bandeau « fermé » visible, bouton « Ouvrir » vers la même page.
	b.quitterModeAdmin(c)
	_, corps, _ = b.get(c, "/sondes")
	if strings.Contains(corps, "data-mode-admin-reste") || strings.Contains(corps, `data-bandeau-admin-ferme hidden`) ||
		!strings.Contains(corps, `action="/mode-admin/ouvrir"`) || !strings.Contains(corps, `name="retour" value="/sondes"`) {
		t.Fatal("mode fermé : bandeau « Ouvrir » absent")
	}
	// Rouvrir avec un code : retour sur la page, mode ouvert.
	b.h.avancer(31 * time.Second)
	_, _, ent := b.post(c, "/mode-admin/ouvrir", url.Values{"csrf": {b.csrf(c, "/sondes")}, "retour": {"/sondes"}, "code": {b.code(secret)}})
	if ent.Get("Location") != "/sondes" {
		t.Fatalf("retour : %q", ent.Get("Location"))
	}
	if _, corps, _ = b.get(c, "/sondes"); !strings.Contains(corps, "Mode administration jusqu") {
		t.Error("mode non rouvert")
	}
	// Mode désactivé (0) : pas de bandeau « fermé ».
	sec := SecuriteDefaut()
	sec.ModeAdmin = 0
	b.c.securite.Store(&sec)
	if _, corps, _ = b.get(c, "/sondes"); strings.Contains(corps, "data-bandeau-admin-ferme") {
		t.Error("bandeau « fermé » alors que le mode est désactivé")
	}
}
