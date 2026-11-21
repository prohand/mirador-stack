// SPDX-License-Identifier: GPL-3.0-or-later

package snmp

import (
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/gosnmp/gosnmp"

	"mirador-stack/internal/agent/metrics"
)

// Équipement simulé : chaque colonne de table a n lignes (n < 0 : sans fin,
// équipement bogué). Les marches ne demandent que des colonnes.
type tables struct {
	n      int
	marche []string
}

func (s *tables) Get(oids []string) (*gosnmp.SnmpPacket, error) {
	p := &gosnmp.SnmpPacket{}
	for _, o := range oids {
		p.Variables = append(p.Variables, gosnmp.SnmpPDU{Name: "." + o, Type: gosnmp.Integer, Value: 1})
	}
	return p, nil
}
func (s *tables) BulkWalkAll(string) ([]gosnmp.SnmpPDU, error) { return nil, nil }
func (s *tables) BulkWalk(racine string, f gosnmp.WalkFunc) error {
	s.marche = append(s.marche, racine)
	for i := 1; s.n < 0 || i <= s.n; i++ {
		if err := f(gosnmp.SnmpPDU{Name: fmt.Sprintf(".%s.%d", racine, i), Type: gosnmp.Counter32, Value: uint(i)}); err != nil {
			return err
		}
	}
	return nil
}

// Relecture du 21/11 : grand switch (1 700 interfaces) : chaque colonne
// utile est complète, et seules les colonnes utiles sont demandées.
func TestColonnesCompletes(t *testing.T) {
	src := &tables{n: 1700}
	b := metrics.NewBatch(256, nil)
	b.Reset(0)
	if err := Releve(src, "standard", b); err != nil {
		t.Fatal(err)
	}
	n := map[string]int{}
	for _, s := range b.Samples[:b.Len()] {
		n[s.Name]++
	}
	for _, nom := range []string{"ifInDiscards", "ifInErrors", "ifOutDiscards", "ifOutErrors", "ifHCInOctets", "ifHCOutOctets"} {
		if n[nom] != 1700 {
			t.Errorf("%s : %d lignes sur 1700", nom, n[nom])
		}
	}
	for _, r := range src.marche {
		if strings.HasSuffix(r, ".2.2.1") || strings.HasSuffix(r, ".31.1.1.1") {
			t.Errorf("entrée de table entière demandée : %s", r)
		}
	}
}

// Relecture du 21/11 : équipement bogué (tables sans fin) : le relevé
// reste borné en mémoire et le dit (tables incomplètes).
func TestEquipementBogueBorne(t *testing.T) {
	var m0, m1 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m0)
	b := metrics.NewBatch(256, nil)
	b.Reset(0)
	_ = Releve(&tables{n: -1}, "synology", b)
	runtime.ReadMemStats(&m1)
	if alloue := m1.TotalAlloc - m0.TotalAlloc; alloue > 40<<20 || b.Len() > maxReponsesReleve+100 {
		t.Errorf("%d Mio alloués, %d points", alloue>>20, b.Len())
	}
	incomplet := false
	for _, s := range b.Samples[:b.Len()] {
		if s.Name == "mirador_snmp_tables_incompletes" && s.Value > 0 {
			incomplet = true
		}
	}
	if !incomplet {
		t.Error("tables coupées non signalées")
	}
}
