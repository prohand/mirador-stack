// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

var reEmpreinte = regexp.MustCompile(`name="empreinte" value="([0-9a-f]+)"`)

// Éditeur de tableaux : création, rangées, ordre, panneau, version
// dépassée, tableau fourni modifié / rétabli / supprimé, export.
func TestEditeurTableaux(t *testing.T) {
	b := nouveauBanc(t)
	c, secret := b.premierAdmin("admin", mdpAdmin)
	csrf := b.csrf(c, "/tableaux")
	poster := func(chemin string, v url.Values) (int, string) {
		t.Helper()
		v.Set("csrf", csrf)
		code, _, h := b.post(c, chemin, v)
		return code, h.Get("Location")
	}
	empreinte := func(nom string) string {
		t.Helper()
		_, corps, _ := b.get(c, "/tableaux/"+nom+"/modifier")
		return trouver(t, reEmpreinte, corps)
	}

	if code, loc := poster("/tableaux", url.Values{"action": {"creer"}, "titre": {"Serveurs Web à moi"}}); code != http.StatusSeeOther || loc != "/tableaux/serveurs-web-a-moi/modifier" {
		t.Fatalf("création : %d %s", code, loc)
	}
	nom := "serveurs-web-a-moi"
	if b.c.origineTableau(nom) != OriginePerso || len(b.c.tableau(nom).Rangees) != 1 {
		t.Fatal("tableau créé absent")
	}
	ici := "/tableaux/" + nom + "/modifier"
	poster(ici, url.Values{"action": {"rangee-ajouter"}, "titre": {"Disques"}, "empreinte": {empreinte(nom)}})
	if len(b.c.tableau(nom).Rangees) != 2 {
		t.Fatal("rangée non ajoutée")
	}
	// Ordre : la 2e rangée d'abord, et le panneau passe dedans.
	poster(ici, url.Values{"action": {"ordre"}, "ordre": {"1:0.0|0:"}, "empreinte": {empreinte(nom)}})
	tab := b.c.tableau(nom)
	if tab.Rangees[0].Titre != "Disques" || len(tab.Rangees[0].Panneaux) != 1 || len(tab.Rangees[1].Panneaux) != 0 {
		t.Fatalf("ordre non appliqué : %+v", tab.Rangees)
	}
	// Ordre incomplet ou doublon : refusé.
	for _, o := range []string{"1:0.0", "1:0.0,0.0|0:", "0:|0:"} {
		avant := empreinteTableau(b.c.tableau(nom))
		poster(ici, url.Values{"action": {"ordre"}, "ordre": {o}, "empreinte": {avant}})
		if empreinteTableau(b.c.tableau(nom)) != avant {
			t.Errorf("ordre %q accepté", o)
		}
	}
	// Version dépassée : refusée.
	poster(ici, url.Values{"action": {"rangee-ajouter"}, "empreinte": {"0000000000000000"}})
	if len(b.c.tableau(nom).Rangees) != 2 {
		t.Fatal("modification sur une version dépassée acceptée")
	}
	// Nouveau panneau : expression fausse refusée, puis enregistré.
	pan := url.Values{"position": {"1.nouveau"}, "type": {"chiffre"}, "titre": {"Machines up"}, "largeur": {"4"},
		"expr0": {"sum(up{instance=~\"$instance\"}"}, "seuils": {"erreur\n1 ok"}, "correspondances": {"0 = Aucune erreur\n1..5 = Peu"},
		"action": {"enregistrer"}}
	pan.Set("empreinte", empreinte(nom))
	poster("/tableaux/"+nom+"/panneau", pan)
	if len(b.c.tableau(nom).Rangees[1].Panneaux) != 0 {
		t.Fatal("expression fausse acceptée")
	}
	pan.Set("expr0", "sum(up)")
	pan.Set("empreinte", empreinte(nom))
	poster("/tableaux/"+nom+"/panneau", pan)
	ps := b.c.tableau(nom).Rangees[1].Panneaux
	if len(ps) != 1 || ps[0].Largeur != 4 || len(ps[0].Seuils) != 2 || ps[0].Seuils[0].Valeur != nil || len(ps[0].Correspondances) != 2 ||
		ps[0].Correspondances[0].Couleur != "erreur" || ps[0].Correspondances[1].De == nil || ps[0].Requetes[0].ID != "A" {
		t.Fatalf("panneau : %+v", ps)
	}
	// Aperçu : rien d'enregistré, la page s'affiche.
	pan.Set("empreinte", empreinte(nom))
	pan.Set("position", "1.0")
	pan.Set("titre", "Autre titre")
	pan.Set("action", "apercu")
	pan.Set("csrf", csrf)
	if code, corps, _ := b.post(c, "/tableaux/"+nom+"/panneau", pan); code != http.StatusOK || !strings.Contains(corps, "Aperçu") ||
		b.c.tableau(nom).Rangees[1].Panneaux[0].Titre != "Machines up" {
		t.Fatalf("aperçu : %d", code)
	}
	// Page du tableau : largeur choisie → grille de 12 colonnes.
	if _, corps, _ := b.get(c, "/tableaux/"+nom); !strings.Contains(corps, `panneaux douze`) || !strings.Contains(corps, `panneau-chiffre l4`) {
		t.Error("largeur absente de la page")
	}
	// Export puis import : nouveau tableau, même contenu.
	_, js, _ := b.get(c, "/tableaux/"+nom+"/export.json")
	var exp Tableau
	if err := json.Unmarshal([]byte(js), &exp); err != nil || exp.Nom != nom {
		t.Fatalf("export : %v", err)
	}

	var corpsImport bytes.Buffer
	mw := multipart.NewWriter(&corpsImport)
	_ = mw.WriteField("csrf", csrf)
	fw, _ := mw.CreateFormFile("fichier", "export.json")
	_, _ = fw.Write([]byte(js))
	_ = mw.Close()
	rq, _ := http.NewRequest(http.MethodPost, b.srv.URL+"/tableaux", &corpsImport)
	rq.Header.Set("Content-Type", mw.FormDataContentType())
	if r, err := c.Do(rq); err != nil || r.StatusCode != http.StatusSeeOther || r.Header.Get("Location") != "/tableaux/serveurs-web-a-moi-2" {
		t.Fatalf("import : %v %v", err, r.Header.Get("Location"))
	}

	// Tableau fourni : proposé (pas installé), aperçu, installé, modifié,
	// rétabli, supprimé (retour au catalogue), réinstallé.
	f := "serveurs-linux"
	titre := tableauxDepart[f].Titre
	if b.c.tableau(f) != nil {
		t.Fatal("tableau fourni installé d'office")
	}
	if _, corps, _ := b.get(c, "/tableaux"); !strings.Contains(corps, "Tableaux proposés par mirador") || !strings.Contains(corps, titre) {
		t.Fatal("catalogue absent")
	}
	if code, corps, _ := b.get(c, "/tableaux/"+f); code != http.StatusOK || !strings.Contains(corps, "Installer ce tableau") {
		t.Fatalf("aperçu du catalogue : %d", code)
	}
	if code, _, _ := b.get(c, "/machines"); code != http.StatusOK {
		t.Error("page Machines cassée sans le tableau Linux installé")
	}
	poster("/tableaux", url.Values{"action": {"installer"}, "nom": {f}})
	if b.c.tableau(f) == nil || b.c.origineTableau(f) != OrigineFourni {
		t.Fatal("installation ratée")
	}
	poster("/tableaux/"+f+"/modifier", url.Values{"action": {"proprietes"}, "titre": {"Mes Linux"}, "periode": {"24h"},
		"variables": {"instance | Machine | label_values(node_uname_info, instance)"}, "empreinte": {empreinte(f)}})
	if b.c.tableau(f).Titre != "Mes Linux" || b.c.origineTableau(f) != OrigineModifie || tableauxDepart[f].Titre != titre {
		t.Fatal("tableau fourni non modifié (ou original touché)")
	}
	poster("/tableaux", url.Values{"action": {"retablir"}, "nom": {f}})
	if b.c.tableau(f).Titre != titre || b.c.origineTableau(f) != OrigineFourni {
		t.Fatal("rétablissement raté")
	}
	if _, _ = poster("/tableaux", url.Values{"action": {"supprimer"}, "nom": {f}, "code": {"000000"}}); b.c.tableau(f) == nil {
		t.Fatal("suppression sans bon code 2FA")
	}
	b.h.avancer(31 * time.Second)
	poster("/tableaux", url.Values{"action": {"supprimer"}, "nom": {f}, "code": {b.code(secret)}})
	if b.c.tableau(f) != nil {
		t.Fatal("tableau fourni non supprimé")
	}
	if _, corps, _ := b.get(c, "/tableaux"); !strings.Contains(corps, `name="nom" value="serveurs-linux"><button name="action" value="installer"`) {
		t.Error("tableau supprimé pas de nouveau proposé")
	}
	if code, _, _ := b.get(c, "/tableaux/"+f+"/supprimer"); code != http.StatusNotFound {
		t.Error("page de suppression d'un tableau non installé")
	}
	poster("/tableaux", url.Values{"action": {"installer"}, "nom": {f}})
	if b.c.tableau(f) == nil {
		t.Fatal("tableau fourni non réinstallé")
	}
	// Tableau créé ici : supprimé pour de bon.
	b.h.avancer(31 * time.Second)
	poster("/tableaux", url.Values{"action": {"supprimer"}, "nom": {nom}, "code": {b.code(secret)}})
	if b.c.tableau(nom) != nil {
		t.Fatal("tableau perso non supprimé")
	}
	// Audit.
	var n int
	_ = b.db.QueryRow(`SELECT COUNT(*) FROM audit WHERE action LIKE 'tableau_%'`).Scan(&n)
	if n < 8 {
		t.Errorf("audit : %d lignes", n)
	}
}

