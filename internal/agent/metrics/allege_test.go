// SPDX-License-Identifier: GPL-3.0-or-later

package metrics

import "testing"

// Envoi allégé (SPEC § 100) : jauge inchangée sautée, renvoyée après la
// garde ou dès qu'elle change ; compteurs, uname et métriques de l'agent
// toujours envoyés.
func TestAllegeur(t *testing.T) {
	a := NouvelAllegeur(120_000)
	lot := func(ts int64, jauge float64) *Batch {
		b := NewBatch(16, nil)
		b.Reset(ts)
		b.Add("node_memory_MemTotal_bytes", 1e9)
		b.Add("node_load1", jauge)
		b.Add("node_cpu_seconds_total", 5, L("cpu", "0"))
		b.Add("node_uname_info", 1, L("release", "6.1"))
		b.Add("mirador_agent_info", 1)
		return b
	}
	noms := func(b *Batch) (s string) {
		for _, x := range b.Samples {
			s += x.Name + " "
		}
		return s
	}
	b := lot(0, 1)
	if a.Filtrer(b) != 0 || len(b.Samples) != 5 {
		t.Fatalf("premier lot : %s", noms(b))
	}
	b = lot(30_000, 1)
	if a.Filtrer(b) != 2 || noms(b) != "node_cpu_seconds_total node_uname_info mirador_agent_info " {
		t.Fatalf("30 s : %s", noms(b))
	}
	b = lot(60_000, 2) // node_load1 change : envoyée
	if a.Filtrer(b) != 1 || noms(b) != "node_load1 node_cpu_seconds_total node_uname_info mirador_agent_info " {
		t.Fatalf("changement : %s", noms(b))
	}
	b = lot(121_000, 2) // MemTotal : garde de 2 min passée
	if a.Filtrer(b) != 1 || noms(b) != "node_memory_MemTotal_bytes node_cpu_seconds_total node_uname_info mirador_agent_info " {
		t.Fatalf("après la garde : %s", noms(b))
	}
	if a.Economises != 4 {
		t.Errorf("économisés : %d", a.Economises)
	}
}

func BenchmarkAllegeur(b *testing.B) {
	a := NouvelAllegeur(120_000)
	lot := NewBatch(1024, nil)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		lot.Reset(int64(i) * 30_000)
		for k := 0; k < 300; k++ {
			lot.Add("node_x", float64(k%7), L("k", "v"))
		}
		a.Filtrer(lot)
	}
}

// Relecture du 21/11 : horloge reculée → les jauges stables repartent.
func TestAllegeurHorlogeReculee(t *testing.T) {
	a := NouvelAllegeur(120_000)
	lot := func(ms int64) *Batch {
		b := NewBatch(1, nil)
		b.Reset(ms)
		b.Add("node_memory_MemTotal_bytes", 42)
		return b
	}
	a.Filtrer(lot(10_000_000))
	b := lot(10_000_000 - 3_600_000) // NTP recule l'horloge d'une heure
	a.Filtrer(b)
	if len(b.Samples) != 1 {
		t.Error("jauge stable non envoyée après un recul d'horloge")
	}
}
