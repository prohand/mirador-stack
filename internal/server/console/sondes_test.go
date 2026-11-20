// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/tsdb"

	"mirador-stack/internal/server/alertes"
	"mirador-stack/internal/server/flotte"
	"mirador-stack/internal/server/servicesmetier"
	essais "mirador-stack/internal/sondes"
)

// Page Sondes (SPEC § 48) : ajout (code 2FA), état, retrait.
func TestPageSondes(t *testing.T) {
	b := nouveauBanc(t)
	s := func(sonde, nom string, v float64, extra ...string) serieTest {
		return serieTest{append([]string{"__name__", nom, "instance", "mirador-server", "job", "sondes", "sonde", sonde}, extra...), constante(v)}
	}
	b.metriquesSimulees(t, []serieTest{
		s("Nextcloud", "mirador_sonde_succes", 0), s("Nextcloud", "mirador_sonde_duree_secondes", 0.9),
		s("Nextcloud", "mirador_sonde_erreur", 1, "erreur", "code 502"),
		s("Vaultwarden", "mirador_sonde_succes", 1), s("Vaultwarden", "mirador_sonde_duree_secondes", 0.041),
		s("Vaultwarden", "mirador_sonde_lente", 0), s("Vaultwarden", "mirador_sonde_code_http", 200),
	})
	cl, secret := b.premierAdmin("admin", mdpAdmin)
	ajouter := func(champs url.Values) string {
		champs.Set("csrf", b.csrf(cl, "/sondes"))
		champs.Set("action", "ajouter")
		champs.Set("code", b.code(secret))
		_, corps, _ := b.post(cl, "/sondes", champs)
		_, corps, _ = b.get(cl, "/sondes")
		return corps
	}
	ajouter(url.Values{"nom": {"Nextcloud"}, "type": {"http"}, "cible": {"https://cloud.exemple.fr/status.php"}, "depuis": {"serveur"},
		"code_http": {"200"}, "contenu": {"installed"}})
	b.h.avancer(31 * time.Second) // code TOTP suivant (anti-rejeu)
	corps := ajouter(url.Values{"nom": {"Vaultwarden"}, "type": {"http"}, "cible": {"https://pass.exemple.fr/alive"}, "depuis": {"serveur"},
		"champ_json": {"status"}, "valeur_json": {"ok"}, "certificat": {"1"}, "certificat_jours": {"21"}})
	for _, attendu := range []string{"<h1>Sondes</h1>", "Échec : code 502", "OK · 41 ms", `<span class="pastille code-sonde ok">Code 200</span>`, "code 200 · contient « installed »",
		"JSON status = « ok » · alerte 21 j avant l&#39;expiration du certificat",
		`class="tuile critique"`, "Temps de réponse médian", `name="nom" value="Nextcloud"`} {
		if !strings.Contains(corps, attendu) {
			t.Errorf("page des sondes : %q absent", attendu)
		}
	}
	if strings.Index(corps, "Nextcloud</strong>") > strings.Index(corps, "Vaultwarden</strong>") {
		t.Error("la sonde en échec doit venir en premier")
	}
	// Refus : collecteur inconnu, cible invalide.
	b.h.avancer(31 * time.Second)
	ajouter(url.Values{"nom": {"X"}, "type": {"tcp"}, "cible": {"a:22"}, "depuis": {"inconnu"}})
	b.h.avancer(31 * time.Second)
	ajouter(url.Values{"nom": {"Y"}, "type": {"http"}, "cible": {"ftp://a"}, "depuis": {"serveur"}})
	liste, _ := flotte.LireSondes(t.Context(), b.c.db)
	if len(liste) != 2 {
		t.Fatalf("sondes enregistrées : %+v", liste)
	}
	// Retrait.
	b.h.avancer(31 * time.Second)
	b.post(cl, "/sondes", url.Values{"csrf": {b.csrf(cl, "/sondes")}, "action": {"supprimer"}, "nom": {"Nextcloud"}, "code": {b.code(secret)}})
	if liste, _ := flotte.LireSondes(t.Context(), b.c.db); len(liste) != 1 || liste[0].Nom != "Vaultwarden" {
		t.Fatalf("après retrait : %+v", liste)
	}
	// Menu : compteur des sondes en échec.
	_, corps, _ = b.get(cl, "/")
	if !strings.Contains(corps, `href="/sondes"`) {
		t.Error("entrée Sondes absente du menu")
	}
}

