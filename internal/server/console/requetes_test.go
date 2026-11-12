// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"mirador-stack/internal/server/auth"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"mirador-stack/internal/server/alertes"
)

func TestAppliquerFiltres(t *testing.T) {
	cas := map[string]string{
		`x{a="1", FILTRES}`:                 `x{a="1"}`,
		`x{FILTRES}`:                        `x`,
		`rate(x{FILTRES}[5m]) / y{FILTRES}`: `rate(x[5m]) / y`,
	}
	for e, v := range cas {
		if r := appliquerFiltres(e, nil); r != v {
			t.Errorf("%s → %s (attendu %s)", e, r, v)
		}
	}
	if r := appliquerFiltres(`x{a="1", FILTRES}`, []string{`instance=~"$instance"`, `site=~"$site"`}); r != `x{a="1", instance=~"$instance", site=~"$site"}` {
		t.Errorf("avec filtres : %s", r)
	}
}

// Chaque requête toute faite est valide, avec ou sans filtres, et son
// panneau passe la vérification complète.
func TestModelesRequetes(t *testing.T) {
	tab := tableauxDepart["serveurs-linux"]
	vus := map[string]bool{}
	for _, m := range modelesRequetes {
		if vus[m.ID] {
			t.Errorf("identifiant en double : %s", m.ID)
		}
		vus[m.ID] = true
		for _, f := range [][]string{nil, filtresTableau(tab)} {
			e := substituer(appliquerFiltres(m.Expr, f), map[string]string{"instance": "", "site": "", "env": ""}, time.Minute, 6*time.Hour)
			if _, err := alertes.AnalyserExpr(e); err != nil {
				t.Errorf("%s : %v\n%s", m.ID, err, e)
			}
		}
		p, err := appliquerModele(tab, Panneau{Type: "courbe", Titre: "x", Requetes: []Requete{{ID: "A"}}}, m.ID, true)
		essai := *tab
		essai.Rangees = []Rangee{{Panneaux: []Panneau{p}}}
		if err != nil || essai.Valider() != nil {
			t.Errorf("%s : panneau refusé : %v %v", m.ID, err, essai.Valider())
		}
	}
	if len(modelesRequetes) < 30 {
		t.Errorf("%d modèles seulement", len(modelesRequetes))
	}
}

func TestExpliquerEtUnite(t *testing.T) {
	cas := []struct{ expr, contient, unite string }{
		{`100 * (1 - avg by (instance) (rate(node_cpu_seconds_total{mode="idle"}[2m])))`, "moyenne par instance", "percent"},
		{`sum by (instance) (rate(node_network_receive_bytes_total[2m]))`, "débit par seconde sur 2 min", "Bps"},
		{`max(node_hwmon_temp_celsius)`, "température", "celsius"},
		{`node_filesystem_avail_bytes{mountpoint="/"}`, "mountpoint = /", "bytes"},
		{`(ssl_cert_not_after_timestamp_seconds - time()) / 86400`, "en jours", "d"},
	}
	for _, c := range cas {
		a, err := alertes.AnalyserExpr(c.expr)
		if err != nil {
			t.Fatal(err)
		}
		if e := expliquer(a); !strings.Contains(e, c.contient) {
			t.Errorf("%s : « %s » sans « %s »", c.expr, e, c.contient)
		}
		if u := proposerUnite(a); u != c.unite {
			t.Errorf("%s : unité %q (attendu %q)", c.expr, u, c.unite)
		}
	}
}

func TestConstruireRequete(t *testing.T) {
	tab := tableauxDepart["serveurs-linux"]
	r, err := construireRequete(tab, "node_network_receive_bytes_total", "debit", "sum", "instance", "device", "eth0", true)
	if err != nil || !strings.HasPrefix(r.Expr, `sum by (instance) (rate(node_network_receive_bytes_total{`) ||
		!strings.Contains(r.Expr, `device="eth0"`) || r.Legende != "{{instance}}" || r.Unite != "Bps" {
		t.Fatalf("%+v %v", r, err)
	}
	if r, _ := construireRequete(tab, "node_load5", "valeur", "max", "", "", "", false); r.Expr != "max(node_load5)" || r.Type != PanneauChiffre {
		t.Errorf("sans regroupement : %+v", r)
	}
	// Rien ne doit pouvoir sortir de la chaîne PromQL.
	for _, mauvais := range [][3]string{{`up} or vector(1) #`, "", ""}, {"up", "device", `x"} or vector(1) #`}, {"up", `a"b`, "x"}} {
		if _, err := construireRequete(tab, mauvais[0], "valeur", "", "", mauvais[1], mauvais[2], false); err == nil {
			t.Errorf("accepté : %v", mauvais)
		}
	}
}

