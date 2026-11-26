// SPDX-License-Identifier: GPL-3.0-or-later

package syslog

import (
	"net/netip"
	"strings"
	"testing"
	"time"

	"mirador-stack/internal/protocole"
)

func TestLireFilterlog(t *testing.T) {
	cas := []struct {
		nom, ligne string
		ok         bool
		ip, proto  string
		port       int
		itf        string
	}{
		{"IPv4 TCP bloqué entrant", "5,,,1000000103,igb1,match,block,in,4,0x0,,64,0,0,DF,6,tcp,60,203.0.113.9,192.168.1.1,46406,22,0,S,1563219582,,29200,,mss;sackOK;TS;nop;wscale",
			false, "", "", 0, ""}, // 203.0.113.0/24 : documentation, pas publique
		{"IPv4 TCP public", "5,,,1000000103,igb1,match,block,in,4,0x0,,64,0,0,DF,6,tcp,60,45.143.203.10,192.168.1.1,46406,23,0,S,1563219582,,29200,,",
			true, "45.143.203.10", "tcp", 23, "igb1"},
		{"IPv6 UDP public", "7,,,02f4bab031b57d1e30553ce08e0ec131,igb0,match,block,in,6,0x00,0x00000,255,udp,17,72,2a01:e0a::1,2a01:e0a::5,546,547,72",
			true, "2a01:e0a::1", "udp", 547, "igb0"},
		{"ICMP sans port", "9,,,abc,igb0,match,block,in,4,0x0,,50,1,0,none,1,icmp,84,89.248.165.73,192.168.1.1,request,1,2",
			true, "89.248.165.73", "icmp", 0, "igb0"},
		{"laissé passer", "5,,,x,igb1,match,pass,in,4,0x0,,64,0,0,DF,6,tcp,60,45.143.203.10,192.168.1.1,46406,23,0,S,1,,2,,", false, "", "", 0, ""},
		{"sortant vers le LAN", "5,,,x,igb1,match,block,out,4,0x0,,64,0,0,DF,6,tcp,60,45.143.203.10,192.168.1.1,46406,23,0,S,1,,2,,", false, "", "", 0, ""},
		{"sortant vers Internet", "5,,,x,igb1,match,block,out,4,0x0,,64,0,0,DF,6,tcp,60,192.168.1.7,93.184.216.34,46406,25,0,S,1,,2,,", true, "93.184.216.34", "tcp", 25, "igb1"},
		{"LAN bloqué vers Internet", "5,,,x,vlan0.35,match,block,in,4,0x0,,64,0,0,DF,17,udp,60,192.168.35.4,8.8.8.8,46406,53,12", true, "8.8.8.8", "udp", 53, "vlan0.35"},
		{"LAN vers LAN", "5,,,x,igb1,match,block,in,4,0x0,,64,0,0,DF,6,tcp,60,192.168.10.4,192.168.1.1,46406,23,0,S,1,,2,,", false, "", "", 0, ""},
		{"trop court", "5,,,x,igb1,match,block,in,4", false, "", "", 0, ""},
		{"interface bizarre", "5,,,x,ig b1;rm,match,block,in,4,0x0,,64,0,0,DF,6,tcp,60,45.143.203.10,192.168.1.1,46406,23,0,S,1,,2,,", true, "45.143.203.10", "tcp", 23, ""},
	}
	for _, c := range cas {
		f, ok := LireFilterlog(c.ligne)
		if ok != c.ok {
			t.Errorf("%s : ok=%v", c.nom, ok)
			continue
		}
		if !ok {
			continue
		}
		if f.IP.String() != c.ip || f.Proto != c.proto || f.Port != c.port || f.Interface != c.itf || f.N != 1 {
			t.Errorf("%s : %+v", c.nom, f)
		}
	}
}

