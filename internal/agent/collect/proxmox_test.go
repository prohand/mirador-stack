// SPDX-License-Identifier: GPL-3.0-or-later

package collect

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"mirador-stack/internal/agent/metrics"
	"mirador-stack/internal/agent/racine"
)

// ecrireArbre : crée les fichiers d'un faux /sys (chemin relatif → contenu).
func ecrireArbre(t *testing.T, racine string, fichiers map[string]string) {
	t.Helper()
	for chemin, contenu := range fichiers {
		complet := filepath.Join(racine, chemin)
		if err := os.MkdirAll(filepath.Dir(complet), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(complet, []byte(contenu), 0o644); err != nil { // #nosec G306 -- test
			t.Fatal(err)
		}
	}
}

func TestProxmox(t *testing.T) {
	sys := t.TempDir()
	if ProxmoxPresent(sys) {
		t.Fatal("Proxmox détecté sans dossier d'invités")
	}
	pression := "some avg10=0.00 avg60=0.00 avg300=0.00 total=2500000\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=500000\n"
	ecrireArbre(t, sys, map[string]string{
		// VM 100 : deux disques, cache inactif retiré de la mémoire.
		"fs/cgroup/qemu.slice/100.scope/cpu.stat":        "usage_usec 12500000\nuser_usec 10000000\nsystem_usec 2500000\n",
		"fs/cgroup/qemu.slice/100.scope/memory.current":  "2147483648\n",
		"fs/cgroup/qemu.slice/100.scope/memory.stat":     "anon 2000000000\nfile 147483648\ninactive_file 147483648\n",
		"fs/cgroup/qemu.slice/100.scope/io.stat":         "8:0 rbytes=1000 wbytes=2000 rios=1 wios=2 dbytes=0 dios=0\n8:16 rbytes=500 wbytes=0 rios=1 wios=0 dbytes=0 dios=0\n",
		"fs/cgroup/qemu.slice/100.scope/cpu.pressure":    pression,
		"fs/cgroup/qemu.slice/100.scope/io.pressure":     pression,
		"fs/cgroup/qemu.slice/100.scope/memory.pressure": pression,
		// Pas des invités : ignorés.
		"fs/cgroup/qemu.slice/autre.scope/cpu.stat": "usage_usec 1\n",
		"fs/cgroup/lxc/ns/cpu.stat":                 "usage_usec 1\n",
		// LXC 101 : pas de PSI ni io.stat (noyau ou réglage) : seulement le reste.
		"fs/cgroup/lxc/101/cpu.stat":       "usage_usec 3000000\n",
		"fs/cgroup/lxc/101/memory.current": "104857600\n",
		// Réseau : vu de l'hôte (tx = reçu par l'invité), deux cartes pour la VM.
		"class/net/tap100i0/statistics/tx_bytes":  "1000\n",
		"class/net/tap100i0/statistics/rx_bytes":  "200\n",
		"class/net/tap100i1/statistics/tx_bytes":  "10\n",
		"class/net/tap100i1/statistics/rx_bytes":  "2\n",
		"class/net/veth101i0/statistics/tx_bytes": "7\n",
		"class/net/veth101i0/statistics/rx_bytes": "3\n",
		"class/net/vmbr0/statistics/tx_bytes":     "99\n",
	})
	// VM sans io.stat (cas réel : Proxmox n'active pas le contrôleur io pour
	// les VM) : disques lus sur les volumes ZFS (102) et LVM (103).
	ecrireArbre(t, sys, map[string]string{
		"fs/cgroup/qemu.slice/102.scope/cpu.stat": "usage_usec 1000000\n",
		"fs/cgroup/qemu.slice/103.scope/cpu.stat": "usage_usec 1000000\n",
		// stat : lectures, fusions, SECTEURS LUS, ms, écritures, fusions, SECTEURS ÉCRITS…
		"block/zd16/stat":    "     100        0     2000      10      50        0     4000      20        0      30      30\n",
		"block/zd32/stat":    "       1        0        8       1       1        0        8       1        0       2       2\n",
		"block/dm-3/stat":    "10 0 100 1 10 0 200 1 0 2 2\n",
		"block/dm-3/dm/name": "pve-vm--103--disk--0\n",
		"block/dm-4/stat":    "10 0 999 1 10 0 999 1 0 2 2\n",
		"block/dm-4/dm/name": "pve-root\n",
	})
	dev := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dev, "zvol", "rpool", "data"), 0o755)
	for nom, cible := range map[string]string{
		"vm-102-disk-0":       "../../../zd16",
		"vm-102-disk-1":       "../../../zd32",
		"vm-102-disk-0-part1": "../../../zd16p1", // partition : ignorée
		"base-900-disk-0":     "../../../zd48",   // modèle : ignoré
	} {
		if err := os.Symlink(cible, filepath.Join(dev, "zvol", "rpool", "data", nom)); err != nil {
			t.Fatal(err)
		}
	}
	if !ProxmoxPresent(sys) {
		t.Fatal("Proxmox non détecté")
	}
	// VM 104 : disque en fichier (qcow2) : ni io.stat ni volume ; mesure de
	// l'assistant root (/proc/<pid>/io).
	ecrireArbre(t, sys, map[string]string{"fs/cgroup/qemu.slice/104.scope/cpu.stat": "usage_usec 1\n"})
	px := NewProxmox(sys)
	px.dev = dev
	px.DisquesRacine = func() map[string]racine.DisqueVM {
		return map[string]racine.DisqueVM{"104": {Lus: 7, Ecrits: 9}, "100": {Lus: 1, Ecrits: 1}}
	}
	obtenu := collecter(t, px)
	vm := "|id=100|type=qemu"
	ct := "|id=101|type=lxc"
	verifier(t, obtenu, map[string]float64{
		"mirador_pve_invite_en_marche" + vm:                                                 1,
		"mirador_pve_invite_en_marche" + ct:                                                 1,
		"mirador_pve_invite_cpu_secondes_total" + vm:                                        12.5,
		"mirador_pve_invite_cpu_secondes_total" + ct:                                        3,
		"mirador_pve_invite_memoire_octets" + vm:                                            2000000000,
		"mirador_pve_invite_memoire_octets" + ct:                                            104857600,
		"mirador_pve_invite_disque_lus_octets_total" + vm:                                   1500,
		"mirador_pve_invite_disque_ecrits_octets_total" + vm:                                2000,
		"mirador_pve_invite_pression_secondes_total" + vm + "|ressource=cpu|niveau=some":    2.5,
		"mirador_pve_invite_pression_secondes_total" + vm + "|ressource=memory|niveau=full": 0.5,
		"mirador_pve_invite_reseau_recus_octets_total" + vm:                                 1010,
		"mirador_pve_invite_reseau_envoyes_octets_total" + vm:                               202,
		"mirador_pve_invite_reseau_recus_octets_total" + ct:                                 7,
		"mirador_pve_invite_reseau_envoyes_octets_total" + ct:                               3,
		"mirador_pve_invite_disque_lus_octets_total|id=102|type=qemu":                       (2000 + 8) * 512,
		"mirador_pve_invite_disque_ecrits_octets_total|id=102|type=qemu":                    (4000 + 8) * 512,
		"mirador_pve_invite_disque_lus_octets_total|id=103|type=qemu":                       100 * 512,
		"mirador_pve_invite_disque_ecrits_octets_total|id=103|type=qemu":                    200 * 512,
		"mirador_pve_invite_disque_lus_octets_total|id=104|type=qemu":                       7,
		"mirador_pve_invite_disque_ecrits_octets_total|id=104|type=qemu":                    9,
		// VM 100 : io.stat du cgroup prioritaire sur la mesure de l'assistant.
		"mirador_pve_invite_disque_lus_octets_total|id=100|type=qemu": 1500,
	})
	// VM : en marche, CPU, mémoire, 2 disque, 5 pression, 2 réseau = 12 ;
	// LXC : en marche, CPU, mémoire, 2 réseau = 5.
	// + VM 102, 103 et 104 : en marche, CPU, 2 disque = 4 chacune.
	if len(obtenu) != 29 {
		t.Errorf("%d points : %v", len(obtenu), obtenu)
	}
	for k := range obtenu {
		if strings.Contains(k, "id=autre") || strings.Contains(k, "id=ns") {
			t.Errorf("faux invité : %s", k)
		}
	}
}

