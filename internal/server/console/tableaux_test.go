// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"encoding/json"
	"errors"
	"mirador-stack/internal/server/auth"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mirador-stack/internal/server/fluxbloques"
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
		// Dates : millisecondes (Grafana, « * 1000 ») ou secondes.
		{1_781_000_000_000, "dateTimeAsLocal", time.Unix(1_781_000_000, 0).In(fuseau.Load()).Format("02/01/2006 15:04"), nil},
		{1_781_000_000, "dateTimeAsIso", time.Unix(1_781_000_000, 0).In(fuseau.Load()).Format("02/01/2006 15:04"), nil},
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
	qr, _ := auth.ImageQR("https://invitation.olvid.io/#AAAA")
	cas := map[string]any{
		"panneau-modifier": donneesPanneau{T: tableauxDepart["serveurs-linux"], Position: "0.0", Unites: unitesProposees, Largeurs: largeursProposees,
			P: Panneau{Type: "courbe", Titre: "x", Unite: "inconnue"}, Requetes: []ligneRequete{{"A", "up", ""}},
			Apercu: &vuePanneau{P: &Panneau{Type: "courbe", Titre: "x"}, Courbe: "apercu", CourbeJSON: `{"x":[1],"series":[]}`}},
		"olvid-installer": donneesInstallOlvid{Adresse: adresseOlvidDefaut, Nom: "mirador", Actif: true},
		"olvid": donneesOlvid{Lien: "https://invitation.olvid.io/#AAAA", QR: qr, DiscussionAct: 7,
			Invitations: []notif.InvitationOlvid{{ID: 1, Nom: "Alice <b>", Etat: "attend", Accepter: true}, {ID: 2, Nom: "Bob", SAS: "1234", SaisirSAS: true}},
			Contacts:    []notif.ContactOlvid{{ID: 5, Nom: "Moi", TeteATete: true}},
			Discussions: []notif.DiscussionOlvid{{ID: 7, Titre: "Moi"}, {ID: 9, Titre: "Groupe"}}},
		"flux":        donneesFluxExemple("monde"),
		"flux-reseau": donneesFluxExemple("reseau"),
	}
	for nom, d := range cas {
		w := httptest.NewRecorder()
		g.executer(w, 200, strings.TrimSuffix(nom, "-reseau"), Page{Admin: true, CSRF: "x", D: d})
		corps := w.Body.String()
		if w.Code != 200 || strings.Contains(corps, "erreur d'affichage") || strings.Contains(corps, "ZgotmplZ") {
			t.Errorf("%s : %d\n%s", nom, w.Code, corps)
		}
		if nom == "olvid" && (!strings.Contains(corps, "1234") || strings.Contains(corps, "Alice <b>") || !strings.Contains(corps, "data:image/png")) {
			t.Error("page Olvid : code SAS, échappement ou QR code")
		}
		if nom == "flux" && (!strings.Contains(corps, "🇫🇷") || !strings.Contains(corps, "45.143.203.10") || !strings.Contains(corps, `data-code="CN"`) ||
			!strings.Contains(corps, "38\u202f214") || !strings.Contains(corps, "−8 % / hier") || strings.Contains(corps, "Sources OPNsense") || !strings.Contains(corps, "voir Mon réseau › Sources") || !strings.Contains(corps, `class="bulle"`) || !strings.Contains(corps, ">30k<") ||
			!strings.Contains(corps, "Top pays")) {
			t.Errorf("page flux (monde) incomplète :\n%s", corps)
		}
		// Classements (SPEC § 139) : protocole, outil, couleur du pare-feu, part, alerte.
		if nom == "flux" {
			for _, attendu := range []string{"<h2>Classements</h2>", `<span class="pr TCP">TCP</span><code>22</code><small>ssh</small>`,
				`<span class="multi">2 pare-feu</span>`, `<small>78 %</small>`, `class="cc-alerte"`, `href="/logs?mots=45.143.203.10&amp;periode=24h"`, `class="cc-l premier"`} {
				if !strings.Contains(corps, attendu) {
					t.Errorf("classements : %q absent", attendu)
				}
			}
			// Retour du 27/11 : chaque vue a ses colonnes (pas de doublon).
			if strings.Contains(corps, "<h3>Bloqué par</h3>") || strings.Contains(corps, "<h3>Pays</h3>") {
				t.Error("monde : colonnes de « Mon réseau » présentes")
			}
		}
		if nom == "flux-reseau" {
			for _, attendu := range []string{"<h3>Bloqué par</h3>", "<h3>Pays</h3>", `<span class="pt pf0"></span><b>fw</b><span class="outil">OPNsense</span>`, "Sources OPNsense"} {
				if !strings.Contains(corps, attendu) {
					t.Errorf("réseau : %q absent", attendu)
				}
			}
			if strings.Contains(corps, "<h3>Ports visés</h3>") || strings.Contains(corps, "<h3>Adresses insistantes</h3>") {
				t.Error("réseau : colonnes de « Monde » présentes")
			}
		}
		if nom == "flux-reseau" && strings.Contains(corps, `class="cc-alerte"`) {
			t.Error("vue réseau : l'alerte est déjà à côté des chemins")
		}
		if nom == "flux-reseau" && (!strings.Contains(corps, `fill="url(#fd-n5-pf0)"`) || !strings.Contains(corps, `href="/flux-bloques?pf=fw&amp;vue=reseau"`) ||
			!strings.Contains(corps, "a été bloquée par 2 pare-feu") || !strings.Contains(corps, "Chemins les plus fréquents") ||
			!strings.Contains(corps, `<tspan class="outil">OPNsense</tspan>`) || !strings.Contains(corps, "78 %")) {
			t.Errorf("page flux (réseau) incomplète :\n%s", corps)
		}
	}
}

