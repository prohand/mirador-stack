// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mirador-stack/internal/server/notif"
)

// TestConvertirGrafana : régénère tableaux/*.json depuis reference/grafana/
// (CONVERTIR_GRAFANA=1). Sans la variable, vérifie seulement que la
// conversion marche toujours.
func TestConvertirGrafana(t *testing.T) {
	fichiers, _ := filepath.Glob("../../../reference/grafana/*.json")
	if len(fichiers) == 0 {
		t.Skip("reference/grafana absent")
	}
	for _, f := range fichiers {
		brut, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		nom := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(f), "grafana-"), ".json")
		nom = strings.TrimSuffix(nom, "-victoriametrics")
		tab, err := DepuisGrafana(nom, brut)
		if err != nil {
			t.Errorf("%s : %v", f, err)
			continue
		}
		if os.Getenv("CONVERTIR_GRAFANA") != "" {
			b, _ := json.MarshalIndent(tab, "", "  ")
			if err := os.WriteFile(filepath.Join("tableaux", nom+".json"), append(b, '\n'), 0o644); err != nil { // #nosec G306 -- fichier du dépôt
				t.Fatal(err)
			}
		}
		n := 0
		for _, r := range tab.Rangees {
			n += len(r.Panneaux)
		}
		t.Logf("%s : %d panneaux repris, %d non repris", nom, n, len(tab.Ignores))
	}
}

// TestTableauxEmbarques : chaque tableau embarqué est valide (toutes ses
// expressions passent le langage restreint).
func TestTableauxEmbarques(t *testing.T) {
	if len(tableauxDepart) < 5 {
		t.Fatalf("%d tableaux embarqués", len(tableauxDepart))
	}
	for nom, tab := range tableauxDepart {
		if err := tab.Valider(); err != nil {
			t.Errorf("%s : %v", nom, err)
		}
	}
}

func TestSubstituer(t *testing.T) {
	vals := map[string]string{"instance": "", "site": "maison"}
	e := substituer(`up{instance=~"$instance", site=~"$site"}[$__rate_interval]`, vals, 30*time.Second, time.Hour)
	if e != `up{instance=~".*", site="maison"}[120s]` {
		t.Errorf("substitution : %s", e)
	}
	// Variable inconnue : laissée telle quelle (l'expression sera refusée).
	if e := substituer(`up{x=~"$autre"}`, vals, time.Minute, time.Hour); !strings.Contains(e, "$autre") {
		t.Error(e)
	}
}

func TestFormater(t *testing.T) {
	deux := 2
	for _, c := range []struct {
		v        float64
		u, att   string
		decimale *int
	}{
		{42.345, "percent", "42,3 %", nil},
		{0.5, "percentunit", "50,0 %", nil},
		{3 * 1024 * 1024, "bytes", "3,0 Mio", nil},
		{2_500_000, "bps", "2,5 Mbit/s", nil},
		{3700, "s", "1 h 01", nil},
		{12, "short", "12", nil},
		{1.23456, "short", "1,23", &deux},
	} {
		if g := formater(c.v, c.u, c.decimale); g != c.att {
			t.Errorf("%v %s : %q, attendu %q", c.v, c.u, g, c.att)
		}
	}
}

// TestGabaritsAvecDonnees : pages rendues avec des données d'exemple (celles
// qu'aucun test de bout en bout n'affiche : Olvid avec un daemon, flux avec
// la GéoIP).
func TestGabaritsAvecDonnees(t *testing.T) {
	g, err := chargerGabarits()
	if err != nil {
		t.Fatal(err)
	}
	qr, _ := ImageQR("https://invitation.olvid.io/#AAAA")
	cas := map[string]any{
		"panneau-modifier": donneesPanneau{T: tableauxDepart["serveurs-linux"], Position: "0.0", Unites: unitesProposees, Largeurs: largeursProposees,
			P: Panneau{Type: "courbe", Titre: "x", Unite: "inconnue"}, Requetes: []ligneRequete{{"A", "up", ""}},
			Apercu: &vuePanneau{P: &Panneau{Type: "courbe", Titre: "x"}, Courbe: "apercu", CourbeJSON: `{"x":[1],"series":[]}`}},
		"olvid-installer": donneesInstallOlvid{Adresse: adresseOlvidDefaut, Nom: "mirador", Actif: true},
		"olvid": donneesOlvid{Lien: "https://invitation.olvid.io/#AAAA", QR: qr, DiscussionAct: 7,
			Invitations: []notif.InvitationOlvid{{ID: 1, Nom: "Alice <b>", Etat: "attend", Accepter: true}, {ID: 2, Nom: "Bob", SAS: "1234", SaisirSAS: true}},
			Contacts:    []notif.ContactOlvid{{ID: 5, Nom: "Moi", TeteATete: true}},
			Discussions: []notif.DiscussionOlvid{{ID: 7, Titre: "Moi"}, {ID: 9, Titre: "Groupe"}}},
		"flux": donneesFlux{Total: 3, IPs: 2, GeoIP: true, Pays: 1, ParPays: []compte{{"FR", 3}}, Carte: carteFlux(map[string]int{"FR": 3, "CN": 900}),
			Derniers: []flux{{T: 1, Machine: "vm", IP: "192.0.2.1", Pays: "FR", Port: "22", Proto: "TCP"}}},
	}
	for nom, d := range cas {
		w := httptest.NewRecorder()
		g.executer(w, 200, nom, Page{Admin: true, CSRF: "x", D: d})
		corps := w.Body.String()
		if w.Code != 200 || strings.Contains(corps, "erreur d'affichage") || strings.Contains(corps, "ZgotmplZ") {
			t.Errorf("%s : %d\n%s", nom, w.Code, corps)
		}
		if nom == "olvid" && (!strings.Contains(corps, "1234") || strings.Contains(corps, "Alice <b>") || !strings.Contains(corps, "data:image/png")) {
			t.Error("page Olvid : code SAS, échappement ou QR code")
		}
		if nom == "flux" && !strings.Contains(corps, "🇫🇷") {
			t.Error("drapeau absent")
		}
	}
}
