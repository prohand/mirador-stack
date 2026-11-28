// SPDX-License-Identifier: GPL-3.0-or-later

package checks

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mirador-stack/internal/agent/config"
)

func checkHTTPContient(nom, contient string) config.Check {
	return config.Check{Type: "http", Nom: nom, Cible: "http://nas.lan/", Contient: contient}
}

// Revue du 28/11 (M4) : un « contient » nouveau ou changé moins d'une
// heure après le précédent changement est mis de côté.
func TestFiltrerContient(t *testing.T) {
	t0 := time.Unix(1_800_000_000, 0)
	a, b := checkHTTPContient("a", "x"), checkHTTPContient("b", "y")
	sans := config.Check{Type: "tcp", Nom: "t", Cible: "nas.lan:22"}
	cas := []struct {
		nom        string
		recus      []config.Check
		etat       etatContient
		maintenant time.Time
		actifs     []bool
		contients  []string
		le         int64
		attente    bool
	}{
		{"premier démarrage : accepté", []config.Check{a, sans}, etatContient{}, t0,
			[]bool{true, true}, []string{"x", ""}, t0.Unix(), false},
		{"rien de changé", []config.Check{a, sans}, etatContient{Le: t0.Unix(), Valeurs: map[string]string{cleContient(a): "x"}}, t0.Add(time.Minute),
			[]bool{true, true}, []string{"x", ""}, t0.Unix(), false},
		{"texte changé trop tôt : ancien gardé", []config.Check{checkHTTPContient("a", "z")},
			etatContient{Le: t0.Unix(), Valeurs: map[string]string{cleContient(a): "x"}}, t0.Add(10 * time.Minute),
			[]bool{true}, []string{"x"}, t0.Unix(), true},
		{"check nouveau trop tôt : il attend", []config.Check{a, b},
			etatContient{Le: t0.Unix(), Valeurs: map[string]string{cleContient(a): "x"}}, t0.Add(59 * time.Minute),
			[]bool{true, false}, []string{"x", "y"}, t0.Unix(), true},
		{"après une heure : accepté", []config.Check{checkHTTPContient("a", "z"), b},
			etatContient{Le: t0.Unix(), Valeurs: map[string]string{cleContient(a): "x"}}, t0.Add(time.Hour),
			[]bool{true, true}, []string{"z", "y"}, t0.Add(time.Hour).Unix(), false},
		{"cible changée = changement", []config.Check{{Type: "http", Nom: "a", Cible: "http://169.254.169.254/", Contient: "x"}},
			etatContient{Le: t0.Unix(), Valeurs: map[string]string{cleContient(a): "x"}}, t0.Add(time.Minute),
			[]bool{false}, []string{"x"}, t0.Unix(), true},
		{"retrait : pas un changement", []config.Check{sans},
			etatContient{Le: t0.Unix(), Valeurs: map[string]string{cleContient(a): "x"}}, t0.Add(time.Minute),
			[]bool{true}, []string{""}, t0.Unix(), false},
		{"horloge revenue en arrière", []config.Check{checkHTTPContient("a", "z")},
			etatContient{Le: t0.Add(48 * time.Hour).Unix(), Valeurs: map[string]string{cleContient(a): "x"}}, t0,
			[]bool{true}, []string{"z"}, t0.Unix(), false},
	}
	for _, c := range cas {
		eff, actifs, nouvel, revoir := filtrerContient(c.recus, c.etat, c.maintenant)
		for i := range c.recus {
			if actifs[i] != c.actifs[i] || eff[i].Contient != c.contients[i] {
				t.Errorf("%s : check %d actif=%v contient=%q", c.nom, i, actifs[i], eff[i].Contient)
			}
		}
		if nouvel.Le != c.le || revoir.IsZero() == c.attente {
			t.Errorf("%s : le=%d revoir=%v", c.nom, nouvel.Le, revoir)
		}
	}
}

