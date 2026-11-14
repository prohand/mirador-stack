// SPDX-License-Identifier: GPL-3.0-or-later

package sondes

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mirador-stack/internal/agent/config"
	"mirador-stack/internal/agent/metrics"
)

func TestSonder(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	s := config.Sonde{Machine: "nas", Adresse: strings.TrimPrefix(srv.URL, "https://")}
	r, err := Sonder(context.Background(), s)
	if err != nil || r.Fin.IsZero() || !strings.Contains(r.Emetteur, "Acme") {
		t.Fatalf("sonde : %+v %v", r, err)
	}
	b := metrics.NewBatch(8, nil)
	b.Reset(0)
	Ajouter(b, s, r, err)
	vus := map[string]bool{}
	for _, x := range b.Samples[:b.Len()] {
		vus[x.Name] = true
		if x.Labels[0].Name != "path" || x.Labels[0].Value != s.Adresse {
			t.Errorf("étiquette path : %+v", x.Labels[:x.N])
		}
	}
	if !vus["probe_success"] || !vus["ssl_cert_not_after_timestamp_seconds"] || !vus["probe_ssl_last_chain_info"] {
		t.Errorf("métriques : %v", vus)
	}
	// Port fermé : probe_success à 0.
	srv.Close()
	r, err = Sonder(context.Background(), s)
	b.Reset(0)
	Ajouter(b, s, r, err)
	if err == nil || b.Samples[1].Name != "probe_success" || b.Samples[1].Value != 0 {
		t.Errorf("port fermé : %v %+v", err, b.Samples[:b.Len()])
	}
}
