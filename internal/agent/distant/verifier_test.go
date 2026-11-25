// SPDX-License-Identifier: GPL-3.0-or-later

package distant

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"mirador-stack/internal/protocole"
)

// « Vérifier maintenant » (SPEC § 127) : l'agent envoie la dernière demande
// faite et lit la plus récente ; sans AvecVerif, rien n'est envoyé.
func TestRecuperateurVerifier(t *testing.T) {
	var recu []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, avec := r.Header[http.CanonicalHeaderKey(protocole.EnteteVerifie)]; avec {
			recu = append(recu, r.Header.Get(protocole.EnteteVerifie))
			w.Header().Set(protocole.EnteteVerifier, "9")
		} else {
			recu = append(recu, "absent")
		}
		w.WriteHeader(http.StatusNotModified)
	}))
	defer srv.Close()
	r := &Recuperateur{URL: srv.URL, Client: srv.Client(), AvecVerif: true, Verifie: 4}
	if _, err := r.Recuperer(context.Background()); err != ErrInchange {
		t.Fatal(err)
	}
	if r.Verifier != 9 || recu[0] != "4" {
		t.Fatalf("agent : envoyé %q, lu %d", recu, r.Verifier)
	}
	col := &Recuperateur{URL: srv.URL, Client: srv.Client()}
	_, _ = col.Recuperer(context.Background())
	if recu[1] != "absent" || col.Verifier != 0 {
		t.Fatalf("sans AvecVerif : %q, %d", recu, col.Verifier)
	}
}
