// SPDX-License-Identifier: GPL-3.0-or-later

package distant

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"mirador-stack/internal/agent/config"
	"mirador-stack/internal/protocole"
)

// Relecture du 21/11 : serveur (bogué, ou mandataire qui retire l'ETag) qui renvoie une
// configuration avec « recuperation » hors bornes : refusée, mais le rythme
// n'était plus suivi d'aucune attente.
func TestRythmeConfigHorsBornes(t *testing.T) {
	var demandes atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if strings.HasSuffix(r.URL.Path, strings.TrimPrefix(protocole.CheminConfig, "")) && r.Method == http.MethodGet {
			demandes.Add(1)
			_, _ = io.WriteString(w, `{"recuperation":"0s"}`)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	cfg := config.Defaut()
	cfg.Serveur = srv.URL
	l := &Lien{Cfg: cfg, Locale: cfg, Token: "x", Client: srv.Client(), Journal: slog.New(slog.DiscardHandler),
		Redemarrer: func(string) {}}
	l.ReglerRythmes(time.Hour, 5*time.Minute, time.Hour, time.Hour, time.Nanosecond)
	ctx, fin := context.WithTimeout(context.Background(), time.Second)
	defer fin()
	l.Tourner(ctx)
	if n := demandes.Load(); n > 3 {
		t.Errorf("%d demandes de configuration en 1 s (rythme normal : 1 toutes les 5 min)", n)
	}
}
