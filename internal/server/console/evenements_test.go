// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
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

// Relecture du 21/11 : le filtre machine passe avant la limite ; une
// machine calme garde ses événements même si une autre en a beaucoup.
func TestEvenementsMachineAvantLimite(t *testing.T) {
	b := nouveauBanc(t)
	m := b.h.maintenant()
	b.machinesApprouvees(t, "bavarde", "calme")
	for i := 0; i < 30; i++ {
		if _, err := b.c.db.Exec(`INSERT INTO changements(machine, le, type, detail) VALUES ('bavarde', ?, 'paquets', 'x 1 → 2')`, m.Add(-time.Duration(i)*time.Minute).Unix()); err != nil {
			t.Fatal(err)
		}
		if _, err := b.c.db.Exec(`INSERT INTO audit(le, qui, action, objet) VALUES (?, 'web:admin', 'machine_renommee', 'bavarde')`, m.Add(-time.Duration(i)*time.Minute).Unix()); err != nil {
			t.Fatal(err)
		}
	}
	_, _ = b.c.db.Exec(`INSERT INTO changements(machine, le, type, detail) VALUES ('calme', ?, 'paquets', 'y 1 → 2')`, m.Add(-2*time.Hour).Unix())
	_, _ = b.c.db.Exec(`INSERT INTO audit(le, qui, action, objet) VALUES (?, 'web:admin', 'machine_renommee', 'calme')`, m.Add(-2*time.Hour).Unix())
	_, _ = b.c.db.Exec(`INSERT INTO audit(le, qui, action, objet) VALUES (?, 'web:admin', 'machine_renommee', 'calme_x')`, m.Add(-2*time.Hour).Unix())
	ev := b.c.journalEvenements(context.Background(), filtreEvenements{Debut: m.Add(-24 * time.Hour), Fin: m, Machine: "calme", Max: 10})
	var chgt, reglage int
	for _, e := range ev {
		switch e.Type {
		case evChangement:
			chgt++
		case evReglage:
			reglage++
		}
	}
	if chgt != 1 || reglage != 1 {
		t.Errorf("machine calme : %d changement(s), %d réglage(s) ; attendu 1 et 1", chgt, reglage)
	}
}

// Relecture du 21/11 : un élément cité par un événement ancien le retrouve
// même si le parc en a eu des centaines d'autres depuis (fiche, element=).
func TestEvenementsCiteAvantLimite(t *testing.T) {
	b := nouveauBanc(t)
	m := b.h.maintenant()
	b.machinesApprouvees(t, "web1")
	for i := 0; i < 400; i++ {
		if _, err := b.c.db.Exec(`INSERT INTO changements(machine, le, type, detail) VALUES ('web1', ?, 'paquets', 'autre 1 → 2')`, m.Add(-time.Duration(i)*time.Minute).Unix()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := b.c.db.Exec(`INSERT INTO audit(le, qui, action, objet) VALUES (?, 'web:admin', 'sonde', 'box-internet')`, m.Add(-3*24*time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	ev := b.c.journalEvenements(context.Background(), filtreEvenements{Debut: m.Add(-7 * 24 * time.Hour), Fin: m, Max: 15, Cite: "box-internet"})
	if len(ev) != 1 || !strings.Contains(ev[0].Texte, "box-internet") {
		t.Fatalf("événements cités : %+v", ev)
	}
	cl, _ := b.premierAdmin("admin", mdpAdmin)
	if _, corps, _ := b.get(cl, "/evenements?element=box-internet"); !strings.Contains(corps, "box-internet") || strings.Contains(corps, "autre 1") {
		t.Error("page Événements filtrée par élément")
	}
}
