// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"net/http"
	"strings"
	"testing"

	"mirador-stack/internal/server/auth"
)

// Menu réorganisable (SPEC § 79) : ordre par groupe et par mode, entrées
// masquées, entrées fixes jamais masquées, barre du bas.
func TestConstruireMenu(t *testing.T) {
	p := &Page{Admin: true, Menu: "logs"}
	construireMenu(p, auth.Utilisateur{Menu: `{"ordi":{"ordre":["logs","alertes"],"caches":["elements","reglages"]},"tel":{"bas":["alertes","sondes"]}}`})
	classe := map[string]string{}
	for _, e := range p.MenuElements {
		classe[e.Cle] = e.Classe
	}
	if !strings.Contains(classe["logs"], "o-1 ") || !strings.Contains(classe["alertes"], "o-2 ") || !strings.Contains(classe["accueil"], "o-3 ") {
		t.Fatalf("ordre ordinateur : %v", classe)
	}
	if !strings.Contains(classe["accueil"], "t-1") { // téléphone : ordre par défaut
		t.Fatalf("ordre téléphone : %q", classe["accueil"])
	}
	if !strings.Contains(classe["elements"], "cache-o") || strings.Contains(classe["reglages"], "cache-o") {
		t.Fatalf("entrées masquées : éléments %q, réglages %q", classe["elements"], classe["reglages"])
	}
	if !strings.Contains(classe["regles"], "o-31") { // second groupe : à partir de 31
		t.Fatalf("second groupe : %q", classe["regles"])
	}
	if len(p.MenuBas) != 2 || p.MenuBas[0].Href != "/alertes" || p.MenuBas[1].Href != "/sondes" {
		t.Fatalf("barre du bas : %+v", p.MenuBas)
	}
	// Compte lecture : pas d'entrées réservées aux admins.
	q := &Page{}
	construireMenu(q, auth.Utilisateur{})
	for _, e := range q.MenuElements {
		if e.Cle == "securite" || e.Cle == "utilisateurs" {
			t.Fatal("entrée admin dans le menu d'un compte lecture")
		}
	}
	if len(q.MenuBas) != 4 {
		t.Fatal("barre du bas par défaut")
	}
}

func TestVerifierMenu(t *testing.T) {
	for nom, d := range map[string]demandeMenu{
		"clé inconnue":      {Mode: "ordi", Ordre: []string{"inconnue"}},
		"doublon":           {Mode: "ordi", Ordre: []string{"logs", "logs"}},
		"réglages masqués":  {Mode: "ordi", Caches: []string{"reglages"}},
		"sécurité masquée":  {Mode: "tel", Caches: []string{"securite"}},
		"5 raccourcis":      {Mode: "tel", Bas: []string{"accueil", "alertes", "machines", "logs", "sondes"}},
		"raccourci inconnu": {Mode: "tel", Bas: []string{"nulle-part"}},
	} {
		if _, err := verifierMenu(d); err == nil {
			t.Errorf("%s : accepté", nom)
		}
	}
	if m, err := verifierMenu(demandeMenu{Mode: "tel", Ordre: []string{"alertes"}, Caches: []string{"audit"}, Bas: []string{"alertes"}}); err != nil || len(m.Bas) != 1 {
		t.Fatalf("menu valide refusé : %v", err)
	}
}

// Enregistrement (« Terminé ») puis menu de la page, et retour par défaut.
func TestPostMenu(t *testing.T) {
	b := nouveauBanc(t)
	c, _ := b.premierAdmin("admin", mdpAdmin)
	csrf := b.csrf(c, "/")
	envoyer := func(corps string) int {
		r, _ := http.NewRequest(http.MethodPost, b.srv.URL+"/compte/menu", strings.NewReader(corps))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-CSRF", csrf)
		resp, err := c.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := envoyer(`{"mode":"tel","ordre":["logs"],"caches":["audit"],"bas":["logs","sondes"]}`); code != http.StatusNoContent {
		t.Fatalf("enregistrement : %d", code)
	}
	_, corps, _ := b.get(c, "/")
	if !strings.Contains(corps, `data-menu-bas="logs sondes"`) || !strings.Contains(corps, `cache-t`) {
		t.Fatal("menu enregistré absent de la page")
	}
	if code := envoyer(`{"mode":"ordi","caches":["securite"]}`); code != http.StatusBadRequest {
		t.Fatalf("Sécurité masquée acceptée : %d", code)
	}
	if code := envoyer(`{"mode":"tel","defaut":true}`); code != http.StatusNoContent {
		t.Fatalf("retour par défaut : %d", code)
	}
	var menu string
	_ = b.db.QueryRow(`SELECT menu FROM utilisateurs WHERE nom = 'admin'`).Scan(&menu)
	if menu != "" {
		t.Fatalf("menu par défaut : %q", menu)
	}
	// Sans jeton CSRF : refusé.
	r, _ := http.NewRequest(http.MethodPost, b.srv.URL+"/compte/menu", strings.NewReader(`{"mode":"ordi"}`))
	if resp, err := c.Do(r); err != nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("sans CSRF : %v", resp.StatusCode)
	}
}