// Requête annulée (page quittée pendant le calcul) : pas gardée en cache ;
// la requête suivante est recalculée (retour du 20/11 : « Context canceled »
// sur tout le tableau Proxmox VE).
func TestCacheSansAnnulation(t *testing.T) {
	b := nouveauBanc(t)
	b.metriquesSimulees(t, []serieTest{{[]string{"__name__", "up", "instance", "pve1", "job", "node"}, constante(1)}})
	annule, fin := context.WithCancel(context.Background())
	fin()
	if _, err := b.c.instant(annule, `up`, b.h.maintenant()); err == nil {
		t.Fatal("requête annulée sans erreur")
	}
	ech, err := b.c.instant(context.Background(), `up`, b.h.maintenant())
	if err != nil || len(ech) != 1 {
		t.Fatalf("après une annulation : %v %+v", err, ech)
	}
	if !requeteAnnulee(context.Background(), errors.New("query was canceled in query execution")) || requeteAnnulee(context.Background(), errors.New("parse error")) {
		t.Error("détection de l'annulation")
	}
}

// Relecture du 21/11 : durées énormes ou négatives, décimales bornées,
// import Grafana avec un nom de fichier quelconque.
func TestRelectureTableaux(t *testing.T) {
	if s := formater(1e10, "s", nil); strings.Contains(s, "-") || !strings.HasSuffix(s, " j") {
		t.Errorf("1e10 s : %q", s)
	}
	if s := dureeLisible(-time.Hour); s != "-1 h 00" {
		t.Errorf("-1 h : %q", s)
	}
	d := 100000000
	tb := &Tableau{Version: 1, Nom: "essai", Titre: "Essai", Periode: "6h", Rangees: []Rangee{{Titre: "r", Panneaux: []Panneau{
		{Type: PanneauChiffre, Titre: "p", Largeur: 3, Decimales: &d, Requetes: []Requete{{ID: "A", Expr: "up"}}}}}}}
	if err := tb.Valider(); err == nil {
		t.Error("100 000 000 décimales acceptées")
	}
	g := `{"title":"Node Exporter Full","time":{"from":"now-90d"},"panels":[{"type":"stat","title":"Up","gridPos":{"w":6},"targets":[{"expr":"up","refId":"A"}]}]}`
	tg, err := DepuisGrafana("Node Exporter Full-1700000000000", []byte(g))
	if err != nil {
		t.Fatalf("import Grafana refusé : %v", err)
	}
	if tg.Periode != "31d" {
		t.Errorf("période : %q", tg.Periode)
	}
}

