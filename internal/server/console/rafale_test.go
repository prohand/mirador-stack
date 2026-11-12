// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// rafale : n requêtes lancées en même temps ; renvoie le nombre de réponses
// par code HTTP.
func (b *banc) rafale(n int, envoi func(i int) int) map[int]int {
	var wg sync.WaitGroup
	var mu sync.Mutex
	vus := map[int]int{}
	depart := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-depart
			code := envoi(i)
			mu.Lock()
			vus[code]++
			mu.Unlock()
		}()
	}
	close(depart)
	wg.Wait()
	return vus
}

// Une rafale de mots de passe envoyés en même temps ne doit pas permettre
// plus d'essais que le seuil de verrouillage (5 par adresse).
func TestRafaleMotsDePasse(t *testing.T) {
	b := nouveauBanc(t)
	b.premierAdmin("admin", mdpAdmin)
	b.h.avancer(16 * time.Minute)
	c := b.client()
	vus := b.rafale(40, func(int) int {
		code, _, _ := b.post(c, "/connexion", url.Values{"nom": {"admin"}, "mot_de_passe": {"faux faux faux"}})
		return code
	})
	t.Logf("mots de passe : %v", vus)
	if vus[http.StatusUnauthorized] > 5 {
		t.Errorf("%d mots de passe essayés dans la rafale, 5 au plus attendus", vus[http.StatusUnauthorized])
	}
}

// Même chose pour les codes 2FA d'une session partielle (5 essais au plus).
func TestRafaleCodes(t *testing.T) {
	b := nouveauBanc(t)
	b.premierAdmin("admin", mdpAdmin)
	b.h.avancer(16 * time.Minute)
	c := b.client()
	if code, _, _ := b.post(c, "/connexion", url.Values{"nom": {"admin"}, "mot_de_passe": {mdpAdmin}}); code != http.StatusSeeOther {
		t.Fatalf("mot de passe : %d", code)
	}
	csrf := b.csrf(c, "/connexion/code")
	vus := b.rafale(40, func(int) int {
		code, _, _ := b.post(c, "/connexion/code", url.Values{"csrf": {csrf}, "code": {"000000"}})
		return code
	})
	t.Logf("codes : %v", vus)
	if vus[http.StatusUnauthorized] > 5 {
		t.Errorf("%d codes essayés dans la rafale, 5 au plus attendus", vus[http.StatusUnauthorized])
	}
}

// Mot de passe connu de l'attaquant : beaucoup de sessions partielles, puis
// un code par session, tous en même temps. Pas plus d'essais que le seuil.
func TestRafaleCodesPlusieursSessions(t *testing.T) {
	b := nouveauBanc(t)
	b.premierAdmin("admin", mdpAdmin)
	b.h.avancer(16 * time.Minute)
	type session struct {
		c    *http.Client
		csrf string
	}
	var sessions []session
	for i := 0; i < 30; i++ {
		c := b.client()
		if code, _, _ := b.post(c, "/connexion", url.Values{"nom": {"admin"}, "mot_de_passe": {mdpAdmin}}); code != http.StatusSeeOther {
			t.Fatalf("mot de passe %d : %d", i, code)
		}
		sessions = append(sessions, session{c, b.csrf(c, "/connexion/code")})
	}
	vus := b.rafale(len(sessions), func(i int) int {
		code, _, _ := b.post(sessions[i].c, "/connexion/code", url.Values{"csrf": {sessions[i].csrf}, "code": {"000000"}})
		return code
	})
	t.Logf("codes : %v", vus)
	if vus[http.StatusUnauthorized] > 5 {
		t.Errorf("%d codes essayés dans la rafale, 5 au plus attendus", vus[http.StatusUnauthorized])
	}
}

// Session complète (cookie volé) : une rafale de codes pour une action
// sensible ne donne pas plus d'essais que le seuil.
func TestRafaleCodesSensibles(t *testing.T) {
	b := nouveauBanc(t)
	c, _ := b.premierAdmin("admin", mdpAdmin)
	b.h.avancer(16 * time.Minute)
	csrf := b.csrf(c, "/utilisateurs")
	b.rafale(40, func(int) int {
		b.post(c, "/utilisateurs", url.Values{"csrf": {csrf}, "action": {"creer"}, "nom": {"intrus"}, "role": {"admin"},
			"mot_de_passe": {mdpAdmin}, "code": {"000000"}})
		return 0
	})
	if n := strings.Count(b.journal.String(), "code 2FA faux (action sensible)"); n > 5 {
		t.Errorf("%d codes essayés dans la rafale, 5 au plus attendus", n)
	}
}
