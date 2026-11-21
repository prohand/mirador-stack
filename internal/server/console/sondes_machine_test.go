// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"mirador-stack/internal/server/flotte"
	essais "mirador-stack/internal/sondes"
)

// Bloc « Sondes » de la page machine (SPEC § 60) : collecteur du site
// proposé d'office, ping, propositions d'après les ports ouverts, refus
// d'une adresse privée depuis le serveur, retrait.
func TestSondesMachine(t *testing.T) {
	b := nouveauBanc(t)
	ctx := t.Context()
	if _, err := b.c.db.Exec(`INSERT INTO machines(nom, statut, type, site, cree_le, vu_le) VALUES
		('coll-maison', 'approuvee', 'collecteur', 'maison', 1, 1), ('coll-bureau', 'approuvee', 'collecteur', 'bureau', 1, 1),
		('nas', 'approuvee', 'agent', 'maison', 1, ?), ('vieux', 'approuvee', 'agent', '', 1, ?)`, b.h.maintenant().Unix(), b.h.maintenant().Unix()); err != nil {
		t.Fatal(err)
	}
	p := func(port, adresse, proto string) serieTest {
		return serieTest{[]string{"__name__", "mirador_port_ecoute", "instance", "nas", "port", port, "adresse", adresse, "protocole", proto}, constante(1)}
	}
	b.metriquesSimulees(t, []serieTest{
		{[]string{"__name__", "mirador_adresse_info", "instance", "nas", "interface", "eth0", "adresse", "fd00::5"}, constante(1)},
		{[]string{"__name__", "mirador_adresse_info", "instance", "nas", "interface", "eth0", "adresse", "192.168.1.5", "principale", "1"}, constante(1)},
		{[]string{"__name__", "mirador_adresse_info", "instance", "nas", "interface", "eth1", "adresse", "10.10.0.5", "principale", "0"}, constante(1)},
		p("22", "*", "ssh"), p("5001", "*", "https"), p("80", "*", "http"), p("111", "*", "tcp"),
		p("53", "192.168.1.5", "tcp"), p("40123", "*", "tcp"),
	})
	cl, secret := b.premierAdmin("admin", mdpAdmin)
	_, corps, _ := b.get(cl, "/machines/nas")
	for _, attendu := range []string{`id="sondes"`, `<option value="coll-maison" selected>site « maison » · collecteur coll-maison</option>`,
		`<input type="checkbox" name="adresse" value="192.168.1.5" checked> <code>192.168.1.5</code> <span class="doux">eth0 · route par défaut</span>`,
		`<input type="checkbox" name="adresse" value="10.10.0.5"> <code>10.10.0.5</code>`, "HTTPS · port 5001 (DSM)", "https://192.168.1.5:5001/", "nas DSM 5001", "http://192.168.1.5/",
		"SSH · port 22", "nas SSH 22", "TCP · port 53 (DNS)", "Enregistrer le ping"} {
		if !strings.Contains(corps, attendu) {
			t.Errorf("bloc Sondes : %q absent", attendu)
		}
	}
	if strings.Contains(corps, "port 111") || strings.Contains(corps, "port 40123") {
		t.Error("rpcbind (111) et ports hauts sans nom ne doivent pas être proposés")
	}
	if !strings.Contains(corps, `value="http|https://192.168.1.5:5001/|nas DSM 5001|true" aria-label="HTTPS · port 5001 (DSM)" checked`) ||
		strings.Contains(corps, `value="tcp|192.168.1.5:53|nas DNS 53|false" aria-label="TCP · port 53 (DNS)" checked`) {
		t.Error("cases cochées d'office : HTTPS, HTTP, SSH seulement")
	}

	envoyer := func(v url.Values) string {
		v.Set("csrf", b.csrf(cl, "/machines/nas"))
		v.Set("code", b.code(secret))
		_, c, _ := b.post(cl, "/machines/nas/sondes", v)
		b.h.avancer(31 * time.Second)
		return c
	}
	// Ping depuis le serveur vers une adresse privée : refusé tout de suite.
	envoyer(url.Values{"action": {"ping"}, "depuis": {"serveur"}, "adresse": {"192.168.1.5"}})
	if l, _ := flotte.LireSondes(ctx, b.c.db); len(l) != 0 {
		t.Fatalf("adresse privée acceptée depuis le serveur : %+v", l)
	}
	// Deux adresses cochées et une tapée (déjà cochée : une seule fois) :
	// un ping par adresse, nommé avec l'adresse.
	envoyer(url.Values{"action": {"ping"}, "depuis": {"coll-maison"}, "adresse": {"192.168.1.5", "10.10.0.5"}, "autre": {"fd00::5, 192.168.1.5"}})
	envoyer(url.Values{"action": {"ajouter"}, "depuis": {"coll-maison"},
		"p": {"http|https://192.168.1.5:5001/|nas DSM 5001|true", "tcp|192.168.1.5:22|nas SSH 22|false"}})
	l, _ := flotte.LireSondes(ctx, b.c.db)
	if len(l) != 5 {
		t.Fatalf("sondes : %+v", l)
	}
	noms := map[string]string{}
	for _, s := range l {
		if s.Machine != "nas" || s.Depuis != "coll-maison" {
			t.Errorf("sonde mal rangée : %+v", s)
		}
		if s.Type == essais.Ping {
			noms[s.Cible] = s.Nom
		}
	}
	if noms["192.168.1.5"] != "nas ping 192.168.1.5" || noms["10.10.0.5"] != "nas ping 10.10.0.5" || noms["fd00::5"] != "nas ping fd00::5" {
		t.Errorf("pings : %v", noms)
	}
	_, corps, _ = b.get(cl, "/machines/nas")
	if !strings.Contains(corps, "3 pings") || !strings.Contains(corps, `name="garder_a" value="nas ping 10.10.0.5" checked`) ||
		!strings.Contains(corps, `name="garder_p" value="nas DSM 5001" checked`) {
		t.Error("après ajout : 3 pings, adresses et propositions déjà créées en cases cochées")
	}
	// Décocher une sonde déjà créée la retire (retour du 19/11) ; le ping
	// n'est pas touché par l'enregistrement des sondes proposées.
	envoyer(url.Values{"action": {"ajouter"}, "depuis": {"coll-maison"},
		"present_p": {"nas DSM 5001", "nas SSH 22"}, "garder_p": {"nas SSH 22"},
		"present_a": {"nas ping 10.10.0.5"}})
	l, _ = flotte.LireSondes(ctx, b.c.db)
	reste := map[string]bool{}
	for _, s := range l {
		reste[s.Nom] = true
	}
	if reste["nas DSM 5001"] || !reste["nas SSH 22"] || !reste["nas ping 10.10.0.5"] || len(l) != 4 {
		t.Fatalf("après décochage : %v", reste)
	}
	// Une sonde d'une autre machine (ou sans machine) ne se retire pas d'ici.
	_ = flotte.EnregistrerSondes(ctx, b.c.db, append(l, essais.Sonde{Nom: "Ailleurs", Type: essais.TCP, Cible: "10.0.0.9:22", Depuis: "coll-maison"}), "test", b.h.maintenant())
	envoyer(url.Values{"action": {"ajouter"}, "depuis": {"coll-maison"}, "present_p": {"Ailleurs"}})
	if l, _ := flotte.LireSondes(ctx, b.c.db); len(l) != 5 {
		t.Fatalf("sonde d'ailleurs retirée : %+v", l)
	}
	// (« Ailleurs » remis de côté pour la suite.)
	_ = flotte.EnregistrerSondes(ctx, b.c.db, l, "test", b.h.maintenant())
	// Plus rien de nouveau : refusé avec un message clair.
	envoyer(url.Values{"action": {"ping"}, "depuis": {"coll-maison"}, "adresse": {"10.10.0.5"}})
	// Retrait.
	envoyer(url.Values{"action": {"retirer"}, "sonde": {"nas ping 10.10.0.5"}})
	if l, _ := flotte.LireSondes(ctx, b.c.db); len(l) != 3 {
		t.Errorf("retrait : %+v", l)
	}

	// Machine sans site, agent ancien : choisir le site, adresse à taper.
	_, corps, _ = b.get(cl, "/machines/vieux")
	for _, attendu := range []string{"Machine sans site", `<option value="">— choisir le site —</option>`, "ne donne pas encore ses adresses",
		"site « bureau » · collecteur coll-bureau"} {
		if !strings.Contains(corps, attendu) {
			t.Errorf("machine sans site : %q absent", attendu)
		}
	}
}

