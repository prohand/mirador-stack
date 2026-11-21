// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"mirador-stack/internal/server/flotte"
)

// Noms d'hôte trop communs (pastille « nom à changer ? »).
func TestNomBanal(t *testing.T) {
	for _, n := range []string{"debian", "Ubuntu", "localhost", "raspberrypi", "pve", "vps-1a2b3c4d", "ip-10-0-0-5", "ubuntu-s-1vcpu-1gb-fra1-01"} {
		if !nomBanal(n) {
			t.Errorf("%q devrait être banal", n)
		}
	}
	for _, n := range []string{"srv-web", "nas", "pve-maison", "rpi-garage", "debianne"} {
		if nomBanal(n) {
			t.Errorf("%q ne devrait pas être banal", n)
		}
	}
}

// Réseau d'une demande comparé à ceux des machines approuvées (pastille
// « adresse inconnue »).
func TestAdresseInconnue(t *testing.T) {
	connus := reseauxConnus([]vueMachine{
		{InfoMachine: flotte.InfoMachine{DerniereIP: "198.51.100.4"}},
		{InfoMachine: flotte.InfoMachine{IPEnrolement: "2001:db8:1:2::10"}},
	})
	cas := map[string]bool{
		"198.51.100.4":        false, // même adresse
		"198.51.100.200":      false, // même /24
		"198.51.100.9:51234":  false, // avec port
		"::ffff:198.51.100.7": false, // IPv4 écrite en IPv6
		"2001:db8:1:2::99":    false, // même /64
		"203.0.113.50":        true,
		"2001:db8:1:3::1":     true,
		"":                    false, // illisible : rien à signaler
		"pas une adresse":     false,
	}
	for ip, attendu := range cas {
		if got := adresseInconnue(ip, connus); got != attendu {
			t.Errorf("%q : %v, attendu %v", ip, got, attendu)
		}
	}
	// Aucune machine connue (la première) : jamais de pastille.
	if adresseInconnue("203.0.113.50", reseauxConnus(nil)) {
		t.Error("pastille sans aucune machine approuvée")
	}
}

// Valeurs de l'adresse ramenées aux choix admis, liens courts.
func TestFiltresMachines(t *testing.T) {
	f := lireFiltresMachines("<script>", "tout", " web ")
	if f.Vue != "site" || f.Filtre != "" || f.Q != "web" {
		t.Fatalf("valeurs non ramenées aux choix admis : %+v", f)
	}
	if l := lireFiltresMachines("", "", "").Lien("vue", "site"); l != "/machines" {
		t.Errorf("lien par défaut : %q", l)
	}
	if l := f.Lien("filtre", "problemes"); l != "/machines?filtre=problemes&q=web" {
		t.Errorf("lien : %q", l)
	}
	if l := lireFiltresMachines("groupe", "", "a&b").Lien("", ""); l != "/machines?vue=groupe&q=a%26b" {
		t.Errorf("recherche non échappée : %q", l)
	}
	if f := lireFiltresMachines("", "", strings.Repeat("x", 500)); len(f.Q) != 100 {
		t.Errorf("recherche non bornée : %d", len(f.Q))
	}
}

func vm(nom, site, groupe string, muette bool, echec, releve, typ string) vueMachine {
	return vueMachine{InfoMachine: flotte.InfoMachine{Nom: nom, Site: site, Groupe: groupe, MajEchec: echec, Type: typ},
		Muette: muette, ReleveePar: releve}
}

