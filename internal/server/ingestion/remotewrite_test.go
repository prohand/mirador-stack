// SPDX-License-Identifier: GPL-3.0-or-later

package ingestion

import (
	"encoding/binary"
	"errors"
	"math"
	"testing"

	"github.com/klauspost/compress/snappy"

	"mirador-stack/internal/agent/metrics"
	"mirador-stack/internal/agent/remotewrite"
)

// --- Petit encodeur protobuf pour fabriquer des cas à la main ---

func pbChaine(dst []byte, champ int, s string) []byte {
	dst = append(dst, byte(champ<<3|2))
	dst = binary.AppendUvarint(dst, uint64(len(s)))
	return append(dst, s...)
}

func pbMessage(dst []byte, champ int, contenu []byte) []byte {
	dst = append(dst, byte(champ<<3|2))
	dst = binary.AppendUvarint(dst, uint64(len(contenu)))
	return append(dst, contenu...)
}

func pbLabel(nom, val string) []byte { return pbChaine(pbChaine(nil, 1, nom), 2, val) }

func pbPoint(v float64, t int64) []byte {
	b := []byte{1<<3 | 1}
	b = binary.LittleEndian.AppendUint64(b, math.Float64bits(v))
	b = append(b, 2<<3|0)
	return binary.AppendUvarint(b, uint64(t))
}

func pbSerie(labels [][2]string, points ...[]byte) []byte {
	var b []byte
	for _, l := range labels {
		b = pbMessage(b, 1, pbLabel(l[0], l[1]))
	}
	for _, p := range points {
		b = pbMessage(b, 2, p)
	}
	return b
}

func pbRequete(series ...[]byte) []byte {
	var b []byte
	for _, s := range series {
		b = pbMessage(b, 1, s)
	}
	return snappy.Encode(nil, b)
}

// Le décodeur relit exactement ce que l'encodeur de l'agent produit.
func TestAllerRetourAgent(t *testing.T) {
	lot := metrics.NewBatch(4, []metrics.Label{metrics.L("instance", "pc"), metrics.L("job", "integrations/unix")})
	lot.Reset(1_700_000_000_123)
	lot.Add("node_load1", 0.5)
	lot.Add("node_filesystem_avail_bytes", 1e9, metrics.L("mountpoint", "/"), metrics.L("fstype", "ext4"))
	var enc remotewrite.Encoder
	corps := enc.Encode(nil, lot)

	series, ignorees, err := DecoderRemoteWrite(corps, LimitesDecodageDefaut())
	if err != nil || ignorees != 0 || len(series) != 2 {
		t.Fatalf("séries %d, ignorées %d, erreur %v", len(series), ignorees, err)
	}
	s := series[1]
	if s.Valeur("__name__") != "node_filesystem_avail_bytes" || s.Valeur("mountpoint") != "/" || s.Valeur("instance") != "pc" {
		t.Errorf("étiquettes : %+v", s.Labels)
	}
	for i := 1; i < len(s.Labels); i++ {
		if s.Labels[i-1].Nom >= s.Labels[i].Nom {
			t.Errorf("étiquettes non triées : %+v", s.Labels)
		}
	}
	if len(s.Points) != 1 || s.Points[0].V != 1e9 || s.Points[0].T != 1_700_000_000_123 {
		t.Errorf("points : %+v", s.Points)
	}
}

func TestSeriesNonConformesIgnorees(t *testing.T) {
	p := pbPoint(1, 1000)
	cas := map[string][]byte{
		"sans __name__":      pbSerie([][2]string{{"job", "x"}}, p),
		"nom invalide":       pbSerie([][2]string{{"__name__", "1abc"}}, p),
		"label invalide":     pbSerie([][2]string{{"__name__", "a"}, {"a-b", "x"}}, p),
		"label réservé":      pbSerie([][2]string{{"__name__", "a"}, {"__x", "x"}}, p),
		"doublon":            pbSerie([][2]string{{"__name__", "a"}, {"x", "1"}, {"x", "2"}}, p),
		"UTF-8 invalide":     pbSerie([][2]string{{"__name__", "a"}, {"x", "\xff"}}, p),
		"valeur trop longue": pbSerie([][2]string{{"__name__", "a"}, {"x", string(make([]byte, 3000))}}, p),
		"sans point":         pbSerie([][2]string{{"__name__", "a"}}),
	}
	bonne := pbSerie([][2]string{{"__name__", "ok"}, {"vide", ""}}, p)
	for nom, s := range cas {
		series, ignorees, err := DecoderRemoteWrite(pbRequete(s, bonne), LimitesDecodageDefaut())
		if err != nil || ignorees != 1 || len(series) != 1 {
			t.Errorf("%s : séries %d, ignorées %d, erreur %v", nom, len(series), ignorees, err)
			continue
		}
		if len(series[0].Labels) != 1 { // l'étiquette vide est retirée
			t.Errorf("%s : %+v", nom, series[0].Labels)
		}
	}
}

func TestTropDEtiquettes(t *testing.T) {
	labels := [][2]string{{"__name__", "a"}}
	for i := 0; i < 70; i++ {
		labels = append(labels, [2]string{"l" + string(rune('a'+i%26)) + string(rune('a'+i/26)), "v"})
	}
	series, ignorees, err := DecoderRemoteWrite(pbRequete(pbSerie(labels, pbPoint(1, 1))), LimitesDecodageDefaut())
	if err != nil || ignorees != 1 || len(series) != 0 {
		t.Errorf("séries %d, ignorées %d, erreur %v", len(series), ignorees, err)
	}
}