// Modifier une sonde (retour du 19/11) : cible HTTPS changée pour le nom du
// site, renommage suivi par les services et les étiquettes ; machine gardée.
func TestModifierSonde(t *testing.T) {
	b := nouveauBanc(t)
	ctx := t.Context()
	if _, err := b.c.db.Exec(`INSERT INTO machines(nom, statut, type, site, cree_le) VALUES ('lan', 'approuvee', 'collecteur', 'maison', 1)`); err != nil {
		t.Fatal(err)
	}
	if err := flotte.EnregistrerSondes(ctx, b.c.db, []essais.Sonde{{Nom: "dhcp-dns02 HTTPS 443", Type: essais.HTTP,
		Cible: "https://192.168.35.250/", Depuis: "lan", NonVerifie: true, Machine: "dhcp-dns02"}}, "test", b.h.maintenant()); err != nil {
		t.Fatal(err)
	}
	if err := servicesmetier.Enregistrer(ctx, b.c.db, []servicesmetier.Service{{Nom: "DNS",
		Composants: []servicesmetier.Composant{{Sorte: "sonde", Nom: "dhcp-dns02 HTTPS 443", Cle: true}}}}, "test", "", b.h.maintenant()); err != nil {
		t.Fatal(err)
	}
	cl, secret := b.premierAdmin("admin", mdpAdmin)
	// Étiquette posée directement (même format que l'interface, § 59).
	_, _ = b.c.db.Exec(`INSERT INTO reglages(cle, valeur, modifie_le, modifie_par) VALUES ('etiquettes_elements', '{"s:dhcp-dns02 HTTPS 443":{"role":"dns"}}', 1, 'test')
		ON CONFLICT(cle) DO UPDATE SET valeur = excluded.valeur`)
	_, corps, _ := b.get(cl, "/sondes/dhcp-dns02%20HTTPS%20443")
	for _, attendu := range []string{`id="modifier"`, `name="ancien" value="dhcp-dns02 HTTPS 443"`, `value="https://192.168.35.250/"`, `name="non_verifie" value="1" checked`,
		`class="machine-liee" href="/machines/dhcp-dns02#sondes"`, `<b>dhcp-dns02</b>`, `<small>sonde la machine</small>dhcp-dns02`} {
		if !strings.Contains(corps, attendu) {
			t.Errorf("fiche de la sonde : %q absent", attendu)
		}
	}
	b.post(cl, "/sondes", url.Values{"csrf": {b.csrf(cl, "/sondes")}, "action": {"modifier"}, "ancien": {"dhcp-dns02 HTTPS 443"},
		"nom": {"DNS02 interface"}, "type": {"http"}, "cible": {"https://dns02.lan/"}, "depuis": {"lan"}, "code": {b.code(secret)}})
	l, _ := flotte.LireSondes(ctx, b.c.db)
	if len(l) != 1 || l[0].Nom != "DNS02 interface" || l[0].Cible != "https://dns02.lan/" || l[0].NonVerifie || l[0].Machine != "dhcp-dns02" {
		t.Fatalf("sonde modifiée : %+v", l)
	}
	if svcs, _ := servicesmetier.Lire(ctx, b.c.db); svcs[0].Composants[0].Nom != "DNS02 interface" {
		t.Errorf("service non suivi : %+v", svcs)
	}
	etq := b.c.lireEtiquettes(ctx)
	if etq["s:DNS02 interface"]["role"] != "dns" || etq["s:dhcp-dns02 HTTPS 443"] != nil {
		t.Errorf("étiquettes non suivies : %v", etq)
	}
	// Machine liée changée par le formulaire, puis retirée (retour du 20/11).
	b.h.avancer(31 * time.Second)
	b.post(cl, "/sondes", url.Values{"csrf": {b.csrf(cl, "/sondes")}, "action": {"modifier"}, "ancien": {"DNS02 interface"},
		"nom": {"DNS02 interface"}, "type": {"http"}, "cible": {"https://dns02.lan/"}, "depuis": {"lan"}, "machine": {"lan"}, "code": {b.code(secret)}})
	if l, _ = flotte.LireSondes(ctx, b.c.db); l[0].Machine != "lan" {
		t.Fatalf("machine liée non changée : %+v", l[0])
	}
	if _, page, _ := b.get(cl, "/sondes"); !strings.Contains(page, `<th>Machine</th>`) || !strings.Contains(page, `class="machine-liee petite" href="/machines/lan#sondes"`) {
		t.Error("colonne Machine absente de la liste des sondes")
	}
	b.h.avancer(31 * time.Second)
	b.post(cl, "/sondes", url.Values{"csrf": {b.csrf(cl, "/sondes")}, "action": {"modifier"}, "ancien": {"DNS02 interface"},
		"nom": {"DNS02 interface"}, "type": {"http"}, "cible": {"https://dns02.lan/"}, "depuis": {"lan"}, "machine": {""}, "code": {b.code(secret)}})
	if l, _ = flotte.LireSondes(ctx, b.c.db); l[0].Machine != "" {
		t.Fatalf("machine liée non retirée : %+v", l[0])
	}
	if _, page, _ := b.get(cl, "/sondes/DNS02%20interface"); !strings.Contains(page, `machine-liee aucune`) {
		t.Error("fiche : « aucune machine » non montré")
	}
	// Nom déjà pris : refusé, rien ne change.
	_ = flotte.EnregistrerSondes(ctx, b.c.db, append(l, essais.Sonde{Nom: "Autre", Type: essais.Ping, Cible: "192.168.35.1", Depuis: "lan"}), "test", b.h.maintenant())
	b.h.avancer(31 * time.Second)
	b.post(cl, "/sondes", url.Values{"csrf": {b.csrf(cl, "/sondes")}, "action": {"modifier"}, "ancien": {"Autre"},
		"nom": {"DNS02 interface"}, "type": {"ping"}, "cible": {"192.168.35.1"}, "depuis": {"lan"}})
	if l, _ := flotte.LireSondes(ctx, b.c.db); len(l) != 2 {
		t.Errorf("nom en double accepté : %+v", l)
	}
}

