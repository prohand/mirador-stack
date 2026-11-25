// SPDX-License-Identifier: GPL-3.0-or-later

package envoi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// transportCompte : transport réel qui compte les fermetures demandées.
type transportCompte struct {
	*http.Transport
	fermetures atomic.Int32
}

func (t *transportCompte) CloseIdleConnections() {
	t.fermetures.Add(1)
	t.Transport.CloseIdleConnections()
}

func TestDelaiDepasse(t *testing.T) {
	if !DelaiDepasse(context.DeadlineExceeded) || !DelaiDepasse(fmt.Errorf("Post : %w", context.DeadlineExceeded)) {
		t.Error("délai dépassé non reconnu")
	}
	// Délai de lecture du réseau (net.Error.Timeout), comme sur ansible.
	if !DelaiDepasse(&net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded}) {
		t.Error("délai réseau non reconnu")
	}
	if DelaiDepasse(nil) || DelaiDepasse(errors.New("HTTP 502")) || DelaiDepasse(&net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}) {
		t.Error("refus pris pour un délai dépassé")
	}
}

// Fermeture après 2 délais dépassés d'affilée ; un succès ou un refus remet
// le compte à zéro.
func TestRenouveleur(t *testing.T) {
	tr := &transportCompte{Transport: TransportServeur()}
	c := &http.Client{Transport: tr}
	var r Renouveleur
	d := context.DeadlineExceeded
	for i, c2 := range []struct {
		err     error
		renouv  bool
		fermees int32
	}{
		{d, false, 0}, {nil, false, 0}, {d, false, 0}, {errors.New("HTTP 502"), false, 0},
		{d, false, 0}, {d, true, 1}, {d, false, 1}, {d, true, 2},
	} {
		if got := r.Noter(c, c2.err); got != c2.renouv || tr.fermetures.Load() != c2.fermees {
			t.Fatalf("étape %d : renouvelé %v, fermetures %d", i, got, tr.fermetures.Load())
		}
	}
}

// Le transport vérifie les connexions HTTP/2 (ping).
func TestTransportServeurPing(t *testing.T) {
	tr := TransportServeur()
	if tr.HTTP2 == nil || tr.HTTP2.SendPingTimeout != pingApres || tr.HTTP2.PingTimeout != pingSansReponse || !tr.ForceAttemptHTTP2 ||
		tr.TLSClientConfig == nil || tr.TLSClientConfig.MinVersion < 0x0303 {
		t.Errorf("transport : %+v", tr)
	}
}

// Retour du 25/11 (ansible) : le serveur ne répond plus sur la connexion
// ouverte ; après 2 délais dépassés, l'expéditeur ferme ses connexions, le
// dit au journal, et le lot finit par passer.
func TestExpediteurRenouvelleConnexion(t *testing.T) {
	var appels atomic.Int32
	bloque := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if appels.Add(1) <= 2 {
			select { // pas de réponse : le client abandonne
			case <-r.Context().Done():
			case <-bloque:
			}
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ts.Close()
	defer close(bloque)
	tr := &transportCompte{Transport: TransportServeur()}
	var journal bytes.Buffer
	var mu sync.Mutex
	f := NewFile(1 << 20)
	e := NewExpediteur(f, Options{
		URL: ts.URL, Token: "t", Delai: 150 * time.Millisecond,
		AttenteMin: 5 * time.Millisecond, AttenteMax: 20 * time.Millisecond,
		Client:  &http.Client{Transport: tr},
		Journal: slog.New(slog.NewTextHandler(&ecrivainSur{w: &journal, mu: &mu}, nil)),
	})
	ctx, annuler := context.WithCancel(context.Background())
	defer annuler()
	go e.Tourner(ctx)
	f.Ajouter([]byte("lot"))
	attendre(t, func() bool { return e.Envoyes.Load() == 1 })
	if tr.fermetures.Load() != 1 || e.Echecs.Load() != 2 {
		t.Errorf("fermetures %d (1 attendue), échecs %d", tr.fermetures.Load(), e.Echecs.Load())
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(journal.String(), "connexion renouvelée") {
		t.Errorf("journal : %s", journal.String())
	}
}

// ecrivainSur : journal lu par le test pendant que l'expéditeur écrit.
type ecrivainSur struct {
	w  io.Writer
	mu *sync.Mutex
}

func (e *ecrivainSur) Write(p []byte) (int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.w.Write(p)
}
