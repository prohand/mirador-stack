// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"strings"
	"testing"
	"time"
)

// Bloc « Changements » compact (retour du 19/11) : 5 lignes montrées, le
// reste replié ; changements identiques du même jour regroupés ; détail long
// coupé, entier en dépliant.
func TestBlocChangementsCompact(t *testing.T) {
	b := nouveauBanc(t)
	m := b.h.maintenant()
	b.machinesApprouvees(t, "pve1")
	ajout := func(il time.Duration, typ, detail string) {
		if _, err := b.c.db.Exec(`INSERT INTO changements(machine, le, type, detail) VALUES ('pve1', ?, ?, ?)`, m.Add(-il).Unix(), typ, detail); err != nil {
			t.Fatal(err)
		}
	}
	long := strings.Repeat("libpaquet-tres-long 1.2.3 → 1.2.4, ", 10)
	ajout(time.Hour, "paquets", long)
	// Même conteneur redémarré 3 fois le même jour : une ligne ×3.
	for i := 0; i < 3; i++ {
		ajout(2*time.Hour+time.Duration(i)*time.Minute, "conteneur", "appli recréé")
	}
	for i := 0; i < 8; i++ {
		ajout(time.Duration(30+i*12)*time.Hour, "agent", "0.3."+string(rune('0'+i)))
	}
	cl, _ := b.premierAdmin("admin", mdpAdmin)
	_, corps, _ := b.get(cl, "/machines/pve1")
	debut := strings.Index(corps, `id="changements"`)
	if debut < 0 {
		t.Fatal("bloc absent")
	}
	for _, attendu := range []string{"12 sur 7 jours", "×3", `class="detail-long"`, "…</summary>", "5 plus anciens", "data-sans-filtre"} {
		if !strings.Contains(corps[debut:], attendu) {
			t.Errorf("bloc Changements : %q absent", attendu)
		}
	}
	if n := strings.Count(corps[debut:strings.Index(corps[debut:], "plus-anciens")+debut], "<tr><td"); n != 5 {
		t.Errorf("%d lignes montrées d'office, attendu 5", n)
	}
}