// Blocs par site : problèmes en premier, équipement sous son collecteur,
// bloc « sans site » en dernier ; filtres et compteurs.
func TestRegrouperMachines(t *testing.T) {
	ms := []vueMachine{
		vm("srv-web", "ovh", "web", false, "", "", "agent"),
		vm("collecteur-lan", "maison", "", false, "", "", "collecteur"),
		vm("synology", "maison", "stockage", true, "", "collecteur-lan", "agent"),
		vm("nas", "maison", "stockage", false, "", "", "agent"),
		vm("pve", "maison", "proxmox", false, "signature refusée", "", "agent"),
		vm("rpi", "", "", false, "", "", "agent"),
	}
	g := regrouperMachines(ms, lireFiltresMachines("", "", ""))
	var titres []string
	for _, x := range g {
		titres = append(titres, x.Titre)
	}
	if strings.Join(titres, ",") != "maison,ovh," {
		t.Fatalf("blocs : %q", titres)
	}
	var noms []string
	for _, m := range g[0].Machines {
		noms = append(noms, m.Nom)
	}
	// collecteur-lan remonte avec son équipement muet, placé juste dessous.
	if strings.Join(noms, ",") != "collecteur-lan,synology,pve,nas" || !g[0].Machines[1].Sous {
		t.Fatalf("ordre du bloc maison : %v", noms)
	}
	if g[0].Problemes != 2 || g[1].Problemes != 0 {
		t.Errorf("problèmes : %d, %d", g[0].Problemes, g[1].Problemes)
	}
	// Filtre « problèmes » : synology seul dans son bloc (collecteur absent : pas « sous »).
	g = regrouperMachines(ms, lireFiltresMachines("liste", "problemes", ""))
	if len(g) != 1 || len(g[0].Machines) != 2 || g[0].Machines[0].Sous || g[0].Machines[1].Sous {
		t.Fatalf("filtre problèmes : %+v", g)
	}
	// Recherche sur le groupe, et compteurs après recherche.
	f := lireFiltresMachines("groupe", "", "STOCK")
	if g = regrouperMachines(ms, f); len(g) != 1 || g[0].Titre != "stockage" || len(g[0].Machines) != 2 {
		t.Fatalf("recherche : %+v", g)
	}
	if c := f.compter(ms); c != (compteursFiltres{Toutes: 2, Problemes: 1, Agents: 1, Collecteurs: 1}) {
		t.Errorf("compteurs : %+v", c)
	}
}

// Page Machines de bout en bout : machines en attente avec leurs
// pastilles, filtres dans l'adresse, liste des en attente bornée.
func TestPageMachines(t *testing.T) {
	b := nouveauBanc(t)
	c, _ := b.premierAdmin("admin", mdpAdmin)
	maintenant := b.h.maintenant().Unix()
	if _, err := b.c.db.Exec(`INSERT INTO machines(nom, statut, type, site, groupe, cree_le, vu_le, derniere_ip, maj_echec) VALUES
		('srv-web', 'approuvee', 'agent', 'ovh', 'web', 1, ?, '203.0.113.10', ''),
		('pve-maison', 'approuvee', 'agent', 'maison', 'proxmox', 1, ?, '198.51.100.4', 'signature refusée'),
		('ancien', 'refusee', 'agent', '', '', 1, 0, '', '')`, maintenant, maintenant); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.db.Exec(`INSERT INTO machines(nom, statut, cree_le, ip_enrolement) VALUES
		('debian', 'en_attente', ?, '192.0.2.77'), ('srv-mail', 'en_attente', ?, '203.0.113.51')`, maintenant-60, maintenant-30); err != nil {
		t.Fatal(err)
	}
	_, corps, _ := b.get(c, "/machines")
	for _, attendu := range []string{"À approuver (2)", "Nom à changer ?", "Adresse inconnue", `href="/machines/ajouter"`,
		"Problèmes (1)", ">ovh<", ">maison<", "refusée", `form="ap-debian"`} {
		if !strings.Contains(corps, attendu) {
			t.Errorf("absent de /machines : %q", attendu)
		}
	}
	// Une seule pastille de chaque : srv-mail a un nom choisi et vient d'un réseau connu.
	if strings.Count(corps, "Nom à changer ?") != 1 || strings.Count(corps, ">Adresse inconnue<") != 1 {
		t.Error("pastilles en trop")
	}
	// Plus ancienne demande d'abord.
	if strings.Index(corps, `form="ap-debian"`) > strings.Index(corps, `form="ap-srv-mail"`) {
		t.Error("ordre des machines en attente")
	}
	_, corps, _ = b.get(c, "/machines?filtre=problemes")
	if strings.Contains(corps, `href="/machines/srv-web"`) || !strings.Contains(corps, `href="/machines/pve-maison"`) {
		t.Error("filtre « problèmes »")
	}
	_, corps, _ = b.get(c, "/machines?q="+url.QueryEscape("aucune-machine"))
	if !strings.Contains(corps, "Aucune machine ne correspond") {
		t.Error("recherche sans résultat")
	}
	// Plus de 5 en attente : les autres derrière un lien.
	for i := 0; i < 5; i++ {
		if _, err := b.c.db.Exec(`INSERT INTO machines(nom, statut, cree_le) VALUES (?, 'en_attente', ?)`, "m"+itoa(int64(i)), maintenant); err != nil {
			t.Fatal(err)
		}
	}
	_, corps, _ = b.get(c, "/machines")
	if !strings.Contains(corps, "À approuver (7)") || !strings.Contains(corps, "Voir les 2 autres") || strings.Count(corps, `form="ap-`) != 5*5 {
		t.Errorf("liste bornée : %d champs", strings.Count(corps, `form="ap-`))
	}
	if _, corps, _ = b.get(c, "/machines?attente=toutes"); strings.Count(corps, `form="ap-`) != 7*5 {
		t.Error("« voir tout » ne montre pas tout")
	}
}

