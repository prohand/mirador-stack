// SPDX-License-Identifier: GPL-3.0-or-later

package distant

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"mirador-stack/internal/protocole"
)

// « Vérifier maintenant » (SPEC § 108) : le collecteur envoie la dernière
// demande faite et lit les nouvelles ; un agent n'envoie rien.
func TestRecuperateurSonde(t *testing.T) {
	var recu []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, avec := r.Header[http.CanonicalHeaderKey(protocole.EnteteSonde)]
		if avec {
			recu = append(recu, r.Header.Get(protocole.EnteteSonde))
			w.Header().Set(protocole.EnteteSonder, protocole.FormerSonder(7, []string{"Box (fibre)"}))
		} else {
			recu = append(recu, "absent")
		}
		w.WriteHeader(http.StatusNotModified)
	}))
	defer srv.Close()
	r := &Recuperateur{URL: srv.URL, Client: srv.Client(), AvecSonde: true, Sonde: 3}
	if _, err := r.Recuperer(context.Background()); err != ErrInchange {
		t.Fatal(err)
	}
	if r.Sonder != 7 || len(r.SonderNoms) != 1 || r.SonderNoms[0] != "Box (fibre)" || recu[0] != "3" {
		t.Fatalf("collecteur : envoyé %q, lu %d %q", recu, r.Sonder, r.SonderNoms)
	}
	agent := &Recuperateur{URL: srv.URL, Client: srv.Client()}
	_, _ = agent.Recuperer(context.Background())
	if recu[1] != "absent" || agent.Sonder != 0 {
		t.Fatalf("agent : %q, %d", recu, agent.Sonder)
	}
}
