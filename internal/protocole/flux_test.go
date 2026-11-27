// SPDX-License-Identifier: GPL-3.0-or-later

package protocole

import (
	"net/netip"
	"strings"
	"testing"
)

func TestLireFluxBloque(t *testing.T) {
	f, ok := LireFluxBloque("[12345.678] DROP_IN: IN=eth0 OUT= MAC=aa SRC=45.143.203.10 DST=1.2.3.4 LEN=40 PROTO=TCP SPT=4444 DPT=22 N=99", false)
	if !ok || f.Regle != "DROP_IN:" || f.Interface != "eth0" || f.Proto != "tcp" || f.Port != 22 || f.N != 1 || f.IP != netip.MustParseAddr("45.143.203.10") {
		t.Fatalf("%+v %v", f, ok)
	}
	if f, _ = LireFluxBloque("x IN=eth0 OUT= SRC=45.143.203.10 PROTO=UDP DPT=53 N=7", true); f.N != 7 || f.Proto != "udp" {
		t.Errorf("N : %+v", f)
	}
	if f, _ = LireFluxBloque("x IN=eth0 OUT= SRC=45.143.203.10 N=99999999", true); f.N != 1 {
		t.Errorf("N trop grand accepté : %+v", f)
	}
	for _, l := range []string{
		"x IN= OUT=eth0 SRC=45.143.203.10",          // sortant sans destination
		"x IN=eth0 OUT= SRC=192.168.1.5 DPT=22",     // LAN
		"x IN=eth0 OUT= SRC=100.64.1.1 DPT=22",      // CGNAT
		"x IN=eth0 OUT= SRC=fe80::1 DPT=22",         // lien local
		"x IN=eth0 OUT= SRC=pas-une-adresse DPT=22", // illisible
		"bonjour",
	} {
		if _, ok := LireFluxBloque(l, true); ok {
			t.Errorf("accepté : %q", l)
		}
	}
	// Traversant venu d'Internet : entrant.
	if f, ok := LireFluxBloque("x IN=eth0 OUT=eth1 SRC=45.143.203.10 DST=192.168.1.5 PROTO=TCP DPT=22", false); !ok || f.Sens != Entrant {
		t.Errorf("traversant : %+v %v", f, ok)
	}
	// Sortants : émis par la machine (IN vide) ou venu du LAN (traversant).
	f, ok = LireFluxBloque("DROP_OUT: IN= OUT=eth0 SRC=192.168.1.5 DST=8.8.4.4 PROTO=UDP DPT=53", false)
	if !ok || f.Sens != Sortant || f.IP.String() != "8.8.4.4" || f.Source.String() != "192.168.1.5" || f.Interface != "eth0" || f.Port != 53 || f.Regle != "DROP_OUT:" {
		t.Errorf("sortant : %+v %v", f, ok)
	}
	if f, ok = LireFluxBloque("x IN=vlan1 OUT=wan SRC=192.168.1.9 DST=93.184.216.34 PROTO=TCP DPT=25", false); !ok || f.Sens != Sortant || f.Interface != "wan" {
		t.Errorf("sortant traversant : %+v %v", f, ok)
	}
	if _, ok = LireFluxBloque("x IN= OUT=eth0 SRC=192.168.1.5 DST=192.168.1.1 DPT=53", false); ok {
		t.Error("sortant vers le LAN accepté")
	}
	// Aller-retour d'un sortant (ligne de comptage).
	g0 := FluxBloque{Sens: Sortant, Interface: "igb0", Proto: "tcp", Port: 25, N: 4, IP: netip.MustParseAddr("93.184.216.34"), Source: netip.MustParseAddr("192.168.1.9")}
	if h, ok := LireFluxBloque(LigneFlux(g0), true); !ok || h.Sens != Sortant || h.N != 4 || h.Source != g0.Source || h.IP != g0.IP {
		t.Errorf("aller-retour sortant : %+v %v", h, ok)
	}
	if PrefixeLigne("[1.2] DROP_IN: IN=eth0 OUT=") != "DROP_IN:" || PrefixeLigne("bonjour") != "" {
		t.Error("préfixe")
	}
	// Aller-retour et bornes.
	g := FluxBloque{Regle: strings.Repeat("é", 50), Interface: "igb0", Proto: "tcp", Port: 443, N: 3, IP: netip.MustParseAddr("2a01:e0a::1")}
	h, ok := LireFluxBloque(LigneFlux(g), true)
	if !ok || h.Port != 443 || h.N != 3 || h.IP != g.IP || len(h.Regle) > 60 || !strings.HasPrefix(h.Regle, "éé") {
		t.Errorf("aller-retour : %+v", h)
	}
}

// Comptage sans interface (nom refusé par le collecteur) : relu quand même
// (revue du 27/11).
func TestLigneFluxSansInterface(t *testing.T) {
	for _, sens := range []string{Entrant, Sortant} {
		f := FluxBloque{Sens: sens, IP: netip.MustParseAddr("9.9.9.9"), Source: netip.MustParseAddr("192.168.1.5"), Proto: "tcp", Port: 443, N: 3}
		g, ok := LireFluxBloque(LigneFlux(f), true)
		if !ok || g.Interface != "" || g.N != 3 || g.Sens != sens {
			t.Errorf("sens %s : %+v, lu=%v (ligne %q)", sens, g, ok, LigneFlux(f))
		}
	}
}