func TestProposerSonde(t *testing.T) {
	cas := []struct {
		port              int
		proto, nom, cible string
	}{
		{443, "https", "nas HTTPS 443", "https://192.168.1.5/"},
		{8123, "http", "nas Home Assistant 8123", "http://192.168.1.5:8123/"},
		{22, "ssh", "nas SSH 22", "192.168.1.5:22"},
		{9999, "tls", "nas TLS 9999", "192.168.1.5:9999"},
	}
	for _, c := range cas {
		p := proposer("nas", "192.168.1.5", c.port, c.proto)
		if p.Nom != c.nom || p.Cible != c.cible {
			t.Errorf("%d %s : %q %q", c.port, c.proto, p.Nom, p.Cible)
		}
		s := essais.Sonde{Nom: p.Nom, Type: typeDeValeur(p.Valeur), Cible: p.Cible, Depuis: "c"}
		if err := s.Valider(); err != nil {
			t.Errorf("%d : sonde proposée invalide : %v", c.port, err)
		}
	}
	if p := proposer("nas", "fd00::5", 443, "https"); p.Cible != "https://[fd00::5]/" {
		t.Errorf("IPv6 : %q", p.Cible)
	}
	if n := nomSonde(strings.Repeat("très-long-nom-", 6)+"x/y", "ping"); len([]rune(n)) > 48 || !strings.HasSuffix(n, " ping") {
		t.Errorf("nom trop long : %q", n)
	}
	as := []adresseMachine{{IP: "fd00::5"}, {IP: "203.0.113.9"}, {IP: "192.168.1.5"}, {IP: "10.0.0.9", Principale: true}}
	trierAdresses(as)
	if as[0].IP != "10.0.0.9" || as[1].IP != "192.168.1.5" || as[2].IP != "203.0.113.9" {
		t.Errorf("ordre des adresses : %v", as)
	}
	if !adressePrivee(essais.Sonde{Cible: "https://10.0.0.2:8443/"}) || adressePrivee(essais.Sonde{Cible: "203.0.113.9:22"}) {
		t.Error("adressePrivee")
	}
}