// L'état survit au redémarrage de l'agent (une configuration reçue =
// un redémarrage) : un deuxième changement juste après reste de côté.
func TestContientPersistant(t *testing.T) {
	fichier := filepath.Join(t.TempDir(), "checks-contient.json")
	o := Options{FichierContient: fichier}
	m := NouveauAvec([]config.Check{checkHTTPContient("a", "x")}, "/proc", o)
	if !m.actifs[0] || m.eff[0].Contient != "x" {
		t.Fatalf("premier : %+v %v", m.eff, m.actifs)
	}
	// « Redémarrage » avec un autre texte : l'ancien reste en service.
	m = NouveauAvec([]config.Check{checkHTTPContient("a", "secret-1")}, "/proc", o)
	if !m.actifs[0] || m.eff[0].Contient != "x" || m.revoir.IsZero() {
		t.Fatalf("changement trop tôt accepté : %+v %v %v", m.eff, m.actifs, m.revoir)
	}
	if m.checks[0].Contient != "secret-1" {
		t.Error("liste reçue modifiée (étiquettes)")
	}
	// Une heure plus tard, la tâche de fond accepte le changement.
	m.appliquerContient(time.Now().Add(time.Hour + time.Second))
	if m.eff[0].Contient != "secret-1" || !m.revoir.IsZero() {
		t.Fatalf("changement non accepté après une heure : %+v", m.eff)
	}
}

// MIRADOR_CHECKS_CONTIENT=non : checks avec « contient » jamais faits.
func TestSansContient(t *testing.T) {
	lu, err := config.Lire(strings.NewReader("MIRADOR_NOM=pc\nMIRADOR_SERVEUR=https://mirador.exemple.org\nMIRADOR_CHECKS_CONTIENT=non\n"))
	if err != nil {
		t.Fatal(err)
	}
	if lu.ChecksContient || !config.Defaut().ChecksContient {
		t.Fatal("MIRADOR_CHECKS_CONTIENT : lu ou défaut faux")
	}
	m := NouveauAvec([]config.Check{checkHTTPContient("a", "x"), {Type: "tcp", Nom: "t", Cible: "nas.lan:22"}}, "/proc",
		Options{SansContient: true})
	if m.actifs[0] || !m.actifs[1] {
		t.Fatalf("actifs : %v", m.actifs)
	}
}

// Lien local (métadonnées des clouds) toujours refusé, même par une
// redirection ; la machine elle-même et le LAN restent permis.
func TestLienLocalRefuse(t *testing.T) {
	var vus int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		vus++
		if r.URL.Path == "/vers-metadonnees" {
			http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()
	ctx, fin := context.WithTimeout(context.Background(), 3*time.Second)
	defer fin()
	if r := checkHTTP(ctx, config.Check{Type: "http", Nom: "ok", Cible: srv.URL + "/"}); !r.ok {
		t.Fatalf("machine locale refusée : %+v", r)
	}
	if r := checkHTTP(ctx, config.Check{Type: "http", Nom: "redir", Cible: srv.URL + "/vers-metadonnees"}); r.ok {
		t.Fatalf("redirection vers le lien local suivie : %+v", r)
	}
	for _, cible := range []string{"http://169.254.169.254/", "http://[fe80::1]/", "http://[::ffff:169.254.169.254]/"} {
		if r := checkHTTP(ctx, config.Check{Type: "http", Nom: "md", Cible: cible}); r.ok || r.valeur["mirador_check_http_code"] != 0 {
			t.Errorf("%s : %+v", cible, r)
		}
	}
	if err := numeroteur().Control("tcp", "169.254.169.254:80", nil); err == nil {
		t.Error("tcp vers le lien local permis")
	}
	if err := numeroteur().Control("tcp", "192.168.1.10:80", nil); err != nil {
		t.Errorf("LAN refusé : %v", err)
	}
	if r := checkTCP(ctx, config.Check{Type: "tcp", Nom: "md", Cible: "169.254.169.254:80"}); r.ok {
		t.Error("tcp lien local : ok")
	}
}
