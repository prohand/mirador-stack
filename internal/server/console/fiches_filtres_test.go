// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"strings"
	"testing"

	"mirador-stack/internal/server/alertes"
	"mirador-stack/internal/server/flotte"
	"mirador-stack/internal/server/servicesmetier"
	essais "mirador-stack/internal/sondes"
)

// Liens des fiches vers des pages filtrées sur l'élément (retour du 19/11,
// SPEC § 68), avec le bandeau « Filtré sur … · tout voir ».
func TestFichesLiensFiltres(t *testing.T) {
	b := nouveauBanc(t)
	ctx := t.Context()
	m := b.h.maintenant()
	b.machinesApprouvees(t, "nas")
	_, _ = b.c.db.Exec(`INSERT INTO machines(nom, statut, type, cree_le) VALUES ('lan', 'approuvee', 'collecteur', 1)`)
	if err := flotte.EnregistrerSondes(ctx, b.c.db, []essais.Sonde{
		{Nom: "HA", Type: essais.HTTP, Cible: "https://ha.lan/", Depuis: "lan", Machine: "nas"},
		{Nom: "Autre", Type: essais.Ping, Cible: "192.168.1.9", Depuis: "lan"},
	}, "test", m); err != nil {
		t.Fatal(err)
	}
	_ = servicesmetier.Enregistrer(ctx, b.c.db, []servicesmetier.Service{
		{Nom: "Domotique", Composants: []servicesmetier.Composant{{Sorte: "sonde", Nom: "HA", Cle: true}}},
		{Nom: "Cloud", Composants: []servicesmetier.Composant{{Sorte: "sonde", Nom: "Autre"}}},
	}, "test", "", m)
	for _, a := range []struct{ regle, etq, resume string }{
		{"SondeEnEchec", `{"alertname":"SondeEnEchec","instance":"lan","sonde":"HA"}`, "Résumé-HA"},
		{"SondeEnEchec", `{"alertname":"SondeEnEchec","instance":"lan","sonde":"Autre"}`, "Résumé-Autre"},
	} {
		_, _ = b.c.db.Exec(`INSERT INTO alertes(regle, etiquettes, gravite, etat, debut, active_le, fin, resume) VALUES (?, ?, 'critical', 'resolue', ?, ?, ?, ?)`,
			a.regle, a.etq, m.Add(-60*60*1e9).UnixMilli(), m.Add(-60*60*1e9).UnixMilli(), m.Add(-30*60*1e9).UnixMilli(), a.resume)
	}
	cl, _ := b.premierAdmin("admin", mdpAdmin)
	_, corps, _ := b.get(cl, "/sondes/HA")
	for _, lien := range []string{`href="/sondes?sonde=HA"`, `href="/disponibilite?element=s%3AHA"`, `href="/alertes?sonde=HA"`,
		`href="/incidents?sonde=HA"`, `href="/evenements?element=HA"`, `href="/machines/nas#sondes"`} {
		if !strings.Contains(corps, lien) {
			t.Errorf("fiche de la sonde : lien %s absent", lien)
		}
	}
	if strings.Contains(corps, `<a href="/evenements">`) {
		t.Error("lien Événements non filtré")
	}
	_, corps, _ = b.get(cl, "/applications/Domotique")
	for _, lien := range []string{`href="/applications?nom=Domotique"`, `href="/incidents?application=Domotique"`, `href="/alertes?application=Domotique"`} {
		if !strings.Contains(corps, lien) {
			t.Errorf("fiche de l'application : lien %s absent", lien)
		}
	}
	// Pages filtrées.
	verifier := func(chemin, present, absent string) {
		t.Helper()
		_, corps, _ := b.get(cl, chemin)
		if !strings.Contains(corps, `class="message filtre-sur"`) || !strings.Contains(corps, "tout voir") {
			t.Errorf("%s : bandeau « Filtré sur » absent", chemin)
		}
		if !strings.Contains(corps, present) || strings.Contains(corps, absent) {
			t.Errorf("%s : %q attendu, %q en trop", chemin, present, absent)
		}
	}
	verifier("/alertes?sonde=HA", "la sonde « HA »", "\x00")
	if hs, err := alertes.HistoriqueFiltre(ctx, b.c.db, alertes.Filtre{Sonde: "HA"}); err != nil || len(hs) != 1 || hs[0].Resume != "Résumé-HA" {
		t.Errorf("historique filtré sur la sonde : %+v %v", hs, err)
	}
	verifier("/sondes?sonde=HA", `<strong>HA</strong>`, `<strong>Autre</strong>`)
	verifier("/applications?nom=Domotique", `href="/applications/Domotique"`, `href="/applications/Cloud"`)
	verifier("/disponibilite?element=s%3AHA", "HA", ">Autre<")
}
