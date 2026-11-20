// SPDX-License-Identifier: GPL-3.0-or-later

package flotte

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"mirador-stack/internal/protocole"
)

// Pannes vues par l'agent (SPEC § 98) : notées, moteur d'alertes réveillé,
// limite par minute, événement invalide refusé.
func TestPannes(t *testing.T) {
	b := nouveauBanc(t)
	var reveils atomic.Int32
	b.f.opt.ReveilAlertes = func() { reveils.Add(1) }
	token, _ := TokenManuel(ctx, b.db, "pc", "admin", t0)
	corps := []byte(`{"type":"service","objet":"nginx.service","texte":"passé en échec","le":1}`)
	if w := b.requete(http.MethodPost, protocole.CheminEvenement, token, corps, ""); w.Code != http.StatusNoContent {
		t.Fatalf("panne : %d %s", w.Code, w.Body)
	}
	ps, err := ListerPannes(ctx, b.db, "pc", t0.Add(-time.Minute), t0.Add(time.Minute), 10)
	if err != nil || len(ps) != 1 || ps[0].Objet != "nginx.service" || ps[0].Type != "service" || ps[0].Machine != "pc" {
		t.Fatalf("pannes : %+v %v", ps, err)
	}
	for i := 0; i < 30 && reveils.Load() == 0; i++ {
		time.Sleep(100 * time.Millisecond)
	}
	if reveils.Load() != 1 {
		t.Error("moteur d'alertes non réveillé")
	}
	for _, c := range []string{`{"type":"autre"}`, `{"type":"service","objet":"a b; rm -rf /"}`, `{"type":"service","objet":"` + strings.Repeat("a", 200) + `"}`} {
		if w := b.requete(http.MethodPost, protocole.CheminEvenement, token, []byte(c), ""); w.Code != http.StatusBadRequest {
			t.Errorf("%s accepté : %d", c, w.Code)
		}
	}
	// Service en boucle : 30 par minute au plus.
	refus := 0
	for i := 0; i < 40; i++ {
		if w := b.requete(http.MethodPost, protocole.CheminEvenement, token, []byte(`{"type":"oom","texte":"1 processus tué(s)"}`), ""); w.Code == http.StatusTooManyRequests {
			refus++
		}
	}
	if refus != 11 {
		t.Errorf("%d refus (11 attendus : 30 par minute, 1 déjà reçu)", refus)
	}
	if w := b.requete(http.MethodPost, protocole.CheminEvenement, "", corps, ""); w.Code != http.StatusUnauthorized {
		t.Errorf("sans token : %d", w.Code)
	}
}
