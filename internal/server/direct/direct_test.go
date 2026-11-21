// SPDX-License-Identifier: GPL-3.0-or-later

package direct

import (
	"testing"
	"time"

	"mirador-stack/internal/protocole"
)

// Demande posée par une page, prolongée par les relevés tant qu'elle
// regarde, arrêtée 30 s après son départ ; relevés gardés 5 min.
func TestHub(t *testing.T) {
	h := Nouveau()
	m := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	h.maintenant = func() time.Time { return m }
	if h.Reste(1) != 0 || h.Recevoir(1, protocole.Direct{}) != 0 {
		t.Fatal("relevé accepté sans demande")
	}
	sig := h.Signal(1)
	a, avant, ok := h.Abonner(1)
	if !ok || len(avant) != 0 || h.Reste(1) != 30 {
		t.Fatalf("abonnement : %v %d", ok, h.Reste(1))
	}
	select {
	case <-sig:
	default:
		t.Error("agent non réveillé")
	}
	m = m.Add(20 * time.Second)
	if r := h.Recevoir(1, protocole.Direct{CPU: 5}); r != 30 {
		t.Errorf("demande non prolongée : %d", r)
	}
	if d := <-a.C; d.CPU != 5 {
		t.Errorf("relevé non transmis : %+v", d)
	}
	h.Desabonner(a)
	m = m.Add(20 * time.Second)
	if r := h.Recevoir(1, protocole.Direct{}); r != 10 {
		t.Errorf("sans page : %d s restantes (10 attendues)", r)
	}
	m = m.Add(11 * time.Second)
	if h.Reste(1) != 0 || h.Recevoir(1, protocole.Direct{}) != 0 {
		t.Error("direct encore demandé 30 s après le départ de la page")
	}
	// Page lente : relevés perdus pour elle, jamais de blocage.
	b, avant, _ := h.Abonner(1)
	if len(avant) != 2 {
		t.Errorf("%d relevés déjà en mémoire (2 attendus)", len(avant))
	}
	for i := 0; i < Gardes+20; i++ {
		h.Recevoir(1, protocole.Direct{})
	}
	if _, avant, _ := h.Abonner(1); len(avant) != Gardes {
		t.Errorf("%d relevés gardés", len(avant))
	}
	h.Desabonner(b)
	h.Desabonner(b) // deux fois : sans effet
	if h.NbAbonnes() != 1 {
		t.Errorf("%d abonnés", h.NbAbonnes())
	}
}

func TestHubMaxAbonnes(t *testing.T) {
	h := Nouveau()
	for i := 0; i < MaxAbonnes; i++ {
		if _, _, ok := h.Abonner(int64(i)); !ok {
			t.Fatal("refus trop tôt")
		}
	}
	if _, _, ok := h.Abonner(999); ok {
		t.Error("au-delà de MaxAbonnes : accepté")
	}
}

// Relecture du 21/11 : 10 min après la fin du direct, les relevés gardés
// sont rendus, même si l'agent attend encore sa configuration (sa machine
// reste connue pour pouvoir le réveiller).
func TestHubRendRelevesApresDirect(t *testing.T) {
	h := Nouveau()
	m := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	h.maintenant = func() time.Time { return m }
	a, _, _ := h.Abonner(1)
	for i := 0; i < 50; i++ {
		h.Recevoir(1, protocole.Direct{})
	}
	h.Desabonner(a)
	for i := 0; i < 30; i++ { // l'agent attend sa configuration chaque minute
		m = m.Add(time.Minute)
		h.Signal(1)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if e := h.machines[1]; e == nil || e.points != nil {
		t.Errorf("après 30 min : machine %v, relevés gardés %v", e != nil, e != nil && e.points != nil)
	}
}