// donneesFluxExemple : page Flux bloqués remplie (sans magasin).
func donneesFluxExemple(vue string) donneesFlux {
	b := fluxbloques.Bilan{Total: 38214, Avant: 41537, AvantConnu: true, NbIP: 2, NbPays: 2, Pic: -1,
		Pays:        []fluxbloques.Compte{{Nom: "CN", N: 30000}, {Nom: "FR", N: 8214}},
		Ports:       []fluxbloques.Compte{{Nom: "tcp/22", N: 30000}, {Nom: "tcp/443", N: 8214}},
		PareFeux:    []fluxbloques.Compte{{Nom: "fw", N: 38000}, {Nom: "vps", N: 214}},
		IPs:         []fluxbloques.Compte{{Nom: "45.143.203.10", N: 300, PareFeux: 2}},
		PaysPareFeu: map[fluxbloques.Paire]int{{A: "CN", B: "fw"}: 30000, {A: "FR", B: "fw"}: 8000, {A: "FR", B: "vps"}: 214},
		PareFeuPort: map[fluxbloques.Paire]int{{A: "fw", B: "tcp/22"}: 30000, {A: "fw", B: "tcp/443"}: 8000, {A: "vps", B: "tcp/443"}: 214},
		Barres:      []fluxbloques.Barre{{N: 10}, {N: 400}, {N: 12}, {N: 9}}, LargeurBarre: time.Hour}
	v := url.Values{"vue": {vue}}
	d := donneesFlux{Vue: vue, Periode: "24h", LienReseau: "/flux-bloques?vue=reseau", NomPeriode: "24 h", B: b, GeoIP: true, Carte: carteFlux(map[string]int{"FR": 3, "CN": 900}),
		Maison: centresPays["FR"], Centres: centresJSON, Comparaison: "−8 % / hier", ComparaisonClasse: "moins",
		Fil:       []ligneFil{{Heure: "19:42:07", Pays: "FR", IP: "45.143.203.10", PareFeu: "fw", Port: "tcp/22", Service: "ssh", N: 3}},
		Sankey:    construireSankey(b, v, map[string]string{"fw": "OPNsense"}, nil),
		Chemins:   cheminsVue([]fluxbloques.Chemin{{Pays: "CN", PareFeu: "fw", Port: "tcp/22", N: 4120}}, v),
		TableSans: 1, Table: []ligneSource{{Machine: "vps", Classe: "erreur"}},
		Sources: []sourceFlux{{Collecteur: "collecteur-lan", Texte: "aucune ligne filterlog reçue depuis 1 h", Classe: "attention"}},
		Note:    "45.143.203.10 a été bloquée par 2 pare-feu sur la période : la même adresse essaie plusieurs de vos machines."}
	d.Pays = rangsFlux(b.Pays, 6, func(r *rangFlux) { r.Code, r.Nom = r.Nom, nomPaysOuCode(r.Nom) })
	d.AlerteIP, d.AlertePF = "45.143.203.10", 2
	d.Classement = colonnesDeVue((&Console{}).classementFlux(b, v, map[string]string{"fw": "OPNsense"}, nil, false, false), vue)
	d.Barres, d.Axe, d.PicTexte = barresFlux(b)
	d.Bord, d.Grille, d.Bulles, d.TopPays = bordCarte, grilleCarte, bullesFlux(b.Pays), d.Pays
	return d
}
