// SPDX-License-Identifier: GPL-3.0-or-later

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLire(t *testing.T) {
	s, err := Lire(strings.NewReader("MIRADOR_DONNEES=/srv/mirador\nMIRADOR_PROXY=172.30.0.1, 10.0.0.0/8\n"))
	if err != nil {
		t.Fatal(err)
	}
	if s.Donnees != "/srv/mirador" || len(s.Proxys) != 2 || s.EcouteAgents != "0.0.0.0:9440" {
		t.Errorf("valeurs mal lues : %+v", s)
	}
	if !s.Proxys[0].Contains(s.Proxys[0].Addr()) || s.Proxys[0].Bits() != 32 {
		t.Errorf("adresse seule mal convertie : %v", s.Proxys[0])
	}
}

func TestRefus(t *testing.T) {
	for nom, contenu := range map[string]string{
		"clé inconnue (réglage de l'interface)": "MIRADOR_RETENTION=30d\n",
		"même écoute":       "MIRADOR_ECOUTE_AGENTS=0.0.0.0:9441\n",
		"TLS incomplet":     "MIRADOR_TLS_CERTIFICAT=/c.pem\n",
		"données relatives": "MIRADOR_DONNEES=donnees\n",
		"proxy invalide":    "MIRADOR_PROXY=nginx\n",
		"écoute sans port":  "MIRADOR_ECOUTE_INTERFACE=0.0.0.0\n",
		"double":            "MIRADOR_DONNEES=/a\nMIRADOR_DONNEES=/b\n",
	} {
		// Proxy déclaré : le refus vient bien du cas testé, pas de l'écoute.
		if nom != "proxy invalide" {
			contenu += "MIRADOR_PROXY=172.30.0.1\n"
		}
		if _, err := Lire(strings.NewReader(contenu)); err == nil {
			t.Errorf("%s : aurait dû être refusé", nom)
		}
	}
}

// Écoute ouverte au réseau : nginx (MIRADOR_PROXY) ou TLS obligatoire.
func TestEcouteOuverte(t *testing.T) {
	local := "MIRADOR_ECOUTE_AGENTS=127.0.0.1:9440\nMIRADOR_ECOUTE_INTERFACE=[::1]:9441\n"
	for nom, cas := range map[string]struct {
		contenu string
		accepte bool
	}{
		"défaut sans proxy":       {"", false},
		"toutes les interfaces":   {"MIRADOR_ECOUTE_AGENTS=127.0.0.1:9440\nMIRADOR_ECOUTE_INTERFACE=:9441\n", false},
		"adresse du réseau":       {"MIRADOR_ECOUTE_AGENTS=127.0.0.1:9440\nMIRADOR_ECOUTE_INTERFACE=192.0.2.10:9441\n", false},
		"défaut avec proxy":       {"MIRADOR_PROXY=172.30.0.1\n", true},
		"défaut avec TLS":         {"MIRADOR_TLS_CERTIFICAT=/c.pem\nMIRADOR_TLS_CLE=/k.pem\n", true},
		"boucle locale sans rien": {local, true},
	} {
		_, err := Lire(strings.NewReader(cas.contenu))
		if (err == nil) != cas.accepte {
			t.Errorf("%s : accepté = %v (erreur : %v)", nom, err == nil, err)
		}
	}
}

func TestLireCle(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "cle")
	if err := os.WriteFile(f, []byte("AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	if c, err := LireCle(f); err != nil || len(c) != 32 {
		t.Fatalf("clé valide refusée : %v", err)
	}
	if err := os.Chmod(f, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LireCle(f); err == nil {
		t.Error("une clé lisible par tous doit être refusée")
	}
	if err := os.WriteFile(f, []byte("courte"), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(f, 0o400); err != nil {
		t.Fatal(err)
	}
	if _, err := LireCle(f); err == nil {
		t.Error("une clé de mauvaise taille doit être refusée")
	}
}
