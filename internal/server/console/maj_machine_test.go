// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"net/url"
	"strings"
	"testing"
)

// Bloc « Mise à jour » de la page machine (SPEC § 44) : échec affiché avec
// sa raison, bouton « Réessayer », commande de secours pour un agent trop
// ancien ; « Mettre à jour maintenant » quand une version attend ; lien
// depuis la liste des machines.
func TestBlocMiseAJour(t *testing.T) {
	b := nouveauBanc(t)
	m := b.h.maintenant()
	for _, l := range []struct{ nom, version, cible, echec, capacites string }{
		{"ancien", "0.3.1", "0.3.5", "0.3.5 : pas de battement en 10 min", "surveillance,elements"},
		{"recent", "0.3.6", "0.3.7", "0.3.7 : self-test en échec", "surveillance,reessai"},
		{"attente", "0.3.5", "0.3.6", "", "surveillance,reessai"},
		{"ajour", "0.3.6", "0.3.6", "", "surveillance,reessai"},
	} {
		if _, err := b.c.db.Exec(`INSERT INTO machines(nom, statut, type, cree_le, vu_le, version_agent, version_cible, maj_echec, capacites)
			VALUES (?, 'approuvee', 'agent', 1, ?, ?, ?, ?, ?)`, l.nom, m.Unix(), l.version, l.cible, l.echec, l.capacites); err != nil {
			t.Fatal(err)
		}
	}
	c, _ := b.premierAdmin("admin", mdpAdmin)

	_, corps, _ := b.get(c, "/machines/ancien")
	bloc := section(corps, `id="maj"`)
	for _, attendu := range []string{"Mise à jour en échec", "0.3.5 : pas de battement en 10 min", "Réessayer la mise à jour",
		"<code>sudo rm /var/lib/mirador-agent/bin/echec.json</code>", `value="maj_forcer"`} {
		if !strings.Contains(bloc, attendu) {
			t.Errorf("agent ancien en échec : %q absent", attendu)
		}
	}
	if _, corps, _ = b.get(c, "/machines/recent"); strings.Contains(section(corps, `id="maj"`), "sudo rm") {
		t.Error("commande de secours montrée à un agent qui sait réessayer")
	}
	if _, corps, _ = b.get(c, "/machines/attente"); !strings.Contains(section(corps, `id="maj"`), "Mettre à jour maintenant") {
		t.Error("« Mettre à jour maintenant » absent (version en attente)")
	}
	if _, corps, _ = b.get(c, "/machines/ajour"); strings.Contains(corps, `id="maj"`) {
		t.Error("bloc Mise à jour sur une machine à jour")
	}
	if _, corps, _ = b.get(c, "/machines"); !strings.Contains(corps, `href="/machines/ancien#maj"`) {
		t.Error("pastille « Mise à jour en échec » sans lien")
	}

	// Le bouton enregistre la demande et la montre.
	csrf := b.csrf(c, "/machines/ancien")
	b.post(c, "/machines/ancien", url.Values{"csrf": {csrf}, "action": {"maj_forcer"}})
	var le int64
	_ = b.c.db.QueryRow(`SELECT maj_forcee_le FROM machines WHERE nom = 'ancien'`).Scan(&le)
	if le < m.Unix() {
		t.Fatalf("demande non enregistrée : %d", le)
	}
	if _, corps, _ = b.get(c, "/machines/ancien"); !strings.Contains(section(corps, `id="maj"`), "fenêtre horaire ignorée pendant 24 h") {
		t.Error("demande récente non signalée")
	}
}
