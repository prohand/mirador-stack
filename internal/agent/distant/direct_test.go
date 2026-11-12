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
	"testing"
	"time"

	"mirador-stack/internal/agent/config"
	"mirador-stack/internal/protocole"
)

// Configuration en temps réel (SPEC § 22), côté agent.
func TestRecupererEnDirect(t *testing.T) {
	var recu http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recu = r.Header.Clone()
		w.Header().Set(protocole.EnteteAttente, "1")
		w.Header().Set(protocole.EnteteActualiser, "42")
		w.WriteHeader(http.StatusNotModified)
	}))
	defer srv.Close()
	r := &Recuperateur{URL: srv.URL, Token: "t", Client: srv.Client(), Attendre: 40 * time.Second, Actualise: 7}
	if _, err := r.Recuperer(context.Background()); err != ErrInchange {
		t.Fatal(err)
	}
	if recu.Get(protocole.EnteteAttendre) != "40" || recu.Get(protocole.EnteteActualise) != "7" {
		t.Errorf("en-têtes envoyés : %v", recu)
	}
	if !r.Attente || r.Actualiser != 42 {
		t.Errorf("réponse lue : attente %v, actualiser %d", r.Attente, r.Actualiser)
	}
}

func TestActualiserRacine(t *testing.T) {
	chemin := filepath.Join(t.TempDir(), "actualiser-racine")
	l := &Lien{Locale: config.Agent{FichierActualiser: chemin}, Journal: slog.New(slog.NewTextHandler(io.Discard, nil))}
	l.actualiserRacine()
	st1, err := os.Stat(chemin)
	if err != nil {
		t.Fatal("fichier signal non écrit")
	}
	// Deuxième demande dans la minute : rien (au plus une relance par minute).
	_ = os.Remove(chemin)
	l.actualiserRacine()
	if _, err := os.Stat(chemin); err == nil {
		t.Error("relance acceptée deux fois dans la minute")
	}
	if st1.Mode().Perm() != 0o600 {
		t.Errorf("droits %v", st1.Mode().Perm())
	}
}