func TestChampsInconnusSautes(t *testing.T) {
	s := pbSerie([][2]string{{"__name__", "a"}}, pbPoint(2, 5))
	s = append(s, 3<<3|0, 7)                          // varint inconnu
	s = append(s, 4<<3|5, 1, 2, 3, 4)                 // 32 bits inconnu
	s = pbMessage(s, 5, []byte("histogramme ignoré")) // message inconnu
	req := pbMessage(nil, 1, s)
	req = pbMessage(req, 3, []byte("métadonnées ignorées")) // champ 3 du WriteRequest
	series, _, err := DecoderRemoteWrite(snappy.Encode(nil, req), LimitesDecodageDefaut())
	if err != nil || len(series) != 1 || series[0].Points[0].V != 2 {
		t.Fatalf("séries %+v, erreur %v", series, err)
	}
}

func TestStructureInvalide(t *testing.T) {
	bonne := pbMessage(nil, 1, pbSerie([][2]string{{"__name__", "a"}}, pbPoint(1, 1)))
	cas := map[string][]byte{
		"pas du snappy":    []byte("bonjour"),
		"tronqué":          snappy.Encode(nil, bonne[:len(bonne)-3]),
		"longueur folle":   snappy.Encode(nil, []byte{1<<3 | 2, 0xff, 0xff, 0xff, 0x0f}),
		"groupe":           snappy.Encode(nil, []byte{1<<3 | 3}),
		"champ 0":          snappy.Encode(nil, []byte{0, 1}),
		"varint sans fin":  snappy.Encode(nil, []byte{1<<3 | 0, 0xff, 0xff}),
		"double tronqué":   snappy.Encode(nil, pbMessage(nil, 1, pbMessage(nil, 2, []byte{1<<3 | 1, 1, 2}))),
		"32 bits tronqués": snappy.Encode(nil, []byte{2<<3 | 5, 1}),
	}
	for nom, corps := range cas {
		if _, _, err := DecoderRemoteWrite(corps, LimitesDecodageDefaut()); !errors.Is(err, ErrFormat) {
			t.Errorf("%s : %v, attendu ErrFormat", nom, err)
		}
	}
}

func TestLimitesDeTaille(t *testing.T) {
	lim := LimitesDecodageDefaut()
	// Bombe snappy : taille annoncée énorme, refusée avant toute allocation.
	bombe := binary.AppendUvarint(nil, 1<<30) // 1 Gio annoncé
	if _, _, err := DecoderRemoteWrite(bombe, lim); !errors.Is(err, ErrTropGros) {
		t.Errorf("bombe : %v", err)
	}
	lim.MaxSeries = 2
	s := pbSerie([][2]string{{"__name__", "a"}}, pbPoint(1, 1))
	if _, _, err := DecoderRemoteWrite(pbRequete(s, s, s), lim); !errors.Is(err, ErrTropGros) {
		t.Errorf("séries : %v", err)
	}
	lim = LimitesDecodageDefaut()
	lim.MaxPoints = 3
	s = pbSerie([][2]string{{"__name__", "a"}}, pbPoint(1, 1), pbPoint(1, 2))
	if _, _, err := DecoderRemoteWrite(pbRequete(s, s), lim); !errors.Is(err, ErrTropGros) {
		t.Errorf("points : %v", err)
	}
	s = pbSerie([][2]string{{"__name__", "a"}}, pbPoint(1, 1), pbPoint(1, 2), pbPoint(1, 3), pbPoint(1, 4))
	if _, _, err := DecoderRemoteWrite(pbRequete(s), lim); !errors.Is(err, ErrTropGros) {
		t.Errorf("points d'une série : %v", err)
	}
}

func TestNomsValides(t *testing.T) {
	for _, n := range []string{"a", "node_load1", "job:rate5m", "_x"} {
		if !NomMetriqueValide(n) {
			t.Errorf("%q devrait être valide", n)
		}
	}
	for _, n := range []string{"", "1a", "a-b", "a b", "é"} {
		if NomMetriqueValide(n) {
			t.Errorf("%q devrait être refusé", n)
		}
	}
	if NomLabelValide("a:b") || !NomLabelValide("le") {
		t.Error("NomLabelValide")
	}
}

// FuzzDecoderRemoteWrite : jamais de panique, jamais de série non conforme.
func FuzzDecoderRemoteWrite(f *testing.F) {
	f.Add(pbRequete(pbSerie([][2]string{{"__name__", "a"}, {"b", "c"}}, pbPoint(1, 2))))
	f.Add(snappy.Encode(nil, []byte{1<<3 | 2, 0xff, 0xff, 0xff, 0x0f}))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, corps []byte) {
		series, _, err := DecoderRemoteWrite(corps, LimitesDecodageDefaut())
		if err != nil {
			return
		}
		for _, s := range series {
			if !labelsValides(&s) || len(s.Points) == 0 {
				t.Fatalf("série non conforme acceptée : %+v", s)
			}
		}
	})
}

func BenchmarkDecoderRemoteWrite(b *testing.B) {
	// Lot typique d'un agent : 800 séries.
	lot := metrics.NewBatch(800, []metrics.Label{metrics.L("instance", "pc"), metrics.L("job", "integrations/unix")})
	lot.Reset(1_700_000_000_000)
	for i := 0; i < 800; i++ {
		lot.Add("node_cpu_seconds_total", float64(i), metrics.L("cpu", string(rune('0'+i%10))), metrics.L("mode", "user"+string(rune('a'+i/10%26))+string(rune('a'+i/260))))
	}
	var enc remotewrite.Encoder
	corps := enc.Encode(nil, lot)
	b.SetBytes(int64(len(corps)))
	b.ReportAllocs()
	for b.Loop() {
		if _, _, err := DecoderRemoteWrite(corps, LimitesDecodageDefaut()); err != nil {
			b.Fatal(err)
		}
	}
}
