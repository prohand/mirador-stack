// SPDX-License-Identifier: GPL-3.0-or-later

package unifi

import (
	"context"
	"os"
	"strings"
	"testing"

	"mirador-stack/internal/agent/config"
)

// TestMain : le faux contrôleur des tests écoute sur la boucle locale,
// refusée en vrai : contrôle LAN retiré, sauf dans TestControleurHorsLAN.
func TestMain(m *testing.M) {
	controleLAN = nil
	os.Exit(m.Run())
}

// Revue du 28/11 : l'adresse réellement jointe doit être du LAN, même
// donnée par un nom (ici « localhost », la boucle locale).
func TestControleurHorsLAN(t *testing.T) {
	controleLAN = config.ControleLAN
	defer func() { controleLAN = nil }()
	srv := controleur(t, true)
	adresse := strings.Replace(srv.URL, "127.0.0.1", "localhost", 1)
	c := Nouveau(config.UniFi{Machine: "udm", Adresse: adresse, CleAPI: "cle-api-de-test-0123456789", Empreinte: empreinte(srv)})
	defer c.Fermer()
	_, err := c.Sites(context.Background())
	if err == nil || !strings.Contains(err.Error(), "hors du LAN refusée") {
		t.Fatalf("boucle locale acceptée : %v", err)
	}
}
