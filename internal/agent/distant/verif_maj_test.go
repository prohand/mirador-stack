// SPDX-License-Identifier: GPL-3.0-or-later

package distant

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// Vérification de mise à jour impossible (SPEC § 61) : rien signalé au
// serveur avant 6 h ; un vrai échec, lui, tout de suite.
func TestFiltrerVerification(t *testing.T) {
	t0 := time.Date(2026, 11, 19, 6, 0, 0, 0, time.UTC)
	e502 := &errVerification{errors.New("version assignée : HTTP 502")}

	depuis, passager, err := filtrerVerification(e502, time.Time{}, t0)
	if err != nil || !passager || !depuis.Equal(t0) {
		t.Fatalf("1re vérification ratée : %v %v %v", depuis, passager, err)
	}
	depuis, passager, err = filtrerVerification(e502, depuis, t0.Add(5*time.Hour))
	if err != nil || !passager || !depuis.Equal(t0) {
		t.Fatalf("5 h plus tard : %v %v %v", depuis, passager, err)
	}
	_, passager, err = filtrerVerification(e502, depuis, t0.Add(6*time.Hour))
	if err == nil || passager || !strings.Contains(err.Error(), "vérification impossible depuis 6h0m0s : version assignée : HTTP 502") {
		t.Fatalf("6 h plus tard : %v %v", passager, err)
	}
	// Réponse revenue : compteur remis à zéro.
	if depuis, _, err = filtrerVerification(nil, depuis, t0.Add(7*time.Hour)); err != nil || !depuis.IsZero() {
		t.Fatalf("après réponse : %v %v", depuis, err)
	}
	// Vrai échec (signature refusée…) : signalé tout de suite.
	vrai := errors.New("signature invalide")
	if _, passager, err = filtrerVerification(vrai, time.Time{}, t0); err != vrai || passager {
		t.Fatalf("vrai échec : %v %v", passager, err)
	}
}
