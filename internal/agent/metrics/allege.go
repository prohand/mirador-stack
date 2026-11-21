// SPDX-License-Identifier: GPL-3.0-or-later

package metrics

import (
	"hash/maphash"
	"math"
	"strings"
)

// Allegeur : envoi allégé (SPEC § 100, demande du 20/11). Une jauge dont la
// valeur n'a pas changé depuis le dernier envoi n'est pas renvoyée, sauf
// toutes les Garde millisecondes (2 min) : les requêtes du serveur
// retrouvent toujours une valeur (elles regardent 5 min en arrière).
//
// Jamais allégés : les compteurs (_total, _count, _sum, _bucket : rate() a
// besoin de deux points récents), node_uname_info (compté pour l'analyse
// des coupures) et les métriques de l'agent lui-même.
type Allegeur struct {
	Garde      int64 // ms
	Economises uint64

	graine maphash.Seed
	vus    map[uint64]vu
	cycles int
}

type vu struct {
	bits uint64 // valeur (bits : NaN égal à NaN)
	le   int64  // dernier envoi (ms)
}

// NouvelAllegeur : garde en millisecondes.
func NouvelAllegeur(garde int64) *Allegeur {
	return &Allegeur{Garde: garde, graine: maphash.MakeSeed(), vus: map[uint64]vu{}}
}

// allegeable : jauge qu'on peut ne pas renvoyer si elle n'a pas changé.
func allegeable(nom string) bool {
	switch {
	case strings.HasSuffix(nom, "_total"), strings.HasSuffix(nom, "_count"), strings.HasSuffix(nom, "_sum"),
		strings.HasSuffix(nom, "_bucket"), strings.HasSuffix(nom, "_created"):
		return false
	case nom == "node_uname_info", strings.HasPrefix(nom, "mirador_agent_"):
		return false
	}
	return true
}

func (a *Allegeur) cle(s *Sample) uint64 {
	var h maphash.Hash
	h.SetSeed(a.graine)
	_, _ = h.WriteString(s.Name)
	for i := 0; i < s.N; i++ {
		_ = h.WriteByte(0)
		_, _ = h.WriteString(s.Labels[i].Name)
		_ = h.WriteByte(1)
		_, _ = h.WriteString(s.Labels[i].Value)
	}
	return h.Sum64()
}

// Filtrer retire du lot (sur place) les jauges inchangées envoyées depuis
// moins de Garde ; renvoie le nombre de points retirés.
func (a *Allegeur) Filtrer(b *Batch) int {
	t := b.TimestampMs
	j, retires := 0, 0
	for i := range b.Samples {
		s := &b.Samples[i]
		garder := true
		if allegeable(s.Name) {
			k, bits := a.cle(s), math.Float64bits(s.Value)
			// d >= 0 : horloge reculée (NTP), on renvoie (relecture du 21/11 :
			// les jauges stables ne partaient plus pendant tout le recul).
			if v, ok := a.vus[k]; ok && v.bits == bits && t-v.le >= 0 && t-v.le < a.Garde {
				garder = false
			} else {
				a.vus[k] = vu{bits: bits, le: t}
			}
		}
		if garder {
			if j != i {
				b.Samples[j] = *s
			}
			j++
		} else {
			retires++
		}
	}
	b.Samples = b.Samples[:j]
	a.Economises += uint64(retires)
	// Ménage : séries disparues (plus vues depuis 10 gardes), toutes les 100 fois.
	if a.cycles++; a.cycles%100 == 0 {
		for k, v := range a.vus {
			if t-v.le > 10*a.Garde {
				delete(a.vus, k)
			}
		}
	}
	return retires
}