// Page d'ajout : étapes, jeton créé puis commande affichée sur la même page.
func TestPageAjouter(t *testing.T) {
	b := nouveauBanc(t)
	c, secret := b.premierAdmin("admin", mdpAdmin)
	_, corps, _ := b.get(c, "/machines/ajouter?quoi=n-importe-quoi")
	if !strings.Contains(corps, "Réglages de la machine Linux") || strings.Contains(corps, "MIRADOR_JETON") {
		t.Fatal("page d'ajout par défaut")
	}
	if _, corps, _ = b.get(c, "/machines/ajouter?quoi=collecteur"); !strings.Contains(corps, `name="type_machine" value="collecteur"`) {
		t.Fatal("choix collecteur")
	}
	csrf := b.csrf(c, "/machines/ajouter")
	b.h.avancer(31 * time.Second)
	_, corps, _ = b.post(c, "/machines/jeton", url.Values{"csrf": {csrf}, "nom": {"srv-web"}, "duree": {"1h"}, "code": {b.code(secret)}})
	if !strings.Contains(corps, "Commande à coller") || !strings.Contains(corps, "mirador-agent-install.sh --serveur https://ADRESSE-INGESTION --nom srv-web") ||
		!strings.Contains(corps, `data-copier="#commande-ajout"`) {
		t.Fatal("commande absente après création du jeton")
	}
	// Nom réservé par l'interface : refusé.
	b.h.avancer(31 * time.Second)
	b.post(c, "/machines/equipement", url.Values{"csrf": {csrf}, "nom": {"ajouter"}, "code": {b.code(secret)}})
	if _, err := flotte.IDMachine(context.Background(), b.c.db, "ajouter"); err == nil {
		t.Error("équipement « ajouter » créé : sa page serait inaccessible")
	}
}

// Relecture du 21/11 : « site:x » cherche la valeur exacte du site (lien
// « +N » de Réglages › Sites), pas un mot dans le nom.
func TestRechercheExacteSite(t *testing.T) {
	f := lireFiltresMachines("", "", "site:maison")
	if !f.trouve(vm("nas", "maison", "", false, "", "", "agent")) {
		t.Error("machine du site non trouvée")
	}
	if f.trouve(vm("maison-pc", "ovh", "", false, "", "", "agent")) || f.trouve(vm("x", "maison2", "", false, "", "", "agent")) {
		t.Error("recherche exacte : nom ou site voisin compté")
	}
	if !lireFiltresMachines("", "", "groupe:web").trouve(vm("a", "", "web", false, "", "", "agent")) {
		t.Error("groupe:")
	}
}
