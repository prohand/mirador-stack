// SPDX-License-Identifier: GPL-3.0-or-later

package domaines

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Revue du 28/11 (B7) : le client RDAP par défaut refuse les adresses
// internes au moment de la connexion (annuaire ou serveur RDAP qui mène
// au serveur lui-même, à un conteneur voisin ou au lien local).
func TestRDAPAdresseInterne(t *testing.T) {
	appels := 0
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		appels++
		_, _ = w.Write([]byte(`{"services":[]}`))
	}))
	defer srv.Close()
	s := &Suivi{Annuaire: srv.URL + "/dns.json"} // Client nil : client par défaut
	for _, adresse := range []string{srv.URL + "/dns.json", strings.Replace(srv.URL, "127.0.0.1", "localhost", 1) + "/x",
		"https://169.254.169.254/latest/meta-data/", "https://[::ffff:10.0.0.1]/"} {
		var v any
		err := s.lireJSON(context.Background(), adresse, &v)
		if err == nil || !strings.Contains(err.Error(), "interne refusée") {
			t.Errorf("%s : %v", adresse, err)
		}
	}
	if appels != 0 {
		t.Errorf("%d requêtes reçues par l'adresse interne", appels)
	}
}