func TestAppliquerOrdre(t *testing.T) {
	tab := &Tableau{Rangees: []Rangee{{Titre: "a", Panneaux: []Panneau{{Titre: "1"}, {Titre: "2"}}}, {Titre: "b"}}}
	if err := appliquerOrdre(tab, "1:0.1|0:0.0"); err != nil || tab.Rangees[0].Titre != "b" || tab.Rangees[0].Panneaux[0].Titre != "2" {
		t.Fatalf("%v %+v", err, tab.Rangees)
	}
}

// Tests de notification : une ligne terminée s'efface ; une en attente reste.
func TestEffacerTestsCanaux(t *testing.T) {
	b := nouveauBanc(t)
	c, _ := b.premierAdmin("admin", mdpAdmin)
	csrf := b.csrf(c, "/reglages/notifications")
	_, _ = b.db.Exec(`INSERT INTO tests_canaux(canal, demande_le, fait_le, statut, erreur) VALUES ('olvid', 1, 2, 'echec', 'x'), ('telegram', 3, 4, 'ok', ''), ('ntfy', 5, 0, '', '')`)
	b.post(c, "/reglages/notifications", url.Values{"csrf": {csrf}, "action": {"test-effacer"}, "id": {"1"}})
	var n int
	_ = b.db.QueryRow(`SELECT COUNT(*) FROM tests_canaux`).Scan(&n)
	if n != 2 {
		t.Fatalf("après effacement d'une ligne : %d", n)
	}
	b.post(c, "/reglages/notifications", url.Values{"csrf": {csrf}, "action": {"tests-effacer"}})
	_ = b.db.QueryRow(`SELECT COUNT(*) FROM tests_canaux`).Scan(&n)
	if n != 1 {
		t.Fatalf("après « tout effacer » : %d (le test en attente doit rester)", n)
	}
}

