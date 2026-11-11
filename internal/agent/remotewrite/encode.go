// SPDX-License-Identifier: GPL-3.0-or-later

// Package remotewrite encode un lot de métriques au format Prometheus
// « remote write » 1.0 : protobuf compressé en snappy (format bloc).
//
// Pourquoi un encodeur écrit à la main (SPEC.md § 5.2) : le message ne contient
// que 4 types simples. Une bibliothèque protobuf complète ajouterait du poids au
// binaire et des allocations ; ici, tout est écrit dans un tampon réutilisé.
//
// Schéma (prompb, champs utilisés) :
//
//	message WriteRequest { repeated TimeSeries timeseries = 1; }
//	message TimeSeries   { repeated Label labels = 1; repeated Sample samples = 2; }
//	message Label        { string name = 1; string value = 2; }
//	message Sample       { double value = 1; int64 timestamp = 2; }
//
// Les étiquettes d'une série doivent être triées par nom (exigence du format).
package remotewrite

import (
	"encoding/binary"
	"math"

	"github.com/klauspost/compress/snappy"

	"mirador-stack/internal/agent/metrics"
)

// Nombre maximal d'étiquettes par série une fois les communes ajoutées.
const maxLabelsSerie = metrics.MaxLabels + 8

// Encoder garde ses tampons d'un cycle à l'autre.
type Encoder struct {
	brut []byte // protobuf non compressé
}

// Encode écrit le lot en protobuf puis le compresse en snappy dans dst
// (réutilisé si sa capacité suffit). Le résultat est prêt à être envoyé.
func (e *Encoder) Encode(dst []byte, b *metrics.Batch) []byte {
	e.brut = AppendWriteRequest(e.brut[:0], b)
	n := snappy.MaxEncodedLen(len(e.brut))
	if cap(dst) < n {
		dst = make([]byte, n)
	}
	return snappy.Encode(dst[:n], e.brut)
}

// AppendWriteRequest ajoute à dst le WriteRequest protobuf (non compressé).
func AppendWriteRequest(dst []byte, b *metrics.Batch) []byte {
	var labels [maxLabelsSerie]metrics.Label
	for i := range b.Samples {
		s := &b.Samples[i]
		n := assembler(&labels, s, b.Common)
		taille := tailleSerie(labels[:n], s.Value, b.TimestampMs)
		dst = appendTag(dst, 1, 2) // champ 1 (timeseries), type « longueur »
		dst = binary.AppendUvarint(dst, u64(taille))
		dst = appendSerie(dst, labels[:n], s.Value, b.TimestampMs)
	}
	return dst
}

// assembler réunit __name__, les étiquettes communes et celles du point, puis
// les trie par nom (tri par insertion : quelques éléments, aucune allocation).
// Une étiquette propre au point remplace une commune de même nom.
func assembler(dst *[maxLabelsSerie]metrics.Label, s *metrics.Sample, common []metrics.Label) int {
	n := 0
	dst[n] = metrics.Label{Name: "__name__", Value: s.Name}
	n++
	for _, l := range s.Labels[:s.N] {
		if l.Value == "" { // une étiquette vide n'existe pas en Prometheus
			continue
		}
		dst[n] = l
		n++
	}
	for _, c := range common {
		if n == len(dst) {
			break
		}
		doublon := false
		for _, l := range s.Labels[:s.N] {
			if l.Name == c.Name {
				doublon = true
				break
			}
		}
		if !doublon && c.Value != "" {
			dst[n] = c
			n++
		}
	}
	for i := 1; i < n; i++ {
		for j := i; j > 0 && dst[j].Name < dst[j-1].Name; j-- {
			dst[j], dst[j-1] = dst[j-1], dst[j]
		}
	}
	return n
}

// --- Calcul des tailles (le protobuf préfixe chaque sous-message par sa taille)

func tailleLabel(l metrics.Label) int {
	return 1 + tailleVarint(u64(len(l.Name))) + len(l.Name) +
		1 + tailleVarint(u64(len(l.Value))) + len(l.Value)
}

// Sample : champ 1 en double (1 + 8 octets), champ 2 en varint.
func tailleSample(ts int64) int {
	return 1 + 8 + 1 + tailleVarint(i64(ts))
}

func tailleSerie(labels []metrics.Label, _ float64, ts int64) int {
	t := 0
	for _, l := range labels {
		tl := tailleLabel(l)
		t += 1 + tailleVarint(u64(tl)) + tl
	}
	ts1 := tailleSample(ts)
	t += 1 + tailleVarint(u64(ts1)) + ts1
	return t
}

// --- Écriture

func appendSerie(dst []byte, labels []metrics.Label, v float64, ts int64) []byte {
	for _, l := range labels {
		dst = appendTag(dst, 1, 2)
		dst = binary.AppendUvarint(dst, u64(tailleLabel(l)))
		dst = appendString(dst, 1, l.Name)
		dst = appendString(dst, 2, l.Value)
	}
	dst = appendTag(dst, 2, 2)
	dst = binary.AppendUvarint(dst, u64(tailleSample(ts)))
	dst = appendTag(dst, 1, 1) // double : type 1 (64 bits)
	dst = binary.LittleEndian.AppendUint64(dst, math.Float64bits(v))
	dst = appendTag(dst, 2, 0) // int64 : type 0 (varint)
	dst = binary.AppendUvarint(dst, i64(ts))
	return dst
}

func appendString(dst []byte, champ int, s string) []byte {
	dst = appendTag(dst, champ, 2)
	dst = binary.AppendUvarint(dst, u64(len(s)))
	return append(dst, s...)
}

// appendTag écrit l'en-tête d'un champ protobuf : (numéro << 3) | type.
func appendTag(dst []byte, champ, typ int) []byte {
	return append(dst, byte(champ<<3|typ)) // #nosec G115 -- numéros de champ < 16, type < 8
}

// u64 convertit une longueur (toujours positive) pour l'encodage varint.
func u64(n int) uint64 {
	return uint64(n) // #nosec G115 -- longueur de tampon, jamais négative
}

// i64 convertit un int64 comme le fait protobuf : un négatif devient son
// complément à deux sur 64 bits (comportement voulu par la spécification).
func i64(x int64) uint64 {
	return uint64(x) // #nosec G115 -- encodage protobuf des int64
}

func tailleVarint(x uint64) int {
	n := 1
	for x >= 0x80 {
		x >>= 7
		n++
	}
	return n
}
