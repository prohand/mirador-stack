// SPDX-License-Identifier: GPL-3.0-or-later

package secrets

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestSecrets(t *testing.T) {
	cle := bytes.Repeat([]byte{1}, 32)
	v, err := Chiffrer(cle, "sauvegarde.s3.secret", "mot-de-passe-très-secret")
	if err != nil || !EstChiffre(v) || strings.Contains(v, "secret") {
		t.Fatalf("%q %v", v, err)
	}
	if c, err := Dechiffrer(cle, "sauvegarde.s3.secret", v); err != nil || c != "mot-de-passe-très-secret" {
		t.Fatalf("%q %v", c, err)
	}
	if _, err := Dechiffrer(cle, "autre.usage", v); !errors.Is(err, ErrSecret) {
		t.Error("secret déplacé dans un autre champ accepté")
	}
	if _, err := Dechiffrer(bytes.Repeat([]byte{2}, 32), "sauvegarde.s3.secret", v); !errors.Is(err, ErrSecret) {
		t.Error("autre clé acceptée")
	}
	if _, err := Dechiffrer(cle, "sauvegarde.s3.secret", v[:len(v)-4]+"AAAA"); !errors.Is(err, ErrSecret) {
		t.Error("valeur modifiée acceptée")
	}
	if _, err := Dechiffrer(cle, "x", "en clair"); err == nil {
		t.Error("valeur en clair acceptée")
	}
	if v2, _ := Chiffrer(cle, "sauvegarde.s3.secret", "mot-de-passe-très-secret"); v2 == v {
		t.Error("même chiffré deux fois")
	}
}
