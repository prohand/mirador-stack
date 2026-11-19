// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"strings"
	"testing"
	"time"
)

// Journal d'événements (SPEC § 53) : alertes, changements, réglages,
// maintenances ; filtres machine et type ; réglages automatiques masqués.
func TestEvenements(t *testing.T) {
	b := nouveauBanc(t)
	m := b.h.maintenant()
	b.machinesApprouvees(t, "web1", "nas")
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := b.c.db.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO alertes(regle, etiquettes, gravite, etat, debut, active_le, fin, resume) VALUES ('AgentMuet',
		'{"alertname":"AgentMuet","instance":"web1"}', 'critical', 'resolue', ?, ?, ?, 'web1 ne remonte plus')`,
		m.Add(-3*time.Hour).UnixMilli(), m.Add(-3*time.Hour).UnixMilli(), m.Add(-2*time.Hour).UnixMilli())
	exec(`INSERT INTO changements(machine, le, type, detail) VALUES ('web1', ?, 'paquets', 'openssl 1 → 2')`, m.Add(-4*time.Hour).Unix())
	exec(`INSERT INTO audit(le, qui, action, objet) VALUES (?, 'web:admin', 'machine_renommee', 'nas')`, m.Add(-time.Hour).Unix())
	exec(`INSERT INTO audit(le, qui, action, objet) VALUES (?, 'mirador', 'regle', 'AgentMuet')`, m.Add(-time.Hour).Unix())
	exec(`INSERT INTO maintenances(cible, debut, fin, auteur, commentaire, cree_le) VALUES ('machine:nas', ?, ?, 'admin', 'disques', ?)`,
		m.Add(-30*time.Minute).Unix(), m.Add(time.Hour).Unix(), m.Unix())
	cl, _ := b.premierAdmin("admin", mdpAdmin)
	_, corps, _ := b.get(cl, "/evenements")
	for _, attendu := range []string{"<h1>Événements</h1>", "Agent muet — web1 ne remonte plus", "Agent muet : terminée (1 h 00)",
		"Paquets : 1 paquet mis à jour", "Maintenance de machine:nas", "Aujourd&#39;hui"} {
		if !strings.Contains(corps, attendu) {
			t.Errorf("page : %q absent", attendu)
		}
	}
	if strings.Count(corps, `class="ev-ligne"`) < 5 || strings.Contains(corps, "(mirador)") {
		t.Error("réglage automatique affiché, ou lignes manquantes")
	}
	// Ordre : le plus récent d'abord (maintenance avant les alertes).
	if strings.Index(corps, "Maintenance de") > strings.Index(corps, "Agent muet : terminée") {
		t.Error("ordre")
	}
	_, corps, _ = b.get(cl, "/evenements?machine=web1&type=alerte")
	if strings.Contains(corps, "Maintenance de") || strings.Contains(corps, "Paquets") || !strings.Contains(corps, "Agent muet") {
		t.Error("filtres machine et type")
	}
}