// Coût : aucune allocation par point au-delà de la lecture des dossiers.
func BenchmarkProxmox(b *testing.B) {
	sys := b.TempDir()
	f := map[string]string{}
	for i := 100; i < 130; i++ { // 30 invités, comme un gros nœud
		d := "fs/cgroup/qemu.slice/" + strconv.Itoa(i) + ".scope/"
		f[d+"cpu.stat"] = "usage_usec 12500000\n"
		f[d+"memory.current"] = "2147483648\n"
		f[d+"memory.stat"] = "inactive_file 1000\n"
		f[d+"io.stat"] = "8:0 rbytes=1000 wbytes=2000 rios=1 wios=2 dbytes=0 dios=0\n"
		f[d+"cpu.pressure"] = "some avg10=0.00 avg60=0.00 avg300=0.00 total=2500000\n"
		f["class/net/tap"+strconv.Itoa(i)+"i0/statistics/tx_bytes"] = "1\n"
		f["class/net/tap"+strconv.Itoa(i)+"i0/statistics/rx_bytes"] = "1\n"
	}
	t := &testing.T{}
	ecrireArbre(t, sys, f)
	p := NewProxmox(sys)
	lot := metrics.NewBatch(1024, nil)
	b.ReportAllocs()
	for b.Loop() {
		lot.Reset(0)
		_ = p.Collecter(lot)
	}
}
