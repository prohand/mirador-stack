// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"strings"
	"testing"
)

// « mirador sur cette machine » (SPEC § 124, maquette validée le 23/11) :
// agent et assistant root côte à côte dans l'onglet Réglages ; machine
// sans assistant ; collecteur avec son compagnon, sans assistant root.
func TestBlocMirador(t *testing.T) {
	b := nouveauBanc(t)
	m := b.h.maintenant()
	r := func(nom string, etq ...string) []string {
		return append([]string{"__name__", nom, "instance", "web1"}, etq...)
	}
	b.metriquesSimulees(t, []serieTest{
		{r("mirador_agent_root_age_seconds"), constante(240)},
		{r("mirador_agent_root_errors"), constante(0)},
		{r("mirador_agent_root_a_jour"), constante(1)},
		{r("mirador_agent_root_info", "version", "0.6.15"), constante(1)},
		{r("node_firewall_input_rules"), constante(8)},
		{r("smartctl_device_smart_status", "device", "sda"), constante(1)},
		{r("smartctl_device_smart_status", "device", "sdb"), constante(1)},
		{r("ssl_certs_found_total"), constante(4)},
		{[]string{"__name__", "mirador_collecteur_compagnon", "instance", "obs"}, constante(1)},
	})
	if _, err := b.c.db.Exec(`INSERT INTO machines(nom, statut, type, cree_le, vu_le, version_agent) VALUES
		('web1', 'approuvee', 'agent', 1, ?, '0.6.15'), ('pc2', 'approuvee', 'agent', 1, ?, '0.6.15'),
		('obs', 'approuvee', 'collecteur', 1, ?, '0.6.15'), ('fw', 'approuvee', 'agent', 1, ?, '')`,
		m.Unix(), m.Unix(), m.Unix(), m.Unix()); err != nil {
		t.Fatal(err)
	}
	cl, _ := b.premierAdmin("admin", mdpAdmin)
	bloc := func(nom string) string {
		_, corps, _ := b.get(cl, "/machines/"+nom)
		i := strings.Index(corps, `id="mirador"`)
		if i < 0 {
			return ""
		}
		return corps[i:min(len(corps), i+6000)]
	}
	web1 := bloc("web1")
	for _, attendu := range []string{"mirador sur cette machine", "<b>mirador-agent</b> <code>0.6.15</code>", ">sans root</span>",
		"mirador-agent (pas root)", "vers le serveur seulement, aucun port ouvert",
		"<b>Assistant root</b> <code>0.6.15</code>", ">root · sans réseau</span>", "il y a 4 min · prochain dans 11 min",
		">pare-feu</span>", ">SMART · 2 disques</span>", ">certificats · 4</span>", "Pourquoi deux morceaux", `value="actualiser"`} {
		if !strings.Contains(web1, attendu) {
			t.Errorf("bloc de web1 : %q absent", attendu)
		}
	}
	if pc2 := bloc("pc2"); !strings.Contains(pc2, "pas d&#39;assistant root : rien n&#39;est lu en root sur cette machine") {
		t.Error("machine sans assistant root : explication absente")
	}
	obs := bloc("obs")
	for _, attendu := range []string{"<b>mirador-collector</b>", "écoute seulement sur le LAN", "<b>Compagnon des processus</b>",
		"mirador-processus (pas root)", "Pas d&#39;assistant root sur un collecteur"} {
		if !strings.Contains(obs, attendu) {
			t.Errorf("bloc du collecteur : %q absent", attendu)
		}
	}
	if strings.Contains(obs, `value="actualiser"`) {
		t.Error("« Actualiser » sur un collecteur")
	}
	if bloc("fw") != "" {
		t.Error("bloc sur une machine sans agent (envoi direct)")
	}
}
