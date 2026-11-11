// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"net/http"
	"net/url"
	"testing"
	"time"
)

// Verrou progressif : 15 min, 30 min, 1 h… 24 h au plus ; la série repart
// de zéro après 24 h sans verrou.
func TestVerrouProgressif(t *testing.T) {
	v := nouveauxVerrous()
	t0 := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	verrouiller := func(t1 time.Time) time.Duration {
		var d time.Duration
		for i := 0; i < 5; i++ {
			d = v.echecAdresse("203.0.113.9", 5, 15*time.Minute, t1)
		}
		return d
	}
	t1 := t0
	for i, attendu := range []time.Duration{15 * time.Minute, 30 * time.Minute, time.Hour, 2 * time.Hour, 4 * time.Hour,
		8 * time.Hour, 16 * time.Hour, 24 * time.Hour, 24 * time.Hour} {
		d := verrouiller(t1)
		if d != attendu {
			t.Fatalf("verrou n° %d : %s, attendu %s", i+1, d, attendu)
		}
		if !v.bloqueAdresse("203.0.113.9", t1.Add(d-time.Second)) || v.bloqueAdresse("203.0.113.9", t1.Add(d+time.Second)) {
			t.Fatalf("verrou n° %d : durée mal appliquée", i+1)
		}
		t1 = t1.Add(d + time.Minute)
	}
	// 24 h sans verrou : retour à la durée de base.
	if d := verrouiller(t1.Add(25 * time.Hour)); d != 15*time.Minute {
		t.Errorf("après 24 h de calme : %s, attendu 15 min", d)
	}
	// Compte : une connexion réussie efface la série.
	for i := 0; i < 2; i++ {
		for j := 0; j < 15; j++ {
			v.echecCompte("admin", 15, 15*time.Minute, t0.Add(time.Duration(i)*time.Hour))
		}
	}
	v.reussite("admin", "203.0.113.5", t0.Add(3*time.Hour))
	var d time.Duration
	for j := 0; j < 15; j++ {
		d = v.echecCompte("admin", 15, 15*time.Minute, t0.Add(4*time.Hour))
	}
	if d != 15*time.Minute {
		t.Errorf("après une réussite : %s, attendu 15 min", d)
	}
}

// Trois sessions « avant le code » au plus par compte : la plus ancienne
// est fermée.
func TestSessionsPartiellesLimitees(t *testing.T) {
	b := nouveauBanc(t)
	b.premierAdmin("admin", mdpAdmin)
	b.h.avancer(16 * time.Minute)
	var clients []*http.Client
	for i := 0; i < 5; i++ {
		c := b.client()
		if code, _, _ := b.post(c, "/connexion", url.Values{"nom": {"admin"}, "mot_de_passe": {mdpAdmin}}); code != http.StatusSeeOther {
			t.Fatalf("mot de passe %d : %d", i, code)
		}
		clients = append(clients, c)
		b.h.avancer(time.Second)
	}
	for i, c := range clients {
		code, _, _ := b.get(c, "/connexion/code")
		if ouverte := code == http.StatusOK; ouverte != (i >= 2) {
			t.Errorf("session partielle %d : ouverte = %v", i, ouverte)
		}
	}
	// La session complète de l'admin n'est pas touchée.
	n := 0
	for _, s := range b.c.sessions.lister() {
		if s.Nom == "admin" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d session(s) complète(s), 1 attendue", n)
	}
}

func TestMotDePasseSimple(t *testing.T) {
	refuses := []string{"aaaaaaaaaaaa", "123456789012", "abcdefghijklmnop", "zyxwvutsrqpo", "abcabcabcabc", "12121212121212",
		"mirador12345", "Mirador-2026", "jean.dupont123"}
	for _, m := range refuses {
		if VerifierSimple(m, "jean.dupont") == nil {
			t.Errorf("%q accepté", m)
		}
	}
	acceptes := []string{mdpAdmin, "mot de passe de bob", "correct cheval pile agrafe", "x7#Lq9!vT2pd", "mirador veille sur mes machines",
		"jean.dupont aime les longues phrases"}
	for _, m := range acceptes {
		if err := VerifierSimple(m, "jean.dupont"); err != nil {
			t.Errorf("%q refusé : %v", m, err)
		}
	}
	// Branché à la création d'un compte et au changement de mot de passe.
	b := nouveauBanc(t)
	if err := CreerUtilisateur(t.Context(), b.db, "lea", RoleLecture, "aaaaaaaaaaaa", "test", "", b.h.maintenant()); err == nil {
		t.Error("création avec un mot de passe trop simple")
	}
	if err := CreerUtilisateur(t.Context(), b.db, "lea", RoleAdmin, mdpAdmin, "test", "", b.h.maintenant()); err != nil {
		t.Fatal(err)
	}
	if err := DefinirMotDePasse(t.Context(), b.db, "lea", "lea-12345678", "test", "", b.h.maintenant()); err == nil {
		t.Error("changement vers un mot de passe trop simple")
	}
}
