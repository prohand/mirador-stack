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
		{[]string{"__name__", "mirador_adresse_info", "instance", "nas", "interface", "eth0", "adresse", "192.168.1.5"}, constante(1)},
		p("22", "*", "ssh"), p("5001", "*", "https"), p("80", "*", "http"), p("111", "*", "tcp"),
		p("53", "192.168.1.5", "tcp"), p("40123", "*", "tcp"),
	})
	cl, secret := b.premierAdmin("admin", mdpAdmin)
	_, corps, _ := b.get(cl, "/machines/nas")
	for _, attendu := range []string{`id="sondes"`, `<option value="coll-maison" selected>site « maison » · collecteur coll-maison</option>`,
		`value="192.168.1.5"`, "HTTPS · port 5001 (DSM)", "https://192.168.1.5:5001/", "nas DSM 5001", "http://192.168.1.5/",
		"SSH · port 22", "nas SSH 22", "TCP · port 53 (DNS)", "Activer le ping"} {
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
	envoyer(url.Values{"action": {"ping"}, "depuis": {"coll-maison"}, "adresse": {"192.168.1.5"}})
	envoyer(url.Values{"action": {"ajouter"}, "depuis": {"coll-maison"},
		"p": {"http|https://192.168.1.5:5001/|nas DSM 5001|true", "tcp|192.168.1.5:22|nas SSH 22|false"}})
	l, _ := flotte.LireSondes(ctx, b.c.db)
	if len(l) != 3 {
		t.Fatalf("sondes : %+v", l)
	}
	for _, s := range l {
		if s.Machine != "nas" || s.Depuis != "coll-maison" {
			t.Errorf("sonde mal rangée : %+v", s)
		}
		if s.Type == essais.Ping && (s.Nom != "nas ping" || s.Cible != "192.168.1.5") {
			t.Errorf("ping : %+v", s)
		}
	}
	_, corps, _ = b.get(cl, "/machines/nas")
	if !strings.Contains(corps, "ping actif") || strings.Contains(corps, "Activer le ping") || !strings.Contains(corps, `title="déjà sondé par « nas DSM 5001 »"`) {
		t.Error("après ajout : ping actif et propositions marquées « déjà sondé »")
	}
	// Retrait.
	envoyer(url.Values{"action": {"retirer"}, "sonde": {"nas ping"}})
	if l, _ := flotte.LireSondes(ctx, b.c.db); len(l) != 2 {
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
	as := []string{"fd00::5", "203.0.113.9", "192.168.1.5"}
	trierAdresses(as)
	if as[0] != "192.168.1.5" || as[1] != "203.0.113.9" {
		t.Errorf("ordre des adresses : %v", as)
	}
	if !adressePrivee(essais.Sonde{Cible: "https://10.0.0.2:8443/"}) || adressePrivee(essais.Sonde{Cible: "203.0.113.9:22"}) {
		t.Error("adressePrivee")
	}
}
