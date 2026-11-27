// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"

	"mirador-stack/internal/server/alertes"
	"mirador-stack/internal/server/flotte"
	"mirador-stack/internal/server/modeles"
)

// Partie C : créer, dupliquer, modifier, exporter, importer et supprimer un
// modèle personnel depuis l'interface.
func TestModelesPersoInterface(t *testing.T) {
	b := nouveauBanc(t)
	c, secret := b.premierAdmin("admin", mdpAdmin)
	ctx := context.Background()
	csrf := b.csrf(c, "/modeles")
	if _, corps, _ := b.get(c, "/modeles"); !strings.Contains(corps, `href="/modeles/nouveau"`) || !strings.Contains(corps, `href="/modeles/nouveau?depuis=linux"`) {
		t.Fatal("boutons Nouveau / Dupliquer absents")
	}
	// Dupliquer « Serveur web » : formulaire prérempli, règles renommées.
	if _, corps, _ := b.get(c, "/modeles/nouveau?depuis=serveur-web"); !strings.Contains(corps, `value="serveur-web-perso"`) ||
		!strings.Contains(corps, `value="WebInjoignablePerso"`) || !strings.Contains(corps, `name="l_nom" aria-label="Nom de la liste" value="adresses"`) {
		t.Fatal("copie de Serveur web non préremplie")
	}
	formulaire := url.Values{"csrf": {csrf}, "action": {"enregistrer"}, "nouveau": {"oui"}, "code": {""},
		"nom": {"mon-appli"}, "titre": {"Mon appli"}, "services": {"mon-appli.service"},
		"l_nom": {"ports", ""}, "l_titre": {"Ports", ""}, "l_type": {"tcp", "http"}, "l_exemple": {"127.0.0.1:8080 | 127.0.0.1:8081", ""}, "l_aide": {"", ""},
		"p_nom": {"min", ""}, "p_titre": {"Minimum", ""}, "p_unite": {"", ""}, "p_defaut": {"1", ""}, "p_min": {"0", ""}, "p_max": {"10", ""},
		"r_nom": {"MonAppliPortFerme", ""}, "r_gravite": {"critical", "critical"}, "r_pour": {"2m", ""}, "r_remplace": {"", ""},
		"r_expr": {`mirador_check_ok{modele="mon-appli"} == 0`, ""}, "r_resume": {"{{ $labels.cible }} fermé", ""}, "r_description": {"", ""},
		"config": {`{"systemd": true}`}}
	// Expression invalide : refusée, saisie gardée sur la page.
	b.h.avancer(31 * time.Second)
	formulaire.Set("code", b.code(secret))
	mauvais := url.Values{}
	for k, v := range formulaire {
		mauvais[k] = append([]string(nil), v...)
	}
	mauvais["r_expr"][0] = "rate(("
	if code, corps, _ := b.post(c, "/modeles/perso", mauvais); code != 200 || !strings.Contains(corps, `value="Mon appli"`) || !strings.Contains(corps, "rate((") {
		t.Fatalf("erreur : saisie perdue (%d)", code)
	}
	if modeles.Trouver("mon-appli") != nil {
		t.Fatal("modèle invalide enregistré")
	}
	// Mode administration ouvert : plus besoin du code.
	formulaire.Set("code", "")
	b.post(c, "/modeles/perso", formulaire)
	m := modeles.Trouver("mon-appli")
	if m == nil || !m.Perso || len(m.Listes) != 1 || m.Listes[0].Exemple != "127.0.0.1:8080\n127.0.0.1:8081" || len(m.Parametres) != 1 || len(m.Regles) != 1 {
		t.Fatalf("modèle enregistré : %+v", m)
	}
	rs, _ := alertes.ListerRegles(ctx, b.c.db)
	trouvee := false
	for _, r := range rs {
		trouvee = trouvee || (r.Nom == "MonAppliPortFerme" && r.Origine == "modele:mon-appli")
	}
	if !trouvee {
		t.Error("règle du modèle non posée")
	}
	// Nom déjà pris : refusé.
	if _, corps, _ := b.post(c, "/modeles/perso", formulaire); !strings.Contains(corps, "déjà pris") {
		t.Error("nom en double accepté")
	}
	// Attribué à une machine avec une liste : checks envoyés.
	if _, err := b.c.db.Exec(`INSERT INTO machines(nom, statut, type, cree_le) VALUES ('app1', 'approuvee', 'agent', 1)`); err != nil {
		t.Fatal(err)
	}
	b.post(c, "/machines/app1", url.Values{"csrf": {csrf}, "action": {"modeles"}, "m": {"mon-appli"}})
	b.post(c, "/machines/app1/modeles/mon-appli", url.Values{"csrf": {csrf}, "l.ports": {"127.0.0.1:8080"}, "p.min": {"2"}})
	id, _ := flotte.IDMachine(ctx, b.c.db, "app1")
	doc, _, _ := flotte.ConfigMachine(ctx, b.c.db, id)
	if !strings.Contains(string(doc), `"modele":"mon-appli"`) || !strings.Contains(string(doc), `"mon-appli.service"`) {
		t.Errorf("configuration : %s", doc)
	}
	// Page Modèles : badge et bouton Modifier ; page du modèle.
	if _, corps, _ := b.get(c, "/modeles"); !strings.Contains(corps, `href="/modeles/perso/mon-appli"`) || !strings.Contains(corps, "personnel") {
		t.Error("modèle personnel absent de la page Modèles")
	}
	if _, corps, _ := b.get(c, "/modeles/perso/mon-appli"); !strings.Contains(corps, "Attribué à") || !strings.Contains(corps, `value="MonAppliPortFerme"`) {
		t.Error("page du modèle")
	}
	// Suppression refusée tant qu'il est attribué.
	b.post(c, "/modeles/perso", url.Values{"csrf": {csrf}, "action": {"supprimer"}, "nom": {"mon-appli"}})
	if modeles.Trouver("mon-appli") == nil {
		t.Fatal("modèle attribué supprimé")
	}
	// Export puis import sous un autre nom.
	code, corps, ent := b.get(c, "/modeles/export/mon-appli")
	if code != 200 || !strings.Contains(ent.Get("Content-Disposition"), "mirador-modele-mon-appli.json") {
		t.Fatalf("export : %d", code)
	}
	var exp map[string]any
	if err := json.Unmarshal([]byte(corps), &exp); err != nil {
		t.Fatal(err)
	}
	importe := strings.NewReplacer(`"mon-appli"`, `"copie"`, `modele=\"mon-appli\"`, `modele=\"copie\"`, "MonAppliPortFerme", "CopiePortFerme").Replace(corps)
	b.post(c, "/modeles/perso", url.Values{"csrf": {csrf}, "action": {"importer"}, "json": {importe}})
	if m := modeles.Trouver("copie"); m == nil || !m.Perso {
		t.Fatal("import")
	}
	b.post(c, "/modeles/perso", url.Values{"csrf": {csrf}, "action": {"importer"}, "json": {strings.ReplaceAll(importe, `"copie"`, `"linux"`)}})
	if modeles.Trouver("linux").Perso {
		t.Error("modèle livré remplacé par un import")
	}
	// Retiré de la machine, puis supprimé.
	b.post(c, "/machines/app1", url.Values{"csrf": {csrf}, "action": {"modeles"}})
	b.post(c, "/modeles/perso", url.Values{"csrf": {csrf}, "action": {"supprimer"}, "nom": {"mon-appli"}})
	if modeles.Trouver("mon-appli") != nil {
		t.Error("modèle non supprimé")
	}
	// Page d'un modèle personnel : bouton Enregistrer pour un admin.
	if _, corps, _ := b.get(c, "/modeles/perso/copie"); !strings.Contains(corps, "Enregistrer le modèle") {
		t.Error("admin : bouton Enregistrer absent")
	}
}
