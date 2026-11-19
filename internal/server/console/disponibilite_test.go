// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestIntervallesDispo(t *testing.T) {
	iv := reunir([]intervalle{{10, 20}, {15, 30}, {40, 50}})
	if len(iv) != 2 || iv[0] != (intervalle{10, 30}) {
		t.Fatalf("réunis : %+v", iv)
	}
	iv = retirer(iv, []intervalle{{12, 18}, {45, 60}})
	if got := dureeDans(iv, 0, 100); got != 2+12+5 {
		t.Fatalf("durée hors maintenances : %d (%+v)", got, iv)
	}
	if dureeDans(iv, 25, 42) != 5+2 {
		t.Fatal("durée dans une fenêtre")
	}
	for v, attendu := range map[float64]string{100: "100", 99.5: "99,5", 99.86: "99,86", 90: "90", 0: "0", 99.999: "100"} {
		if pct(v) != attendu {
			t.Errorf("pct(%v) : %q", v, pct(v))
		}
	}
	for s, attendu := range map[int64]string{0: "0", 30: "moins d'1 min", 720: "12 min", 20460: "5 h 41", 3*86400 + 7200: "3 j 2 h"} {
		if dureeArret(s) != attendu {
			t.Errorf("%d s : %q", s, dureeArret(s))
		}
	}
}

// Page Disponibilité (SPEC § 49) : panne de 2 h dont 1 h en maintenance,
// objectif 99,9 % dépassé ; objectif changé par un administrateur.
func TestPageDisponibilite(t *testing.T) {
	b := nouveauBanc(t)
	m := b.h.maintenant()
	b.machinesApprouvees(t, "web1", "nas")
	_, _ = b.c.db.Exec(`UPDATE machines SET cree_le = ?`, m.Add(-60*24*time.Hour).Unix())
	debut := m.Add(-3 * 24 * time.Hour)
	if _, err := b.c.db.Exec(`INSERT INTO alertes(regle, etiquettes, gravite, etat, debut, active_le, fin, resume)
		VALUES ('AgentMuet', '{"alertname":"AgentMuet","instance":"web1","severity":"critical"}', 'critical', 'resolue', ?, ?, ?, '')`,
		debut.UnixMilli(), debut.UnixMilli(), debut.Add(2*time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.db.Exec(`INSERT INTO maintenances(cible, debut, fin, auteur, cree_le) VALUES ('machine:web1', ?, ?, 'admin', ?)`,
		debut.Unix(), debut.Add(time.Hour).Unix(), debut.Unix()); err != nil {
		t.Fatal(err)
	}
	cl, secret := b.premierAdmin("admin", mdpAdmin)
	b.post(cl, "/disponibilite", url.Values{"csrf": {b.csrf(cl, "/disponibilite")}, "cle": {"m:web1"}, "objectif": {"99,9"}, "code": {b.code(secret)}})
	_, corps, _ := b.get(cl, "/disponibilite")
	// 1 h d'arrêt sur 30 j : 99,86 % ; permis à 99,9 % : 43 min.
	for _, attendu := range []string{"<h1>Disponibilité</h1>", "99,86 %", "1 h 00 / 43 min", "Dépassé de 16 min", `value="99,9"`, "100 %"} {
		if !strings.Contains(corps, attendu) {
			t.Errorf("page : %q absent", attendu)
		}
	}
	if strings.Index(corps, "<strong>web1</strong>") > strings.Index(corps, "<strong>nas</strong>") {
		t.Error("web1 (sous l'objectif) doit venir en premier")
	}
	if strings.Count(corps, `<i class="a"`)+strings.Count(corps, `<i class="k"`) != 1 {
		t.Errorf("un seul jour avec arrêt attendu")
	}
}
