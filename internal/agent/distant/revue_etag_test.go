// SPDX-License-Identifier: GPL-3.0-or-later

package distant

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Écriture de la configuration ratée une fois (disque plein, dossier
// absent…) : l'ETag est déjà retenu, le serveur répond 304 ensuite et le
// changement n'est jamais appliqué.
func TestRevueChangementPerduApresEchecEcriture(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if strings.HasSuffix(r.URL.Path, "/config") {
			if r.Header.Get("If-None-Match") == `"v2"` {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("ETag", `"v2"`)
			_, _ = io.WriteString(w, `{"systemd":false}`)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	cfg := base()
	cfg.Serveur = srv.URL
	dossier := filepath.Join(t.TempDir(), "absent") // panne passagère : dossier pas encore là
	cfg.FichierConfigDistante = filepath.Join(dossier, "distante.json")
	var redem atomic.Int32
	l := &Lien{Cfg: cfg, Locale: cfg, Token: "x", Client: srv.Client(), Journal: slog.New(slog.DiscardHandler),
		Redemarrer: func(string) { redem.Add(1) }}
	l.ReglerRythmes(time.Hour, 50*time.Millisecond, time.Hour, time.Hour, time.Nanosecond)
	ctx, fin := context.WithTimeout(context.Background(), 2*time.Second)
	defer fin()
	go func() { time.Sleep(200 * time.Millisecond); _ = os.MkdirAll(dossier, 0o700) }() // la panne cesse
	l.Tourner(ctx)
	if redem.Load() == 0 {
		t.Fatal("la panne a cessé, mais la nouvelle configuration n'est jamais appliquée (304 sur l'ETag déjà retenu)")
	}
}
