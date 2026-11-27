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

// Mises à jour par vagues depuis la page (SPEC § 141) : lancer, voir les
// vagues, mettre en pause ; « Réessayer les échecs ».
func TestPageVagues(t *testing.T) {
	b := nouveauBanc(t)
	c, secret := b.premierAdmin("admin", mdpAdmin)
	for _, q := range []string{
		`INSERT INTO versions_binaires(produit, version, architecture, sha256, taille, signature, publiee_le) VALUES ('mirador-agent', '0.7.0', 'amd64', 'x', 1, 'x', 1)`,
		`INSERT INTO machines(nom, statut, site, version_agent, cree_le, vu_le, battement_le) VALUES ('essai', 'approuvee', 'Paris', '0.6.0', 0, 0, 1)`,
		`INSERT INTO machines(nom, statut, site, version_agent, maj_echec, cree_le, vu_le, battement_le) VALUES ('p2', 'approuvee', 'Paris', '0.6.0', 'signature refusée', 0, 0, 1)`,
	} {
		if _, err := b.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	jeton := b.csrf(c, "/mises-a-jour")
	_, corps, _ := b.get(c, "/mises-a-jour")
	if !strings.Contains(corps, "Déployer par vagues") || !strings.Contains(corps, "Réessayer les 1 échec") {
		t.Fatal("bloc vagues ou bouton « Réessayer » absent")
	}
	b.post(c, "/mises-a-jour", url.Values{"csrf": {jeton}, "action": {"vagues"}, "version": {"0.7.0"}, "essai": {"essai"}, "site": {"Paris"}, "code": {b.code(secret)}})
	_, corps, _ = b.get(c, "/mises-a-jour")
	for _, attendu := range []string{"Déploiement de 0.7.0 par vagues", "Machine d&#39;essai · essai", "Site Paris", "Tout le parc", "Mettre en pause"} {
		if !strings.Contains(corps, attendu) {
			t.Errorf("%q absent", attendu)
		}
	}
	b.post(c, "/mises-a-jour", url.Values{"csrf": {jeton}, "action": {"vagues_pause"}, "code": {b.code(secret)}})
	if _, corps, _ = b.get(c, "/mises-a-jour"); !strings.Contains(corps, "en pause") {
		t.Error("pause non montrée")
	}
	b.post(c, "/mises-a-jour", url.Values{"csrf": {jeton}, "action": {"reessayer_echecs"}, "code": {b.code(secret)}})
}

// Alertes, grand parc : groupe par site, résumé d'un groupe par règle.
func TestGroupesAlertesGrandParc(t *testing.T) {
	sites := map[string]string{"p1": "Paris", "p2": "Paris", "l1": "Lyon", "m1": "Marseille", "x": ""}
	if k, titre, lien := cleGroupeAlerte(resumeAlerte{Machine: "p1", Regle: "DisqueFaible"}, "site", sites); k != "s:Paris" || titre != "Site Paris" || lien != "/machines?site=Paris" {
		t.Errorf("site : %q %q %q", k, titre, lien)
	}
	if k, titre, _ := cleGroupeAlerte(resumeAlerte{Machine: "x"}, "site", sites); k != "s:" || titre != "sans site" {
		t.Errorf("sans site : %q %q", k, titre)
	}
	if k, _, _ := cleGroupeAlerte(resumeAlerte{Machine: "p1", Regle: "DisqueFaible"}, "regle", sites); k != "r:DisqueFaible" {
		t.Errorf("règle : %q", k)
	}
	var as []vueAlerte
	for _, m := range []string{"p1", "p2", "l1", "m1"} {
		as = append(as, vueAlerte{resumeAlerte: resumeAlerte{Machine: m}})
	}
	if r := resumeGroupe(as, "regle", sites); r != "4 machines · Paris 2, Lyon 1, + 1 site" {
		t.Errorf("résumé : %q", r)
	}
}
