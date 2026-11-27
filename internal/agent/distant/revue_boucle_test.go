// SPDX-License-Identifier: GPL-3.0-or-later

package distant

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"mirador-stack/internal/agent/config"
)

// Le serveur (flotte/collecteurs.go) envoie « "emetteurs":[] » quand tous
// les émetteurs appartiennent à des équipements détachés. Chaque démarrage
// du collecteur redémarre aussitôt : boucle sans fin.
func TestRevueBoucleRedemarrage(t *testing.T) {
	doc := `{"collecteur":{"equipements":["opnsense"],"syslog":{"ecoute":"192.168.1.10:6514","nom_certificat":"syslog.lan.example.org","emetteurs":[]}}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if strings.HasSuffix(r.URL.Path, "/config") {
			w.Header().Set("ETag", `"x"`)
			_, _ = io.WriteString(w, doc)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	locale := config.DefautCollecteur()
	locale.Nom = "collecteur-lan"
	locale.URLMetriques = "https://ingest.example.org/api/v1/write"
	locale.Serveur = srv.URL
	locale.FichierConfigDistante = filepath.Join(t.TempDir(), "distante.json")
	if err := locale.Valider(); err != nil {
		t.Fatal(err)
	}
	demarrer := func() int32 {
		var redem atomic.Int32
		cfg := ConfigAuDemarrage(locale, slog.New(slog.DiscardHandler))
		l := &Lien{Cfg: cfg, Locale: locale, Token: "x", Client: srv.Client(), Journal: slog.New(slog.DiscardHandler),
			Redemarrer: func(string) { redem.Add(1) }}
		l.ReglerRythmes(time.Hour, time.Hour, time.Hour, time.Hour, time.Nanosecond)
		ctx, fin := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer fin()
		l.Tourner(ctx)
		return redem.Load()
	}
	if n := demarrer(); n != 1 {
		t.Fatalf("1er démarrage : %d redémarrage(s), 1 attendu (nouvelle configuration)", n)
	}
	for i := 2; i <= 4; i++ {
		if n := demarrer(); n != 0 {
			t.Errorf("démarrage %d : configuration INCHANGÉE et pourtant %d redémarrage(s)", i, n)
		}
	}
}
