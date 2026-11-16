// SPDX-License-Identifier: GPL-3.0-or-later

package flotte

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Flux Atom des Releases : 5 dernières versions, noms invalides ignorés,
// GitHub lent ou absent : erreur rapide (la page reste utilisable).
func TestDernieresVersionsGitHub(t *testing.T) {
	var entrees strings.Builder
	for _, tag := range []string{"v0.3.2", "nightly", "v0.3.1", "0.3.0", "v0.2.9", "v0.2.8", "v0.2.7"} {
		entrees.WriteString(`<entry><updated>2026-11-16T17:00:00Z</updated><link rel="alternate" type="text/html" href="https://github.com/prohand/mirador-stack/releases/tag/` +
			tag + `"/><title>` + tag + `</title></entry>`)
	}
	flux := `<?xml version="1.0" encoding="UTF-8"?><feed xmlns="http://www.w3.org/2005/Atom">` + entrees.String() + `</feed>`
	lent := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/prohand/mirador-stack/releases.atom" {
			http.NotFound(w, r)
			return
		}
		if lent {
			time.Sleep(6 * time.Second)
		}
		_, _ = w.Write([]byte(flux))
	}))
	defer srv.Close()
	src := SourceGitHub{Base: srv.URL, Client: srv.Client()}
	vs, err := DernieresVersionsGitHub(context.Background(), src, "prohand/mirador-stack", 5)
	if err != nil {
		t.Fatal(err)
	}
	var tags []string
	for _, v := range vs {
		tags = append(tags, v.Tag)
	}
	if strings.Join(tags, ",") != "v0.3.2,v0.3.1,v0.3.0,v0.2.9,v0.2.8" || vs[0].Date.IsZero() {
		t.Fatalf("versions : %v", vs)
	}
	if _, err := DernieresVersionsGitHub(context.Background(), src, "../etc", 5); err == nil {
		t.Error("dépôt invalide accepté")
	}
	if _, err := DernieresVersionsGitHub(context.Background(), src, "autre/depot", 5); err == nil {
		t.Error("dépôt absent : erreur attendue")
	}
	lent = true
	t0 := time.Now()
	if _, err := DernieresVersionsGitHub(context.Background(), src, "prohand/mirador-stack", 5); err == nil || time.Since(t0) > 5*time.Second {
		t.Errorf("GitHub lent : %v après %v", err, time.Since(t0))
	}
}