// Sites et types : listes gérées dans les réglages, imposées aux formulaires.
func TestSitesEtTypes(t *testing.T) {
	b := nouveauBanc(t)
	c, secret := b.premierAdmin("admin", mdpAdmin)
	csrf := b.csrf(c, "/reglages/sites")
	ctx := context.Background()
	if l := b.c.lireListes(ctx); len(l.Sites) != 0 || !slices.Contains(l.Types, "vm") {
		t.Fatalf("listes par défaut : %+v", l)
	}
	b.post(c, "/reglages/sites", url.Values{"csrf": {csrf}, "liste": {"sites"}, "action": {"ajouter"}, "valeur": {"maison"}})
	b.post(c, "/reglages/sites", url.Values{"csrf": {csrf}, "liste": {"sites"}, "action": {"ajouter"}, "valeur": {"pas bon"}})
	if l := b.c.lireListes(ctx); !slices.Equal(l.Sites, []string{"maison"}) {
		t.Fatalf("sites : %+v", l.Sites)
	}
	if _, corps, _ := b.get(c, "/machines"); !strings.Contains(corps, `<option >maison</option>`) && !strings.Contains(corps, `<option>maison</option>`) {
		t.Error("site absent de la liste déroulante")
	}
	// Jeton : site hors liste refusé, site de la liste accepté.
	b.h.avancer(31 * time.Second)
	b.post(c, "/machines/jeton", url.Values{"csrf": {csrf}, "duree": {"1h"}, "site": {"ailleurs"}, "code": {b.code(secret)}})
	var n int
	_ = b.db.QueryRow(`SELECT COUNT(*) FROM jetons_enrolement`).Scan(&n)
	if n != 0 {
		t.Fatal("jeton créé avec un site inconnu")
	}
	b.h.avancer(31 * time.Second)
	b.post(c, "/machines/jeton", url.Values{"csrf": {csrf}, "duree": {"1h"}, "site": {"maison"}, "env": {"vm"}, "code": {b.code(secret)}})
	var site, env string
	_ = b.db.QueryRow(`SELECT site, env FROM jetons_enrolement`).Scan(&site, &env)
	if site != "maison" || env != "vm" {
		t.Fatalf("jeton : %q %q", site, env)
	}
	// Valeur utilisée par une machine : suppression refusée.
	_, _ = b.db.Exec(`INSERT INTO machines(nom, statut, cree_le, site) VALUES ('pc', 'approuvee', 1, 'maison')`)
	b.post(c, "/reglages/sites", url.Values{"csrf": {csrf}, "liste": {"sites"}, "action": {"supprimer"}, "valeur": {"maison"}})
	if l := b.c.lireListes(ctx); !slices.Contains(l.Sites, "maison") {
		t.Fatal("site utilisé supprimé")
	}
	// Groupes : même liste ; nom choisi au jeton (11/11).
	b.post(c, "/reglages/sites", url.Values{"csrf": {csrf}, "liste": {"groupes"}, "action": {"ajouter"}, "valeur": {"dns"}})
	if l := b.c.lireListes(ctx); !slices.Contains(l.Groupes, "dns") {
		t.Fatalf("groupes : %+v", l.Groupes)
	}
	b.h.avancer(31 * time.Second)
	b.post(c, "/machines/jeton", url.Values{"csrf": {csrf}, "duree": {"1h"}, "groupe": {"inconnu"}, "nom": {"dns02"}, "code": {b.code(secret)}})
	_ = b.db.QueryRow(`SELECT COUNT(*) FROM jetons_enrolement`).Scan(&n)
	if n != 1 {
		t.Fatal("jeton créé avec un groupe inconnu")
	}
	b.h.avancer(31 * time.Second)
	b.post(c, "/machines/jeton", url.Values{"csrf": {csrf}, "duree": {"1h"}, "groupe": {"dns"}, "nom": {"DNS02"}, "code": {b.code(secret)}})
	var groupe, nom string
	_ = b.db.QueryRow(`SELECT groupe, nom FROM jetons_enrolement WHERE nom != ''`).Scan(&groupe, &nom)
	if groupe != "dns" || nom != "dns02" {
		t.Fatalf("jeton : groupe %q, nom %q", groupe, nom)
	}
	if _, corps, _ := b.get(c, "/reglages/agents"); !strings.Contains(corps, `portee=groupe:dns`) {
		t.Error("groupe absent de la configuration des agents")
	}
	// Approbation : nom corrigé et groupe choisi.
	_, _ = b.db.Exec(`INSERT INTO machines(nom, statut, cree_le) VALUES ('debian', 'en_attente', 1)`)
	b.post(c, "/machines/debian", url.Values{"csrf": {csrf}, "action": {"approuver"}, "nouveau_nom": {"srv-test"},
		"site": {""}, "env": {""}, "groupe": {"dns"}})
	var statut string
	_ = b.db.QueryRow(`SELECT statut, groupe FROM machines WHERE nom = 'srv-test'`).Scan(&statut, &groupe)
	if statut != "approuvee" || groupe != "dns" {
		t.Fatalf("approbation : statut %q, groupe %q", statut, groupe)
	}
	// Site proposé par la machine (hors liste), gardé à l'approbation :
	// ajouté à la liste.
	_, _ = b.db.Exec(`INSERT INTO machines(nom, statut, cree_le, site, env) VALUES ('pc9', 'en_attente', 1, 'bureau', 'vm')`)
	b.post(c, "/machines/pc9", url.Values{"csrf": {csrf}, "action": {"approuver"}, "nouveau_nom": {"pc9"},
		"site": {"bureau"}, "env": {"vm"}, "groupe": {""}})
	if l := b.c.lireListes(ctx); !slices.Contains(l.Sites, "bureau") {
		t.Errorf("site proposé non ajouté à la liste : %v", l.Sites)
	}
}

