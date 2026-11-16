// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/html"

	"mirador-stack/internal/server/alertes"
	"mirador-stack/internal/server/auth"
	"mirador-stack/internal/server/notif"
)

// champsFormulaire : valeurs que le navigateur enverrait pour le premier
// formulaire marqué « attribut » (champs texte, cases cochées, listes).
func champsFormulaire(t *testing.T, corps, attribut string) url.Values {
	t.Helper()
	doc, err := html.Parse(strings.NewReader(corps))
	if err != nil {
		t.Fatal(err)
	}
	attr := func(n *html.Node, k string) (string, bool) {
		for _, a := range n.Attr {
			if a.Key == k {
				return a.Val, true
			}
		}
		return "", false
	}
	var form *html.Node
	var chercher func(*html.Node)
	chercher = func(n *html.Node) {
		if form != nil {
			return
		}
		if n.Type == html.ElementNode && n.Data == "form" {
			if _, ok := attr(n, attribut); ok {
				form = n
				return
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			chercher(c)
		}
	}
	chercher(doc)
	if form == nil {
		t.Fatalf("formulaire %s absent", attribut)
	}
	v := url.Values{}
	texte := func(n *html.Node) string {
		var b strings.Builder
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if c.Type == html.TextNode {
				b.WriteString(c.Data)
			}
		}
		return b.String()
	}
	var parcourir func(*html.Node)
	parcourir = func(n *html.Node) {
		if n.Type == html.ElementNode {
			nom, _ := attr(n, "name")
			switch n.Data {
			case "input":
				typ, _ := attr(n, "type")
				val, _ := attr(n, "value")
				_, coche := attr(n, "checked")
				if nom != "" && ((typ != "checkbox" && typ != "radio") || coche) {
					v.Add(nom, val)
				}
			case "textarea":
				if nom != "" {
					v.Add(nom, texte(n))
				}
			case "select":
				choisi, premier, vu, trouve := "", "", false, false
				for o := n.FirstChild; o != nil; o = o.NextSibling {
					if o.Type != html.ElementNode || o.Data != "option" {
						continue
					}
					val, ok := attr(o, "value")
					if !ok {
						val = texte(o)
					}
					if !vu {
						premier, vu = val, true
					}
					if _, s := attr(o, "selected"); s {
						choisi, trouve = val, true
					}
				}
				if !trouve {
					choisi = premier
				}
				v.Add(nom, choisi)
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			parcourir(c)
		}
	}
	parcourir(form)
	return v
}

// Bloc « Général » rangé ailleurs (SPEC § 38) : mode en haut, adresse de
// l'interface et fuseau dans Interface, dépendances dans Silences ; le
// formulaire des notifications ne les efface plus.
func TestReglagesGeneralRanges(t *testing.T) {
	b := nouveauBanc(t)
	c, _ := b.premierAdmin("admin", mdpAdmin)
	ctx := context.Background()
	r, _ := notif.LireReglages(ctx, b.db)
	r.Lien, r.Fuseau, r.ReglesPanne = "https://console.exemple.fr", "Europe/Paris", []string{"AgentMuet"}
	if err := notif.EcrireReglages(ctx, b.db, r, cleTest, "test", b.h.maintenant()); err != nil {
		t.Fatal(err)
	}
	csrf := b.csrf(c, "/reglages/notifications")

	// Le formulaire de la page, renvoyé tel quel (seul « regrouper » change).
	_, corps, _ := b.get(c, "/reglages/notifications")
	for _, absent := range []string{`name="lien"`, `name="fuseau"`, `name="regles_panne"`, "<h2>Général</h2>"} {
		if strings.Contains(corps, absent) {
			t.Errorf("encore dans Notifications : %s", absent)
		}
	}
	for _, attendu := range []string{`id="canal-telegram"`, "Envoi des notifications", `data-barre-enregistrer`, "Secours : aucun", `name="grouper_par"`} {
		if !strings.Contains(corps, attendu) {
			t.Errorf("absent de Notifications : %s", attendu)
		}
	}
	v := champsFormulaire(t, corps, "data-barre-enregistrer")
	v.Set("csrf", csrf)
	v.Set("grouper_par", "alertname, site")
	_, _, ent := b.post(c, "/reglages/notifications", v)
	if ent.Get("Location") != "/reglages/notifications" {
		t.Fatalf("retour : %q", ent.Get("Location"))
	}
	r, _ = notif.LireReglages(ctx, b.db)
	if strings.Join(r.GrouperPar, ",") != "alertname,site" {
		_, page, _ := b.get(c, "/reglages/notifications")
		i := strings.Index(page, `class="message erreur"`)
		t.Fatalf("formulaire non enregistré : %v (%s)", r.GrouperPar, page[max(i, 0):max(i, 0)+300])
	}
	if r.Lien != "https://console.exemple.fr" || strings.Join(r.ReglesPanne, ",") != "AgentMuet" || r.Mode != "ombre" {
		t.Errorf("réglages hors formulaire effacés : %+v / %v / %s", r.Lien, r.ReglesPanne, r.Mode)
	}

	// Mode : bouton en haut de la page.
	// Sans canal configuré, « Activer l'envoi » est refusé avec un message.
	b.post(c, "/reglages/notifications", url.Values{"csrf": {csrf}, "action": {"mode"}, "mode": {"actif"}})
	if r, _ = notif.LireReglages(ctx, b.db); r.Mode != "ombre" {
		t.Errorf("mode actif accepté sans canal : %s", r.Mode)
	}
	if _, page, _ := b.get(c, "/reglages/notifications"); !strings.Contains(page, `class="message erreur"`) || !strings.Contains(page, "Activer l'envoi") {
		t.Error("refus du mode actif : message ou bouton absent")
	}
	b.post(c, "/reglages/notifications", url.Values{"csrf": {csrf}, "action": {"mode"}, "mode": {"ombre"}})
	if r, _ = notif.LireReglages(ctx, b.db); r.Mode != "ombre" {
		t.Errorf("mode : %s", r.Mode)
	}
	// Adresse de l'interface : depuis Réglages › Interface, retour là-bas.
	_, corps, _ = b.get(c, "/reglages/interface")
	if !strings.Contains(corps, `value="https://console.exemple.fr"`) || !strings.Contains(corps, `id="adresse-interface"`) {
		t.Error("adresse de l'interface absente de Réglages › Interface")
	}
	_, _, ent = b.post(c, "/reglages/notifications", url.Values{"csrf": {csrf}, "action": {"lien"}, "lien": {"https://mirador.exemple.fr"}})
	if r, _ = notif.LireReglages(ctx, b.db); r.Lien != "https://mirador.exemple.fr" || !strings.HasPrefix(ent.Get("Location"), "/reglages/interface") {
		t.Errorf("lien : %q, retour %q", r.Lien, ent.Get("Location"))
	}
	// Dépendances : depuis Silences et maintenances.
	if _, corps, _ = b.get(c, "/sourdine"); !strings.Contains(corps, `name="regles_panne" value="AgentMuet"`) {
		t.Error("règles « machine en panne » absentes de Silences et maintenances")
	}
	b.post(c, "/reglages/notifications", url.Values{"csrf": {csrf}, "action": {"regles_panne"}, "regles_panne": {"AgentMuet, ProxmoxMuet"}})
	if r, _ = notif.LireReglages(ctx, b.db); strings.Join(r.ReglesPanne, ",") != "AgentMuet,ProxmoxMuet" {
		t.Errorf("règles panne : %v", r.ReglesPanne)
	}
	// Un seul fuseau : celui de l'interface sert aussi aux messages.
	b.post(c, "/reglages/interface", url.Values{"csrf": {csrf}, "fuseau": {"America/Montreal"}, "theme": {"auto"}, "menu": {"gauche"}})
	if r, _ = notif.LireReglages(ctx, b.db); r.Fuseau != "America/Montreal" {
		t.Errorf("fuseau des messages non aligné : %s", r.Fuseau)
	}
}

// Accueil des Réglages, Utilisateurs, Sécurité (SPEC § 38).
func TestReglagesUtilisateursSecurite(t *testing.T) {
	b := nouveauBanc(t)
	c, secret := b.premierAdmin("admin", mdpAdmin)
	ctx := context.Background()
	if err := auth.CreerUtilisateur(ctx, b.db, "lea", auth.RoleLecture, "mot de passe de lea", "test", "", b.h.maintenant()); err != nil {
		t.Fatal(err)
	}
	_, corps, _ := b.get(c, "/reglages")
	for _, attendu := range []string{"pas de secours", "adresse d&#39;ingestion vide", `class="mini-jauge"`, "dernier export : jamais",
		"2 comptes", "2 points à regarder", `href="#i-envoi"`} {
		if !strings.Contains(corps, attendu) {
			t.Errorf("accueil des réglages : %q absent", attendu)
		}
	}
	_, corps, _ = b.get(c, "/utilisateurs")
	for _, attendu := range []string{`<strong>admin</strong> <span class="pastille">vous</span>`, "2FA à créer", "1 sans 2FA", `href="#nouveau"`,
		`value="desactiver"`, `value="supprimer"`} {
		if !strings.Contains(corps, attendu) {
			t.Errorf("utilisateurs : %q absent", attendu)
		}
	}
	_, corps, _ = b.get(c, "/securite")
	for _, attendu := range []string{"1 compte n&#39;a pas encore créé sa 2FA (lea)", "Interface ouverte à toutes les adresses", `name="inactivite" value="30"`,
		`name="duree_max" value="12"`, "cette session"} {
		if !strings.Contains(corps, attendu) {
			t.Errorf("sécurité : %q absent", attendu)
		}
	}
	// Durées saisies dans l'unité du champ, ou écrites (« 1h30m »).
	csrf := b.csrf(c, "/securite")
	b.h.avancer(31 * time.Second)
	b.post(c, "/securite", url.Values{"csrf": {csrf}, "inactivite": {"45"}, "duree_max": {"1h30m"}, "verrou": {"20"}, "mode_admin": {"10"},
		"echecs": {"5"}, "passkey_seule": {"1"}, "code": {b.code(secret)}})
	s, _ := LireSecurite(ctx, b.db)
	if time.Duration(s.Inactivite) != 45*time.Minute || time.Duration(s.DureeMax) != 90*time.Minute || time.Duration(s.Verrou) != 20*time.Minute {
		t.Errorf("durées : %+v", s)
	}
	if _, corps, _ = b.get(c, "/securite"); !strings.Contains(corps, `name="duree_max" value="1h30m"`) {
		t.Error("durée qui ne tombe pas juste : affichée telle quelle")
	}
	// « Fermer toutes les autres » : la session courante reste.
	autre := b.connecter("admin", mdpAdmin, secret)
	b.post(c, "/securite/fermer-session", url.Values{"csrf": {csrf}, "session": {"autres"}})
	if code, _, _ := b.get(c, "/securite"); code != 200 {
		t.Fatalf("session courante fermée : %d", code)
	}
	if code, _, _ := b.get(autre, "/compte"); code == 200 {
		t.Error("autre session encore ouverte")
	}
}

func TestEnUnite(t *testing.T) {
	cas := map[string]string{
		enUnite(alertes.Duree(30*time.Minute), "min"): "30",
		enUnite(alertes.Duree(12*time.Hour), "h"):     "12",
		enUnite(alertes.Duree(90*time.Minute), "h"):   alertes.Duree(90 * time.Minute).String(),
		enUnite(0, "min"): "0",
	}
	for got, attendu := range cas {
		if got != attendu {
			t.Errorf("%q, attendu %q", got, attendu)
		}
	}
}
