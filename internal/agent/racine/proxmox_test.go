// SPDX-License-Identifier: GPL-3.0-or-later

package racine

import (
	"os"
	"path/filepath"
	"testing"
)

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
