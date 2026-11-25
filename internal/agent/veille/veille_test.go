// SPDX-License-Identifier: GPL-3.0-or-later

package veille

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"

	"mirador-stack/internal/protocole"
)

func TestNomUnite(t *testing.T) {
	for p, att := range map[dbus.ObjectPath]string{
		"/org/freedesktop/systemd1/unit/nginx_2eservice":              "nginx.service",
		"/org/freedesktop/systemd1/unit/getty_40tty1_2eservice":       "getty@tty1.service",
		"/org/freedesktop/systemd1/unit/systemd_2djournald_2eservice": "systemd-journald.service",
		"/org/freedesktop/systemd1/autre":                             "",
	} {
		if n := nomUnite(p); n != att {
			t.Errorf("%s : %q", p, n)
		}
	}
}

// Signal systemd : seulement « failed », seulement les .service suivis,
// une fois par minute et par unité.
func TestTraiter(t *testing.T) {
	var mu sync.Mutex
	var recus []protocole.EvenementAgent
	v := &Veilleur{Garder: func(u string) bool { return u != "ignore.service" },
		Signaler: func(e protocole.EvenementAgent) { mu.Lock(); recus = append(recus, e); mu.Unlock() }}
	sig := func(chemin string, etat string) *dbus.Signal {
		return &dbus.Signal{Path: dbus.ObjectPath(chemin), Name: "org.freedesktop.DBus.Properties.PropertiesChanged",
			Body: []any{"org.freedesktop.systemd1.Unit", map[string]dbus.Variant{"ActiveState": dbus.MakeVariant(etat)}, []string{}}}
	}
	v.traiter(sig("/org/freedesktop/systemd1/unit/nginx_2eservice", "active"))
	v.traiter(sig("/org/freedesktop/systemd1/unit/nginx_2eservice", "failed"))
	v.traiter(sig("/org/freedesktop/systemd1/unit/nginx_2eservice", "failed")) // même minute : une fois
	v.traiter(sig("/org/freedesktop/systemd1/unit/ignore_2eservice", "failed"))
	v.traiter(sig("/org/freedesktop/systemd1/unit/sshd_2esocket", "failed"))
	if len(recus) != 1 || recus[0].Type != protocole.EvenementService || recus[0].Objet != "nginx.service" {
		t.Fatalf("signalés : %+v", recus)
	}
	if err := recus[0].Valider(); err != nil {
		t.Errorf("événement refusé par le serveur : %v", err)
	}
}

// Retour à la normale (SPEC § 127) : signalé une fois, seulement pour une
// unité vue en échec (un « active » ordinaire ne dit rien), même juste après
// sa panne ; les états de passage (activating) attendent.
func TestRetour(t *testing.T) {
	var recus []protocole.EvenementAgent
	v := &Veilleur{Signaler: func(e protocole.EvenementAgent) { recus = append(recus, e) }}
	sig := func(unite, etat string) *dbus.Signal {
		return &dbus.Signal{Path: dbus.ObjectPath("/org/freedesktop/systemd1/unit/" + unite), Name: "org.freedesktop.DBus.Properties.PropertiesChanged",
			Body: []any{"org.freedesktop.systemd1.Unit", map[string]dbus.Variant{"ActiveState": dbus.MakeVariant(etat)}, []string{}}}
	}
	v.traiter(sig("cron_2eservice", "active")) // jamais en échec : rien
	v.traiter(sig("nginx_2eservice", "failed"))
	v.traiter(sig("nginx_2eservice", "activating"))
	v.traiter(sig("nginx_2eservice", "active"))
	v.traiter(sig("nginx_2eservice", "active")) // déjà signalé
	v.noterEchec("redis.service", true)         // en échec avant l'écoute (lireEchecs)
	v.traiter(sig("redis_2eservice", "inactive"))
	types := []string{}
	for _, e := range recus {
		types = append(types, e.Type+" "+e.Objet+" "+e.Texte)
		if err := e.Valider(); err != nil {
			t.Errorf("%+v refusé par le serveur : %v", e, err)
		}
	}
	att := []string{"service nginx.service passé en échec", "service-retour nginx.service de nouveau actif",
		"service-retour redis.service sorti de l'échec (arrêté)"}
	if len(types) != len(att) {
		t.Fatalf("signalés : %q", types)
	}
	for i := range att {
		if types[i] != att[i] {
			t.Errorf("%d : %q, attendu %q", i, types[i], att[i])
		}
	}
}

// oom_kill qui augmente : signalé dans les 2 relectures.
func TestOOM(t *testing.T) {
	ancien := rythmeOOM
	rythmeOOM = 20 * time.Millisecond
	t.Cleanup(func() { rythmeOOM = ancien })
	dir := t.TempDir()
	vmstat := filepath.Join(dir, "vmstat")
	_ = os.WriteFile(vmstat, []byte("nr_free_pages 1\noom_kill 3\n"), 0o644)
	recu := make(chan protocole.EvenementAgent, 4)
	v := &Veilleur{Proc: dir, Signaler: func(e protocole.EvenementAgent) { recu <- e }}
	ctx, fin := context.WithCancel(context.Background())
	defer fin()
	go v.Tourner(ctx)
	time.Sleep(60 * time.Millisecond)
	_ = os.WriteFile(vmstat, []byte("nr_free_pages 1\noom_kill 5\n"), 0o644)
	select {
	case e := <-recu:
		if e.Type != protocole.EvenementOOM || e.Texte != "2 processus tué(s)" {
			t.Errorf("événement : %+v", e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("oom_kill non signalé")
	}
}