// Couleur du code de retour HTTP sous l'état (retour du 19/11).
func TestCouleurCodeSonde(t *testing.T) {
	s := essais.Sonde{Nom: "S", Type: essais.HTTP, Depuis: "lan"}
	for code, classe := range map[float64]string{200: "ok", 204: "ok", 301: "info", 404: "attention", 503: "erreur"} {
		lues := map[string]map[string]alertes.Echantillon{"code": {cleSonde("lan", "S"): {Valeur: code}}}
		if txt, c := codeSondeLu(lues, s); c != classe || !strings.HasPrefix(txt, "Code ") {
			t.Errorf("code %v : %q %q, attendu %q", code, txt, c, classe)
		}
	}
	if txt, _ := codeSondeLu(map[string]map[string]alertes.Echantillon{}, essais.Sonde{Type: essais.TCP}); txt != "" {
		t.Error("TCP : pas de code")
	}
}

// Règle SondeCertificatBientotExpire (SPEC § 81) : seuil propre à chaque
// sonde, valeur en jours.
func TestRegleCertificatSonde(t *testing.T) {
	db, err := tsdb.Open(t.TempDir(), nil, nil, tsdb.DefaultOptions(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	m := time.Now()
	app := db.Appender(context.Background())
	ajout := func(sonde, nom string, v float64) {
		_, _ = app.Append(0, labels.FromStrings("__name__", nom, "instance", "lan", "job", "sondes", "sonde", sonde, "type", "http", "cible", "https://"+sonde), m.UnixMilli(), v)
	}
	ajout("bientot", "mirador_sonde_certificat_fin_timestamp_seconds", float64(m.Add(10*24*time.Hour).Unix()))
	ajout("bientot", "mirador_sonde_certificat_alerte_jours", 14)
	ajout("loin", "mirador_sonde_certificat_fin_timestamp_seconds", float64(m.Add(10*24*time.Hour).Unix()))
	ajout("loin", "mirador_sonde_certificat_alerte_jours", 7)
	_ = app.Commit()
	l := alertes.NouveauLecteur(db, nil)
	for _, r := range alertes.ReglesMirador() {
		if r.Nom != "SondeCertificatBientotExpire" {
			continue
		}
		ech, err := l.Instant(context.Background(), r.Expr, m)
		if err != nil || len(ech) != 1 || ech[0].Etiquettes.Get("sonde") != "bientot" || ech[0].Valeur < 9.9 || ech[0].Valeur > 10 {
			t.Fatalf("%v %+v", err, ech)
		}
		return
	}
	t.Fatal("règle absente")
}