// Vue d'ensemble réorganisable (SPEC § 83) : enregistrement par mode,
// classes de la page et du fragment, menu gardé, retour par défaut.
func TestPostAccueil(t *testing.T) {
	b := nouveauBanc(t)
	c, _ := b.premierAdmin("admin", mdpAdmin)
	csrf := b.csrf(c, "/")
	envoyer := func(chemin, corps string) int {
		r, _ := http.NewRequest(http.MethodPost, b.srv.URL+chemin, strings.NewReader(corps))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-CSRF", csrf)
		resp, err := c.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := envoyer("/compte/menu", `{"mode":"ordi","caches":["audit"]}`); code != http.StatusNoContent {
		t.Fatalf("menu : %d", code)
	}
	if code := envoyer("/compte/accueil", `{"mode":"ordi","ordre":["stockage","machines","parc","tuiles"],"caches":["chaleur","alertes"]}`); code != http.StatusNoContent {
		t.Fatalf("enregistrement : %d", code)
	}
	for _, chemin := range []string{"/", "/fragments/accueil"} {
		_, corps, _ := b.get(c, chemin)
		for _, attendu := range []string{`class="kpi o-0 t-3" data-bloc="stockage"`, `class="kpi o-1 t-0" data-bloc="machines"`,
			`class="kpi o-2 t-1 cache-o" data-bloc="alertes"`, `class="carte o-0 t-6" data-parc data-bloc="parc"`, `class="kpis o-1 t-0" data-bloc="tuiles"`,
			`data-bloc="chaleur"`} {
			if !strings.Contains(corps, attendu) {
				t.Errorf("%s : %q absent", chemin, attendu)
			}
		}
	}
	if _, corps, _ := b.get(c, "/"); !strings.Contains(corps, "cache-o") || !strings.Contains(corps, `data-modifier-accueil`) {
		t.Error("menu masqué perdu ou bouton absent")
	}
	for _, mauvais := range []string{`{"mode":"ordi","ordre":["inconnu"]}`, `{"mode":"ordi","ordre":["parc","parc"]}`, `{"mode":"montre"}`,
		`{"mode":"tel","caches":["machines","alertes","notifications","stockage","tuiles","sites","alertes-cours","prises","masquees","attente","parc","chaleur","bas"]}`} {
		if code := envoyer("/compte/accueil", mauvais); code != http.StatusBadRequest {
			t.Errorf("%s accepté : %d", mauvais, code)
		}
	}
	envoyer("/compte/accueil", `{"mode":"ordi","defaut":true}`)
	envoyer("/compte/menu", `{"mode":"ordi","defaut":true}`)
	var menu string
	_ = b.db.QueryRow(`SELECT menu FROM utilisateurs WHERE nom = 'admin'`).Scan(&menu)
	if menu != "" {
		t.Fatalf("par défaut : %q", menu)
	}
}

// Barre du bas du téléphone : libellés courts (« Accueil », « Tâches »),
// libellé complet en infobulle (retour du 20/11).
func TestLibellesCourtsBarreDuBas(t *testing.T) {
	b := nouveauBanc(t)
	c, _ := b.premierAdmin("admin", mdpAdmin)
	_, corps, _ := b.get(c, "/")
	i := strings.Index(corps, `class="nav-bas"`)
	if i < 0 {
		t.Fatal("barre du bas absente")
	}
	bas := corps[i:]
	bas = bas[:strings.Index(bas, "</nav>")]
	if !strings.Contains(bas, `title="Vue d&#39;ensemble"`) || !strings.Contains(bas, "<span>Accueil</span>") || strings.Contains(bas, "<span>Vue d&#39;ensemble</span>") {
		t.Errorf("barre du bas :\n%s", bas)
	}
	if !strings.Contains(corps, `<span class="txt">Vue d&#39;ensemble</span>`) {
		t.Error("le menu complet doit garder « Vue d'ensemble »")
	}
}
