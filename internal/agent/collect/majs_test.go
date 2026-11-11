// SPDX-License-Identifier: GPL-3.0-or-later

package collect

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// Extrait réel de « apt-get --simulate dist-upgrade » (Debian 12 et Ubuntu 24.04).
const sortieApt = `NOTE: This is only a simulation!
      apt-get needs root privileges for real execution.
Reading package lists...
Building dependency tree...
Calculating upgrade...
The following packages will be upgraded:
  libssl3 openssl tzdata
3 upgraded, 0 newly installed, 0 to remove and 0 not upgraded.
Inst libssl3 [3.0.15-1~deb12u1] (3.0.17-1~deb12u2 Debian-Security:12/stable-security [amd64])
Inst openssl [3.0.15-1~deb12u1] (3.0.17-1~deb12u2 Debian-Security:12/stable-security [amd64])
Inst tzdata [2024b-0+deb12u1] (2025b-0+deb12u1 Debian:12.11/stable [all])
Inst libc6 [2.39-0ubuntu8.4] (2.39-0ubuntu8.5 Ubuntu:24.04/noble-security [amd64])
Conf libssl3 (3.0.17-1~deb12u2 Debian-Security:12/stable-security [amd64])
`

func TestCompterMisesAJour(t *testing.T) {
	total, sec := compterMisesAJour([]byte(sortieApt))
	if total != 4 || sec != 3 {
		t.Errorf("total=%d sécurité=%d, attendu 4 et 3", total, sec)
	}
	if total, sec := compterMisesAJour(nil); total != 0 || sec != 0 {
		t.Error("sortie vide : 0 attendu")
	}
}

func TestMisesAJourEnFond(t *testing.T) {
	dir := t.TempDir()
	m := NewMisesAJour()
	// apt-get factice : un fichier qui existe, et une fonction de lancement simulée.
	m.commande = filepath.Join(dir, "apt-get")
	if err := os.WriteFile(m.commande, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	m.fichierRedemarrage = filepath.Join(dir, "reboot-required")
	var appels atomic.Int32
	m.lancer = func(context.Context) ([]byte, error) {
		appels.Add(1)
		return []byte(sortieApt), nil
	}
	t0 := time.Unix(1_000_000, 0)
	maintenant := t0
	m.maintenant = func() time.Time { return maintenant }

	// Premier passage : la mesure démarre en fond, pas encore de valeur.
	p := collecter(t, m)
	if _, ok := p["apt_upgrades_pending"]; ok {
		t.Error("aucune valeur attendue avant la fin de la première mesure")
	}
	if p["node_reboot_required"] != 0 {
		t.Error("pas de redémarrage requis attendu")
	}
	attendreCond(t, func() bool { m.mu.Lock(); defer m.mu.Unlock(); return m.ok })

	if err := os.WriteFile(m.fichierRedemarrage, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	maintenant = t0.Add(time.Hour)
	p = collecter(t, m)
	verifier(t, p, map[string]float64{
		"apt_upgrades_pending":          4,
		"apt_upgrades_security_pending": 3,
		"node_reboot_required":          1,
	})
	if appels.Load() != 1 {
		t.Errorf("apt-get ne doit être lancé qu'une fois en 6 h (%d appels)", appels.Load())
	}
	maintenant = t0.Add(7 * time.Hour)
	collecter(t, m)
	attendreCond(t, func() bool { return appels.Load() == 2 })
}

// TestAptGetReel lance la vraie simulation si apt-get est présent (machine
// Debian/Ubuntu) : vérifie que la commande marche sans droits root.
func TestAptGetReel(t *testing.T) {
	if _, err := os.Stat(cheminAptGet); err != nil {
		t.Skip("apt-get absent")
	}
	if testing.Short() {
		t.Skip("test lent")
	}
	m := NewMisesAJour()
	ctx, annuler := context.WithTimeout(context.Background(), time.Minute)
	defer annuler()
	sortie, err := m.lancerAptGet(ctx)
	if err != nil {
		t.Fatalf("apt-get --simulate : %v", err)
	}
	total, sec := compterMisesAJour(sortie)
	t.Logf("machine de test : %d mises à jour, dont %d de sécurité", total, sec)
}

func attendreCond(t *testing.T, cond func() bool) {
	t.Helper()
	fin := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(fin) {
			t.Fatal("délai dépassé")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
