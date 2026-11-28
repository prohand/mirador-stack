// SPDX-License-Identifier: GPL-3.0-or-later

package syslog

import (
	"net/netip"
	"testing"
	"time"
)

// B16 (revue du 28/11) : le débit filterlog (par adresse IP) est vérifié
// AVANT le comptage des blocages ; les autres journaux gardent leur débit.
func TestFilterlogDebitAvantComptage(t *testing.T) {
	heure := time.Date(2026, 11, 28, 10, 0, 0, 0, time.UTC)
	var flux CompteurFlux
	recues := 0
	s, err := Nouveau(Options{Ecoute: "192.168.1.250:6514", Debit: 1, Rafale: 2, Flux: &flux,
		Maintenant: func() time.Time { return heure }, Sortie: func(Ligne) bool { recues++; return true }})
	if err != nil {
		t.Fatal(err)
	}
	a, b := netip.MustParseAddr("192.168.1.1"), netip.MustParseAddr("192.168.1.2")
	trame := []byte("<134>Nov 28 10:00:00 filterlog[1]: " + lignePerfFilterlog)
	for range 30 {
		s.recevoir("opnsense", a, trame)
	}
	// Rafale filterlog : 2 × multipleFilterlog = 20 ; le reste est refusé
	// sans être compté.
	if total, _ := flux.Totaux(); total != 20 {
		t.Errorf("blocages comptés : %d, attendu 20", total)
	}
	if n := s.refus[RefusDebit].Load(); n != 10 {
		t.Errorf("refus débit : %d, attendu 10", n)
	}
	// Une autre adresse de la même plage a son propre seau.
	for range 5 {
		s.recevoir("opnsense", b, trame)
	}
	if total, _ := flux.Totaux(); total != 25 {
		t.Errorf("autre adresse : %d blocages comptés, attendu 25", total)
	}
	// Les autres journaux de l'équipement ne sont pas touchés.
	s.recevoir("opnsense", a, []byte("<14>sshd[1]: connexion"))
	if recues != 1 {
		t.Errorf("journal ordinaire refusé après filterlog (%d reçues)", recues)
	}
	// Le temps rend des jetons (débit 10/s pour filterlog).
	heure = heure.Add(time.Second)
	s.recevoir("opnsense", a, trame)
	if total, _ := flux.Totaux(); total != 26 {
		t.Errorf("après 1 s : %d blocages comptés, attendu 26", total)
	}
}

// BenchmarkRecevoirFilterlog : une ligne filterlog reçue (analyse, débit
// par IP, comptage, ligne jetée).
func BenchmarkRecevoirFilterlog(b *testing.B) {
	var flux CompteurFlux
	s, err := Nouveau(Options{Ecoute: "192.168.1.250:6514", Debit: 1e9, Rafale: 1e9, Flux: &flux,
		Sortie: func(Ligne) bool { return true }})
	if err != nil {
		b.Fatal(err)
	}
	ip := netip.MustParseAddr("192.168.1.1")
	trame := []byte("<134>1 2026-11-27T10:00:00.123456+01:00 opnsense.lan filterlog 12345 - [meta sequenceId=\"1\"] " + lignePerfFilterlog)
	b.ReportAllocs()
	for b.Loop() {
		s.recevoir("opnsense", ip, trame)
	}
}

// BenchmarkPrendreFilterlog : coût du seul débit par IP (ajouté le 28/11).
func BenchmarkPrendreFilterlog(b *testing.B) {
	heure := time.Now()
	s := &Serveur{opt: Options{Debit: 1e9, Rafale: 1e9}, seauxIP: map[netip.Addr]*seau{}}
	ip := netip.MustParseAddr("192.168.1.1")
	b.ReportAllocs()
	for b.Loop() {
		s.prendreFilterlog(ip, heure)
	}
}

// Nombre d'adresses suivies borné ; les muettes sont oubliées.
func TestFilterlogSeauxBornes(t *testing.T) {
	heure := time.Date(2026, 11, 28, 10, 0, 0, 0, time.UTC)
	s := &Serveur{opt: Options{Debit: 1, Rafale: 1, Maintenant: func() time.Time { return heure }},
		seauxIP: map[netip.Addr]*seau{}}
	base := netip.MustParseAddr("10.0.0.0")
	ip := base
	for range maxSeauxIP {
		ip = ip.Next()
		if !s.prendreFilterlog(ip, heure) {
			t.Fatal("adresse refusée avant la limite")
		}
	}
	if s.prendreFilterlog(ip.Next(), heure) {
		t.Fatal("adresse de trop acceptée")
	}
	if len(s.seauxIP) != maxSeauxIP {
		t.Fatalf("%d seaux", len(s.seauxIP))
	}
	heure = heure.Add(2 * oubliSeauIP)
	if !s.prendreFilterlog(ip.Next(), heure) || len(s.seauxIP) != 1 {
		t.Errorf("adresses muettes non oubliées : %d seaux", len(s.seauxIP))
	}
}
