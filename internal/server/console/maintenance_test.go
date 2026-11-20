// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"mirador-stack/internal/server/notif"
)

// Maintenance récurrente depuis la page (SPEC § 81) : chaque mois, de 23:00
// à 01:00 (le lendemain), et prochaine fenêtre affichée.
func TestMaintenanceMensuelle(t *testing.T) {
	b := nouveauBanc(t)
	c, _ := b.premierAdmin("admin", mdpAdmin)
	csrf := b.csrf(c, "/sourdine")
	b.post(c, "/sourdine", url.Values{"csrf": {csrf}, "action": {"maintenance"}, "cible": {"machine:pve1"},
		"repeter": {"mois"}, "jour_mois": {"31"}, "de": {"23:00"}, "a": {"01:00"}, "commentaire": {"sauvegarde"}})
	ms, err := notif.ListerMaintenances(context.Background(), b.db)
	if err != nil || len(ms) != 1 || ms[0].Recurrence == nil {
		t.Fatalf("maintenance : %v %+v", err, ms)
	}
	if r := ms[0].Recurrence; r.Rythme != "mois" || r.JourMois != 31 || r.Heure != "23:00" || r.Duree.String() != "2h" {
		t.Fatalf("récurrence : %+v", r)
	}
	_, corps, _ := b.get(c, "/sourdine")
	if !strings.Contains(corps, "le dernier jour de chaque mois à 23:00 pendant 2h") || !strings.Contains(corps, "Prochaine :") {
		t.Errorf("page :\n%s", extrait(corps, "Fenêtres de maintenance"))
	}
	// Heures illisibles : refusé.
	b.post(c, "/sourdine", url.Values{"csrf": {csrf}, "action": {"maintenance"}, "cible": {"machine:pve1"},
		"repeter": {"jour"}, "de": {"25:00"}, "a": {"01:00"}})
	if ms, _ := notif.ListerMaintenances(context.Background(), b.db); len(ms) != 1 {
		t.Errorf("heure invalide acceptée : %d", len(ms))
	}
}
