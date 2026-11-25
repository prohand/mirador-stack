// SPDX-License-Identifier: GPL-3.0-or-later

package collect

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"mirador-stack/internal/agent/metrics"
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
	m.dpkg.chemin = filepath.Join(dir, "dpkg.log") // pas celui de la machine de test
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

// Échec (SPEC § 127) : motif envoyé, nouvel essai 15 min plus tard (et pas
// 6 h) ; nouvelles listes d'apt (apt update) : nouveau calcul 2 min après.
func TestMisesAJourEchecEtListes(t *testing.T) {
	dir := t.TempDir()
	m := NewMisesAJour()
	m.dpkg.chemin = filepath.Join(dir, "dpkg.log")
	m.commande = filepath.Join(dir, "apt-get")
	if err := os.WriteFile(m.commande, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	m.fichierRedemarrage = filepath.Join(dir, "reboot-required")
	listes := filepath.Join(dir, "lists")
	if err := os.Mkdir(listes, 0o700); err != nil {
		t.Fatal(err)
	}
	m.listesApt = []string{listes}
	var appels atomic.Int32
	var echouer atomic.Bool
	echouer.Store(true)
	m.lancer = func(context.Context) ([]byte, error) {
		appels.Add(1)
		if echouer.Load() {
			return nil, errors.New("E: Could not open \"lock\"\nsuite")
		}
		return []byte(sortieApt), nil
	}
	t0 := time.Unix(1_000_000, 0)
	maintenant := t0
	m.maintenant = func() time.Time { return maintenant }
	// Pendant l'échec, Collecter renvoie l'erreur : on garde les points quand même.
	collecter := func(t *testing.T, c Collecteur) map[string]float64 {
		t.Helper()
		b := metrics.NewBatch(64, nil)
		b.Reset(0)
		_ = c.Collecter(b)
		out := map[string]float64{}
		for i := range b.Samples {
			out[cle(&b.Samples[i])] = b.Samples[i].Value
		}
		return out
	}

	collecter(t, m)
	attendreCond(t, func() bool { m.mu.Lock(); defer m.mu.Unlock(); return m.erreur != nil && !m.enCours })
	maintenant = t0.Add(time.Minute)
	p := collecter(t, m)
	if v, ok := p["mirador_agent_mises_a_jour_echec|raison=E: Could not open lock suite"]; !ok || v != 1 {
		t.Errorf("motif de l'échec absent : %v", p)
	}
	maintenant = t0.Add(14 * time.Minute)
	collecter(t, m)
	if appels.Load() != 1 {
		t.Fatalf("nouvel essai avant 15 min (%d appels)", appels.Load())
	}
	echouer.Store(false)
	maintenant = t0.Add(16 * time.Minute)
	collecter(t, m)
	attendreCond(t, func() bool { m.mu.Lock(); defer m.mu.Unlock(); return m.ok && m.erreur == nil })
	maintenant = t0.Add(17 * time.Minute)
	p = collecter(t, m)
	if _, ok := p["mirador_agent_mises_a_jour_echec|raison=E: Could not open lock suite"]; ok {
		t.Error("échec encore signalé après un calcul réussi")
	}
	if p["apt_upgrades_calcule_le"] != float64(t0.Add(16*time.Minute).Unix()) {
		t.Errorf("heure du calcul : %v", p["apt_upgrades_calcule_le"])
	}

	// apt update : les listes changent, nouveau calcul 2 min plus tard.
	n := appels.Load()
	futur := time.Now().Add(time.Hour)
	if err := os.Chtimes(listes, futur, futur); err != nil {
		t.Fatal(err)
	}
	maintenant = t0.Add(20 * time.Minute)
	collecter(t, m) // changement vu
	maintenant = t0.Add(21 * time.Minute)
	collecter(t, m)
	if appels.Load() != n {
		t.Fatal("calcul avant les 2 min d'attente")
	}
	maintenant = t0.Add(23 * time.Minute)
	collecter(t, m)
	attendreCond(t, func() bool { return appels.Load() == n+1 })
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

// Liste des paquets (SPEC § 42.3) : nom, versions, sécurité ; nouveau
// paquet sans version installée ; plafond de 200 (le total reste juste).
func TestLireMisesAJour(t *testing.T) {
	// Nouveau paquet : dans la liste, mais compté à part (comme « apt list
	// --upgradable », retour du 25/11), même venant de -security.
	ps, total, sec, neufs := lireMisesAJour([]byte(sortieApt + "Inst neuf (1.0-1 Debian:12/stable [amd64])\n" +
		"Inst linux-image-6.1.0-40-amd64 (6.1.153-1 Debian-Security:12/stable-security [amd64])\n"))
	if total != 4 || sec != 3 || neufs != 2 || len(ps) != 6 {
		t.Fatalf("total %d, sécurité %d, nouveaux %d, liste %d", total, sec, neufs, len(ps))
	}
	if p := ps[0]; p.nom != "libssl3" || p.avant != "3.0.15-1~deb12u1" || p.apres != "3.0.17-1~deb12u2" || !p.securite {
		t.Errorf("libssl3 : %+v", p)
	}
	if p := ps[2]; p.nom != "tzdata" || p.securite || p.apres != "2025b-0+deb12u1" {
		t.Errorf("tzdata : %+v", p)
	}
	if p := ps[4]; p.nom != "neuf" || p.avant != "" || p.apres != "1.0-1" {
		t.Errorf("nouveau paquet : %+v", p)
	}
	var beaucoup bytes.Buffer
	for i := 0; i < 250; i++ {
		fmt.Fprintf(&beaucoup, "Inst p%d [1] (2 Debian:12/stable [amd64])\n", i)
	}
	if ps, total, _, _ := lireMisesAJour(beaucoup.Bytes()); total != 250 || len(ps) != maxPaquetsListes {
		t.Errorf("plafond : total %d, liste %d", total, len(ps))
	}
}
