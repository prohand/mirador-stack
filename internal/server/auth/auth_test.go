// SPDX-License-Identifier: GPL-3.0-or-later

package auth

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestMotDePasse(t *testing.T) {
	e, err := Empreinte("douze caractères")
	if err != nil {
		t.Fatal(err)
	}
	if !Comparer(e, "douze caractères") || Comparer(e, "douze caracteres") {
		t.Error("comparaison fausse")
	}
	if _, err := Empreinte("court"); err == nil {
		t.Error("mot de passe court accepté")
	}
	// Empreinte abîmée : refus sans calcul démesuré.
	if Comparer("$argon2id$v=19$m=99999999,t=1,p=1$AAAA$AAAA", "x") {
		t.Error("empreinte abîmée acceptée")
	}
}

func TestTOTP(t *testing.T) {
	// Vecteur de la RFC 6238 (SHA-1, T = 59 s → 94287082, 6 derniers chiffres).
	secret := b32.EncodeToString([]byte("12345678901234567890"))
	if c, _ := codeTOTP(secret, 59/30); c != "287082" {
		t.Fatalf("RFC 6238 : %s", c)
	}
	t0 := time.Unix(59, 0)
	pas, ok := VerifierTOTP(secret, "287 082", t0, 0)
	if !ok || pas != 1 {
		t.Fatal("code juste refusé")
	}
	if _, ok := VerifierTOTP(secret, "287082", t0, pas); ok {
		t.Error("code rejoué accepté")
	}
	if _, ok := VerifierTOTP(secret, "287082", t0.Add(5*time.Minute), 0); ok {
		t.Error("code trop vieux accepté")
	}
	if !strings.HasPrefix(URITOTP("admin", secret), "otpauth://totp/mirador:admin?") {
		t.Error("URI otpauth")
	}
}

// Revue de sécurité du 11/11 : verrouillage.
func TestVerrousRevue(t *testing.T) {
	v := NouveauxVerrous()
	t0 := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	// IPv6 : tout le /64 compte ensemble.
	for i := 0; i < 5; i++ {
		v.EchecAdresse(fmt.Sprintf("2001:db8:1:2::%x", i+1), 5, time.Hour, t0)
	}
	if !v.BloqueAdresse("2001:db8:1:2:ffff::9", t0) || v.BloqueAdresse("2001:db8:1:3::1", t0) {
		t.Fatal("verrou IPv6 par /64 raté")
	}
	if CleAdresse("192.0.2.7") != "192.0.2.7" || CleAdresse("::ffff:192.0.2.7") != "192.0.2.7" {
		t.Error("IPv4 (et IPv4 dans IPv6) : adresse complète attendue")
	}
	// Connexion réussie hier depuis 203.0.113.5 (adresse de confiance).
	v.Reussite("admin", "203.0.113.5", t0.Add(-24*time.Hour))
	// Un compte verrouillé le reste malgré une avalanche de noms bidon.
	for i := 0; i < 15; i++ {
		v.EchecCompte("admin", 15, time.Hour, t0)
	}
	for i := 0; i < entreesMax+50; i++ {
		v.EchecCompte(fmt.Sprintf("bidon%d", i), 15, time.Hour, t0)
	}
	if !v.BloqueCompte("admin", "198.51.100.1", t0) {
		t.Fatal("verrou de compte effacé par la saturation de la table")
	}
	// Une adresse déjà connectée à ce compte n'est pas bloquée par le verrou
	// du compte (un inconnu ne peut pas enfermer l'admin dehors).
	if v.BloqueCompte("admin", "203.0.113.5", t0) || !v.BloqueCompte("admin", "203.0.113.6", t0) {
		t.Error("adresse de confiance mal gérée")
	}
	v.Lever()
	if v.BloqueCompte("admin", "198.51.100.1", t0) {
		t.Error("lever n'a pas levé le verrou")
	}
}

// Les essais en cours sont rendus : rien ne reste réservé après coup.
func TestEssaisRendus(t *testing.T) {
	v := NouveauxVerrous()
	t0 := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	var fins []func()
	for i := 0; i < 5; i++ {
		f, ok := v.Debuter("admin", "203.0.113.9", 5, t0)
		if !ok {
			t.Fatalf("essai %d refusé", i)
		}
		fins = append(fins, f)
	}
	if _, ok := v.Debuter("admin", "203.0.113.9", 5, t0); ok {
		t.Fatal("6e essai simultané accepté")
	}
	// Une autre adresse n'est pas gênée (seuil du compte : 15).
	if f, ok := v.Debuter("admin", "203.0.113.10", 5, t0); !ok {
		t.Fatal("autre adresse refusée")
	} else {
		f()
	}
	for _, f := range fins {
		f()
	}
	if len(v.enCours) != 0 {
		t.Fatalf("essais encore réservés : %v", v.enCours)
	}
	// Les échecs comptés réduisent la place : 3 échecs → 2 essais à la fois.
	for i := 0; i < 3; i++ {
		v.EchecAdresse("203.0.113.9", 5, time.Minute, t0)
	}
	f1, ok1 := v.Debuter("admin", "203.0.113.9", 5, t0)
	f2, ok2 := v.Debuter("admin", "203.0.113.9", 5, t0)
	_, ok3 := v.Debuter("admin", "203.0.113.9", 5, t0)
	if !ok1 || !ok2 || ok3 {
		t.Fatalf("après 3 échecs : %v %v %v, attendu vrai vrai faux", ok1, ok2, ok3)
	}
	f1()
	f2()
}

// Verrou progressif : 15 min, 30 min, 1 h… 24 h au plus ; la série repart
// de zéro après 24 h sans verrou.
func TestVerrouProgressif(t *testing.T) {
	v := NouveauxVerrous()
	t0 := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	verrouiller := func(t1 time.Time) time.Duration {
		var d time.Duration
		for i := 0; i < 5; i++ {
			d = v.EchecAdresse("203.0.113.9", 5, 15*time.Minute, t1)
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
		if !v.BloqueAdresse("203.0.113.9", t1.Add(d-time.Second)) || v.BloqueAdresse("203.0.113.9", t1.Add(d+time.Second)) {
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
			v.EchecCompte("admin", 15, 15*time.Minute, t0.Add(time.Duration(i)*time.Hour))
		}
	}
	v.Reussite("admin", "203.0.113.5", t0.Add(3*time.Hour))
	var d time.Duration
	for j := 0; j < 15; j++ {
		d = v.EchecCompte("admin", 15, 15*time.Minute, t0.Add(4*time.Hour))
	}
	if d != 15*time.Minute {
		t.Errorf("après une réussite : %s, attendu 15 min", d)
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
	acceptes := []string{"une longue phrase de passe", "mot de passe de bob", "correct cheval pile agrafe", "x7#Lq9!vT2pd", "mirador veille sur mes machines",
		"jean.dupont aime les longues phrases"}
	for _, m := range acceptes {
		if err := VerifierSimple(m, "jean.dupont"); err != nil {
			t.Errorf("%q refusé : %v", m, err)
		}
	}
}
