// SPDX-License-Identifier: GPL-3.0-or-later

package server

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"mirador-stack/internal/server/config"
	"mirador-stack/internal/server/console"
	"mirador-stack/internal/server/secrets"
	"mirador-stack/internal/server/stockage"
)

// TestDemoInterface : serveur de démonstration pour regarder l'interface
// (captures d'écran, essais à la main). DEMO_INTERFACE=ADRESSE (ex.
// 127.0.0.1:9441) ; le secret 2FA du compte « admin » est écrit dans
// DEMO_SECRET. Tourne DEMO_DUREE (défaut 10 min).
func TestDemoInterface(t *testing.T) {
	adr := os.Getenv("DEMO_INTERFACE")
	if adr == "" {
		t.Skip("DEMO_INTERFACE non défini")
	}
	dir := t.TempDir()
	cleF := filepath.Join(dir, "cle")
	_ = os.WriteFile(cleF, []byte("AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=\n"), 0o400)
	cfg := config.Defaut()
	cfg.EcouteAgents, cfg.EcouteInterface = portLibre(t), adr
	cfg.Donnees, cfg.FichierCle = filepath.Join(dir, "donnees"), cleF
	s, err := Demarrer(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Arreter()
	ctx, now := context.Background(), time.Now()
	machines := []string{"srv-test", "pve1", "vps-web", "nas-backup", "vm-docker"}
	var series []stockage.Serie
	for i, m := range machines {
		_, _ = s.db.Exec(`INSERT INTO machines(nom, statut, cree_le, vu_le, site, env, groupe, version_agent, architecture, derniere_ip)
			VALUES (?, 'approuvee', ?, ?, 'maison', 'vm', 'lan', '0.5.0', 'amd64', '192.0.2.1')`, m, now.Unix()-86400, now.Unix()-int64(i*100))
		mk := func(nom string, f func(k int) float64, etq ...string) {
			sr := stockage.Serie{Labels: []stockage.Label{{Nom: "__name__", Valeur: nom}, {Nom: "instance", Valeur: m},
				{Nom: "job", Valeur: "integrations/unix"}, {Nom: "site", Valeur: "maison"}, {Nom: "env", Valeur: "vm"}}}
			for j := 0; j+1 < len(etq); j += 2 {
				sr.Remplacer(etq[j], etq[j+1])
			}
			for k := 0; k < 720; k++ {
				sr.Points = append(sr.Points, stockage.Point{T: now.Add(time.Duration(k-719) * 30 * time.Second).UnixMilli(), V: f(k)})
			}
			series = append(series, sr)
		}
		ph := float64(i)
		mk("node_uname_info", func(int) float64 { return 1 })
		// Compteur « idle » toujours croissant : CPU entre ~5 et ~35 %.
		mk("node_cpu_seconds_total", func(k int) float64 { return float64(k)*(26-2*ph) - 90*math.Sin(float64(k)/30+ph) }, "cpu", "0", "mode", "idle")
		mk("node_memory_MemAvailable_bytes", func(k int) float64 { return float64(8<<30) * (0.62 - 0.09*ph + 0.06*math.Sin(float64(k)/90)) })
		mk("node_memory_MemTotal_bytes", func(int) float64 { return 8 << 30 })
		mk("node_load5", func(k int) float64 { return 0.2 + 0.1*math.Sin(float64(k)/30+ph) })
		mk("node_time_seconds", func(k int) float64 { return float64(now.Unix()) })
		mk("node_boot_time_seconds", func(int) float64 { return float64(now.Unix() - int64(86400*(i+1))) })
		mk("node_filesystem_avail_bytes", func(int) float64 { return float64(40-7*i) * (1 << 30) }, "mountpoint", "/", "fstype", "ext4", "device", "/dev/sda1")
		mk("node_filesystem_size_bytes", func(int) float64 { return 50 << 30 }, "mountpoint", "/", "fstype", "ext4", "device", "/dev/sda1")
		mk("node_network_receive_bytes_total", func(k int) float64 { return float64(k) * 3e6 * (1 + ph/3) }, "device", "eth0")
		mk("node_network_transmit_bytes_total", func(k int) float64 { return float64(k) * 1e6 }, "device", "eth0")
		mk("node_systemd_unit_state", func(int) float64 { return map[bool]float64{true: 1, false: 0}[i == 2] }, "name", "nginx.service", "state", "failed")
	}
	if err := s.met.Ecrire(ctx, series); err != nil {
		t.Fatal(err)
	}
	_, _ = s.db.Exec(`INSERT INTO machines(nom, statut, cree_le, ip_enrolement) VALUES ('nouvelle-vm', 'en_attente', ?, '198.51.100.20')`, now.Unix())
	_, _ = s.db.Exec(`INSERT INTO alertes(regle, etiquettes, gravite, etat, debut, active_le, resume, valeur_max)
		VALUES ('ServiceEnEchec', '{"alertname":"ServiceEnEchec","instance":"vps-web","name":"nginx.service","severity":"warning"}', 'warning', 'active', ?, ?, 'Service nginx.service en échec sur vps-web', 1)`,
		now.Add(-40*time.Minute).UnixMilli(), now.Add(-35*time.Minute).UnixMilli())
	var lignes []stockage.Ligne
	for k := 0; k < 60; k++ {
		lignes = append(lignes, stockage.Ligne{T: now.Add(time.Duration(-k) * time.Minute).UnixMilli(), Msg: fmt.Sprintf("Accepted publickey for admin from 192.0.2.%d port 5%03d ssh2", k%7, k),
			Champs: map[string]string{"host": machines[k%5], "job": "journal", "unit": "ssh.service", "level": "info"}})
	}
	lignes = append(lignes, stockage.Ligne{T: now.UnixMilli(), Msg: "nginx: [emerg] bind() to 0.0.0.0:443 failed (98: Address already in use)",
		Champs: map[string]string{"host": "vps-web", "job": "journal", "unit": "nginx.service", "level": "err"}})
	_ = s.jx.Ecrire(ctx, lignes)
	_ = console.CreerUtilisateur(ctx, s.db, "admin", console.RoleAdmin, "une longue phrase de passe", "demo", "", now)
	secret, _ := console.NouveauSecretTOTP()
	ch, _ := secrets.Chiffrer(s.cle, "console.totp", secret)
	_ = console.DefinirTOTP(ctx, s.db, "admin", ch, 0, "demo", "", now)
	if f := os.Getenv("DEMO_SECRET"); f != "" {
		_ = os.WriteFile(f, []byte(secret), 0o600)
	}
	d, _ := time.ParseDuration(os.Getenv("DEMO_DUREE"))
	if d == 0 {
		d = 10 * time.Minute
	}
	t.Logf("interface sur http://%s (admin / une longue phrase de passe)", adr)
	time.Sleep(d)
}
