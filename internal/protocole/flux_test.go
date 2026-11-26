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
		"x IN=eth0 OUT=eth1 SRC=45.143.203.10",      // traversant
		"x IN= OUT=eth0 SRC=45.143.203.10",          // sortant
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
	// Aller-retour et bornes.
	g := FluxBloque{Regle: strings.Repeat("é", 50), Interface: "igb0", Proto: "tcp", Port: 443, N: 3, IP: netip.MustParseAddr("2a01:e0a::1")}
	h, ok := LireFluxBloque(LigneFlux(g), true)
	if !ok || h.Port != 443 || h.N != 3 || h.IP != g.IP || len(h.Regle) > 60 || !strings.HasPrefix(h.Regle, "éé") {
		t.Errorf("aller-retour : %+v", h)
	}
}
