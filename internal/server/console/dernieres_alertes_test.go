// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"strings"
	"testing"
	"time"

	"mirador-stack/internal/server/flotte"
	essais "mirador-stack/internal/sondes"
)

// « Dernières alertes » (SPEC § 72) : page machine (onglet Activité) et
// fiche d'une sonde ; retour à la normale ou « En cours » ; une condition
// restée en attente (jamais passée en alerte) n'est pas montrée.
func TestDernieresAlertes(t *testing.T) {
	b := nouveauBanc(t)
	m := b.h.maintenant()
	b.machinesApprouvees(t, "nas")
	_, _ = b.c.db.Exec(`INSERT INTO machines(nom, statut, type, cree_le) VALUES ('lan', 'approuvee', 'collecteur', 1)`)
	_ = flotte.EnregistrerSondes(t.Context(), b.c.db, []essais.Sonde{{Nom: "HA", Type: essais.HTTP, Cible: "https://ha.lan/", Depuis: "lan"}}, "test", m)
	ajout := func(regle, etq, etat string, debut, fin time.Duration, active bool) {
		t.Helper()
		act, f := m.Add(-debut).UnixMilli(), int64(0)
		if !active {
			act = 0
		}
		if fin > 0 {
			f = m.Add(-fin).UnixMilli()
		}
		if _, err := b.c.db.Exec(`INSERT INTO alertes(regle, etiquettes, gravite, etat, debut, active_le, fin, resume) VALUES (?, ?, 'critical', ?, ?, ?, ?, ?)`,
			regle, etq, etat, m.Add(-debut).UnixMilli(), act, f, regle+" résumé"); err != nil {
			t.Fatal(err)
		}
	}
	ajout("DisquePlein", `{"instance":"nas"}`, "resolue", 3*time.Hour, 2*time.Hour, true)
	ajout("ChargeElevee", `{"instance":"nas"}`, "active", 20*time.Minute, 0, true)
	ajout("MemoireHaute", `{"instance":"nas"}`, "resolue", time.Hour, 50*time.Minute, false) // jamais passée en alerte
	ajout("SondeEnEchec", `{"instance":"lan","sonde":"HA"}`, "resolue", 5*time.Hour, 4*time.Hour, true)
	cl, _ := b.premierAdmin("admin", mdpAdmin)
	_, corps, _ := b.get(cl, "/machines/nas")
	for _, attendu := range []string{`id="dernieres-alertes"`, "DisquePlein", "ChargeElevee", `class="pastille ok">✓`, `class="pastille erreur">En cours`,
		"1 h", `href="/alertes?machine=nas"`} {
		if !strings.Contains(corps, attendu) {
			t.Errorf("page machine : %q absent", attendu)
		}
	}
	if strings.Contains(corps, "MemoireHaute") {
		t.Error("condition jamais passée en alerte montrée")
	}
	_, corps, _ = b.get(cl, "/sondes/HA")
	if !strings.Contains(corps, `id="dernieres-alertes"`) || !strings.Contains(corps, "SondeEnEchec") || strings.Contains(corps, "DisquePlein") ||
		!strings.Contains(corps, `href="/alertes?sonde=HA"`) {
		t.Error("fiche de la sonde : dernières alertes de la sonde seulement")
	}
}
