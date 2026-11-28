// SPDX-License-Identifier: GPL-3.0-or-later

package snmp

import (
	"strings"
	"testing"

	"mirador-stack/internal/agent/config"
)

// Revue du 28/11 : l'adresse réellement jointe doit être du LAN, même
// donnée par un nom (« localhost » : la boucle locale) ; contrôle fait par
// gosnmp à l'ouverture du socket UDP.
func TestOuvrirHorsLAN(t *testing.T) {
	c := config.CibleSNMP{Machine: "nas", Adresse: "localhost:161", Prereglage: "standard", Utilisateur: "lecture",
		Auth: "SHA256", MotDePasse: "mot-de-passe", Chiffrement: "AES", CleChiffre: "cle-de-chiffrement"}
	if _, _, err := OuvrirReseau(c); err == nil || !strings.Contains(err.Error(), "hors du LAN refusée") {
		t.Fatalf("boucle locale acceptée : %v", err)
	}
	c.Adresse = "169.254.169.254:161"
	if _, _, err := OuvrirReseau(c); err == nil || !strings.Contains(err.Error(), "hors du LAN refusée") {
		t.Fatalf("lien local accepté : %v", err)
	}
	// Adresse du LAN : pas refusée par le contrôle (le socket UDP s'ouvre
	// si la machine a une route ; rien n'est envoyé ici).
	c.Adresse = "192.168.1.10:161"
	_, fermer, err := OuvrirReseau(c)
	if err != nil && strings.Contains(err.Error(), "hors du LAN refusée") {
		t.Fatalf("LAN refusé : %v", err)
	}
	if err == nil {
		fermer()
	}
}
