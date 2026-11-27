// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"mirador-stack/internal/server/flotte"
)

// Grand parc (SPEC § 141) : approuver toutes les machines en attente,
// changer le site de plusieurs machines d'un coup, filtre par site,
// page compacte (blocs sans problème repliés).
func TestMachinesMasse(t *testing.T) {
	b := nouveauBanc(t)
	c, _ := b.premierAdmin("admin", mdpAdmin)
	for _, q := range []string{
		`INSERT INTO reglages(cle, valeur, modifie_le, modifie_par) VALUES ('machines.listes', '{"sites":["Paris","Lyon"],"types":["vm"],"groupes":["web"]}', 0, 'x')`,
	} {
		if _, err := b.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 60; i++ {
		statut := "approuvee"
		if i >= 57 {
			statut = "en_attente"
		}
		if _, err := b.db.Exec(`INSERT INTO machines(nom, statut, site, groupe, cree_le, vu_le) VALUES (?, ?, 'Paris', 'web', 0, ?)`,
			fmt.Sprintf("m%02d", i), statut, b.h.maintenant().Unix()); err != nil {
			t.Fatal(err)
		}
	}
	jeton := b.csrf(c, "/machines")
	// Approuver les 3 en attente d'un coup.
	if code, _, _ := b.post(c, "/machines/masse", url.Values{"csrf": {jeton}, "action": {"approuver_tout"}}); code != http.StatusSeeOther {
		t.Fatalf("approuver_tout : %d", code)
	}
	ms, _ := flotte.ListerMachines(t.Context(), b.db)
	for _, m := range ms {
		if m.Statut != "approuvee" {
			t.Errorf("%s : %s après « tout approuver »", m.Nom, m.Statut)
		}
	}
	// m01 et m02 passent à Lyon ; le groupe est gardé.
	if code, _, _ := b.post(c, "/machines/masse", url.Values{"csrf": {jeton}, "action": {"etiquettes"}, "m": {"m01", "m02"},
		"site": {"Lyon"}, "env": {garderValeur}, "groupe": {garderValeur}}); code != http.StatusSeeOther {
		t.Fatalf("etiquettes : %d", code)
	}
	ms, _ = flotte.ListerMachines(t.Context(), b.db)
	lyon := 0
	for _, m := range ms {
		if m.Site == "Lyon" {
			lyon++
			if m.Groupe != "web" {
				t.Errorf("%s : groupe perdu (%q)", m.Nom, m.Groupe)
			}
		}
	}
	if lyon != 2 {
		t.Errorf("%d machines à Lyon, 2 attendues", lyon)
	}
	// Site inconnu : refusé, rien ne change.
	b.post(c, "/machines/masse", url.Values{"csrf": {jeton}, "action": {"etiquettes"}, "m": {"m03"}, "site": {"Mars"}, "env": {garderValeur}, "groupe": {garderValeur}})
	if m, _ := flotte.ListerMachines(t.Context(), b.db); m[3].Site == "Mars" {
		t.Error("site inconnu accepté")
	}
	// Aucune case cochée : refusé.
	b.post(c, "/machines/masse", url.Values{"csrf": {jeton}, "action": {"actualiser"}})
	// Page : 60 machines → compacte, blocs « tout va bien » repliés ;
	// filtre Lyon : les 2 machines.
	_, corps, _ := b.get(c, "/machines")
	if !strings.Contains(corps, "Voir les 58") || !strings.Contains(corps, `id="masse"`) {
		t.Errorf("page compacte : bloc Paris non replié ou barre absente")
	}
	_, corps, _ = b.get(c, "/machines?site=Lyon")
	if !strings.Contains(corps, `href="/machines/m01"`) || strings.Contains(corps, `href="/machines/m05"`) {
		t.Error("filtre site : mauvaises machines")
	}
}
