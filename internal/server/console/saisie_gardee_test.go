// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"mirador-stack/internal/server/flotte"
)

// Retour du 22/11 : une sonde de certificat qui vise un équipement pas
// encore rattaché : refus avec un message qui dit quoi faire, page ouverte
// en haut (sans ancre, le message est visible) et texte saisi réaffiché une
// fois (ajout ligne par ligne de la nouvelle page).
func TestSaisieGardeeApresRefus(t *testing.T) {
	b := nouveauBanc(t)
	c, secret := b.premierAdmin("admin", mdpAdmin)
	ctx := context.Background()
	_, _ = b.c.db.Exec(`INSERT INTO machines(nom, statut, type, cree_le) VALUES ('collecteur-lan', 'approuvee', 'collecteur', 1), ('nas', 'approuvee', 'agent', 1), ('fw', 'approuvee', 'agent', 1)`)
	idC, _ := flotte.IDMachine(ctx, b.c.db, "collecteur-lan")
	idNas, _ := flotte.IDMachine(ctx, b.c.db, "nas")
	_ = flotte.RattacherEquipement(ctx, b.c.db, idC, idNas, "admin", time.Now())
	_, _, entetes := b.post(c, "/machines/collecteur-lan", url.Values{"csrf": {b.csrf(c, "/machines")}, "action": {"sonde_ajouter"}, "code": {b.code(secret)},
		"sonde_adresse": {"fw.ous.local.srv-home.fr:10443"}, "machine": {"fw"}, "sonde_nom": {"fw.lan"}})
	if loc := entetes.Get("Location"); strings.Contains(loc, "#") {
		t.Errorf("redirection avec ancre après un refus : %q", loc)
	}
	_, page, _ := b.get(c, "/machines/collecteur-lan")
	if !strings.Contains(page, "rattacher d&#39;abord") {
		t.Error("message sans la marche à suivre")
	}
	if !strings.Contains(page, `value="fw.ous.local.srv-home.fr:10443"`) || !strings.Contains(page, `value="fw.lan"`) {
		t.Error("saisie perdue après le refus")
	}
	// Une seule fois : la page suivante montre la configuration enregistrée.
	if _, page, _ := b.get(c, "/machines/collecteur-lan"); strings.Contains(page, "fw.ous.local.srv-home.fr:10443") {
		t.Error("saisie réaffichée deux fois")
	}
}
