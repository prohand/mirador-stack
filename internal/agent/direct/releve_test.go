// SPDX-License-Identifier: GPL-3.0-or-later

package direct

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Faux /proc et /sys : deux relevés à une seconde d'écart.
func TestRelever(t *testing.T) {
	dir := t.TempDir()
	proc, sys := filepath.Join(dir, "proc"), filepath.Join(dir, "sys")
	ecrire := func(chemin, contenu string) {
		_ = os.MkdirAll(filepath.Dir(chemin), 0o755)
		if err := os.WriteFile(chemin, []byte(contenu), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// eno1 physique, vmbr0 (pont : double eno1) et tap100i0 ignorés.
	_ = os.MkdirAll(filepath.Join(sys, "class/net/eno1/device"), 0o755)
	_ = os.MkdirAll(filepath.Join(sys, "class/net/vmbr0"), 0o755)
	_ = os.MkdirAll(filepath.Join(sys, "block/sda/device"), 0o755)
	ecrire(filepath.Join(dir, "passwd"), "root:x:0:0:root:/root:/bin/bash\n")
	etape := func(cpu, idle, rx, tx, lu, ecr, ticksKvm string) {
		ecrire(filepath.Join(proc, "stat"), "cpu  "+cpu+" 0 0 "+idle+" 0 0 0 0 0 0\ncpu0 1 0 0 1\ncpu1 1 0 0 1\nintr 0\n")
		ecrire(filepath.Join(proc, "loadavg"), "2.90 2.10 1.50 3/400 1234\n")
		ecrire(filepath.Join(proc, "meminfo"), "MemTotal:       32000000 kB\nMemAvailable:   12000000 kB\n")
		ecrire(filepath.Join(proc, "net/dev"), "Inter-|   Receive\n face |bytes\n    lo: 999 0 0 0 0 0 0 0 999 0 0 0 0 0 0 0\n  eno1: "+rx+" 0 0 0 0 0 0 0 "+tx+" 0 0 0 0 0 0 0\n vmbr0: "+rx+" 0 0 0 0 0 0 0 "+tx+" 0 0 0 0 0 0 0\ntap100i0: 5 0 0 0 0 0 0 0 5 0 0 0 0 0 0 0\n")
		ecrire(filepath.Join(proc, "diskstats"), "   8       0 sda 1 0 "+lu+" 0 1 0 "+ecr+" 0 0 0 0\n   8       1 sda1 1 0 "+lu+" 0 1 0 "+ecr+" 0 0 0 0\n 253       0 dm-0 1 0 "+lu+" 0 1 0 "+ecr+" 0 0 0 0\n")
		// kvm (VM 102) : nom avec espace et parenthèse, comme le noyau peut en donner.
		ecrire(filepath.Join(proc, "2210/stat"), "2210 (kvm (vm) x) S 1 1 1 0 -1 0 0 0 0 0 "+ticksKvm+" 0 0 0 20 0 1 0 100 1000 2048\n")
		ecrire(filepath.Join(proc, "2210/cgroup"), "0::/qemu.slice/102.scope\n")
		ecrire(filepath.Join(proc, "77/stat"), "77 (sshd) S 1 1 1 0 -1 0 0 0 0 0 1 1 0 0 20 0 1 0 100 1000 512\n")
		ecrire(filepath.Join(proc, "77/cgroup"), "0::/system.slice/ssh.service\n")
	}
	r := &Releveur{Proc: proc, Sys: sys, Passwd: filepath.Join(dir, "passwd")}
	t0 := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	etape("1000", "9000", "1000", "500", "100", "50", "100")
	d := r.Relever(t0)
	if d.CPU != 0 || d.NetRecu != 0 || d.Coeurs != 2 || d.Charge != 2.9 || d.MemTotale != 32000000*1024 || d.MemUtilisee != 20000000*1024 {
		t.Fatalf("premier relevé : %+v", d)
	}
	// Une seconde : 100 ticks dont 40 de travail ; kvm +50 ticks (50 % d'un cœur).
	etape("1040", "9060", "3001000", "1000500", "2148", "1074", "150")
	d = r.Relever(t0.Add(time.Second))
	if d.CPU < 39.9 || d.CPU > 40.1 {
		t.Errorf("CPU %.1f (40 attendu)", d.CPU)
	}
	if d.NetRecu != 3000000 || d.NetEnvoye != 1000000 {
		t.Errorf("réseau %v / %v (eno1 seule attendue)", d.NetRecu, d.NetEnvoye)
	}
	if d.DisqueLu != 2048*512 || d.DisqueEcrit != 1024*512 {
		t.Errorf("disques %v / %v (sda seul attendu)", d.DisqueLu, d.DisqueEcrit)
	}
	if len(d.Processus) != 2 {
		t.Fatalf("processus : %+v", d.Processus)
	}
	p := d.Processus[0]
	if p.PID != 2210 || p.Nom != "kvm (vm) x" || p.CPU != 50 || p.Groupe != "VM 102" || p.Memoire != 2048*float64(os.Getpagesize()) {
		t.Errorf("premier processus : %+v", p)
	}
	if p.Utilisateur == "" {
		t.Error("utilisateur vide")
	}
}

func TestGroupeCgroup(t *testing.T) {
	for c, att := range map[string]string{
		"0::/qemu.slice/105.scope\n":                         "VM 105",
		"0::/lxc/200/ns/system.slice\n":                      "CT 200",
		"0::/lxc.payload.201/init.scope\n":                   "CT 201",
		"0::/system.slice/docker-3f2a1b4c5d6e7f8a9b.scope\n": "docker 3f2a1b4c5d6e",
		"0::/user.slice\n":                                   "",
	} {
		if g := groupeCgroup(c); g != att {
			t.Errorf("%q : %q", strings.TrimSpace(c), g)
		}
	}
}

// Coût d'un relevé sur la machine qui fait les tests (vrai /proc).
func BenchmarkRelever(b *testing.B) {
	r := Nouveau()
	t := time.Now()
	r.Relever(t)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		t = t.Add(time.Second)
		r.Relever(t)
	}
}
