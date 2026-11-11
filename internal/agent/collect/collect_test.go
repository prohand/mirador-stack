// SPDX-License-Identifier: GPL-3.0-or-later

package collect

import (
	"errors"
	"syscall"
	"testing"
	"time"

	"mirador-stack/internal/agent/config"
	"mirador-stack/internal/agent/metrics"
)

const (
	procTest = "testdata/proc"
	sysTest  = "testdata/sys"
)

// cle : nom de métrique + étiquettes, pour retrouver un point dans le lot.
func cle(s *metrics.Sample) string {
	k := s.Name
	for _, l := range s.Labels[:s.N] {
		k += "|" + l.Name + "=" + l.Value
	}
	return k
}

// collecter lance un collecteur et renvoie ses points sous forme de table.
func collecter(t *testing.T, c Collecteur) map[string]float64 {
	t.Helper()
	b := metrics.NewBatch(64, nil)
	b.Reset(0)
	if err := c.Collecter(b); err != nil {
		t.Fatalf("%s : %v", c.Nom(), err)
	}
	out := map[string]float64{}
	for i := range b.Samples {
		k := cle(&b.Samples[i])
		if _, existe := out[k]; existe {
			t.Errorf("%s : point en double %s", c.Nom(), k)
		}
		out[k] = b.Samples[i].Value
	}
	return out
}

// verifier compare les valeurs attendues (les autres points sont ignorés).
func verifier(t *testing.T, obtenu map[string]float64, attendu map[string]float64) {
	t.Helper()
	for k, v := range attendu {
		got, ok := obtenu[k]
		if !ok {
			t.Errorf("point absent : %s", k)
			continue
		}
		if got != v {
			t.Errorf("%s = %v, attendu %v", k, got, v)
		}
	}
}

func TestCPU(t *testing.T) {
	p := collecter(t, NewCPU(procTest))
	if len(p) != 2*8+1 {
		t.Errorf("%d points, attendu 17 (2 CPU × 8 modes + btime)", len(p))
	}
	verifier(t, p, map[string]float64{
		"node_cpu_seconds_total|cpu=0|mode=user":   23.50,
		"node_cpu_seconds_total|cpu=1|mode=idle":   81.25,
		"node_cpu_seconds_total|cpu=1|mode=iowait": 2.60,
		"node_boot_time_seconds":                   1762500000,
	})
}

func TestChargeMemoireVmstat(t *testing.T) {
	verifier(t, collecter(t, NewCharge(procTest)), map[string]float64{
		"node_load1": 0.42, "node_load5": 1.05, "node_load15": 0.98,
	})
	m := collecter(t, NewMemoire(procTest))
	if len(m) != 6 {
		t.Errorf("mémoire : %d points, attendu 6", len(m))
	}
	verifier(t, m, map[string]float64{
		"node_memory_MemTotal_bytes":     4028292 * 1024,
		"node_memory_MemAvailable_bytes": 2516732 * 1024,
		"node_memory_SwapFree_bytes":     1048060 * 1024,
	})
	verifier(t, collecter(t, NewVMStat(procTest)), map[string]float64{
		"node_vmstat_pswpin": 12, "node_vmstat_pswpout": 34, "node_vmstat_oom_kill": 2,
	})
}

func TestPression(t *testing.T) {
	verifier(t, collecter(t, NewPression(procTest)), map[string]float64{
		"node_pressure_cpu_waiting_seconds_total":    1.5,
		"node_pressure_io_stalled_seconds_total":     1.25,
		"node_pressure_memory_stalled_seconds_total": 0.0001,
	})
	// Sans PSI (dossier absent) : aucun point, aucune erreur.
	if p := collecter(t, NewPression("testdata/absent")); len(p) != 0 {
		t.Errorf("sans PSI, aucun point attendu : %v", p)
	}
}

func TestDisquesReseau(t *testing.T) {
	def := config.Defaut()
	d := collecter(t, NewDisques(procTest, def.ExclureDisques))
	verifier(t, d, map[string]float64{
		"node_disk_read_bytes_total|device=sda":      1203450 * 512,
		"node_disk_written_bytes_total|device=sda":   2048000 * 512,
		"node_disk_io_time_seconds_total|device=sda": 35.21,
	})
	for k := range d {
		if contient(k, "loop0") || contient(k, "dm-0") {
			t.Errorf("disque exclu présent : %s", k)
		}
	}
	r := collecter(t, NewReseau(procTest, def.ExclureInterfaces))
	if len(r) != 6 {
		t.Errorf("réseau : %d points, attendu 6 (eth0 seule)", len(r))
	}
	verifier(t, r, map[string]float64{
		"node_network_receive_bytes_total|device=eth0":  98765432,
		"node_network_receive_errs_total|device=eth0":   3,
		"node_network_receive_drop_total|device=eth0":   7,
		"node_network_transmit_bytes_total|device=eth0": 12345678,
		"node_network_transmit_drop_total|device=eth0":  2,
	})
}

