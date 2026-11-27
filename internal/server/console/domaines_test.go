// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"strings"
	"testing"
	"time"
)

// Page Certificats › Noms de domaine (demande du 27/11) : une ligne par
// domaine principal, couleur selon l'échéance, erreur du registre lisible.
func TestPageDomaines(t *testing.T) {
	b := nouveauBanc(t)
	m := b.h.maintenant()
	for _, x := range []struct {
		nom, sous string
		expire    time.Time
		erreur    string
	}{{"example.org", "@, cloud, pass", m.Add(20 * 24 * time.Hour), ""}, {"example.net", "www", m.Add(200 * 24 * time.Hour), ""},
		{"example.de", "www", time.Time{}, "date d'expiration non publiée par le registre"}} {
		ex := int64(0)
		if !x.expire.IsZero() {
			ex = x.expire.Unix()
		}
		if _, err := b.c.db.Exec(`INSERT INTO domaines(nom, sous, expire, verifie_le, erreur) VALUES (?, ?, ?, ?, ?)`,
			x.nom, x.sous, ex, m.Unix(), x.erreur); err != nil {
			t.Fatal(err)
		}
	}
	c, _ := b.premierAdmin("admin", mdpAdmin)
	_, corps, _ := b.get(c, "/reglages/certificats")
	for _, attendu := range []string{"<h2>Noms de domaine</h2>", `<span class="pastille attention">dans 20 j</span>`,
		"example.org, cloud, pass", `<span class="pastille ok">dans 200 j</span>`, `<span class="pastille">inconnue</span>`,
		"date d&#39;expiration non publiée par le registre"} {
		if !strings.Contains(corps, attendu) {
			t.Errorf("page Certificats : %q absent", attendu)
		}
	}
}
