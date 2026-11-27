// SPDX-License-Identifier: GPL-3.0-or-later

package domaines

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mirador-stack/internal/server/base"
	essais "mirador-stack/internal/sondes"
)

func TestPrincipal(t *testing.T) {
	for h, attendu := range map[string]string{
		"cloud.example.org": "example.org", "Example.ORG.": "example.org", "a.b.example.co.uk": "example.co.uk",
		"192.0.2.10": "", "nas": "", "nas.lan": "", "box.home": "", "co.uk": "", "2001:db8::1": "",
	} {
		if p := Principal(h); p != attendu {
			t.Errorf("%s → %q, attendu %q", h, p, attendu)
		}
	}
	g := Regrouper([]essais.Sonde{
		{Type: essais.HTTP, Cible: "https://cloud.example.org/status.php"},
		{Type: essais.HTTP, Cible: "https://pass.example.org/"},
		{Type: essais.TCP, Cible: "example.org:22"},
		{Type: essais.Ping, Cible: "192.0.2.1"},
		{Type: essais.DNS, Cible: "192.0.2.53", Question: "www.example.net"},
		{Type: essais.HTTP, Cible: "https://nas.lan:5001/"},
	})
	if len(g) != 2 || strings.Join(g["example.org"], ",") != "@,cloud,pass" || strings.Join(g["example.net"], ",") != "www" {
		t.Fatalf("regroupement : %v", g)
	}
}

// Annuaire IANA et registre simulés (https) : date lue, un domaine par
// ligne, relu après 24 h, erreur gardée avec la dernière date connue.
func TestSuivi(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := base.Ouvrir(filepath.Join(dir, "mirador.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := base.Migrer(ctx, db, dir); err != nil {
		t.Fatal(err)
	}
	appels := map[string]int{}
	panne := false
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		appels[r.URL.Path]++
		switch r.URL.Path {
		case "/dns.json":
			_, _ = w.Write([]byte(`{"services":[[["org","net"],["http://ignore.example/","` + srv.URL + `/rdap/"]]]}`))
		case "/rdap/domain/example.org":
			if panne {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			_, _ = w.Write([]byte(`{"events":[{"eventAction":"registration","eventDate":"2010-01-01T00:00:00Z"},{"eventAction":"expiration","eventDate":"2026-12-21T10:00:00Z"}]}`))
		case "/rdap/domain/example.net":
			_, _ = w.Write([]byte(`{"events":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	t0 := time.Date(2026, 11, 27, 21, 0, 0, 0, time.UTC)
	maintenant := t0
	ss := []essais.Sonde{{Type: essais.HTTP, Cible: "https://cloud.example.org/"}, {Type: essais.HTTP, Cible: "https://www.example.net/"},
		{Type: essais.HTTP, Cible: "https://x.example.com/"}}
	s := &Suivi{DB: db, Client: srv.Client(), Annuaire: srv.URL + "/dns.json", Maintenant: func() time.Time { return maintenant },
		Sondes: func(context.Context) ([]essais.Sonde, error) { return ss, nil }}
	if err := s.Tour(ctx); err != nil {
		t.Fatal(err)
	}
	ds, _ := Lister(ctx, db)
	if len(ds) != 3 || ds[2].Nom != "example.org" || !ds[2].Expire.Equal(time.Date(2026, 12, 21, 10, 0, 0, 0, time.UTC)) ||
		strings.Join(ds[2].Sous, ",") != "cloud" {
		t.Fatalf("domaines : %+v", ds)
	}
	if ds[0].Erreur != "pas de RDAP pour .com" || ds[1].Erreur != "date d'expiration non publiée par le registre" {
		t.Errorf("erreurs : %q / %q", ds[0].Erreur, ds[1].Erreur)
	}
	// Une heure plus tard : rien n'est relu.
	maintenant = t0.Add(time.Hour)
	_ = s.Tour(ctx)
	if appels["/rdap/domain/example.org"] != 1 || appels["/dns.json"] != 1 {
		t.Errorf("relu trop tôt : %v", appels)
	}
	// 25 h plus tard, registre en panne : la date connue reste.
	maintenant, panne = t0.Add(25*time.Hour), true
	_ = s.Tour(ctx)
	ds, _ = Lister(ctx, db)
	if ds[2].Expire.IsZero() || ds[2].Erreur != "réponse 502" {
		t.Errorf("après une panne : %+v", ds[2])
	}
	// Sonde retirée : domaine oublié.
	ss = ss[:1]
	_ = s.Tour(ctx)
	if ds, _ = Lister(ctx, db); len(ds) != 1 {
		t.Errorf("domaines après retrait : %+v", ds)
	}
}