// Page Sondes › « + Ajouter une sonde » › « Depuis une machine » (SPEC § 69) :
// même bloc que la page machine, retour sur la page Sondes.
func TestAjoutSondeDepuisPageSondes(t *testing.T) {
	b := nouveauBanc(t)
	if _, err := b.c.db.Exec(`INSERT INTO machines(nom, statut, type, site, cree_le, vu_le) VALUES
		('coll', 'approuvee', 'collecteur', 'maison', 1, 1), ('nas', 'approuvee', 'agent', 'maison', 1, 1)`); err != nil {
		t.Fatal(err)
	}
	b.metriquesSimulees(t, []serieTest{
		{[]string{"__name__", "mirador_adresse_info", "instance", "nas", "interface", "eth0", "adresse", "192.168.1.5", "principale", "1"}, constante(1)},
		{[]string{"__name__", "mirador_port_ecoute", "instance", "nas", "port", "443", "adresse", "*", "protocole", "https"}, constante(1)},
	})
	cl, secret := b.premierAdmin("admin", mdpAdmin)
	_, corps, _ := b.get(cl, "/sondes")
	if !strings.Contains(corps, `<a class="bouton" href="/sondes?ajout=1#ajouter">+ Ajouter une sonde</a>`) || strings.Contains(corps, `id="ajouter"`) {
		t.Error("bouton « + Ajouter une sonde » (comme « Ajouter une machine »), panneau fermé")
	}
	_, corps, _ = b.get(cl, "/sondes?ajout=1")
	for _, attendu := range []string{`id="ajouter"`, "Fermer", `data-onglet="machine"`, `data-onglet="manuel"`, "<option>nas</option>", `name="cible"`} {
		if !strings.Contains(corps, attendu) {
			t.Errorf("page Sondes : %q absent", attendu)
		}
	}
	// Liste « Depuis une machine » (pas « Machine liée », qui propose toutes les machines).
	if i := strings.Index(corps, `name="ajouter"`); i < 0 || strings.Contains(corps[i:i+strings.Index(corps[i:], "</select>")], "<option>coll</option>") {
		t.Error("un collecteur n'est pas une machine à sonder ici")
	}
	_, corps, _ = b.get(cl, "/sondes?ajouter=nas")
	for _, attendu := range []string{`id="ajouter"`, `data-onglet-force="machine"`, `<option selected>nas</option>`,
		`action="/machines/nas/sondes"`, `name="retour" value="sondes"`, "https://192.168.1.5/", `value="192.168.1.5" checked`} {
		if !strings.Contains(corps, attendu) {
			t.Errorf("page Sondes, nas choisie : %q absent", attendu)
		}
	}
	_, _, h := b.post(cl, "/machines/nas/sondes", url.Values{"csrf": {b.csrf(cl, "/sondes")}, "code": {b.code(secret)}, "retour": {"sondes"},
		"action": {"ping"}, "depuis": {"coll"}, "adresse": {"192.168.1.5"}})
	if loc := h.Get("Location"); !strings.HasPrefix(loc, "/sondes?ajouter=nas") {
		t.Errorf("retour après ajout : %q", loc)
	}
	if l, _ := flotte.LireSondes(t.Context(), b.c.db); len(l) != 1 || l[0].Machine != "nas" {
		t.Errorf("ping non ajouté : %+v", l)
	}
}

// Relecture du 21/11 : nom de sonde toujours valide, même avec une cible
// très longue (avant : plantage).
func TestNomSondeLong(t *testing.T) {
	for _, c := range []struct{ machine, quoi string }{
		{"nas", "ping monserveur-de-sauvegarde.maison.example.org"},
		{strings.Repeat("m", 60), "ping " + strings.Repeat("a", 80)},
		{"nas", "ping"},
	} {
		n := nomSonde(c.machine, c.quoi)
		if len([]rune(n)) > 48 || n == "" {
			t.Errorf("%q → %q (%d)", c.quoi, n, len([]rune(n)))
		}
	}
}
