// SPDX-License-Identifier: GPL-3.0-or-later

package envoi

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestFileBornee(t *testing.T) {
	f := NewFile(10)
	f.Ajouter([]byte("aaaa")) // 4
	f.Ajouter([]byte("bbbb")) // 8
	f.Ajouter([]byte("cccc")) // 12 > 10 : « aaaa » est jeté
	lots, octets, jetes := f.Etat()
	if lots != 2 || octets != 8 || jetes != 1 {
		t.Fatalf("état = %d lots, %d octets, %d jetés", lots, octets, jetes)
	}
	if string(f.Premier()) != "bbbb" {
		t.Errorf("le plus ancien restant devrait être bbbb : %q", f.Premier())
	}
	// Un lot plus gros que toute la file est refusé sans vider la file.
	if f.Ajouter(make([]byte, 11)) {
		t.Error("lot trop gros accepté")
	}
	if lots, _, _ := f.Etat(); lots != 2 {
		t.Error("un lot refusé ne doit pas vider la file")
	}
}

func TestRetirerApresEviction(t *testing.T) {
	f := NewFile(8)
	premier := []byte("aaaa")
	f.Ajouter(premier)
	envoiEnCours := f.Premier()
	// Pendant l'envoi, deux lots arrivent et poussent « aaaa » dehors.
	f.Ajouter([]byte("bbbb"))
	f.Ajouter([]byte("cccc"))
	f.Retirer(envoiEnCours) // ne doit PAS retirer « bbbb »
	if string(f.Premier()) != "bbbb" {
		t.Errorf("Retirer a enlevé le mauvais lot : premier = %q", f.Premier())
	}
	if string(envoiEnCours) != "aaaa" {
		t.Error("le lot en cours d'envoi a été modifié")
	}
}

// serveurTest compte les requêtes et répond avec les codes de la liste.
type serveurTest struct {
	mu     sync.Mutex
	codes  []int
	recus  [][]byte
	auth   []string
	heures []string
	appels atomic.Int32
}

func (s *serveurTest) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.appels.Add(1)
	corps, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	defer s.mu.Unlock()
	code := http.StatusNoContent
	if len(s.codes) > 0 {
		code, s.codes = s.codes[0], s.codes[1:]
	}
	if code < 300 {
		s.recus = append(s.recus, corps)
	}
	s.auth = append(s.auth, r.Header.Get("Authorization"))
	s.heures = append(s.heures, r.Header.Get("X-Mirador-Heure"))
	w.WriteHeader(code)
}

func lancer(t *testing.T, srv *serveurTest, f *File) (context.CancelFunc, *Expediteur) {
	t.Helper()
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	e := NewExpediteur(f, Options{
		URL:        ts.URL,
		Token:      "jeton-de-test-0123456789",
		Entetes:    map[string]string{"Content-Encoding": "snappy"},
		AttenteMin: 5 * time.Millisecond,
		AttenteMax: 20 * time.Millisecond,
		Journal:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	ctx, annuler := context.WithCancel(context.Background())
	go e.Tourner(ctx)
	return annuler, e
}

// attendre patiente jusqu'à ce que cond soit vraie (2 s maximum).
func attendre(t *testing.T, cond func() bool) {
	t.Helper()
	fin := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(fin) {
			t.Fatal("délai dépassé")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestEnvoiDansLOrdreAvecReprise(t *testing.T) {
	// Le serveur est en panne (503) deux fois, puis répond.
	srv := &serveurTest{codes: []int{503, 503}}
	f := NewFile(1 << 20)
	annuler, e := lancer(t, srv, f)
	defer annuler()

	f.Ajouter([]byte("lot1"))
	f.Ajouter([]byte("lot2"))
	attendre(t, func() bool { return e.Envoyes.Load() == 2 })

	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.recus) != 2 || !bytes.Equal(srv.recus[0], []byte("lot1")) || !bytes.Equal(srv.recus[1], []byte("lot2")) {
		t.Errorf("lots reçus dans le désordre ou manquants : %q", srv.recus)
	}
	if e.Echecs.Load() != 2 {
		t.Errorf("2 échecs attendus, %d", e.Echecs.Load())
	}
	if srv.auth[0] != "Bearer jeton-de-test-0123456789" {
		t.Errorf("en-tête Authorization incorrect : %q", srv.auth[0])
	}
}

func TestLotRejeteAbandonne(t *testing.T) {
	// 400 : le lot est mal formé, il est abandonné et le suivant part.
	srv := &serveurTest{codes: []int{400}}
	f := NewFile(1 << 20)
	annuler, e := lancer(t, srv, f)
	defer annuler()
	f.Ajouter([]byte("mauvais"))
	f.Ajouter([]byte("bon"))
	attendre(t, func() bool { return e.Envoyes.Load() == 1 })
	if e.Rejetes.Load() != 1 {
		t.Errorf("1 rejet attendu, %d", e.Rejetes.Load())
	}
}

func TestLotTropGrosAbandonne(t *testing.T) {
	// 413 : le serveur ne l'acceptera jamais, inutile de bloquer la file.
	srv := &serveurTest{codes: []int{413}}
	f := NewFile(1 << 20)
	annuler, e := lancer(t, srv, f)
	defer annuler()
	f.Ajouter([]byte("énorme"))
	f.Ajouter([]byte("bon"))
	attendre(t, func() bool { return e.Envoyes.Load() == 1 })
	if e.Rejetes.Load() != 1 {
		t.Errorf("1 rejet attendu, %d", e.Rejetes.Load())
	}
	// Heure d'envoi jointe (décalage d'horloge mesuré par le serveur).
	srv.mu.Lock()
	defer srv.mu.Unlock()
	ms, err := strconv.ParseInt(srv.heures[0], 10, 64)
	if err != nil || time.Since(time.UnixMilli(ms)).Abs() > time.Minute {
		t.Errorf("X-Mirador-Heure : %q", srv.heures[0])
	}
}

func TestTokenRefuseOnGarde(t *testing.T) {
	// 403 (token pas encore ajouté dans nginx) : on garde le lot et on réessaie.
	srv := &serveurTest{codes: []int{403, 403, 403}}
	f := NewFile(1 << 20)
	annuler, e := lancer(t, srv, f)
	defer annuler()
	f.Ajouter([]byte("lot"))
	attendre(t, func() bool { return e.Envoyes.Load() == 1 })
	if e.Rejetes.Load() != 0 {
		t.Error("un 403 ne doit pas faire abandonner le lot")
	}
}
