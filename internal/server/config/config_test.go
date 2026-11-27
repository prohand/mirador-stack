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

// Mise à jour du serveur depuis l'interface (SPEC § 102) : dossiers par
// défaut, « non » la désactive, chemin relatif refusé.
func TestMajServeurDossiers(t *testing.T) {
	s, err := Lire(strings.NewReader("MIRADOR_ECOUTE_AGENTS=127.0.0.1:9440\nMIRADOR_ECOUTE_INTERFACE=127.0.0.1:9441\n"))
	if err != nil || s.MajDemande != "/var/lib/mirador-maj" || s.MajEtat != "/var/lib/mirador-maj-etat" {
		t.Fatalf("défauts : %+v, %v", s, err)
	}
	s, err = Lire(strings.NewReader("MIRADOR_ECOUTE_AGENTS=127.0.0.1:9440\nMIRADOR_ECOUTE_INTERFACE=127.0.0.1:9441\nMIRADOR_MAJ_DEMANDE=non\n"))
	if err != nil || s.MajDemande != "" {
		t.Fatalf("désactivée : %+v, %v", s, err)
	}
	if _, err := Lire(strings.NewReader("MIRADOR_ECOUTE_AGENTS=127.0.0.1:9440\nMIRADOR_ECOUTE_INTERFACE=127.0.0.1:9441\nMIRADOR_MAJ_ETAT=relatif\n")); err == nil {
		t.Error("chemin relatif accepté")
	}
}

// Taille du parc (SPEC § 141) : homelab inchangé, grand parc proportionné.
func TestDimensions(t *testing.T) {
	d := DimensionsPour(40)
	if d.Series != 48_000 || d.Memoire != 200<<20 || d.Places != 4 || d.Legeres != 16 || d.Telechargements != 2 || d.RequetesIP != 50 {
		t.Errorf("40 machines : %+v", d)
	}
	if d := DimensionsPour(0); d.Machines != TailleParcDefaut {
		t.Errorf("défaut : %+v", d)
	}
	d = DimensionsPour(1500)
	if d.Series != 1_800_000 || d.Memoire < 5<<30 || d.Memoire > 6<<30 || d.Places != 30 || d.Legeres != 150 || d.Telechargements != 5 {
		t.Errorf("1 500 machines : %+v (mémoire %d Mio)", d, d.Memoire>>20)
	}
	base := "MIRADOR_PROXY=127.0.0.1\n"
	s, err := Lire(strings.NewReader(base + "MIRADOR_TAILLE_PARC=1500\n"))
	if err != nil || s.TailleParc != 1500 {
		t.Fatalf("lecture : %v %v", s.TailleParc, err)
	}
	for _, mauvais := range []string{"0", "abc", "999999"} {
		if _, err := Lire(strings.NewReader(base + "MIRADOR_TAILLE_PARC=" + mauvais + "\n")); err == nil {
			t.Errorf("%q accepté", mauvais)
		}
	}
}
