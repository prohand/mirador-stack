// SPDX-License-Identifier: GPL-3.0-or-later

package web

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

func TestIPClient(t *testing.T) {
	proxys := []netip.Prefix{netip.MustParsePrefix("172.30.0.1/32")}
	var vue netip.Addr
	h := IPClient(proxys)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { vue = AdresseClient(r) }))

	cas := []struct {
		nom, remote, xff, attendu string
	}{
		{"client direct", "203.0.113.7:5555", "", "203.0.113.7"},
		{"client direct qui ment", "203.0.113.7:5555", "1.2.3.4", "203.0.113.7"},
		{"via nginx", "172.30.0.1:40000", "198.51.100.9", "198.51.100.9"},
		{"via nginx, en-tête forgé en amont", "172.30.0.1:40000", "1.2.3.4, 198.51.100.9", "198.51.100.9"},
		{"via nginx, en-tête illisible", "172.30.0.1:40000", "n'importe quoi", "172.30.0.1"},
		{"IPv6", "[2001:db8::1]:443", "", "2001:db8::1"},
	}
	for _, c := range cas {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = c.remote
		if c.xff != "" {
			r.Header.Set("X-Forwarded-For", c.xff)
		}
		h.ServeHTTP(httptest.NewRecorder(), r)
		if vue.String() != c.attendu {
			t.Errorf("%s : %s, attendu %s", c.nom, vue, c.attendu)
		}
	}
}

func TestEntetesSecurite(t *testing.T) {
	h := EntetesSecurite(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	for k, v := range map[string]string{
		"X-Frame-Options":        "DENY",
		"X-Content-Type-Options": "nosniff",
		"Referrer-Policy":        "same-origin",
	} {
		if w.Header().Get(k) != v {
			t.Errorf("%s = %q, attendu %q", k, w.Header().Get(k), v)
		}
	}
	c := w.Header().Get("Content-Security-Policy")
	for _, interdit := range []string{"unsafe-inline", "unsafe-eval", "http:", "https:", "*"} {
		if contient(c, interdit) {
			t.Errorf("CSP trop permissive (%s) : %s", interdit, c)
		}
	}
	if !contient(c, "frame-ancestors 'none'") || !contient(c, "default-src 'self'") {
		t.Errorf("CSP incomplète : %s", c)
	}
}

func contient(s, sous string) bool {
	for i := 0; i+len(sous) <= len(s); i++ {
		if s[i:i+len(sous)] == sous {
			return true
		}
	}
	return false
}

func TestServeursLimites(t *testing.T) {
	s := Nouveau(Options{EcouteAgents: "127.0.0.1:0", EcouteInterface: "127.0.0.1:0",
		Journal: journalMuet(), Agents: http.NotFoundHandler(), Interface: http.NotFoundHandler()})
	for _, srv := range []*http.Server{s.agents, s.iface} {
		if srv.ReadHeaderTimeout == 0 || srv.ReadTimeout == 0 || srv.WriteTimeout == 0 || srv.IdleTimeout == 0 {
			t.Error("tous les délais doivent être fixés (clients lents)")
		}
		if srv.MaxHeaderBytes > 64<<10 {
			t.Error("taille des en-têtes non bornée")
		}
	}
}

func journalMuet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
