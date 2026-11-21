// SPDX-License-Identifier: GPL-3.0-or-later

package distant

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"mirador-stack/internal/agent/config"
	"mirador-stack/internal/protocole"
)

// Direct (SPEC § 97), côté agent : l'en-tête de la configuration lance les
// relevés ; ils partent chaque seconde jusqu'à « 0 » ; l'agent dit au
// serveur qu'il est déjà en direct.
func TestBoucleDirect(t *testing.T) {
	var envois atomic.Int32
	var actif atomic.Value
	actif.Store("")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case protocole.CheminConfig:
			actif.Store(r.Header.Get(protocole.EnteteDirectActif))
			w.Header().Set(protocole.EnteteDirect, "30")
			w.WriteHeader(http.StatusNotModified)
		case protocole.CheminDirect:
			var d protocole.Direct
			if err := json.NewDecoder(r.Body).Decode(&d); err != nil || d.CPU != 12.5 || r.Header.Get("Authorization") != "Bearer t" {
				t.Errorf("relevé reçu : %+v, %v", d, err)
			}
			reste := "5"
			if envois.Add(1) >= 3 {
				reste = "0" // plus personne ne regarde
			}
			w.Header().Set(protocole.EnteteDirect, reste)
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()
	l := &Lien{Cfg: config.Agent{Serveur: srv.URL}, Token: "t", Client: srv.Client(), Journal: slog.New(slog.NewTextHandler(io.Discard, nil)),
		releveDirect: func(time.Time) protocole.Direct { return protocole.Direct{CPU: 12.5} }}
	r := &Recuperateur{URL: srv.URL + protocole.CheminConfig, Token: "t", Client: srv.Client(), DirectActif: l.enDirect.Load}
	if _, err := r.Recuperer(context.Background()); err != ErrInchange || r.Direct != 30 {
		t.Fatalf("direct lu : %d (%v)", r.Direct, err)
	}
	l.enDirect.Store(true)
	fini := make(chan struct{})
	go func() { l.boucleDirect(context.Background()); close(fini) }()
	_, _ = r.Recuperer(context.Background())
	if actif.Load() != "1" {
		t.Error("l'agent en direct ne le dit pas au serveur")
	}
	select {
	case <-fini:
	case <-time.After(10 * time.Second):
		t.Fatal("le direct ne s'arrête pas sur « 0 »")
	}
	if n := envois.Load(); n != 3 || l.enDirect.Load() {
		t.Errorf("%d relevés, en direct : %v", n, l.enDirect.Load())
	}
}

// Serveur qui ne connaît pas le direct (404) : arrêt après 3 essais.
func TestBoucleDirectAncienServeur(t *testing.T) {
	var envois atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		envois.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	l := &Lien{Cfg: config.Agent{Serveur: srv.URL}, Token: "t", Client: srv.Client(), Journal: slog.New(slog.NewTextHandler(io.Discard, nil)),
		releveDirect: func(time.Time) protocole.Direct { return protocole.Direct{} }}
	l.enDirect.Store(true)
	l.boucleDirect(context.Background())
	if n := envois.Load(); n != 3 || l.enDirect.Load() {
		t.Errorf("%d essais (3 attendus avant l'arrêt)", n)
	}
}

// Relecture du 21/11 : le délai d'essai d'une mise à jour est compté depuis
// son début, pas depuis le dernier redémarrage.
func TestResteEssai(t *testing.T) {
	debut := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		maintenant time.Time
		attendu    time.Duration
	}{
		{debut.Add(3 * time.Minute), 7 * time.Minute},
		{debut.Add(12 * time.Minute), 0},
		{debut.Add(-time.Hour), 10 * time.Minute}, // horloge reculée : au plus le délai
	} {
		if r := resteEssai(debut, 10*time.Minute, c.maintenant); r != c.attendu {
			t.Errorf("%v : %v, attendu %v", c.maintenant.Sub(debut), r, c.attendu)
		}
	}
	if resteEssai(time.Time{}, 10*time.Minute, debut) != 10*time.Minute {
		t.Error("essai sans date : délai complet attendu")
	}
}