// Chaque icône proposée est dessinée (sinon pastille vide).
func TestIconesDessinees(t *testing.T) {
	b, _ := fichiers.ReadFile("gabarits/morceaux.html")
	for _, i := range append(slices.Clone(iconesPanneaux), "mur", "logs") {
		if !strings.Contains(string(b), `<symbol id="i-`+i+`"`) {
			t.Errorf("icône %q non dessinée", i)
		}
	}
	for _, tab := range tableauxDepart {
		if tab.Icone == "" {
			t.Errorf("tableau fourni %s sans icône", tab.Nom)
		}
	}
}

// Logs et Flux bloqués : proposés, pas installés d'office.
func TestRaccourcisPages(t *testing.T) {
	b := nouveauBanc(t)
	c, _ := b.premierAdmin("admin", mdpAdmin)
	csrf := b.csrf(c, "/tableaux")
	if _, corps, _ := b.get(c, "/tableaux"); strings.Contains(corps, `class="carte lien-carte" href="/logs"`) {
		t.Fatal("raccourci Logs installé d'office")
	}
	b.post(c, "/tableaux", url.Values{"csrf": {csrf}, "action": {"installer-page"}, "nom": {"logs"}})
	if _, corps, _ := b.get(c, "/tableaux"); !strings.Contains(corps, `class="carte lien-carte" href="/logs"`) {
		t.Fatal("raccourci Logs non installé")
	}
	b.post(c, "/tableaux", url.Values{"csrf": {csrf}, "action": {"retirer-page"}, "nom": {"logs"}})
	if _, corps, _ := b.get(c, "/tableaux"); strings.Contains(corps, `class="carte lien-carte" href="/logs"`) {
		t.Fatal("raccourci Logs non retiré")
	}
}
