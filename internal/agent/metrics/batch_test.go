// SPDX-License-Identifier: GPL-3.0-or-later

package metrics

import "testing"

func TestBatchReutilise(t *testing.T) {
	b := NewBatch(4, nil)
	b.Reset(1)
	b.Add("a", 1, L("x", "1"))
	b.Add("b", 2)
	if b.Len() != 2 || b.Samples[0].N != 1 || b.Samples[1].N != 0 {
		t.Fatalf("lot mal rempli : %+v", b.Samples)
	}
	capAvant := cap(b.Samples)
	b.Reset(2)
	if b.Len() != 0 || cap(b.Samples) != capAvant || b.TimestampMs != 2 {
		t.Error("Reset doit vider le lot en gardant sa mémoire")
	}
}

func TestBatchTropDEtiquettes(t *testing.T) {
	b := NewBatch(1, nil)
	b.Reset(0)
	trop := make([]Label, MaxLabels+1)
	b.Add("x", 1, trop...)
	if b.Len() != 0 || b.Ignored != 1 {
		t.Errorf("un point avec trop d'étiquettes doit être ignoré et compté")
	}
}

func TestInterner(t *testing.T) {
	in := NewInterner(2)
	a := in.Get([]byte("sda"))
	b := in.Get([]byte("sda"))
	if a != b || in.Len() != 1 {
		t.Error("même contenu → même chaîne attendue")
	}
	in.Get([]byte("sdb"))
	in.Get([]byte("sdc")) // dépasse la limite : l'interneur se vide puis repart
	if in.Len() > 2 {
		t.Errorf("l'interneur dépasse sa limite : %d", in.Len())
	}
}

func BenchmarkAdd(b *testing.B) {
	lot := NewBatch(1024, nil)
	in := NewInterner(64)
	dev := []byte("sda")
	b.ReportAllocs()
	for b.Loop() {
		lot.Reset(0)
		for i := 0; i < 500; i++ {
			lot.Add("node_disk_read_bytes_total", float64(i), L("device", in.Get(dev)))
		}
	}
}
