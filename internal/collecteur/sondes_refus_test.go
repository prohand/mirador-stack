// SPDX-License-Identifier: GPL-3.0-or-later

package collecteur

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"mirador-stack/internal/agent/config"
	"mirador-stack/internal/agent/metrics"
	essais "mirador-stack/internal/sondes"
)

// Revue du 28/11 (M4) : les sondes du collecteur ne joignent jamais la
// boucle locale ni le lien local (métadonnées des clouds), même par une
// redirection ou un nom ; le LAN reste permis.
func TestSondesCollecteurRefus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	o := OptionsSonde(nil)
	ctx := context.Background()
	for _, cible := range []string{srv.URL, strings.Replace(srv.URL, "127.0.0.1", "localhost", 1),
		"http://169.254.169.254/latest/meta-data/", "http://[::ffff:169.254.169.254]/", "http://0.0.0.0:1/"} {
		r := essais.Essayer(ctx, essais.Sonde{Type: essais.HTTP, Cible: cible}, o)
		if r.OK || !strings.Contains(r.Erreur, "refusée") {
			t.Errorf("%s : %+v", cible, r)
		}
	}
	if r := essais.Essayer(ctx, essais.Sonde{Type: essais.TCP, Cible: "169.254.169.254:80"}, o); r.OK || !strings.Contains(r.Erreur, "refusée") {
		t.Errorf("tcp lien local : %+v", r)
	}
	if o.Refuser(net.ParseIP("192.168.1.10")) || o.Refuser(net.ParseIP("10.0.0.3")) {
		t.Error("LAN refusé : c'est le but des sondes du collecteur")
	}
}

// Veto local MIRADOR_SONDES=non : aucune sonde demandée par le serveur
// n'est faite (services ni certificats).
func TestVetoSondes(t *testing.T) {
	var appels atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { appels.Add(1) }))
	defer srv.Close()
	lu, err := config.LireDepuis(strings.NewReader("MIRADOR_NOM=col\nMIRADOR_SERVEUR=https://mirador.exemple.org\nMIRADOR_SONDES=non\n"), config.DefautCollecteur())
	if err != nil {
		t.Fatal(err)
	}
	if lu.SondesActives {
		t.Fatal("MIRADOR_SONDES=non non lu")
	}
	if !config.DefautCollecteur().SondesActives {
		t.Fatal("sondes permises par défaut (rien de cassé)")
	}
	cfg := lu
	cfg.Collecteur.Services = []essais.Sonde{{Nom: "x", Type: essais.HTTP, Cible: srv.URL, Depuis: "lan"}}
	cfg.Collecteur.Sondes = []config.Sonde{{Machine: "nas", Adresse: srv.Listener.Addr().String()}}
	ctx, fin := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer fin()
	var lots atomic.Int32
	decl := &Declencheurs{}
	decl.Canal("x")                // essai demandé tout de suite :
	decl.Declencher([]string{"x"}) // sans le veto, il partirait
	TournerReleves(ctx, cfg, func(*metrics.Batch) { lots.Add(1) }, nil, decl)
	if lots.Load() != 0 || appels.Load() != 0 {
		t.Fatalf("sonde faite malgré le veto : %d lots, %d appels", lots.Load(), appels.Load())
	}
	// Témoin : sans le veto, les sondes partent (lots envoyés).
	cfg.SondesActives = true
	ctx2, fin2 := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer fin2()
	decl = &Declencheurs{}
	decl.Canal("x")
	decl.Declencher([]string{"x"})
	TournerReleves(ctx2, cfg, func(*metrics.Batch) { lots.Add(1) }, nil, decl)
	if lots.Load() == 0 {
		t.Fatal("témoin : aucune sonde faite sans le veto")
	}
}
