// SPDX-License-Identifier: GPL-3.0-or-later

package flotte

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"mirador-stack/internal/protocole"
	"mirador-stack/internal/server/base"
)

// Connexions SSH (demande du 27/11) : LAN ignoré, adresse nouvelle une
// fois, revue plus tard sans alerte, « C'est moi » partout, 90 jours.
func TestConnexionsSSH(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := base.Ouvrir(filepath.Join(dir, "mirador.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := base.Migrer(ctx, db, dir); err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 11, 27, 21, 0, 0, 0, time.UTC)
	ev := func(ip string) protocole.EvenementAgent {
		return protocole.EvenementAgent{Type: protocole.EvenementSSH, Objet: ip, Texte: "admin · publickey"}
	}
	noter := func(machine, ip string, le time.Time) bool {
		n, err := NoterConnexionSSH(ctx, db, 1, machine, ev(ip), le)
		if err != nil {
			panic(err)
		}
		return n
	}
	for _, ip := range []string{"192.168.1.10", "10.0.0.1", "100.64.3.2", "fe80::1", "127.0.0.1", "::ffff:192.168.1.10"} {
		if noter("web1", ip, t0) {
			t.Errorf("%s : adresse locale signalée", ip)
		}
	}
	if !noter("web1", "198.51.100.23", t0) {
		t.Fatal("adresse nouvelle non signalée")
	}
	if noter("web1", "198.51.100.23", t0.Add(time.Hour)) {
		t.Error("même adresse signalée deux fois")
	}
	if !noter("web2", "198.51.100.23", t0) {
		t.Error("autre machine : adresse nouvelle pour elle")
	}
	// Journal des événements : une ligne par adresse nouvelle.
	var n int
	_ = db.QueryRow(`SELECT count(*) FROM pannes WHERE type = 'ssh'`).Scan(&n)
	if n != 2 {
		t.Errorf("événements notés : %d", n)
	}
	l, _ := SSHNouvelles(ctx, db, t0.Add(time.Hour))
	if len(l) != 2 || l[0].Utilisateur != "admin · publickey" {
		t.Fatalf("séries à écrire : %+v", l)
	}
	if l, _ := SSHNouvelles(ctx, db, t0.Add(25*time.Hour)); len(l) != 0 {
		t.Errorf("tenue au-delà de 24 h : %+v", l)
	}
	// Revue après 90 jours : de nouveau nouvelle.
	if !noter("web1", "198.51.100.23", t0.Add(91*24*time.Hour)) {
		t.Error("revue après 90 jours : non signalée")
	}
	// « C'est moi » : plus rien, sur aucune machine.
	if err := MarquerSSHConnue(ctx, db, "198.51.100.23", "admin", t0); err != nil {
		t.Fatal(err)
	}
	if l, _ := SSHNouvelles(ctx, db, t0.Add(91*24*time.Hour)); len(l) != 0 {
		t.Errorf("connue mais encore signalée : %+v", l)
	}
	if noter("web3", "198.51.100.23", t0) {
		t.Error("adresse connue signalée sur une autre machine")
	}
	if err := MarquerSSHConnue(ctx, db, "pas une adresse", "admin", t0); err == nil {
		t.Error("adresse invalide acceptée")
	}
}