func TestCompteurFluxEtRelecture(t *testing.T) {
	var c CompteurFlux
	f, _ := LireFilterlog("81,,,x,igb0,match,block,in,4,0x0,,64,0,0,DF,6,tcp,60,45.143.203.10,192.168.1.1,46406,23,0,S,1,,2,,")
	for range 3 {
		c.Ajouter("fw", f)
	}
	f2 := f
	f2.Port = 22
	c.Ajouter("fw", f2)
	lignes := c.Vider(time.Unix(100, 0))
	if len(lignes) != 2 {
		t.Fatalf("lignes : %+v", lignes)
	}
	vus := map[int]int{}
	for _, l := range lignes {
		if l.Hote != "fw" {
			t.Errorf("hôte : %q", l.Hote)
		}
		g, ok := protocole.LireFluxBloque(l.Texte, true)
		if !ok || g.IP != netip.MustParseAddr("45.143.203.10") || g.Regle != "règle 81" || g.Interface != "igb0" || g.Proto != "tcp" {
			t.Errorf("relecture de %q : %+v %v", l.Texte, g, ok)
		}
		vus[g.Port] = g.N
	}
	if vus[23] != 3 || vus[22] != 1 {
		t.Errorf("comptes : %v", vus)
	}
	if len(c.Vider(time.Now())) != 0 {
		t.Error("fenêtre pas remise à zéro")
	}
	// Mémoire bornée : au-delà de MaxClesFlux clés, compté à part.
	for i := range MaxClesFlux + 10 {
		g := f
		g.Port = i%65535 + 1
		g.IP = netip.AddrFrom4([4]byte{45, byte(i >> 16), byte(i >> 8), byte(i)})
		c.Ajouter("fw", g)
	}
	if total, perdus := c.Totaux(); perdus != 10 || total != uint64(4+MaxClesFlux+10) {
		t.Errorf("totaux : %d %d", total, perdus)
	}
	if n := len(c.Vider(time.Now())); n != MaxClesFlux {
		t.Errorf("clés : %d", n)
	}
	if !strings.Contains(protocole.LigneFlux(f), "N=1") {
		t.Error("N absent")
	}
}

// Coût d'une ligne filterlog (SPEC § 135) : lecture et comptage.
func BenchmarkFilterlog(b *testing.B) {
	var c CompteurFlux
	lignes := []string{
		"5,,,1000000103,igb1,match,block,in,4,0x0,,64,0,0,DF,6,tcp,60,45.143.203.10,192.168.1.1,46406,23,0,S,1563219582,,29200,,",
		"5,,,1000000103,igb1,match,pass,out,4,0x0,,64,0,0,DF,6,tcp,60,192.168.1.10,1.1.1.1,46406,443,0,S,1563219582,,29200,,",
	}
	b.ReportAllocs()
	for i := range b.N {
		if f, ok := LireFilterlog(lignes[i%2]); ok {
			c.Ajouter("fw", f)
		}
	}
}

// Trame réelle d'OPNsense (relevé du 27/11, adresses de destination
// remplacées) : RFC 5424 avec données structurées, interface de VLAN ;
// seule la source publique sur le WAN est comptée.
func TestFilterlogTrameOPNsense(t *testing.T) {
	trames := []string{
		`<134>1 2026-11-27T00:05:24+01:00 fw.exemple filterlog 35108 - [meta sequenceId="1388"] 27,,,02f4bab031b57d1e30553ce08e0ec131,vlan0.55,match,block,in,4,0x0,,64,12130,0,DF,17,udp,48,192.168.55.1,198.51.100.129,39684,19302,28`,
		`<134>1 2026-11-27T00:05:25+01:00 fw.exemple filterlog 35108 - [meta sequenceId="1389"] 27,,,02f4bab031b57d1e30553ce08e0ec131,ix1,match,block,in,4,0x0,,246,33774,0,none,6,tcp,44,194.180.48.39,198.51.100.251,58868,5911,0,S,1187347970,,1024,,mss`,
		`<134>1 2026-11-27T00:05:25+01:00 fw.exemple filterlog 35108 - [meta sequenceId="1390"] 27,,,02f4bab031b57d1e30553ce08e0ec131,vlan0.35,match,block,in,4,0x0,,64,1132,0,DF,17,udp,32,192.168.35.4,255.255.255.255,34434,10001,12`,
	}
	var comptes []protocole.FluxBloque
	for _, tr := range trames {
		m, ok := Analyser([]byte(tr))
		if !ok || m.App != "filterlog" || m.Niveau != "info" {
			t.Fatalf("trame : %+v %v", m, ok)
		}
		if f, ok := LireFilterlog(m.Texte); ok {
			comptes = append(comptes, f)
		}
	}
	if len(comptes) != 1 || comptes[0].Sens != protocole.Entrant || comptes[0].IP.String() != "194.180.48.39" || comptes[0].Port != 5911 || comptes[0].Interface != "ix1" || comptes[0].Regle != "règle 27" {
		t.Errorf("comptés : %+v", comptes)
	}
}
