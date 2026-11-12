// SPDX-License-Identifier: GPL-3.0-or-later

package collect

import (
	"os"
	"path/filepath"
	"testing"

	"mirador-stack/internal/agent/metrics"
)

// arbre crée des fichiers d'exemple (chemin → contenu) sous un dossier de test.
func arbre(t *testing.T, fichiers map[string]string) string {
	t.Helper()
	racine := t.TempDir()
	for chemin, contenu := range fichiers {
		p := filepath.Join(racine, chemin)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(contenu), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return racine
}

func materielExemple(t *testing.T) string {
	return arbre(t, map[string]string{
		"sys/class/hwmon/hwmon0/name":        "nct6798\n",
		"sys/class/hwmon/hwmon0/fan1_input":  "1180\n",
		"sys/class/hwmon/hwmon0/fan2_input":  "0\n",
		"sys/class/hwmon/hwmon0/temp1_input": "41000\n",
		"sys/block/md0/md/degraded":          "1\n",
		"sys/block/md0/md/raid_disks":        "2\n",
		"proc/spl/kstat/zfs/rpool/state":     "ONLINE\n",
		"proc/spl/kstat/zfs/reserve/state":   "DEGRADED\n",
		"proc/spl/kstat/zfs/arcstats":        "sans rapport\n", // pas un pool
	})
}

// Éteint (défaut) : rien n'est mesuré, l'agent signale seulement ce qui existe.
func TestMaterielDetecte(t *testing.T) {
	r := materielExemple(t)
	p := collecter(t, NewMateriel(filepath.Join(r, "sys"), filepath.Join(r, "proc"), false, false, false))
	verifier(t, p, map[string]float64{
		"mirador_agent_detecte|genre=ventilateurs|nom=capteurs": 2,
		"mirador_agent_detecte|genre=zfs|nom=rpool":             1,
		"mirador_agent_detecte|genre=zfs|nom=reserve":           1,
		"mirador_agent_detecte|genre=mdraid|nom=md0":            1,
	})
	if len(p) != 4 {
		t.Errorf("4 séries attendues (détection seule), obtenu %d : %v", len(p), p)
	}
}

// Activé : les mesures partent, et plus aucune série de détection.
func TestMaterielActive(t *testing.T) {
	r := materielExemple(t)
	p := collecter(t, NewMateriel(filepath.Join(r, "sys"), filepath.Join(r, "proc"), true, true, true))
	verifier(t, p, map[string]float64{
		"node_hwmon_fan_rpm|chip=nct6798|sensor=fan1":       1180,
		"node_hwmon_fan_rpm|chip=nct6798|sensor=fan2":       0,
		"node_zfs_zpool_state|zpool=rpool|state=online":     1,
		"node_zfs_zpool_state|zpool=reserve|state=online":   0,
		"node_zfs_zpool_state|zpool=reserve|state=degraded": 1,
		"node_md_degraded|device=md0":                       1,
		"node_md_disks_required|device=md0":                 2,
	})
	for k := range p {
		if contient(k, MetriqueDetecte) {
			t.Errorf("série de détection inattendue quand tout est activé : %s", k)
		}
	}
}

// Machine sans ventilateur, ni ZFS, ni RAID (une VM) : aucune série.
func TestMaterielAbsent(t *testing.T) {
	r := arbre(t, map[string]string{"sys/class/hwmon/hwmon0/temp1_input": "41000\n"})
	if p := collecter(t, NewMateriel(filepath.Join(r, "sys"), filepath.Join(r, "proc"), true, true, true)); len(p) != 0 {
		t.Errorf("aucune série attendue, obtenu %v", p)
	}
}

// Validation service par service : seuls les services validés sont suivis,
// les autres sont proposés (sauf les services de base du système).
func TestSystemdValidation(t *testing.T) {
	src := sourceExemple()
	src.actives = append(src.actives, "systemd-timesyncd.service")
	src.props["systemd-timesyncd.service"] = proprietesService{Type: "simple", ConditionOK: true, Etat: "active"}
	s := newSystemdAvecSource(src, nil)
	s.Valider([]string{"ssh.service"})
	p := collecter(t, s)
	verifier(t, p, map[string]float64{
		"node_systemd_unit_enabled_active|name=ssh.service":        1,
		"node_systemd_enabled_units_total":                         1,
		"mirador_agent_detecte|genre=service|nom=cron.service":     1,
		"mirador_agent_detecte|genre=service|nom=nginx.service":    1,
		"mirador_agent_detecte|genre=service|nom=monappli.service": 1,
	})
	for _, absent := range []string{
		"node_systemd_unit_enabled_active|name=cron.service",                // pas validé : pas surveillé
		"mirador_agent_detecte|genre=service|nom=ssh.service",               // déjà validé
		"mirador_agent_detecte|genre=service|nom=systemd-timesyncd.service", // service de base
		"mirador_agent_detecte|genre=service|nom=zfs-import-cache.service",  // oneshot : ne « devrait » pas tourner
	} {
		if _, ok := p[absent]; ok {
			t.Errorf("série inattendue : %s", absent)
		}
	}
	// L'échec d'une unité reste suivi, validée ou non.
	if p["node_systemd_unit_state|name=cron.service|state=failed"] != 1 {
		t.Error("node_systemd_unit_state doit rester envoyé pour toutes les unités")
	}
}

// Coût d'un passage (toutes les 60 s) : tout activé, puis détection seule.
func BenchmarkMateriel(b *testing.B) {
	for _, cas := range []struct {
		nom   string
		actif bool
	}{{"active", true}, {"detection", false}} {
		b.Run(cas.nom, func(b *testing.B) {
			r := b.TempDir()
			for chemin, contenu := range map[string]string{
				"sys/class/hwmon/hwmon0/name": "nct6798\n", "sys/class/hwmon/hwmon0/fan1_input": "1180\n",
				"sys/class/hwmon/hwmon0/fan2_input": "900\n", "sys/block/md0/md/degraded": "0\n",
				"sys/block/md0/md/raid_disks": "2\n", "proc/spl/kstat/zfs/rpool/state": "ONLINE\n",
			} {
				p := filepath.Join(r, chemin)
				_ = os.MkdirAll(filepath.Dir(p), 0o750)
				_ = os.WriteFile(p, []byte(contenu), 0o600)
			}
			m := NewMateriel(filepath.Join(r, "sys"), filepath.Join(r, "proc"), cas.actif, cas.actif, cas.actif)
			lot := metrics.NewBatch(64, nil)
			b.ReportAllocs()
			for b.Loop() {
				lot.Reset(0)
				_ = m.Collecter(lot)
			}
		})
	}
}