func contient(s, sous string) bool {
	for i := 0; i+len(sous) <= len(s); i++ {
		if s[i:i+len(sous)] == sous {
			return true
		}
	}
	return false
}

// statfsFactice simule statfs : 4 Kio par bloc, valeurs fixes ; /mnt/nas bloque.
func statfsFactice(bloque chan struct{}) statfsFunc {
	return func(chemin string, st *syscall.Statfs_t) error {
		switch chemin {
		case "/mnt/nas":
			<-bloque
			return errors.New("serveur NFS mort")
		case "/", "/mnt/mon disque", "/run":
			*st = syscall.Statfs_t{Bsize: 4096, Blocks: 1000, Bavail: 250, Files: 500, Ffree: 100}
			return nil
		}
		return errors.New("inconnu")
	}
}

func TestSystemesFichiers(t *testing.T) {
	def := config.Defaut()
	s := NewSystemesFichiers(procTest, def.ExclureMontages, def.ExclureTypesFS, def.ExclureTypesFSDistant)
	bloque := make(chan struct{})
	defer close(bloque)
	s.statfs = statfsFactice(bloque)
	s.delaiDistant = 50 * time.Millisecond

	debut := time.Now()
	p := collecter(t, s)
	if time.Since(debut) > 2*time.Second {
		t.Error("un montage NFS bloqué ne doit pas bloquer la collecte")
	}
	verifier(t, p, map[string]float64{
		"node_filesystem_size_bytes|device=/dev/sda1|fstype=ext4|mountpoint=/":               4096000,
		"node_filesystem_avail_bytes|device=/dev/sda1|fstype=ext4|mountpoint=/":              1024000,
		"node_filesystem_files_free|device=/dev/sdb1|fstype=ext4|mountpoint=/mnt/mon disque": 100,
		"node_filesystem_files|device=tmpfs|fstype=tmpfs|mountpoint=/run":                    500,
	})
	for k := range p {
		for _, interdit := range []string{"mountpoint=/proc", "mountpoint=/sys", "fstype=overlay", "mountpoint=/mnt/nas"} {
			if contient(k, interdit) {
				t.Errorf("montage exclu ou bloqué présent : %s", k)
			}
		}
	}
	// Second cycle : le montage NFS est toujours bloqué, il est sauté sans attendre.
	debut = time.Now()
	collecter(t, s)
	if time.Since(debut) > 40*time.Millisecond {
		t.Error("un montage déjà bloqué ne doit pas être réinterrogé")
	}
}

func TestTemperatures(t *testing.T) {
	p := collecter(t, NewTemperatures(sysTest))
	verifier(t, p, map[string]float64{
		"node_hwmon_temp_celsius|chip=coretemp|sensor=temp1": 45,
		"node_hwmon_temp_celsius|chip=coretemp|sensor=temp2": 47.5,
		"node_thermal_zone_temp|type=x86_pkg_temp|zone=0":    51,
	})
}

func TestUname(t *testing.T) {
	u := NewUname()
	u.maintenant = func() time.Time { return time.Unix(1762500000, 500_000_000) }
	p := collecter(t, u)
	if p["node_time_seconds"] != 1762500000.5 {
		t.Errorf("node_time_seconds = %v", p["node_time_seconds"])
	}
	trouve := false
	for k, v := range p {
		if contient(k, "node_uname_info|") && contient(k, "sysname=Linux") && v == 1 {
			trouve = true
		}
	}
	if !trouve {
		t.Errorf("node_uname_info absent ou incomplet : %v", p)
	}
}

func TestDecimal(t *testing.T) {
	for entree, attendu := range map[string]float64{"0.42": 0.42, "12": 12, "3.005": 3.005} {
		if v, ok := decimal([]byte(entree)); !ok || v != attendu {
			t.Errorf("decimal(%q) = %v, %v", entree, v, ok)
		}
	}
	for _, mauvais := range []string{"", "a", "1.a", "-1", "1.2.3"} {
		if _, ok := decimal([]byte(mauvais)); ok {
			t.Errorf("decimal(%q) aurait dû échouer", mauvais)
		}
	}
}

// FuzzLecturesProc : les analyseurs ne doivent jamais paniquer sur un contenu
// inattendu (fichier tronqué, noyau différent…).
func FuzzLecturesProc(f *testing.F) {
	f.Add([]byte("cpu0 1 2 3 4 5 6 7 8\nbtime 12\n"))
	f.Add([]byte("  eth0: 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16\n"))
	f.Add([]byte("8 0 sda 1 2 3 4 5 6 7 8 9 10 11\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		for len(data) > 0 {
			var ligne []byte
			ligne, data = ligneSuivante(data)
			for len(ligne) > 0 {
				var c []byte
				c, ligne = champSuivant(ligne)
				entier(c)
				decimal(c)
				entierSigne(c)
				if len(c) == 0 {
					break
				}
			}
		}
		decoderOctal(string(data))
	})
}
