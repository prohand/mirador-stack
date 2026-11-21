// SPDX-License-Identifier: GPL-3.0-or-later

package unifi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"mirador-stack/internal/agent/config"
	"mirador-stack/internal/agent/metrics"
)

// Relecture du 21/11 : une seule session pour tous les relevés ; une
// session expirée (401) est rouverte une fois ; fermée à la fin.
func TestSessionGardee(t *testing.T) {
	var logins, logouts, expirer atomic.Int64
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/auth/login":
			logins.Add(1)
			http.SetCookie(w, &http.Cookie{Name: "TOKEN", Value: "x", Path: "/"})
			w.Header().Set("X-Csrf-Token", "c")
			return
		case "/api/auth/logout":
			logouts.Add(1)
			return
		}
		if expirer.CompareAndSwap(1, 0) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"name":"ap","mac":"aa","type":"uap","state":1}]}`))
	}))
	defer srv.Close()
	h := sha256.Sum256(srv.Certificate().Raw)
	c := Nouveau(config.UniFi{Machine: "udm", Adresse: srv.URL, Utilisateur: "lecture", MotDePasse: "x", Sites: []string{"default"}, Empreinte: hex.EncodeToString(h[:])})
	ctx := context.Background()
	for i := 0; i < 50; i++ {
		if i == 25 {
			expirer.Store(1) // la session expire côté contrôleur
		}
		b := metrics.NewBatch(16, nil)
		b.Reset(0)
		if err := c.Releve(ctx, b); err != nil {
			t.Fatal(i, err)
		}
		if b.Len() == 0 {
			t.Fatal("relevé vide")
		}
	}
	c.Deconnecter(ctx)
	if logins.Load() != 2 || logouts.Load() != 1 {
		t.Errorf("50 relevés : %d connexions (2 attendues : début + session expirée), %d déconnexions", logins.Load(), logouts.Load())
	}
}
