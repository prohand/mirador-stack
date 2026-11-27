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
	b.post(c, page, url.Values{"csrf": {b.csrf(c, page)}, "action": {"ssh_connue"}, "adresse": {"203.0.113.66"}})
	var adr string
	_ = b.c.db.QueryRow(`SELECT adresse FROM ssh_connues`).Scan(&adr)
	if adr != "198.51.100.23" {
		t.Fatalf("adresse connue : %q (celle du formulaire ne doit pas compter)", adr)
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
