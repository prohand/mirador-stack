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

// montrerBlocsAccueil : disposition « ordinateur » enregistrée sans rien
// masquer (le tableau du parc et la carte de chaleur, masqués par défaut,
// ne sont plus construits tant qu'ils le restent : SPEC § 141).
func montrerBlocsAccueil(t *testing.T, b *banc, c *http.Client) {
	t.Helper()
	jeton := b.csrf(c, "/")
	r, _ := http.NewRequest(http.MethodPost, b.srv.URL+"/compte/accueil", strings.NewReader(`{"mode":"ordi","ordre":["bandeau"],"caches":[]}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-CSRF", jeton)
	r.Header.Set("X-Requested-With", "mirador")
	resp, err := c.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode >= 400 {
		t.Fatalf("disposition : %d", resp.StatusCode)
	}
}

// Vue d'ensemble, grand parc : sites à problème en tête, machines à
// regarder seules nommées, sites verts au-delà de 8 résumés.
func TestCompacterApercu(t *testing.T) {
	var sites []siteApercu
	for i := 0; i < 12; i++ {
		s := siteApercu{Titre: fmt.Sprintf("s%02d", i)}
		for j := 0; j < 10; j++ {
			s.Machines = append(s.Machines, puceEquipement{Nom: fmt.Sprintf("m%02d-%d", i, j), Point: "ok"})
		}
		sites = append(sites, s)
	}
	sites[7].Machines[0].Point = "erreur"
	sites[3].Machines[0].Point = "attention"
	out, verts := compacterApercu(sites)
	if len(out) != 8 || verts != 4 || out[0].Titre != "s07" || out[1].Titre != "s03" {
		t.Fatalf("%d sites, %d verts, ordre %s %s", len(out), verts, out[0].Titre, out[1].Titre)
	}
	if s := out[0]; len(s.Machines) != 1 || s.Ok != 9 || s.ARegarder != 1 || s.PctErreur != 10 || s.Lien != "/machines?site=s07" {
		t.Errorf("site à problème : %+v", s)
	}
}
