// SPDX-License-Identifier: GPL-3.0-or-later

package remotewrite

import (
	"encoding/binary"
	"errors"
	"math"
	"testing"

	"github.com/klauspost/compress/snappy"

	"mirador-stack/internal/agent/metrics"
)

// serieLue est une série décodée par le petit décodeur de test ci-dessous.
type serieLue struct {
	labels []metrics.Label
	valeur float64
	ts     int64
}

// decoder relit un WriteRequest. C'est un décodeur protobuf minimal, écrit
// indépendamment de l'encodeur, pour vérifier que le résultat est conforme.
func decoder(b []byte) ([]serieLue, error) {
	var series []serieLue
	for len(b) > 0 {
		champ, typ, corps, reste, err := lireChamp(b)
		if err != nil {
			return nil, err
		}
		b = reste
		if champ != 1 || typ != 2 {
			return nil, errors.New("WriteRequest : champ inattendu")
		}
		s, err := decoderSerie(corps)
		if err != nil {
			return nil, err
		}
		series = append(series, s)
	}
	return series, nil
}

func decoderSerie(b []byte) (serieLue, error) {
	var s serieLue
	for len(b) > 0 {
		champ, _, corps, reste, err := lireChamp(b)
		if err != nil {
			return s, err
		}
		b = reste
		switch champ {
		case 1:
			var l metrics.Label
			for len(corps) > 0 {
				c, _, v, r, err := lireChamp(corps)
				if err != nil {
					return s, err
				}
				corps = r
				if c == 1 {
					l.Name = string(v)
				} else {
					l.Value = string(v)
				}
			}
			s.labels = append(s.labels, l)
		case 2:
			for len(corps) > 0 {
				c, t, v, r, err := lireChamp(corps)
				if err != nil {
					return s, err
				}
				corps = r
				if c == 1 && t == 1 {
					s.valeur = math.Float64frombits(binary.LittleEndian.Uint64(v))
				} else if c == 2 && t == 0 {
					x, _ := binary.Uvarint(v)
					s.ts = int64(x)
				}
			}
		}
	}
	return s, nil
}

// lireChamp lit un champ : renvoie numéro, type, contenu et le reste.
func lireChamp(b []byte) (champ, typ int, corps, reste []byte, err error) {
	tag, n := binary.Uvarint(b)
	if n <= 0 {
		return 0, 0, nil, nil, errors.New("tag illisible")
	}
	b = b[n:]
	champ, typ = int(tag>>3), int(tag&7)
	switch typ {
	case 0:
		_, m := binary.Uvarint(b)
		if m <= 0 {
			return 0, 0, nil, nil, errors.New("varint illisible")
		}
		return champ, typ, b[:m], b[m:], nil
	case 1:
		if len(b) < 8 {
			return 0, 0, nil, nil, errors.New("double tronqué")
		}
		return champ, typ, b[:8], b[8:], nil
	case 2:
		l, m := binary.Uvarint(b)
		if m <= 0 || uint64(len(b)-m) < l {
			return 0, 0, nil, nil, errors.New("longueur illisible")
		}
		return champ, typ, b[m : m+int(l)], b[m+int(l):], nil
	}
	return 0, 0, nil, nil, errors.New("type inconnu")
}

func lotExemple() *metrics.Batch {
	common := []metrics.Label{
		metrics.L("instance", "srv-test-mirador"), metrics.L("job", "node"),
		metrics.L("site", "bureau"), metrics.L("env", "vm"),
	}
	b := metrics.NewBatch(16, common)
	b.Reset(1_762_500_000_000)
	b.Add("node_load5", 0.42)
	b.Add("node_cpu_seconds_total", 12345.67, metrics.L("cpu", "0"), metrics.L("mode", "idle"))
	// Étiquette propre « job » : elle doit remplacer la commune.
	b.Add("mirador_test", -1.5, metrics.L("job", "autre"), metrics.L("vide", ""))
	return b
}

func TestEncodeRelecture(t *testing.T) {
	b := lotExemple()
	var e Encoder
	compresse := e.Encode(nil, b)

	brut, err := snappy.Decode(nil, compresse)
	if err != nil {
		t.Fatalf("snappy illisible : %v", err)
	}
	series, err := decoder(brut)
	if err != nil {
		t.Fatalf("protobuf illisible : %v", err)
	}
	if len(series) != 3 {
		t.Fatalf("3 séries attendues, %d lues", len(series))
	}

	// Série 2 : étiquettes triées, communes incluses.
	attendu := []metrics.Label{
		metrics.L("__name__", "node_cpu_seconds_total"), metrics.L("cpu", "0"), metrics.L("env", "vm"),
		metrics.L("instance", "srv-test-mirador"), metrics.L("job", "node"), metrics.L("mode", "idle"), metrics.L("site", "bureau"),
	}
	if len(series[1].labels) != len(attendu) {
		t.Fatalf("étiquettes : %v", series[1].labels)
	}
	for i, l := range attendu {
		if series[1].labels[i] != l {
			t.Errorf("étiquette %d : %v, attendu %v", i, series[1].labels[i], l)
		}
	}
	if series[1].valeur != 12345.67 || series[1].ts != 1_762_500_000_000 {
		t.Errorf("valeur ou horodatage faux : %+v", series[1])
	}

	// Série 3 : job propre gagne, étiquette vide supprimée, valeur négative.
	for _, l := range series[2].labels {
		if l.Name == "job" && l.Value != "autre" {
			t.Errorf("l'étiquette propre aurait dû remplacer la commune : %v", l)
		}
		if l.Name == "vide" {
			t.Error("une étiquette vide ne doit pas être envoyée")
		}
	}
	if series[2].valeur != -1.5 {
		t.Errorf("valeur négative mal encodée : %v", series[2].valeur)
	}
}

func TestTriEtiquettes(t *testing.T) {
	b := lotExemple()
	brut := AppendWriteRequest(nil, b)
	series, err := decoder(brut)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range series {
		for i := 1; i < len(s.labels); i++ {
			if s.labels[i-1].Name >= s.labels[i].Name {
				t.Fatalf("étiquettes non triées : %v", s.labels)
			}
		}
	}
}

// BenchmarkEncode mesure l'encodage d'un lot réaliste (~600 points, la taille
// attendue d'un cycle sur une VM). Objectif : 0 allocation par cycle une fois
// les tampons dimensionnés.
func BenchmarkEncode(b *testing.B) {
	lot := metrics.NewBatch(1024, lotExemple().Common)
	lot.Reset(1_762_500_000_000)
	modes := []string{"user", "nice", "system", "idle", "iowait", "irq", "softirq", "steal"}
	cpus := []string{"0", "1", "2", "3"}
	for i := 0; i < 18; i++ {
		for _, c := range cpus {
			for _, m := range modes {
				lot.Add("node_cpu_seconds_total", float64(i), metrics.L("cpu", c), metrics.L("mode", m))
			}
		}
	}
	var e Encoder
	dst := e.Encode(nil, lot)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		dst = e.Encode(dst, lot)
	}
	b.ReportMetric(float64(lot.Len()), "points/op")
}