// Points d'entrée : vérification (erreur en français), modèle inséré.
func TestPagesRequetes(t *testing.T) {
	b := nouveauBanc(t)
	c, _ := b.premierAdmin("admin", mdpAdmin)
	csrf := b.csrf(c, "/tableaux")
	b.post(c, "/tableaux", url.Values{"csrf": {csrf}, "action": {"installer"}, "nom": {"serveurs-linux"}})
	_, corps, _ := b.post(c, "/tableaux/serveurs-linux/verifier", url.Values{"csrf": {csrf}, "expr": {"sum(up"}})
	if !strings.Contains(corps, `"ok":false`) || !strings.Contains(corps, "erreur de syntaxe") {
		t.Errorf("vérification : %s", corps)
	}
	if code, _, _ := b.post(c, "/tableaux/serveurs-linux/verifier", url.Values{"expr": {"up"}}); code != http.StatusForbidden {
		t.Errorf("sans jeton CSRF : %d", code)
	}
	_, corps, _ = b.get(c, "/tableaux/serveurs-linux/modifier")
	emp := trouver(t, reEmpreinte, corps)
	code, corps, _ := b.post(c, "/tableaux/serveurs-linux/panneau", url.Values{"csrf": {csrf}, "empreinte": {emp}, "position": {"0.nouveau"},
		"type": {"courbe"}, "titre": {"Nouveau panneau"}, "modele": {"temp-machines"}, "action": {"modele"}})
	if code != http.StatusOK || !strings.Contains(corps, "max by (instance) (node_hwmon_temp_celsius{site=~&#34;$site&#34;") || !strings.Contains(corps, "Température par machine") {
		t.Errorf("modèle non inséré : %d", code)
	}
}

// Carte du monde : pays connus, niveaux 0 à 5 selon le nombre de flux.
func TestCarteFlux(t *testing.T) {
	if len(paysCarte) < 150 || nomsPays["FR"] != "France" {
		t.Fatalf("%d pays, FR = %q", len(paysCarte), nomsPays["FR"])
	}
	c := carteFlux(map[string]int{"FR": 1, "CN": 1000, "ZZ": 5})
	niv := map[string]int{}
	for _, p := range c {
		niv[p.Code] = p.Niveau
		if !strings.HasPrefix(p.D, "M") || strings.ContainsAny(p.D, "<>\"'") {
			t.Fatalf("chemin de %s suspect", p.Code)
		}
	}
	if niv["CN"] != 5 || niv["FR"] < 1 || niv["FR"] > 2 || niv["DE"] != 0 {
		t.Errorf("niveaux : %v", niv)
	}
}

// Volume des logs : point de données (machine vérifiée).
func TestVolumeLogs(t *testing.T) {
	b := nouveauBanc(t)
	c, _ := b.premierAdmin("admin", mdpAdmin)
	if code, corps, _ := b.get(c, `/donnees/volume-logs?periode=24h&instance=a"}`); code != http.StatusBadRequest || !strings.Contains(corps, "machine invalide") {
		t.Errorf("instance piégée : %d %s", code, corps)
	}
	if code, _, _ := b.get(c, "/donnees/volume-logs?periode=6h&instance=pc"); code != http.StatusOK {
		t.Errorf("volume : %d", code)
	}
	if _, corps, _ := b.get(c, "/logs"); !strings.Contains(corps, "/donnees/volume-logs?periode=") {
		t.Error("courbe absente de la page Logs")
	}
}

// Revue de sécurité du 11/11 : code 2FA d'une action sensible essayé avec
// une session volée : 5 codes faux ferment la session ; changement de mot
// de passe verrouillé AVANT le calcul (pas d'oracle).
func TestEssaisSensibles(t *testing.T) {
	b := nouveauBanc(t)
	c, _ := b.premierAdmin("admin", mdpAdmin)
	b.h.avancer(16 * time.Minute)
	csrf := b.csrf(c, "/utilisateurs")
	for i := 0; i < essaisSensiblesMax; i++ {
		b.post(c, "/utilisateurs", url.Values{"csrf": {csrf}, "action": {"creer"}, "nom": {"pirate"}, "role": {"admin"},
			"mot_de_passe": {"une longue phrase pirate"}, "code": {"000000"}})
	}
	if code, _, _ := b.get(c, "/utilisateurs"); code != http.StatusSeeOther {
		t.Fatalf("session encore ouverte après %d codes faux : %d", essaisSensiblesMax, code)
	}
	if u, err := auth.LireUtilisateur(context.Background(), b.db, "pirate"); err == nil {
		t.Fatalf("compte créé : %+v", u)
	}
}
