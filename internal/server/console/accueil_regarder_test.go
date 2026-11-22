// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"strings"
	"testing"
)

// Vue d'ensemble (maquette validée le 22/11, SPEC § 120) : bandeau du plus
// grave, liste « À regarder », aperçu par site, ligne de pied ; le tableau
// du parc et la carte de chaleur complète masqués par défaut.
func TestAccueilARegarder(t *testing.T) {
	b := nouveauBanc(t)
	b.machinesApprouvees(t, "web1", "pve1")
	_, _ = b.c.db.Exec(`INSERT INTO machines(nom, statut, type, site, cree_le, vu_le) VALUES ('nas-backup', 'approuvee', 'agent', 'maison', 1, 1)`)
	_, _ = b.c.db.Exec(`UPDATE machines SET maj_echec = '0.5.1 : pas de battement en 10 min, retour à 0.5.0' WHERE nom = 'pve1'`)
	_, _ = b.c.db.Exec(`INSERT INTO machines(nom, statut, cree_le) VALUES ('nouvelle', 'en_attente', 1)`)
	c, _ := b.premierAdmin("admin", mdpAdmin)
	_, corps, _ := b.get(c, "/")
	for _, attendu := range []string{
		`<div class="bandeau-accueil erreur">`, "<h2>1 machine ne répond plus</h2>",
		"<b>1 machine muette</b>", "<b>pve1 : mise à jour en échec</b>", "retour à 0.5.0",
		"<b>1 machine en attente d&#39;approbation</b>", "<b>Aucun canal de notification</b>",
		`<a class="pastille apercu-erreur" href="/machines/nas-backup">`, "<h3>maison",
		`data-bloc="sondes"`, `data-bloc="certificats"`, "Aucun certificat suivi", `class="pied-accueil`,
	} {
		if !strings.Contains(corps, attendu) {
			t.Errorf("vue d'ensemble : %q absent", attendu)
		}
	}
	// Masqués par défaut (ordinateur et téléphone), encore dans la page.
	for _, bloc := range []string{`data-parc data-bloc="parc"`, `data-bloc="chaleur"`} {
		i := strings.Index(corps, bloc)
		if i < 0 || !strings.Contains(corps[max(0, i-60):i], "cache-o cache-t") {
			t.Errorf("%s : pas masqué par défaut", bloc)
		}
	}
	// Plus de case « Notifications » (dans la ligne du bas).
	if strings.Contains(corps, `data-bloc="notifications"`) {
		t.Error("case Notifications encore là")
	}
}
