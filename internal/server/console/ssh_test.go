// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// « C'est moi » sur l'alerte SshAdresseNouvelle (demande du 27/11) :
// l'adresse vient de l'alerte, elle devient connue, l'action est auditée.
func TestSSHCestMoi(t *testing.T) {
	b := nouveauBanc(t)
	m := b.h.maintenant()
	inserer := func(regle, etq string) string {
		res, err := b.c.db.Exec(`INSERT INTO alertes(regle, etiquettes, gravite, etat, debut, active_le) VALUES (?, ?, 'warning', 'active', ?, ?)`,
			regle, etq, m.UnixMilli(), m.UnixMilli())
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		return "/alertes/" + strconv.FormatInt(id, 10)
	}
	page := inserer("SshAdresseNouvelle", `{"alertname":"SshAdresseNouvelle","instance":"web1","adresse":"198.51.100.23","utilisateur":"admin · publickey"}`)
	c, _ := b.premierAdmin("admin", mdpAdmin)
	if _, corps, _ := b.get(c, page); !strings.Contains(corps, `value="ssh_connue"`) || !strings.Contains(corps, "<code>198.51.100.23</code>") {
		t.Fatal("bouton « C'est moi » absent")
	}
	if _, corps, _ := b.get(c, page); !strings.Contains(corps, `name="portee" value="machine"`) || !strings.Contains(corps, "Seulement sur web1") {
		t.Error("bouton « Seulement sur web1 » absent")
	}
	b.post(c, page, url.Values{"csrf": {b.csrf(c, page)}, "action": {"ssh_connue"}, "portee": {"machine"}, "adresse": {"203.0.113.66"}})
	var adr, machine string
	_ = b.c.db.QueryRow(`SELECT prefixe, machine FROM ssh_connues`).Scan(&adr, &machine)
	if adr != "198.51.100.23/32" || machine != "web1" {
		t.Fatalf("adresse connue : %q %q (celle du formulaire ne doit pas compter)", adr, machine)
	}
	var n int
	_ = b.c.db.QueryRow(`SELECT count(*) FROM audit WHERE action = 'ssh_adresse_connue'`).Scan(&n)
	if n != 1 {
		t.Errorf("audit : %d", n)
	}
	// Autre alerte : pas de bouton, action refusée.
	autre := inserer("CpuEleve", `{"alertname":"CpuEleve","instance":"web1","adresse":"192.0.2.1"}`)
	if _, corps, _ := b.get(c, autre); strings.Contains(corps, `value="ssh_connue"`) {
		t.Error("bouton sur une autre alerte")
	}
	b.post(c, autre, url.Values{"csrf": {b.csrf(c, autre)}, "action": {"ssh_connue"}})
	_ = b.c.db.QueryRow(`SELECT count(*) FROM ssh_connues`).Scan(&n)
	if n != 1 {
		t.Errorf("adresse d'une autre alerte rendue connue : %d", n)
	}
}

// Réglages › Sécurité : adresses SSH connues, ajout (toutes ou une machine)
// et retrait (retour du 27/11).
func TestSSHConnuesSecurite(t *testing.T) {
	b := nouveauBanc(t)
	b.machinesApprouvees(t, "web1")
	c, secret := b.premierAdmin("admin", mdpAdmin)
	ajouter := func(plage, machine string) {
		b.post(c, "/securite/ssh", url.Values{"csrf": {b.csrf(c, "/securite")}, "action": {"ajouter"}, "plage": {plage}, "machine": {machine}, "note": {"box"},
			"code": {b.code(secret)}})
	}
	// Code 2FA ressaisi même en mode administration (revue du 28/11).
	b.post(c, "/securite/ssh", url.Values{"csrf": {b.csrf(c, "/securite")}, "action": {"ajouter"}, "plage": {"192.0.2.0/24"}})
	ajouter("203.0.113.0/24", "")
	ajouter("198.51.100.7", "web1")
	ajouter("0.0.0.0/0", "")         // trop large : refusé
	ajouter("192.0.2.1", "inconnue") // machine inconnue : refusé
	_, corps, _ := b.get(c, "/securite")
	for _, attendu := range []string{"Adresses SSH connues (2)", "<code>203.0.113.0/24</code>", "<code>198.51.100.7/32</code>", `href="/machines/web1">web1</a>`} {
		if !strings.Contains(corps, attendu) {
			t.Errorf("page Sécurité : %q absent", attendu)
		}
	}
	var id string
	_ = b.c.db.QueryRow(`SELECT id FROM ssh_connues WHERE machine = 'web1'`).Scan(&id)
	b.post(c, "/securite/ssh", url.Values{"csrf": {b.csrf(c, "/securite")}, "action": {"retirer"}, "id": {id}})
	var n int
	_ = b.c.db.QueryRow(`SELECT count(*) FROM ssh_connues`).Scan(&n)
	if n != 1 {
		t.Errorf("après retrait : %d", n)
	}
	_ = b.c.db.QueryRow(`SELECT count(*) FROM audit WHERE action IN ('ssh_adresse_connue', 'ssh_adresse_retiree')`).Scan(&n)
	if n != 3 {
		t.Errorf("audit : %d", n)
	}
}
