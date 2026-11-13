// SPDX-License-Identifier: GPL-3.0-or-later

package racine

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var sprintf = fmt.Sprintf

func TestLireProxmox(t *testing.T) {
	ancien := dossierPVE
	t.Cleanup(func() { dossierPVE = ancien })
	dossierPVE = t.TempDir()

	// Sans Proxmox : rien, sans erreur.
	if p, err := lireProxmox(); p != nil || err != nil {
		t.Fatalf("sans Proxmox : %+v %v", p, err)
	}

	ecrire := func(chemin, contenu string) {
		t.Helper()
		c := filepath.Join(dossierPVE, chemin)
		_ = os.MkdirAll(filepath.Dir(c), 0o755)
		if err := os.WriteFile(c, []byte(contenu), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	ecrire(".vmlist", `{"version": 12, "ids": {
		"100": {"node": "pve1", "type": "qemu", "version": 3},
		"101": {"node": "pve1", "type": "lxc", "version": 4},
		"102": {"node": "pve1", "type": "qemu", "version": 5},
		"103": {"node": "pve1", "type": "qemu", "version": 6},
		"200": {"node": "pve2", "type": "qemu", "version": 7},
		"0abc": {"node": "pve1", "type": "qemu", "version": 8}}}`)
	if err := os.Symlink("nodes/pve1", filepath.Join(dossierPVE, "local")); err != nil {
		t.Fatal(err)
	}
	// VM : nom, mémoire, 2 sockets × 4 cœurs ; l'instantané est ignoré ;
	// les autres clés (mot de passe, args…) ne sont jamais gardées.
	ecrire("nodes/pve1/qemu-server/100.conf", "boot: order=scsi0\ncores: 4\nsockets: 2\nmemory: 8192\nname: web-01\nargs: -secret x\ncipassword: $5$abc\n\n[avant-maj]\nname: ancien-nom\nmemory: 1024\n")
	// LXC : hostname, cœurs.
	ecrire("nodes/pve1/lxc/101.conf", "arch: amd64\ncores: 2\nhostname: dns-01\nmemory: 512\n")
	// VM 102 : nom invalide (non gardé), cœurs par défaut.
	ecrire("nodes/pve1/qemu-server/102.conf", "name: <script>\nmemory: 2048\n")
	// VM 103 : fichier remplacé par un lien : jamais suivi.
	_ = os.MkdirAll(filepath.Join(dossierPVE, "nodes/pve1/qemu-server"), 0o755)
	if err := os.Symlink("/etc/passwd", filepath.Join(dossierPVE, "nodes/pve1/qemu-server/103.conf")); err != nil {
		t.Fatal(err)
	}

	p, err := lireProxmox()
	if err != nil {
		t.Fatal(err)
	}
	if p.Noeud != "pve1" || len(p.Invites) != 4 {
		t.Fatalf("lecture : %+v", p)
	}
	attendu := []Invite{
		{ID: "100", Type: "qemu", Nom: "web-01", Memoire: 8192, Coeurs: 8},
		{ID: "101", Type: "lxc", Nom: "dns-01", Memoire: 512, Coeurs: 2},
		{ID: "102", Type: "qemu", Memoire: 2048, Coeurs: 1},
		{ID: "103", Type: "qemu"}, // configuration non lue (lien), invité gardé
	}
	for i, a := range attendu {
		if p.Invites[i] != a {
			t.Errorf("invité %d : %+v, attendu %+v", i, p.Invites[i], a)
		}
	}

	// .vmlist abîmé : erreur, pas de plantage.
	ecrire(".vmlist", "{pas du json")
	if _, err := lireProxmox(); err == nil {
		t.Error(".vmlist illisible accepté")
	}
}

func TestLireDisquesVM(t *testing.T) {
	ac, ap := dossierCgroup, dossierProc
	t.Cleanup(func() { dossierCgroup, dossierProc = ac, ap })
	dossierCgroup, dossierProc = t.TempDir(), t.TempDir()
	if d, err := lireDisquesVM(); d != nil || err != nil {
		t.Fatalf("sans Proxmox : %v %v", d, err)
	}
	ecrire := func(racine, chemin, contenu string) {
		t.Helper()
		c := filepath.Join(racine, chemin)
		_ = os.MkdirAll(filepath.Dir(c), 0o755)
		if err := os.WriteFile(c, []byte(contenu), 0o644); err != nil { // #nosec G306 -- test
			t.Fatal(err)
		}
	}
	io := "rchar: 999\nwchar: 999\nsyscr: 1\nsyscw: 1\nread_bytes: %d\nwrite_bytes: %d\ncancelled_write_bytes: 0\n"
	ecrire(dossierCgroup, "qemu.slice/114.scope/cgroup.procs", "4242\n")
	ecrire(dossierProc, "4242/io", sprintf(io, 1000, 2000))
	ecrire(dossierCgroup, "qemu.slice/202.scope/cgroup.procs", "10\n11\n") // deux processus
	ecrire(dossierProc, "10/io", sprintf(io, 1, 2))
	ecrire(dossierProc, "11/io", sprintf(io, 10, 20))
	ecrire(dossierCgroup, "qemu.slice/autre.scope/cgroup.procs", "12\n") // pas une VM
	ecrire(dossierProc, "12/io", sprintf(io, 5, 5))
	d, err := lireDisquesVM()
	if err != nil {
		t.Fatal(err)
	}
	if len(d) != 2 || d["114"] != (DisqueVM{1000, 2000}) || d["202"] != (DisqueVM{11, 22}) {
		t.Errorf("disques : %+v", d)
	}
}

func TestDemandeDisques(t *testing.T) {
	chemin := filepath.Join(t.TempDir(), "actualiser-racine")
	t0 := time.Now()
	if demandeDisques(chemin, t0) {
		t.Error("fichier absent : demande vue")
	}
	_ = os.WriteFile(chemin, []byte("disques 2026-11-13T10:00:00Z\n"), 0o600)
	if !demandeDisques(chemin, t0) {
		t.Error("demande « disques » récente non vue")
	}
	if demandeDisques(chemin, t0.Add(2*time.Minute)) {
		t.Error("demande vieille de 2 min prise (passage du minuteur : doit être complet)")
	}
	_ = os.WriteFile(chemin, []byte("2026-11-13T10:00:00Z\n"), 0o600) // « Actualiser » de l'interface
	if demandeDisques(chemin, t0) {
		t.Error("Actualiser pris pour un passage léger")
	}
	_ = os.WriteFile(chemin, []byte("disques\n"), 0o600)
	lien := chemin + ".lien"
	_ = os.Symlink(chemin, lien)
	if demandeDisques(lien, t0) {
		t.Error("lien suivi")
	}
}

// Lecture refusée (unité d'avant la 0.2.24) : message qui dit quoi faire.
func TestLireDisquesVMRefus(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root lit tout : refus impossible à simuler")
	}
	ac, ap := dossierCgroup, dossierProc
	t.Cleanup(func() { dossierCgroup, dossierProc = ac, ap })
	dossierCgroup, dossierProc = t.TempDir(), t.TempDir()
	_ = os.MkdirAll(filepath.Join(dossierCgroup, "qemu.slice/114.scope"), 0o755)
	_ = os.WriteFile(filepath.Join(dossierCgroup, "qemu.slice/114.scope/cgroup.procs"), []byte("7\n"), 0o644) // #nosec G306 -- test
	_ = os.MkdirAll(filepath.Join(dossierProc, "7"), 0o755)
	_ = os.WriteFile(filepath.Join(dossierProc, "7/io"), []byte("read_bytes: 1\nwrite_bytes: 1\n"), 0o000)
	if _, err := lireDisquesVM(); err == nil || !strings.Contains(err.Error(), "relancer le script") {
		t.Errorf("refus : %v", err)
	}
}
