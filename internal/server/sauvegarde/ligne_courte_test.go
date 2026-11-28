// SPDX-License-Identifier: GPL-3.0-or-later

package sauvegarde

import (
	"strings"
	"testing"
)

// Revue du 28/11 : l'erreur d'envoi S3 ne recopie plus la réponse entière,
// seulement une courte ligne nettoyée (le code S3 s'il est là).
func TestLigneCourte(t *testing.T) {
	cas := []struct{ entree, attendu string }{
		{`<?xml version="1.0"?><Error><Code>AccessDenied</Code><Message>Access Denied</Message><RequestId>X</RequestId></Error>`, "AccessDenied"},
		{"Service indisponible\nsuite très longue\n", "Service indisponible"},
		{"", ""},
		{"a\x1b[31mrouge\x07", "a[31mrouge"},
		{strings.Repeat("é", 300), strings.Repeat("é", 120) + "…"},
	}
	for _, c := range cas {
		if got := ligneCourte([]byte(c.entree)); got != c.attendu {
			t.Errorf("ligneCourte(%.30q) = %q, attendu %q", c.entree, got, c.attendu)
		}
	}
}
