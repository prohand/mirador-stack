// SPDX-License-Identifier: GPL-3.0-or-later

package release

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"

	"mirador-stack/internal/protocole"
)

// Manifeste de l'image du serveur (SPEC § 102) : signé, vérifié par
// mirador-maj-serveur ; toute retouche casse la signature, un manifeste
// d'agent n'est pas accepté comme image.
func TestSignerImage(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	autre, _, _ := ed25519.GenerateKey(rand.Reader)
	cle, cleAutre := base64.StdEncoding.EncodeToString(pub), base64.StdEncoding.EncodeToString(autre)
	emp := "sha256:" + strings.Repeat("ab", 32)
	if _, err := SignerImage(priv, "sha256:court", "1.2.3"); err == nil {
		t.Error("empreinte invalide acceptée")
	}
	if _, err := SignerImage(priv, emp, "1.2"); err == nil {
		t.Error("version invalide acceptée")
	}
	m, err := SignerImage(priv, emp, "1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := VerifierImage(m, "1.2.3", []string{cleAutre, cle}); err != nil || got != emp {
		t.Fatalf("vérification : %q, %v", got, err)
	}
	if _, err := VerifierImage(m, "1.2.4", []string{cle}); err == nil {
		t.Error("autre version acceptée")
	}
	if _, err := VerifierImage(m, "1.2.3", []string{cleAutre}); err == nil {
		t.Error("mauvaise clé acceptée")
	}
	triche := *m
	triche.SHA256 = strings.Repeat("cd", 32)
	if _, err := VerifierImage(&triche, "1.2.3", []string{cle}); err == nil {
		t.Error("empreinte modifiée acceptée")
	}
	agent := *m
	agent.Produit = protocole.ProduitAgent
	if _, err := VerifierImage(&agent, "1.2.3", []string{cle}); err == nil {
		t.Error("manifeste d'un autre produit accepté")
	}
}
