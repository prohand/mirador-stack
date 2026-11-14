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

// Collecteur dans l'interface (SPEC § 23.13) : jeton « Collecteur du LAN »,
// rattachement d'un équipement, équipement créé « relevé par ».
func TestCollecteurInterface(t *testing.T) {
	b := nouveauBanc(t)
	c, secret := b.premierAdmin("admin", mdpAdmin)
	ctx := context.Background()
	csrf := b.csrf(c, "/machines")
	b.h.avancer(31 * time.Second)
	_, corps, _ := b.post(c, "/machines/jeton", url.Values{"csrf": {csrf}, "type_machine": {"collecteur"}, "nom": {"collecteur-lan"},
		"duree": {"1h"}, "maj_auto": {"1"}, "code": {b.code(secret)}})
	if !strings.Contains(corps, "/api/agent/v1/install/collecteur -o mirador-collector-install.sh") || !strings.Contains(corps, "--auto-update") {
		t.Fatal("commande d'installation du collecteur absente")
	}
	// Collecteur approuvé (simulé) et une machine ordinaire.
	if _, err := b.c.db.Exec(`INSERT INTO machines(nom, statut, type, cree_le) VALUES ('collecteur-lan', 'approuvee', 'collecteur', 1), ('nas', 'approuvee', 'agent', 1)`); err != nil {
		t.Fatal(err)
	}
	idC, _ := flotte.IDMachine(ctx, b.c.db, "collecteur-lan")
	if _, corps, _ = b.get(c, "/machines/collecteur-lan"); !strings.Contains(corps, `id="equipements"`) || !strings.Contains(corps, "<option>nas</option>") {
		t.Fatal("bloc des équipements absent")
	}
	// Mode administration ouvert : plus de code.
	b.post(c, "/machines/collecteur-lan", url.Values{"csrf": {csrf}, "action": {"rattacher"}, "equipement": {"nas"}})
	if eqs, _ := flotte.EquipementsDe(ctx, b.c.db, idC); len(eqs) != 1 || eqs[0].Nom != "nas" {
		t.Fatalf("rattachement : %+v", eqs)
	}
	if _, corps, _ = b.get(c, "/machines/nas"); !strings.Contains(corps, `Relevé par le collecteur <a href="/machines/collecteur-lan">`) {
		t.Error("« relevé par » absent de la page de l'équipement")
	}
	// Équipement sans agent créé « relevé par » : pas de token, rattaché.
	if _, corps, _ = b.get(c, "/machines"); !strings.Contains(corps, `<select name="collecteur">`) {
		t.Fatal("choix « Relevé par » absent")
	}
	_, corps, _ = b.post(c, "/machines/equipement", url.Values{"csrf": {csrf}, "nom": {"imprimante"}, "collecteur": {"collecteur-lan"}})
	if strings.Contains(corps, "Token de") {
		t.Error("token créé pour un équipement relevé par un collecteur")
	}
	if eqs, _ := flotte.EquipementsDe(ctx, b.c.db, idC); len(eqs) != 2 {
		t.Fatalf("équipement relevé non rattaché : %+v", eqs)
	}
	// Détacher depuis la page du collecteur.
	b.post(c, "/machines/collecteur-lan", url.Values{"csrf": {csrf}, "action": {"detacher"}, "equipement": {"nas"}})
	if eqs, _ := flotte.EquipementsDe(ctx, b.c.db, idC); len(eqs) != 1 || eqs[0].Nom != "imprimante" {
		t.Fatalf("détachement : %+v", eqs)
	}
	// Sans code ni mode administration : refusé.
	b.h.avancer(DureeModeAdmin + time.Minute)
	b.post(c, "/machines/collecteur-lan", url.Values{"csrf": {csrf}, "action": {"rattacher"}, "equipement": {"nas"}})
	if eqs, _ := flotte.EquipementsDe(ctx, b.c.db, idC); len(eqs) != 1 {
		t.Error("rattachement sans code 2FA")
	}
}
