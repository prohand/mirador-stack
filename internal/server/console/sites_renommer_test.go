// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"net/url"
	"slices"
	"strings"
	"testing"
)

// Renommer, fusionner et ranger les machines (SPEC § 107).
func TestSitesRenommer(t *testing.T) {
	b := nouveauBanc(t)
	c, _ := b.premierAdmin("admin", mdpAdmin)
	csrf := b.csrf(c, "/reglages/sites")
	ctx := context.Background()
	poster := func(v url.Values) (string, string) {
		v.Set("csrf", csrf)
		_, corps, ent := b.post(c, "/reglages/sites", v)
		return corps, ent.Get("Location")
	}
	for _, x := range [][2]string{{"sites", "maison"}, {"sites", "bureau"}, {"groupes", "dns"}, {"groupes", "web"}} {
		poster(url.Values{"liste": {x[0]}, "action": {"ajouter"}, "valeur": {x[1]}})
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := b.db.Exec(q, args...); err != nil {
			t.Fatalf("%s : %v", q, err)
		}
	}
	exec(`INSERT INTO machines(nom, statut, cree_le, site, groupe) VALUES ('pc1', 'approuvee', 1, 'maison', 'dns'), ('pc2', 'approuvee', 1, 'maison', '')`)
	exec(`INSERT INTO maintenances(cible, auteur, cree_le) VALUES ('site:maison', 'admin', 1), ('groupe:dns', 'admin', 1)`)
	exec(`INSERT INTO reglages(cle, valeur, modifie_le) VALUES ('config_agent:groupe:dns', '{"a":1}', 1), ('config_agent:groupe:web', '{"b":2}', 1)`)
	exec(`INSERT INTO modeles_attribues(portee, modele, modifie_le) VALUES ('groupe:dns', 'linux', 1), ('groupe:dns', 'docker', 1), ('groupe:web', 'linux', 1)`)
	exec(`INSERT INTO regles(nom, definition, modifie_le) VALUES ('MaisonLente', '{"expr":"up{site=\"maison\"} == 0"}', 1)`)
	exec(`INSERT INTO tableaux(nom, contenu, modifie_le, modifie_par) VALUES ('maison-reseau', '{"requete":"x{site=\"maison\"}"}', 1, 'admin')`)

	// Page : machines en pastilles, « Aussi utilisé par », panneau avec l'impact.
	_, corps, _ := b.get(c, "/reglages/sites?liste=sites&renommer=maison")
	for _, attendu := range []string{`href="/machines/pc1"`, "1 maintenance", "Renommer « maison »", "<strong>2 machines</strong>",
		`href="/regles/modifier/MaisonLente"`, `href="/tableaux/maison-reseau"`, "à vérifier"} {
		if !strings.Contains(corps, attendu) {
			t.Errorf("page : %q absent", attendu)
		}
	}

	// Renommer maison → domicile : machines, maintenance et liste suivent.
	poster(url.Values{"liste": {"sites"}, "action": {"renommer"}, "valeur": {"maison"}, "nouveau": {"domicile"}})
	var n int
	_ = b.db.QueryRow(`SELECT COUNT(*) FROM machines WHERE site = 'domicile'`).Scan(&n)
	if n != 2 {
		t.Fatalf("machines renommées : %d", n)
	}
	_ = b.db.QueryRow(`SELECT COUNT(*) FROM maintenances WHERE cible = 'site:domicile'`).Scan(&n)
	if n != 1 {
		t.Error("maintenance non suivie")
	}
	if l := b.c.lireListes(ctx); !slices.Contains(l.Sites, "domicile") || slices.Contains(l.Sites, "maison") {
		t.Errorf("liste : %v", l.Sites)
	}
	_ = b.db.QueryRow(`SELECT COUNT(*) FROM audit WHERE action = 'renommer_site'`).Scan(&n)
	if n != 1 {
		t.Error("audit du renommage absent")
	}

	// Nom déjà pris : rien sans confirmation, retour au panneau.
	_, loc := poster(url.Values{"liste": {"sites"}, "action": {"renommer"}, "valeur": {"domicile"}, "nouveau": {"bureau"}})
	if _ = b.db.QueryRow(`SELECT COUNT(*) FROM machines WHERE site = 'bureau'`).Scan(&n); n != 0 || !strings.Contains(loc, "nouveau=bureau") {
		t.Fatalf("fusion sans confirmation : %d machine(s), retour %q", n, loc)
	}
	if _, corps, _ = b.get(c, loc); !strings.Contains(corps, `name="fusionner"`) {
		t.Error("case « Fusionner » absente")
	}
	poster(url.Values{"liste": {"sites"}, "action": {"renommer"}, "valeur": {"domicile"}, "nouveau": {"bureau"}, "fusionner": {"1"}})
	if _ = b.db.QueryRow(`SELECT COUNT(*) FROM machines WHERE site = 'bureau'`).Scan(&n); n != 2 {
		t.Fatalf("fusion : %d", n)
	}
	if l := b.c.lireListes(ctx); slices.Contains(l.Sites, "domicile") || !slices.Contains(l.Sites, "bureau") {
		t.Errorf("liste après fusion : %v", l.Sites)
	}

	// Groupes : fusion dns → web ; réglages et modèles de « web » gardés.
	poster(url.Values{"liste": {"groupes"}, "action": {"renommer"}, "valeur": {"dns"}, "nouveau": {"web"}, "fusionner": {"1"}})
	var v string
	_ = b.db.QueryRow(`SELECT valeur FROM reglages WHERE cle = 'config_agent:groupe:web'`).Scan(&v)
	_ = b.db.QueryRow(`SELECT COUNT(*) FROM reglages WHERE cle = 'config_agent:groupe:dns'`).Scan(&n)
	if v != `{"b":2}` || n != 0 {
		t.Errorf("réglages d'agent après fusion : %q, ancien %d", v, n)
	}
	var mods []string
	rows, _ := b.db.Query(`SELECT modele FROM modeles_attribues WHERE portee = 'groupe:web' ORDER BY modele`)
	for rows.Next() {
		var m string
		_ = rows.Scan(&m)
		mods = append(mods, m)
	}
	_ = rows.Close()
	if !slices.Equal(mods, []string{"docker", "linux"}) {
		t.Errorf("modèles après fusion : %v", mods)
	}
	_ = b.db.QueryRow(`SELECT COUNT(*) FROM maintenances WHERE cible = 'groupe:web'`).Scan(&n)
	if n != 1 {
		t.Error("maintenance de groupe non suivie")
	}
	if _ = b.db.QueryRow(`SELECT COUNT(*) FROM machines WHERE groupe = 'web'`).Scan(&n); n != 1 {
		t.Errorf("machine du groupe : %d", n)
	}

	// Noms refusés.
	poster(url.Values{"liste": {"types"}, "action": {"renommer"}, "valeur": {"vm"}, "nouveau": {"pas bon"}})
	if l := b.c.lireListes(ctx); !slices.Contains(l.Types, "vm") {
		t.Error("nom invalide accepté")
	}

	// « Machines… » : pc1 passe en type vm, puis plus aucune.
	poster(url.Values{"liste": {"types"}, "action": {"machines"}, "valeur": {"vm"}, "machine": {"pc1"}})
	_ = b.db.QueryRow(`SELECT env FROM machines WHERE nom = 'pc1'`).Scan(&v)
	if v != "vm" {
		t.Fatalf("pc1 : type %q", v)
	}
	if _, corps, _ = b.get(c, "/reglages/sites?liste=types&machines=vm"); !strings.Contains(corps, `value="pc1" checked`) {
		t.Error("pc1 non cochée dans « Machines… »")
	}
	poster(url.Values{"liste": {"types"}, "action": {"machines"}, "valeur": {"vm"}})
	_ = b.db.QueryRow(`SELECT env FROM machines WHERE nom = 'pc1'`).Scan(&v)
	if v != "" {
		t.Errorf("pc1 décochée : type %q", v)
	}
}
