// SPDX-License-Identifier: GPL-3.0-or-later

package snmp

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gosnmp/gosnmp"

	"mirador-stack/internal/agent/config"
	"mirador-stack/internal/agent/metrics"
)

// fauxNAS : un Synology simulé (OID → valeur).
type fauxNAS map[string]gosnmp.SnmpPDU

func (f fauxNAS) pdu(oid string, t gosnmp.Asn1BER, v any) {
	f[oid] = gosnmp.SnmpPDU{Name: "." + oid, Type: t, Value: v}
}

func (f fauxNAS) Get(oids []string) (*gosnmp.SnmpPacket, error) {
	p := &gosnmp.SnmpPacket{}
	for _, o := range oids {
		if v, ok := f[o]; ok {
			p.Variables = append(p.Variables, v)
		} else {
			p.Variables = append(p.Variables, gosnmp.SnmpPDU{Name: "." + o, Type: gosnmp.NoSuchObject})
		}
	}
	return p, nil
}

func (f fauxNAS) BulkWalkAll(racine string) ([]gosnmp.SnmpPDU, error) {
	var out []gosnmp.SnmpPDU
	for o, v := range f {
		if strings.HasPrefix(o, racine+".") {
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func nasSimule() fauxNAS {
	f := fauxNAS{}
	f.pdu("1.3.6.1.2.1.1.3.0", gosnmp.TimeTicks, uint32(123456))
	f.pdu("1.3.6.1.4.1.2021.4.5.0", gosnmp.Integer, 8000000)
	f.pdu("1.3.6.1.4.1.6574.1.1.0", gosnmp.Integer, 1)
	f.pdu("1.3.6.1.4.1.6574.1.2.0", gosnmp.Integer, 41)
	f.pdu("1.3.6.1.4.1.6574.1.5.1.0", gosnmp.OctetString, []byte("DS920+"))
	f.pdu("1.3.6.1.4.1.6574.1.5.3.0", gosnmp.OctetString, []byte("DSM 7.2.2-72806\x00"))
	f.pdu("1.3.6.1.4.1.6574.4.2.1.0", gosnmp.OctetString, []byte("OL"))
	for i, nom := range []string{"Disk 1", "Disk 2"} {
		n := string(rune('1' + i))
		f.pdu("1.3.6.1.4.1.6574.2.1.1.2."+n, gosnmp.OctetString, []byte(nom))
		f.pdu("1.3.6.1.4.1.6574.2.1.1.3."+n, gosnmp.OctetString, []byte("WD40EFRX"))
		f.pdu("1.3.6.1.4.1.6574.2.1.1.6."+n, gosnmp.Integer, 35+i)
		f.pdu("1.3.6.1.4.1.6574.2.1.1.13."+n, gosnmp.Integer, 1)
	}
	f.pdu("1.3.6.1.4.1.6574.3.1.1.2.0", gosnmp.OctetString, []byte("Volume 1"))
	f.pdu("1.3.6.1.4.1.6574.3.1.1.3.0", gosnmp.Integer, 1)
	f.pdu("1.3.6.1.4.1.6574.3.1.1.4.0", gosnmp.Counter64, uint64(1)<<40)
	f.pdu("1.3.6.1.4.1.6574.3.1.1.5.0", gosnmp.Counter64, uint64(4)<<40)
	f.pdu("1.3.6.1.2.1.31.1.1.1.1.3", gosnmp.OctetString, []byte("eth0"))
	f.pdu("1.3.6.1.2.1.31.1.1.1.6.3", gosnmp.Counter64, uint64(987654321))
	f.pdu("1.3.6.1.2.1.2.2.1.14.3", gosnmp.Counter32, uint(2))
	return f
}

func points(b *metrics.Batch) map[string]float64 {
	out := map[string]float64{}
	for _, s := range b.Samples[:b.Len()] {
		k := s.Name
		for _, l := range s.Labels[:s.N] {
			k += "|" + l.Name + "=" + l.Value
		}
		out[k] = s.Value
	}
	return out
}

func TestReleveSynology(t *testing.T) {
	b := metrics.NewBatch(64, nil)
	b.Reset(0)
	if err := Releve(nasSimule(), "synology", b); err != nil {
		t.Fatal(err)
	}
	p := points(b)
	attendus := map[string]float64{
		"systemStatus":                               1,
		"temperature":                                41,
		"sysUpTime":                                  123456,
		"modelName|modelName=DS920+":                 1,
		"version|version=DSM 7.2.2-72806":            1,
		"upsInfoStatus|upsInfoStatus=OL":             1,
		"diskTemperature|diskID=Disk 2":              36,
		"diskModel|diskID=Disk 1|diskModel=WD40EFRX": 1,
		"diskHealthStatus|diskID=Disk 1":             1,
		"raidStatus|raidName=Volume 1":               1,
		"raidTotalSize|raidName=Volume 1":            float64(uint64(4) << 40),
		"ifHCInOctets|ifName=eth0|ifIndex=3":         987654321,
		"ifInErrors|ifName=eth0|ifIndex=3":           2,
	}
	for k, v := range attendus {
		if got, ok := p[k]; !ok || got != v {
			t.Errorf("%s = %v (présent %v)", k, got, ok)
		}
	}
	// OID absents (NoSuchObject) : rien d'émis.
	if _, ok := p["powerStatus"]; ok {
		t.Error("valeur absente émise")
	}
	// « standard » : pas d'OID Synology.
	b.Reset(0)
	_ = Releve(nasSimule(), "standard", b)
	if _, ok := points(b)["systemStatus"]; ok {
		t.Error("OID Synology relevé en préréglage standard")
	}
	if err := Releve(fauxNAS{}, "synology", b); err == nil {
		t.Error("équipement muet : pas d'erreur")
	}
}

type sourceEnPanne struct{}

func (sourceEnPanne) Get([]string) (*gosnmp.SnmpPacket, error) {
	return nil, errors.New("délai dépassé")
}
func (sourceEnPanne) BulkWalkAll(string) ([]gosnmp.SnmpPDU, error) {
	return nil, errors.New("délai dépassé")
}

// Releveur : up=1 et étiquettes instance / job="snmp" ; up=0 si l'équipement
// ne répond pas ; un secret encore chiffré est refusé.
func TestReleveur(t *testing.T) {
	var lots []*metrics.Batch
	cible := config.CibleSNMP{Machine: "nas", Prereglage: "synology", Intervalle: config.Duree(time.Hour)}
	for _, src := range []Source{nasSimule(), sourceEnPanne{}} {
		r := &Releveur{Cible: cible, Envoyer: func(b *metrics.Batch) { lots = append(lots, b) },
			Ouvrir: func(config.CibleSNMP) (Source, func(), error) { return src, func() {}, nil }}
		ctx, fin := context.WithCancel(context.Background())
		fin() // un seul relevé
		r.Tourner(ctx)
	}
	if len(lots) != 2 {
		t.Fatalf("%d lots", len(lots))
	}
	if p := points(lots[0]); p["up"] != 1 || lots[0].Common[0].Value != "nas" || lots[0].Common[1].Value != "snmp" {
		t.Errorf("lot : %v %v", p["up"], lots[0].Common)
	}
	if p := points(lots[1]); p["up"] != 0 {
		t.Errorf("équipement en panne : up=%v", p["up"])
	}
	if _, _, err := OuvrirReseau(config.CibleSNMP{MotDePasse: config.PrefixeSecretChiffre + "x"}); err == nil {
		t.Error("secret chiffré accepté")
	}
}

// BenchmarkReleve : traitement d'un relevé Synology (hors réseau).
//
//	go test ./internal/collecteur/snmp/ -bench Releve -benchmem -run '^$'
func BenchmarkReleve(b *testing.B) {
	nas := nasSimule()
	lot := metrics.NewBatch(256, nil)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		lot.Reset(0)
		_ = Releve(nas, "synology", lot)
	}
}
