// SPDX-License-Identifier: GPL-3.0-or-later

package motifs

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"mirador-stack/internal/server/base"
)

func TestRegroupeur(t *testing.T) {
	msgs := []string{
		"Invalid user admin from 203.0.113.5 port 51234",
		"Invalid user oracle from 198.51.100.7 port 4022",
		"Invalid user git from 192.0.2.1 port 60000",
		"DHCPACK(eth0) 192.168.1.20 aa:bb:cc:dd:ee:01 phone-alice",
		"DHCPACK(eth0) 192.168.1.21 aa:bb:cc:dd:ee:02 tv-salon",
		"Started apt-daily.service - Daily apt download activities.",
		"Started logrotate.service - Rotate log files.",
		"status update time (2.017 seconds)",
		"status update time (0.391 seconds)",
	}
	r := NouveauRegroupeur()
	for i, m := range msgs {
		r.Ajouter(i, m)
	}
	textes := map[string]int{}
	for _, g := range r.Groupes() {
		textes[Texte(Gabarit(g.Jetons))] = len(g.Indices)
	}
	attendus := map[string]int{
		"Invalid user ‹MOT› from ‹IP› port ‹N›":                      3,
		"DHCPACK(eth0) ‹IP› ‹IP› ‹MOT›":                              2, // adresse MAC vue comme une IP (comme avant)
		"status update time (‹N› seconds)":                           2,
		"Started apt-daily.service - Daily apt download activities.": 1, // phrases différentes : pas fusionnées
	}
	for txt, n := range attendus {
		if textes[txt] != n {
			t.Errorf("%q : %d (groupes : %v)", txt, textes[txt], textes)
		}
	}
}

func baseTest(t testing.TB) (ctx context.Context, dossier string) {
	return context.Background(), t.TempDir()
}

func TestRegistre(t *testing.T) {
	ctx, d := baseTest(t)
	db, err := base.Ouvrir(filepath.Join(d, "mirador.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := base.Migrer(ctx, db, filepath.Join(d, "copies")); err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	r, err := OuvrirRegistre(ctx, db, nil, t0)
	if err != nil {
		t.Fatal(err)
	}
	r.Ajouter("Invalid user admin from 203.0.113.5 port 51234", t0.Unix())
	r.Ajouter("Invalid user git from 192.0.2.1 port 60000", t0.Add(time.Hour).Unix())
	if err := r.Ecrire(ctx); err != nil {
		t.Fatal(err)
	}
	// 10 jours plus tard : un motif jamais vu.
	t1 := t0.Add(10 * 24 * time.Hour)
	r.Ajouter("kernel: ata3.00: exception Emask 0x0 SAct 0x1 frozen", t1.Unix())
	if err := r.Ecrire(ctx); err != nil {
		t.Fatal(err)
	}
	// Relu en base (redémarrage du serveur).
	r2, err := OuvrirRegistre(ctx, db, nil, t1)
	if err != nil || r2.Nombre() != 2 {
		t.Fatalf("relu : %d motifs, %v", r2.Nombre(), err)
	}
	premier, ok := r2.Vu(Jetons("Invalid user root from 10.0.0.1 port 22"))
	if !ok || premier != t0.Unix() || r2.Nouveau(premier, t1) {
		t.Errorf("motif ancien : %v %v", premier, ok)
	}
	premier, ok = r2.Vu(Jetons("kernel: ata3.00: exception Emask 0x0 SAct 0x1 frozen"))
	if !ok || !r2.Nouveau(premier, t1) || r2.Apprentissage(t1) {
		t.Errorf("motif nouveau : %v %v", premier, ok)
	}
	// Pendant les 7 premiers jours : jamais « nouveau ».
	if r.Nouveau(t0.Add(time.Hour).Unix(), t0.Add(2*time.Hour)) || !r.Apprentissage(t0.Add(time.Hour)) {
		t.Error("apprentissage")
	}
}

func TestRegistrePlafond(t *testing.T) {
	ctx, d := baseTest(t)
	db, _ := base.Ouvrir(filepath.Join(d, "mirador.db"))
	defer db.Close()
	_, _ = base.Migrer(ctx, db, filepath.Join(d, "copies"))
	r, _ := OuvrirRegistre(ctx, db, nil, time.Now())
	for i := 0; i < MaxMotifs+50; i++ {
		r.Ajouter(fmt.Sprintf("app%d démarre", i), int64(i))
	}
	if n := r.Nombre(); n > MaxMotifs || n < MaxMotifs*8/10 {
		t.Fatalf("%d motifs gardés", n)
	}
	if err := r.Ecrire(ctx); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = db.QueryRow(`SELECT count(*) FROM motifs_logs`).Scan(&n)
	if n != r.Nombre() {
		t.Errorf("base %d, mémoire %d", n, r.Nombre())
	}
}

// Coût du registre (lignes réalistes variées).
func BenchmarkRegistre(b *testing.B) {
	ctx, d := baseTest(b)
	db, _ := base.Ouvrir(filepath.Join(d, "mirador.db"))
	defer db.Close()
	_, _ = base.Migrer(ctx, db, filepath.Join(d, "copies"))
	r, _ := OuvrirRegistre(ctx, db, nil, time.Now())
	modeles := []string{
		`203.0.113.%d - - [18/Nov/2026:10:%02d:11 +0100] "GET /api/states HTTP/1.1" 200 %d "-" "Mozilla/5.0" %dms`,
		"2026-11-18 10:%02d:%02d.123 DEBUG (MainThread) [homeassistant.components.mqtt] Received message on zigbee2mqtt/capteur_%d: b'{\"temperature\":%d}'",
		"status update time (%d.%03d seconds)",
		"Invalid user u%d from 198.51.100.%d port %d",
		"DHCPACK(eth0) 192.168.1.%d aa:bb:cc:dd:ee:%02x host-%d",
		"Started unit-%d.service - Some job %d.",
	}
	lignes := make([]string, 5000)
	for i := range lignes {
		m := modeles[i%len(modeles)]
		lignes[i] = fmt.Sprintf(m, i%250, i%60, i*7%9999, i%400)
	}
	b.ResetTimer()
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		r.Ajouter(lignes[i%len(lignes)], int64(i))
		i++
	}
	b.ReportMetric(float64(r.Nombre()), "motifs")
}
