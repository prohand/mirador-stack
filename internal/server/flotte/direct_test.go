// SPDX-License-Identifier: GPL-3.0-or-later

package flotte

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"mirador-stack/internal/protocole"
	"mirador-stack/internal/server/direct"
)

// Direct (SPEC § 97), côté serveur : une page qui s'abonne réveille l'agent
// en attente ; la réponse donne les secondes ; les relevés sont acceptés
// pendant la demande, refusés (« 0 ») sinon ; un relevé invalide est refusé.
func TestDirectServeur(t *testing.T) {
	b := nouveauBanc(t)
	hub := direct.Nouveau()
	b.f.opt.Direct = hub
	token, _ := TokenManuel(ctx, b.db, "pc", "admin", t0)
	id, _ := IDMachine(ctx, b.db, "pc")
	releve := []byte(`{"t":1,"cpu":12.5,"coeurs":4,"charge":0.5,"mem_utilisee":1,"mem_totale":2,"net_recu":0,"net_envoye":0,"disque_lu":0,"disque_ecrit":0}`)
	// Sans page : relevé ignoré, l'agent s'arrête.
	if w := b.requete(http.MethodPost, protocole.CheminDirect, token, releve, ""); w.Code != http.StatusNoContent || w.Header().Get(protocole.EnteteDirect) != "0" {
		t.Fatalf("sans demande : %d %q", w.Code, w.Header().Get(protocole.EnteteDirect))
	}
	w := b.requete(http.MethodGet, protocole.CheminConfig, token, nil, "")
	etag := w.Header().Get("ETag")
	// Agent en attente ; une page s'abonne 200 ms après : réponse aussitôt.
	abonne := make(chan *direct.Abonne, 1)
	go func() { time.Sleep(200 * time.Millisecond); a, _, _ := hub.Abonner(id); abonne <- a }()
	debut := time.Now()
	w = b.requete(http.MethodGet, protocole.CheminConfig, token, nil, "", "If-None-Match", etag, protocole.EnteteAttendre, "3")
	if d := time.Since(debut); w.Code != http.StatusNotModified || d > 1500*time.Millisecond || w.Header().Get(protocole.EnteteDirect) == "" {
		t.Fatalf("réveil : %d après %v, direct %q", w.Code, d, w.Header().Get(protocole.EnteteDirect))
	}
	a := <-abonne // la page est abonnée (synchronisé : pas d'accès concurrent)
	// Direct demandé mais pas encore commencé : pas d'attente.
	debut = time.Now()
	_ = b.requete(http.MethodGet, protocole.CheminConfig, token, nil, "", "If-None-Match", etag, protocole.EnteteAttendre, "3")
	if time.Since(debut) > time.Second {
		t.Error("l'agent pas encore en direct a attendu")
	}
	// Relevé pendant la demande : transmis à la page.
	w = b.requete(http.MethodPost, protocole.CheminDirect, token, releve, "")
	if w.Code != http.StatusNoContent || w.Header().Get(protocole.EnteteDirect) == "0" {
		t.Fatalf("relevé : %d %q", w.Code, w.Header().Get(protocole.EnteteDirect))
	}
	if d := <-a.C; d.CPU != 12.5 {
		t.Errorf("relevé transmis : %+v", d)
	}
	// Agent déjà en direct : il attend normalement.
	debut = time.Now()
	_ = b.requete(http.MethodGet, protocole.CheminConfig, token, nil, "", "If-None-Match", etag, protocole.EnteteAttendre, "2",
		protocole.EnteteDirectActif, "1")
	if time.Since(debut) < 1900*time.Millisecond {
		t.Error("agent déjà en direct : réponse sans attente")
	}
	// Relevé invalide, trop gros, ou sans token.
	for _, c := range [][]byte{[]byte(`{"cpu":500}`), []byte(`{"inconnu":1}`), []byte(`{"cpu":1,"processus":[{"pid":1,"nom":"` + strings.Repeat("a", 9000) + `"}]}`)} {
		if w := b.requete(http.MethodPost, protocole.CheminDirect, token, c, ""); w.Code != http.StatusBadRequest {
			t.Errorf("relevé invalide accepté : %d", w.Code)
		}
	}
	if w := b.requete(http.MethodPost, protocole.CheminDirect, "", releve, ""); w.Code != http.StatusUnauthorized {
		t.Errorf("sans token : %d", w.Code)
	}
}

// Grand parc (SPEC § 141) : « Actualiser » une machine ne réveille qu'elle.
func TestSignalParMachine(t *testing.T) {
	a, b := attenteSignalMachine(9001), attenteSignalMachine(9002)
	DemanderActualisation(9002, time.Now())
	select {
	case <-a:
		t.Fatal("la machine 9001 réveillée pour 9002")
	default:
	}
	select {
	case <-b:
	default:
		t.Fatal("la machine 9002 pas réveillée")
	}
	DemanderVerification(9001, time.Now())
	select {
	case <-a:
	default:
		t.Fatal("vérification de 9001 : pas réveillée")
	}
}
