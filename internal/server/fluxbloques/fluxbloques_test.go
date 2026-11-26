// SPDX-License-Identifier: GPL-3.0-or-later

package fluxbloques

import (
	"context"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mirador-stack/internal/protocole"
	"mirador-stack/internal/server/base"
	"mirador-stack/internal/server/stockage"
)

type horloge struct{ t time.Time }

func (h *horloge) maintenant() time.Time { return h.t }

func flux(ip string, proto string, port, n int) protocole.FluxBloque {
	return protocole.FluxBloque{IP: netip.MustParseAddr(ip), Proto: proto, Port: port, N: n, Interface: "eth0"}
}

var paysTest = map[string]string{"45.0.0.1": "CN", "45.0.0.2": "CN", "89.0.0.1": "NL", "167.0.0.1": "US"}

func ouvrir(t *testing.T, chemin string, h *horloge) *Magasin {
	t.Helper()
	db, err := base.Ouvrir(chemin)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	m, err := Nouveau(context.Background(), Options{DB: db, Maintenant: h.maintenant,
		Pays: func(a netip.Addr) string { return paysTest[a.String()] }})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestMagasinBilanEtRedemarrage(t *testing.T) {
	ctx := context.Background()
	chemin := filepath.Join(t.TempDir(), "flux.db")
	h := &horloge{time.Date(2026, 11, 26, 19, 20, 30, 0, time.UTC)}
	m := ouvrir(t, chemin, h)
	m.Recevoir("fw", flux("45.0.0.1", "tcp", 22, 10), h.t)
	m.Recevoir("vps", flux("45.0.0.1", "tcp", 22, 2), h.t)
	m.Recevoir("fw", flux("89.0.0.1", "tcp", 5432, 5), h.t.Add(-2*time.Minute))
	m.Recevoir("fw", flux("167.0.0.1", "udp", 53, 1), h.t)
	m.Recevoir("", flux("167.0.0.1", "udp", 53, 1), h.t) // sans pare-feu : ignoré

	b, err := m.Bilan(ctx, h.t.Add(-time.Hour), h.t, Filtre{}, 60)
	if err != nil {
		t.Fatal(err)
	}
	if b.Total != 18 || b.NbIP != 3 || b.NbPays != 3 || len(b.Barres) != 61 {
		t.Fatalf("bilan : total %d, ip %d, pays %d, barres %d", b.Total, b.NbIP, b.NbPays, len(b.Barres))
	}
	if b.Pays[0] != (Compte{Nom: "CN", N: 12}) || b.Ports[0].Nom != "tcp/22" || b.PareFeux[0] != (Compte{Nom: "fw", N: 16}) {
		t.Errorf("classements : %+v %+v %+v", b.Pays, b.Ports, b.PareFeux)
	}
	if b.IPs[0].Nom != "45.0.0.1" || b.IPs[0].PareFeux != 2 {
		t.Errorf("adresses : %+v", b.IPs)
	}
	if b.PaysPareFeu[Paire{"CN", "vps"}] != 2 || b.PareFeuPort[Paire{"fw", "tcp/5432"}] != 5 {
		t.Errorf("liens : %v %v", b.PaysPareFeu, b.PareFeuPort)
	}
	// Filtres.
	if b, _ := m.Bilan(ctx, h.t.Add(-time.Hour), h.t, Filtre{PareFeu: "vps"}, 60); b.Total != 2 || len(b.IPs) != 1 {
		t.Errorf("filtre pare-feu : %+v", b)
	}
	if b, _ := m.Bilan(ctx, h.t.Add(-time.Hour), h.t, Filtre{Pays: "CN"}, 60); b.Total != 12 || len(b.Ports) != 1 || len(b.IPs) != 1 || !b.Estime {
		t.Errorf("filtre pays : %+v", b)
	}
	if b, _ := m.Bilan(ctx, h.t.Add(-time.Hour), h.t, Filtre{Port: "tcp/5432"}, 60); b.Total != 5 || b.Pays[0].Nom != "NL" || !b.SansAdresses || len(b.IPs) != 0 {
		t.Errorf("filtre port : %+v", b)
	}
	// Fil en direct.
	ev, _ := m.Depuis(0, 2)
	if len(ev) != 2 || ev[1].Port != "udp/53" || ev[1].Pays != "US" || ev[0].ID >= ev[1].ID {
		t.Errorf("fil : %+v", ev)
	}
	if ev, _ := m.Depuis(m.DernierID(), 10); len(ev) != 0 {
		t.Errorf("fil après le dernier : %+v", ev)
	}

	// Écriture, puis redémarrage dans la même heure : rien de perdu ni compté deux fois.
	m.Ecrire(ctx, true)
	m2 := ouvrir(t, chemin, h)
	m2.Recevoir("fw", flux("45.0.0.2", "tcp", 23, 4), h.t)
	m2.Ecrire(ctx, true)
	h.t = h.t.Add(2 * time.Hour) // heure terminée : écrite par la boucle
	m2.Ecrire(ctx, false)
	b, err = m2.Bilan(ctx, h.t.Add(-24*time.Hour), h.t, Filtre{}, 24)
	if err != nil {
		t.Fatal(err)
	}
	if b.Total != 22 || b.NbIP != 4 || b.Pays[0] != (Compte{Nom: "CN", N: 16}) {
		t.Fatalf("après redémarrage : total %d, ip %d, %+v", b.Total, b.NbIP, b.Pays)
	}
	// Période d'avant connue (une heure écrite au moins dans la base) ?
	h.t = h.t.Add(25 * time.Hour)
	heureDonnees := time.Date(2026, 11, 26, 19, 0, 0, 0, time.UTC)
	b, _ = m2.Bilan(ctx, heureDonnees.Add(2*time.Hour), heureDonnees.Add(4*time.Hour), Filtre{}, 2)
	if !b.AvantConnu || b.Avant != 22 || b.Total != 0 {
		t.Errorf("période d'avant : %+v", b)
	}
	// Base plus jeune que la période d'avant : pas de comparaison trompeuse.
	if b, _ = m2.Bilan(ctx, h.t.Add(-24*time.Hour), h.t, Filtre{}, 24); b.AvantConnu {
		t.Errorf("période d'avant incomplète comparée : %+v", b.Avant)
	}

	// Conservation : adresses 7 jours, le reste 30 jours.
	h.t = h.t.Add(8 * 24 * time.Hour)
	m2.Nettoyer(ctx)
	b, _ = m2.Bilan(ctx, h.t.Add(-30*24*time.Hour), h.t, Filtre{}, 30)
	if b.Total != 22 || b.NbIP != 0 {
		t.Errorf("après 8 jours : total %d, ip %d", b.Total, b.NbIP)
	}
	h.t = h.t.Add(30 * 24 * time.Hour)
	m2.Nettoyer(ctx)
	if b, _ = m2.Bilan(ctx, h.t.Add(-30*24*time.Hour), h.t, Filtre{}, 30); b.Total != 0 {
		t.Errorf("après 38 jours : %d", b.Total)
	}
}

func TestMagasinBorneEtPic(t *testing.T) {
	ctx := context.Background()
	h := &horloge{time.Date(2026, 11, 26, 19, 59, 0, 0, time.UTC)}
	m, err := Nouveau(ctx, Options{Maintenant: h.maintenant})
	if err != nil {
		t.Fatal(err)
	}
	// Une minute de balayage massif : mémoire bornée.
	for i := range maxIPMinute + 50 {
		m.Recevoir("fw", flux(netip.AddrFrom4([4]byte{45, 1, byte(i >> 8), byte(i)}).String(), "tcp", i%1000+1, 1), h.t)
	}
	if n := len(m.minutes[(h.t.Unix()/60)%60].ips); n != maxIPMinute {
		t.Errorf("adresses gardées : %d", n)
	}
	if n := len(m.minutes[(h.t.Unix()/60)%60].ports); n > maxAutresMinute+1 {
		t.Errorf("ports gardés : %d", n)
	}
	// Quelques blocages réguliers avant : la minute du balayage sort du lot.
	for k := 1; k <= 30; k++ {
		m.Recevoir("fw", flux("89.0.0.1", "tcp", 22, 3), h.t.Add(-time.Duration(k)*time.Minute))
	}
	b, err := m.Bilan(ctx, h.t.Add(-time.Hour), h.t.Add(time.Minute), Filtre{}, 60)
	if err != nil {
		t.Fatal(err)
	}
	if !b.IPPlus || b.Total != maxIPMinute+50+90 || b.Pic < 0 || b.Barres[b.Pic].N != maxIPMinute+50 || b.PicIP == "" {
		t.Errorf("bilan : total %d, plus %v, pic %d %q", b.Total, b.IPPlus, b.Pic, b.PicIP)
	}
	min, enCours := m.ParMinute(3)
	if len(min) != 3 || min[2] != 3 || enCours != maxIPMinute+50 {
		t.Errorf("par minute : %v %d", min, enCours)
	}
	// Ligne très en retard (tampon d'un agent) : comptée maintenant.
	m.Recevoir("fw", flux("89.0.0.1", "tcp", 22, 1), h.t.Add(-5*time.Hour))
	if _, enCours := m.ParMinute(1); enCours != maxIPMinute+51 {
		t.Errorf("ligne en retard : %d", enCours)
	}
}

// Branchement sur l'ingestion et réglage par machine (SPEC § 136).
func TestFluxLignesEtReglages(t *testing.T) {
	h := &horloge{time.Date(2026, 11, 26, 19, 20, 0, 0, time.UTC)}
	nouveau := func() *Flux {
		e, _ := Nouveau(context.Background(), Options{Maintenant: h.maintenant})
		so, _ := Nouveau(context.Background(), Options{Maintenant: h.maintenant})
		return NouveauFlux(e, so)
	}
	ligne := func(hote, msg, ident string) stockage.Ligne {
		c := map[string]string{"host": hote}
		if ident != "" {
			c["ident"] = ident
		}
		return stockage.Ligne{T: h.t.UnixMilli(), Msg: msg, Champs: c}
	}
	comptage := ligne("fw", "règle 81 IN=igb0 OUT= SRC=45.0.0.1 PROTO=TCP DPT=23 N=40", protocole.IdentFlux)
	sortantFw := ligne("fw", "règle 9 IN= OUT=igb0 SRC=192.168.1.9 DST=93.184.216.34 PROTO=TCP DPT=25 N=3 SENS=sortant", protocole.IdentFlux)
	entrant := ligne("vps", "DROP_IN: IN=eth0 OUT= SRC=45.0.0.2 PROTO=TCP DPT=22", "")
	sortant := ligne("vps", "DROP_OUT: IN= OUT=eth0 SRC=10.0.0.5 DST=8.8.4.4 PROTO=UDP DPT=53", "")
	lan := ligne("vps", "DROP_IN: IN=eth0 OUT= SRC=192.168.1.4 PROTO=TCP DPT=22", "")
	ordinaire := ligne("vps", "bonjour", "")

	// Défaut : entrants comptés, sortants ignorés, lignes du pare-feu pas gardées.
	x := nouveau()
	garde := x.Lignes(true, []stockage.Ligne{comptage, sortantFw, entrant, sortant, lan, ordinaire})
	if len(garde) != 1 || garde[0].Msg != "bonjour" {
		t.Fatalf("gardées : %+v", garde)
	}
	if x.Entrants.Recus() != 41 || x.Sortants.Recus() != 0 {
		t.Errorf("comptés : %d entrants, %d sortants", x.Entrants.Recus(), x.Sortants.Recus())
	}
	// Une ligne de comptage venue d'un agent : log ordinaire, comptée 1.
	if garde := x.Lignes(false, []stockage.Ligne{ligne("vps", comptage.Msg, protocole.IdentFlux)}); len(garde) != 0 || x.Entrants.Recus() != 42 {
		t.Errorf("agent : %d gardées, %d comptés", len(garde), x.Entrants.Recus())
	}

	// Sortants cochés, lignes gardées sur vps ; fw en lecture désactivée.
	x = nouveau()
	x.DefinirReglages(map[string]Reglage{
		"vps": {Lecture: LectureAuto, Entrants: true, Sortants: true, GarderLignes: true},
		"fw":  {Lecture: LectureNon, Entrants: true},
	})
	garde = x.Lignes(true, []stockage.Ligne{comptage, sortantFw, entrant, sortant, ordinaire})
	if len(garde) != 3 || x.Entrants.Recus() != 1 || x.Sortants.Recus() != 1 {
		t.Fatalf("gardées %d, entrants %d, sortants %d", len(garde), x.Entrants.Recus(), x.Sortants.Recus())
	}
	b, _ := x.Sortants.Bilan(context.Background(), h.t.Add(-time.Hour), h.t.Add(time.Minute), Filtre{}, 60)
	if len(b.Sources) != 1 || b.Sources[0].Nom != "10.0.0.5" || b.IPs[0].Nom != "8.8.4.4" {
		t.Errorf("sortants : sources %+v, adresses %+v", b.Sources, b.IPs)
	}

	// Seulement certains préfixes : les autres lignes du noyau restent des logs ordinaires.
	x = nouveau()
	x.DefinirReglages(map[string]Reglage{"vps": {Lecture: LecturePrefixes, Prefixes: []string{"DROP_OUT:"}, Entrants: true, Sortants: true}})
	garde = x.Lignes(false, []stockage.Ligne{entrant, sortant})
	if len(garde) != 1 || garde[0].Msg != entrant.Msg || x.Entrants.Recus() != 0 || x.Sortants.Recus() != 1 {
		t.Errorf("préfixes : gardées %+v, %d / %d", garde, x.Entrants.Recus(), x.Sortants.Recus())
	}
	// Réglage bancal : normalisé.
	r := Reglage{Lecture: "n'importe", Prefixes: []string{" ", strings.Repeat("x", 50), "UFW BLOCK"}}.Normaliser()
	if r.Lecture != LectureAuto || len(r.Prefixes) != 1 {
		t.Errorf("normalisé : %+v", r)
	}
	if r = (Reglage{Lecture: LecturePrefixes}).Normaliser(); r.Lecture != LectureAuto {
		t.Errorf("préfixes vides : %+v", r)
	}
}

// Coût côté serveur (SPEC § 135) : une ligne iptables reçue.
func BenchmarkLignes(b *testing.B) {
	e, _ := Nouveau(context.Background(), Options{Pays: func(netip.Addr) string { return "CN" }})
	so, _ := Nouveau(context.Background(), Options{Pays: func(netip.Addr) string { return "CN" }})
	m := NouveauFlux(e, so)
	l := []stockage.Ligne{{T: time.Now().UnixMilli(), Msg: "[12345.6] DROP_IN: IN=eth0 OUT= MAC=00 SRC=45.143.203.10 DST=1.2.3.4 LEN=40 PROTO=TCP SPT=4444 DPT=22",
		Champs: map[string]string{"host": "vps"}}}
	ordinaire := []stockage.Ligne{{T: time.Now().UnixMilli(), Msg: "Started Session 12 of User root.", Champs: map[string]string{"host": "vps"}}}
	b.Run("iptables", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			m.Lignes(false, l)
		}
	})
	b.Run("ordinaire", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			m.Lignes(false, ordinaire)
		}
	})
}
