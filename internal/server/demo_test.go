// SPDX-License-Identifier: GPL-3.0-or-later

package server

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math"
	"mirador-stack/internal/server/auth"
	"mirador-stack/internal/server/certsyslog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"mirador-stack/internal/server/alertes"
	"mirador-stack/internal/server/config"
	"mirador-stack/internal/server/flotte"
	"mirador-stack/internal/server/modeles"
	"mirador-stack/internal/server/notif"
	"mirador-stack/internal/server/secrets"
	"mirador-stack/internal/server/servicesmetier"
	"mirador-stack/internal/server/stockage"
	essais "mirador-stack/internal/sondes"
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
			// 6 h de données, plus 30 min « en avance » : la page garde des
			// valeurs actuelles le temps des captures.
			for k := 0; k < 780; k++ {
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
		// CPU par mode et répartition de la mémoire (aires empilées, SPEC § 31).
		for j, mode := range []string{"user", "system", "iowait"} {
			part := []float64{3, 1.2, 0.4}[j]
			mk("node_cpu_seconds_total", func(k int) float64 { return float64(k)*part*(1+ph/4) + 20*math.Sin(float64(k)/40+ph+float64(j)) }, "cpu", "0", "mode", mode)
		}
		mk("node_memory_Cached_bytes", func(k int) float64 { return float64(8<<30) * (0.25 + 0.03*math.Sin(float64(k)/120)) })
		mk("node_memory_Buffers_bytes", func(int) float64 { return 200 << 20 })
		mk("node_load5", func(k int) float64 { return 0.2 + 0.1*math.Sin(float64(k)/30+ph) })
		mk("node_time_seconds", func(k int) float64 { return float64(now.Unix()) })
		mk("node_boot_time_seconds", func(k int) float64 {
			if m == "pve1" && k >= 600 { // redémarrée il y a une heure (événement sur les courbes)
				return float64(now.Add(-time.Hour).Unix())
			}
			return float64(now.Unix() - int64(86400*(i+1)))
		})
		mk("node_filesystem_avail_bytes", func(int) float64 { return float64(40-7*i) * (1 << 30) }, "mountpoint", "/", "fstype", "ext4", "device", "/dev/sda1")
		mk("node_filesystem_size_bytes", func(int) float64 { return 50 << 30 }, "mountpoint", "/", "fstype", "ext4", "device", "/dev/sda1")
		mk("node_network_receive_bytes_total", func(k int) float64 { return float64(k) * 3e6 * (1 + ph/3) }, "device", "eth0")
		mk("node_network_transmit_bytes_total", func(k int) float64 { return float64(k) * 1e6 }, "device", "eth0")
		mk("node_systemd_unit_state", func(int) float64 { return map[bool]float64{true: 1, false: 0}[i == 2] }, "name", "nginx.service", "state", "failed")
		// Bloc « À surveiller » (SPEC § 19) : ce que chaque machine envoie déjà.
		un := func(int) float64 { return 1 }
		mk("mirador_agent_info", un, "version", "0.5.0")
		mk("node_systemd_enabled_units_total", func(int) float64 { return 3 })
		mk("node_systemd_enabled_units_down", func(int) float64 { return 0 })
		for _, svc := range []string{"ssh.service", "cron.service", "nginx.service"} {
			mk("node_systemd_unit_enabled_active", un, "name", svc)
		}
		mk("apt_upgrades_pending", func(int) float64 { return float64(3 * i) })
		// Services, paquets, cœurs (SPEC § 42) : blocs repliables et panneau de détail.
		for _, svc := range []string{"ssh.service", "cron.service", "docker.service", "sauvegarde.service", "systemd-journald.service"} {
			mk("node_systemd_unit_state", func(int) float64 { return 0 }, "name", svc, "state", "failed")
		}
		mk("node_systemd_unit_enabled_active", un, "name", "docker.service")
		if i > 0 {
			mk("apt_upgrades_security_pending", func(int) float64 { return 1 })
			for j, p := range [][3]string{{"openssl", "3.0.15-1~deb12u1", "3.0.17-1~deb12u2"}, {"curl", "7.88.1-10+deb12u8", "7.88.1-10+deb12u12"}, {"tzdata", "2025a-0+deb12u1", "2025b-0+deb12u1"}} {
				if j < 3*i {
					mk("apt_upgrade_package", un, "package", p[0], "from", p[1], "to", p[2], "security", map[bool]string{true: "1", false: "0"}[j == 0])
				}
			}
		}
		mk("node_cpu_seconds_total", func(k int) float64 { return float64(k)*(20-3*ph) + 40*math.Sin(float64(k)/25+ph) }, "cpu", "1", "mode", "idle")
		mk("node_cpu_seconds_total", func(k int) float64 { return float64(k) * 4 }, "cpu", "1", "mode", "user")
		mk("mirador_logs_journal_lines_read_total", func(k int) float64 { return float64(k) })
		mk("node_firewall_input_policy_drop", un, "tool", "nftables", "policy", "drop")
		mk("node_firewall_input_rules", func(int) float64 { return 14 }, "tool", "nftables", "policy", "drop")
		if m == "srv-test" { // disque qui se remplit : « plein dans 12 j »
			mk("node_filesystem_avail_bytes", func(k int) float64 { return (15 - 0.05*float64(k)/120) * (1 << 30) }, "mountpoint", "/var", "fstype", "ext4", "device", "/dev/sda2")
			mk("node_filesystem_size_bytes", func(int) float64 { return 100 << 30 }, "mountpoint", "/var", "fstype", "ext4", "device", "/dev/sda2")
		}
		if m == "pve1" { // machine physique : capteurs, disques, et des nouveautés à valider
			mk("node_hwmon_temp_celsius", func(int) float64 { return 54 }, "chip", "coretemp", "sensor", "temp1")
			mk("node_hwmon_temp_celsius", func(int) float64 { return 43 }, "chip", "nct6798", "sensor", "temp2")
			// Tâches planifiées (SPEC § 80) : timers, sauvegardes Proxmox.
			for _, x := range []struct {
				src, nom  string
				il, duree time.Duration
				code      int
				proch     time.Duration
			}{{"systemd", "sauvegarde-restic", 5 * time.Hour, 42 * time.Minute, 0, 19 * time.Hour}, {"systemd", "certbot", 3 * time.Hour, 20 * time.Second, 0, 9 * time.Hour},
				{"systemd", "apt-daily", 2 * time.Hour, time.Minute, 0, 10 * time.Hour}, {"pve", "vzdump", 6 * time.Hour, 25 * time.Minute, 1, 0},
				{"systemd", "logrotate", 8 * time.Hour, 3 * time.Second, 0, 16 * time.Hour}} {
				fin := now.Add(-x.il)
				mk("mirador_tache_fin_timestamp_seconds", func(int) float64 { return float64(fin.Unix()) }, "source", x.src, "tache", x.nom)
				mk("mirador_tache_debut_timestamp_seconds", func(int) float64 { return float64(fin.Add(-x.duree).Unix()) }, "source", x.src, "tache", x.nom)
				mk("mirador_tache_code", func(int) float64 { return float64(x.code) }, "source", x.src, "tache", x.nom)
				if x.proch > 0 {
					p := now.Add(x.proch)
					mk("mirador_tache_prochaine_timestamp_seconds", func(int) float64 { return float64(p.Unix()) }, "source", x.src, "tache", x.nom)
				}
				if x.code != 0 {
					mk("mirador_tache_message", un, "source", x.src, "tache", x.nom, "message", "job errors")
				}
			}
			for i, d := range []string{"sda", "sdb", "nvme0"} {
				mk("smartctl_device_smart_status", un, "device", d)
				base := 36 + 6*float64(i) // nvme0 plus chaud, qui monte à la fin (alerte « Disque chaud »)
				mk("smartctl_device_temperature", func(k int) float64 {
					v := base + 2*math.Sin(float64(k)/40)
					if d == "nvme0" && k > 700 {
						v = 60
					}
					return v
				}, "device", d, "temperature_type", "current")
			}
			mk("ssl_certs_found_total", func(int) float64 { return 2 })
			// Détail par élément (SPEC § 21).
			mk("ssl_cert_not_after_timestamp_seconds", func(int) float64 { return float64(now.Add(54 * 24 * time.Hour).Unix()) },
				"path", "/etc/pve/local/pveproxy-ssl.pem", "cn", "pve1.maison.lan", "issuer", "R11")
			mk("ssl_cert_not_after_timestamp_seconds", func(int) float64 { return float64(now.Add(5 * 24 * time.Hour).Unix()) },
				"path", "/etc/ssl/local/imprimante.crt", "cn", "imprimante.lan", "issuer", "CA maison")
			for _, d := range []string{"sda", "sdb", "nvme0n1"} {
				mk("node_disk_io_time_seconds_total", func(k int) float64 { return float64(k) }, "device", d)
			}
			mk("node_network_receive_bytes_total", func(k int) float64 { return float64(k) }, "device", "vmbr0")
			mk("node_filesystem_size_bytes", func(int) float64 { return 2 << 40 }, "mountpoint", "/mnt/sauvegarde", "fstype", "ext4", "device", "/dev/sdb1")
			mk("mirador_agent_detecte", func(int) float64 { return 3 }, "genre", "ventilateurs", "nom", "capteurs")
			mk("mirador_agent_detecte", un, "genre", "zfs", "nom", "rpool")
			mk("mirador_agent_detecte", un, "genre", "service", "nom", "pve-cluster.service")
			mk("mirador_agent_detecte", un, "genre", "service", "nom", "postfix.service")
		}
		if m == "vm-docker" {
			mk("node_docker_up", un)
			for j, ct := range []string{"traefik", "postgres", "appli"} {
				mk("node_docker_container_running", un, "name", ct, "project", "site", "state", "running", "restart_policy", "unless-stopped")
				mk("node_docker_container_started_timestamp_seconds", func(int) float64 { return float64(now.Add(-time.Duration(j+1) * 26 * time.Hour).Unix()) }, "name", ct, "project", "site")
				mk("node_docker_container_memory_limit_bytes", func(int) float64 { return 3 << 30 }, "name", ct, "project", "site")
				// Plus gourmands (SPEC § 31) : postgres en tête.
				part := []float64{0.05, 0.35, 0.15}[j]
				mk("node_docker_container_cpu_seconds_total", func(k int) float64 { return float64(k) * 30 * part }, "name", ct, "project", "site")
				mk("node_docker_container_memory_bytes", func(int) float64 { return part * 6 * (1 << 30) }, "name", ct, "project", "site")
			}
			mk("node_docker_container_healthy", func(int) float64 { return 0 }, "name", "appli", "project", "site")
			mk("node_docker_container_healthy", un, "name", "postgres", "project", "site")
			mk("node_docker_container_running", func(int) float64 { return 0 }, "name", "cache", "project", "site", "state", "exited", "restart_policy", "always")
			mk("mirador_logs_docker_streams", func(int) float64 { return 3 })
		}
	}
	// Mise à jour en échec (SPEC § 44) : bloc « Réessayer la mise à jour » sur pve1.
	_, _ = s.db.Exec(`UPDATE machines SET version_cible = '0.5.1', maj_echec = '0.5.1 : pas de battement en 10 min, retour à 0.5.0' WHERE nom = 'pve1'`)
	// Agents récents (capacité « surveillance ») ; pve1 valide ses services un par un.
	_, _ = s.db.Exec(`UPDATE machines SET capacites = 'surveillance,elements'`)
	_, _ = s.db.Exec(`INSERT INTO reglages(cle, valeur, modifie_le, modifie_par)
		SELECT 'config_agent:machine:' || id, '{"systemd_ignorer":["fwupd-refresh.service"],"systemd_surveiller":["cron.service","nginx.service","ssh.service"],"systemd_validation":true,"ignorer_interfaces":["ens19"],"certificats_ajout":["/etc/nginx/ssl/*.crt"]}', ?, 'demo'
		FROM machines WHERE nom = 'pve1'`, now.Unix())
	if err := s.met.Ecrire(ctx, series); err != nil {
		t.Fatal(err)
	}
	_, _ = s.db.Exec(`INSERT INTO machines(nom, statut, cree_le, ip_enrolement) VALUES ('nouvelle-vm', 'en_attente', ?, '198.51.100.20')`, now.Unix())
	_, _ = s.db.Exec(`INSERT INTO alertes(regle, etiquettes, gravite, etat, debut, active_le, resume, valeur_max)
		VALUES ('ServiceEnEchec', '{"alertname":"ServiceEnEchec","instance":"vps-web","name":"nginx.service","severity":"warning"}', 'warning', 'active', ?, ?, 'Service nginx.service en échec sur vps-web', 1)`,
		now.Add(-40*time.Minute).UnixMilli(), now.Add(-35*time.Minute).UnixMilli())
	// Tâches suivies (SPEC § 80) : un timer de pve1, la sauvegarde
	// Proxmox, un ping en retard.
	_, _ = s.db.Exec(`INSERT INTO taches(cle, nom, source, machine, periode, marge, gravite, cree_le) VALUES
		('pve1|systemd|sauvegarde-restic', 'sauvegarde-restic', 'systemd', 'pve1', 0, 3600, 'warning', ?),
		('pve1|pve|vzdump', 'vzdump', 'pve', 'pve1', 86400, 7200, 'critical', ?)`, now.Add(-48*time.Hour).Unix(), now.Add(-48*time.Hour).Unix())
	_, _ = s.db.Exec(`INSERT INTO taches(cle, nom, source, machine, periode, marge, gravite, jeton, cree_le, debut, fin, code) VALUES
		('ping|demo', 'Sauvegarde NAS (Hyper Backup)', 'ping', 'nas-backup', 86400, 3600, 'warning', 'x', ?, ?, ?, 0)`,
		now.Add(-10*24*time.Hour).Unix(), now.Add(-50*time.Hour).Unix(), now.Add(-49*time.Hour).Unix())
	// Disque chaud (règle d'un modèle, seuil par machine) : grande courbe
	// avec axes et seuil sur la page de l'alerte (retour du 19/11).
	_, _ = s.db.Exec(`INSERT INTO alertes(regle, etiquettes, gravite, etat, debut, active_le, resume, valeur_max)
		VALUES ('PhysiqueDisqueChaud', '{"alertname":"PhysiqueDisqueChaud","instance":"pve1","job":"integrations/unix","device":"nvme0","temperature_type":"current","severity":"warning"}', 'warning', 'active', ?, ?, 'Disque nvme0 à 60 °C sur pve1', 60)`,
		now.Add(-30*time.Minute).UnixMilli(), now.Add(-10*time.Minute).UnixMilli())
	// Alertes aux noms techniques variés : libellés lisibles (SPEC § 73).
	for i, x := range [][4]string{
		{"MiseAJourAgentEchouee", "pve1", "warning", "La mise à jour de l'agent a échoué sur pve1"},
		{"SondeEnEchec", "nas-backup", "critical", "Sonde nas-backup ping en échec"},
		{"OpnsenseCpuEleve", "vm-docker", "warning", "CPU à 92 % sur le pare-feu"},
		{"AgentMuet", "srv-test", "critical", "srv-test ne remonte plus de métriques"},
	} {
		_, _ = s.db.Exec(`INSERT INTO alertes(regle, etiquettes, gravite, etat, debut, active_le, resume, valeur_max)
			VALUES (?, ?, ?, 'active', ?, ?, ?, 1)`, x[0], `{"alertname":"`+x[0]+`","instance":"`+x[1]+`","severity":"`+x[2]+`"}`, x[2],
			now.Add(-time.Duration(20+i*15)*time.Minute).UnixMilli(), now.Add(-time.Duration(18+i*15)*time.Minute).UnixMilli(), x[3])
	}
	// Collecteur du LAN, certificat du syslog en cours (avancement, SPEC § 32) ;
	// compte DNS fictif (jamais utilisé : la démarche est simulée).
	_ = certsyslog.DefinirReglages(ctx, s.db, s.cle, certsyslog.Saisie{Fournisseur: "desec", Domaine: "exemple.org",
		Identifiants: map[string]string{"jeton": "jeton-de-demo"}}, "demo", now)
	_, _ = s.db.Exec(`INSERT INTO machines(nom, statut, type, site, cree_le, vu_le, version_agent) VALUES ('collecteur-lan', 'approuvee', 'collecteur', 'maison', ?, ?, '0.3.0'),
		('collecteur-bureau', 'approuvee', 'collecteur', 'bureau', ?, ?, '0.3.0')`, now.Unix()-3600, now.Unix(), now.Unix()-3600, now.Unix())
	// Machine sans site (SPEC § 60) : on choisit le site du ping.
	_, _ = s.db.Exec(`INSERT INTO machines(nom, statut, cree_le, vu_le, version_agent, architecture) VALUES ('portable', 'approuvee', ?, ?, '0.5.0', 'amd64')`,
		now.Unix()-3600, now.Unix())
	_, _ = s.db.Exec(`INSERT INTO reglages(cle, valeur, modifie_le, modifie_par) SELECT 'config_agent:machine:' || id,
		'{"collecteur":{"syslog":{"ecoute":"192.168.1.10:6515","nom_certificat":"syslog.lan.exemple.org"}}}', ?, 'demo' FROM machines WHERE nom = 'collecteur-lan'`, now.Unix())
	_, _ = s.db.Exec(`INSERT INTO certificats_syslog(machine_id, csr, csr_recue_le, etape, etape_le, etape_jusqua)
		SELECT id, 'demande', ?, 'propagation', ?, ? FROM machines WHERE nom = 'collecteur-lan'`, now.Add(-2*time.Minute).Unix(), now.Add(-30*time.Second).Unix(), now.Add(30*time.Second).Unix())
	// Coupure passée du site « maison » (SPEC § 39) : page Alertes › Coupures.
	for i, m := range []string{"srv-test", "pve1"} {
		_, _ = s.db.Exec(`INSERT INTO alertes(regle, etiquettes, gravite, etat, debut, active_le, fin, resume, valeur_max)
			VALUES ('AgentMuet', ?, 'critical', 'resolue', ?, ?, ?, ?, 1)`, `{"alertname":"AgentMuet","instance":"`+m+`","severity":"critical"}`,
			now.Add(-5*time.Hour).UnixMilli(), now.Add(-5*time.Hour+time.Duration(i)*20*time.Second).UnixMilli(),
			now.Add(-4*time.Hour-30*time.Minute+time.Duration(i)*15*time.Second).UnixMilli(), m+" ne remonte plus de métriques")
	}
	// Alerte passée et changement sur pve1 : bandes et traits des courbes (SPEC § 31).
	_, _ = s.db.Exec(`INSERT INTO alertes(regle, etiquettes, gravite, etat, debut, active_le, fin, resume, valeur_max)
		VALUES ('ChargeElevee', '{"alertname":"ChargeElevee","instance":"pve1","severity":"warning"}', 'warning', 'resolue', ?, ?, ?, 'Charge élevée sur pve1', 4)`,
		now.Add(-4*time.Hour).UnixMilli(), now.Add(-4*time.Hour).UnixMilli(), now.Add(-3*time.Hour).UnixMilli())
	_, _ = s.db.Exec(`INSERT INTO audit(le, qui, action, objet) SELECT ?, 'web:admin', 'config_agent', 'machine:' || id FROM machines WHERE nom = 'pve1'`,
		now.Add(-2*time.Hour).Unix())
	// Sondes (SPEC § 48) : une depuis le serveur (sans réseau ici : en
	// échec), une depuis le collecteur, et des résultats simulés.
	_ = flotte.EnregistrerSondes(ctx, s.db, []essais.Sonde{
		{Nom: "Nextcloud", Type: essais.HTTP, Cible: "https://cloud.exemple.invalid/status.php", Depuis: essais.ServeurLocal, Code: 200, Contenu: "installed"},
		{Nom: "Home Assistant", Type: essais.HTTP, Cible: "https://ha.lan:8123/api/", Depuis: "collecteur-lan", NonVerifie: true, Machine: "vm-docker",
			ChampJSON: "message", ValeurJSON: "API running.", Certificat: 14},
		{Nom: "DNS interne", Type: essais.DNS, Cible: "192.168.1.2", Question: "nas.lan", Depuis: "collecteur-lan"},
	}, "demo", now)
	// Maintenances (SPEC § 81) : chaque mois, et tout un site.
	_, _ = notif.AjouterMaintenance(ctx, s.db, "machine:pve1", now, now, &notif.Recurrence{Rythme: "mois", JourMois: 1, Heure: "03:00",
		Duree: alertes.Duree(2 * time.Hour), Fuseau: "Europe/Paris"}, "admin", "mises à jour Proxmox", now)
	_, _ = notif.AjouterMaintenance(ctx, s.db, "site:maison", now.Add(48*time.Hour), now.Add(52*time.Hour), nil, "admin", "coupure de courant prévue (EDF)", now)
	var sondesDemo []stockage.Serie
	for k := 0; k < 120; k++ {
		ts := now.Add(time.Duration(-k) * 30 * time.Second).UnixMilli()
		for _, x := range []struct {
			nom   string
			duree float64
		}{{"Home Assistant", 0.035 + float64(k%7)/1000}, {"DNS interne", 0.002 + float64(k%3)/1000}} {
			for nom, v := range map[string]float64{"mirador_sonde_succes": 1, "mirador_sonde_duree_secondes": x.duree, "mirador_sonde_lente": 0, "mirador_sonde_code_http": 200} {
				sondesDemo = append(sondesDemo, stockage.Serie{Labels: []stockage.Label{{Nom: "__name__", Valeur: nom}, {Nom: "cible", Valeur: "-"},
					{Nom: "instance", Valeur: "collecteur-lan"}, {Nom: "job", Valeur: "sondes"}, {Nom: "sonde", Valeur: x.nom}},
					Points: []stockage.Point{{T: ts, V: v}}})
			}
		}
	}
	_ = s.met.Ecrire(ctx, sondesDemo)
	// Adresses et ports ouverts trouvés par l'agent (SPEC § 60).
	var ecoutes []stockage.Serie
	ajout := func(inst, nom string, etq ...string) {
		sr := stockage.Serie{Labels: []stockage.Label{{Nom: "__name__", Valeur: nom}, {Nom: "instance", Valeur: inst}, {Nom: "job", Valeur: "integrations/unix"}}}
		for j := 0; j+1 < len(etq); j += 2 {
			sr.Remplacer(etq[j], etq[j+1])
		}
		for k := -10; k < 60; k++ {
			sr.Points = append(sr.Points, stockage.Point{T: now.Add(time.Duration(k) * 30 * time.Second).UnixMilli(), V: 1})
		}
		ecoutes = append(ecoutes, sr)
	}
	for _, inst := range []string{"vm-docker", "portable"} {
		ip := map[string]string{"vm-docker": "192.168.1.20", "portable": "192.168.50.7"}[inst]
		ajout(inst, "mirador_adresse_info", "interface", "vmbr0", "adresse", ip, "principale", "1")
		ajout(inst, "mirador_adresse_info", "interface", "vmbr1", "adresse", "10.10.0.20", "principale", "0")
		ajout(inst, "mirador_adresse_info", "interface", "vmbr0", "adresse", "fd00::20", "principale", "1")
		for _, p := range [][2]string{{"22", "ssh"}, {"80", "http"}, {"443", "https"}, {"8123", "http"}, {"9443", "https"}, {"5432", "tcp"}, {"111", "tcp"}} {
			ajout(inst, "mirador_port_ecoute", "port", p[0], "adresse", "*", "protocole", p[1])
		}
	}
	_ = s.met.Ecrire(ctx, ecoutes)
	// Services (SPEC § 50).
	_ = servicesmetier.Enregistrer(ctx, s.db, []servicesmetier.Service{
		{Nom: "Domotique", Composants: []servicesmetier.Composant{{Sorte: "sonde", Nom: "Home Assistant", Cle: true},
			{Sorte: "machine", Nom: "vm-docker", Cle: true}, {Sorte: "sonde", Nom: "DNS interne"}}},
		{Nom: "Sauvegardes", Composants: []servicesmetier.Composant{{Sorte: "machine", Nom: "nas-backup", Cle: true}, {Sorte: "machine", Nom: "pve1"}}},
		{Nom: "Nextcloud", Composants: []servicesmetier.Composant{{Sorte: "sonde", Nom: "Nextcloud", Cle: true}, {Sorte: "machine", Nom: "vps-web"}}},
	}, "demo", "", now)
	// Changements repérés sur pve1 (SPEC § 47) : bloc de la fiche et traits des courbes.
	for _, c := range []struct {
		il    time.Duration
		typ   string
		texte string
	}{
		{5 * time.Hour, "paquets", "openssl 3.0.15-1 → 3.0.15-2, linux-image-amd64 6.8.12-4 → 6.8.12-5, libc6 2.36-9 → 2.36-9+deb12u9"},
		{4*time.Hour + 50*time.Minute, "redemarrage", "Machine redémarrée, noyau 6.8.12-4-pve → 6.8.12-5-pve"},
		{90 * time.Minute, "conteneur", "homeassistant recréé : image home-assistant:2026.11.1 → home-assistant:2026.11.2"},
		{30 * time.Minute, "agent", "mirador-agent 0.2.31 → 0.2.32"},
	} {
		_, _ = s.db.Exec(`INSERT INTO changements(machine, le, type, detail) VALUES ('pve1', ?, ?, ?)`, now.Add(-c.il).Unix(), c.typ, c.texte)
	}
	var lignes []stockage.Ligne
	for k := 0; k < 60; k++ {
		lignes = append(lignes, stockage.Ligne{T: now.Add(time.Duration(-k) * time.Minute).UnixMilli(), Msg: fmt.Sprintf("Accepted publickey for admin from 192.0.2.%d port 5%03d ssh2", k%7, k),
			Champs: map[string]string{"host": machines[k%5], "job": "journal", "unit": "ssh.service", "level": "info"}})
	}
	// Page Logs (SPEC § 33) : avertissements et erreurs répartis sur 6 h.
	for k := 0; k < 40; k++ {
		ts := now.Add(-time.Duration(k*9+3) * time.Minute).UnixMilli()
		switch {
		case k%5 == 0:
			lignes = append(lignes, stockage.Ligne{T: ts, Msg: fmt.Sprintf("request to backend failed, retrying in 2 s (attempt %d/5)", k%4+1),
				Champs: map[string]string{"host": "vm-docker", "job": "docker", "container": "appli", "level": "warning"}})
		case k%7 == 0:
			lignes = append(lignes, stockage.Ligne{T: ts, Msg: fmt.Sprintf("error: PAM: Authentication failure for illegal user admin from 198.51.100.%d", k),
				Champs: map[string]string{"host": "srv-test", "job": "journal", "unit": "ssh.service", "level": "err"}})
		case k%3 == 0:
			lignes = append(lignes, stockage.Ligne{T: ts, Msg: "authentication failed for user root@pam from 192.0.2.44",
				Champs: map[string]string{"host": "pve1", "job": "journal", "unit": "pvedaemon", "level": "warning"}})
		default:
			lignes = append(lignes, stockage.Ligne{T: ts, Msg: fmt.Sprintf("Started Session %d of User root.", 100+k),
				Champs: map[string]string{"host": machines[k%5], "job": "journal", "unit": "systemd-logind.service", "level": "info"}})
		}
	}
	lignes = append(lignes, stockage.Ligne{T: now.UnixMilli(), Msg: "nginx: [emerg] bind() to 0.0.0.0:443 failed (98: Address already in use)",
		Champs: map[string]string{"host": "vps-web", "job": "journal", "unit": "nginx.service", "level": "err"}})
	_ = s.jx.Ecrire(ctx, lignes)
	// Modèles de surveillance (SPEC § 24) : Linux pour le groupe « lan »
	// (disque à 20 %), Machine physique sur pve1 (capteurs à 70 °C) ; Docker,
	// ZFS et Certificats seront proposés par la détection.
	_, _ = s.db.Exec(`INSERT INTO reglages(cle, valeur, modifie_le, modifie_par) VALUES ('machines.listes',
		'{"sites":["maison"],"types":["vm"],"groupes":["lan"]}', ?, 'demo') ON CONFLICT(cle) DO NOTHING`, now.Unix())
	_ = modeles.Definir(ctx, s.db, modeles.PorteeGroupe("lan"), "linux", modeles.EtatActif, map[string]float64{"disque_libre_pct": 20}, nil, "demo", now)
	var idPve int64
	_ = s.db.QueryRow(`SELECT id FROM machines WHERE nom = 'pve1'`).Scan(&idPve)
	_ = modeles.Definir(ctx, s.db, modeles.PorteeMachine(idPve), "physique", modeles.EtatActif, map[string]float64{"temp_max": 70}, nil, "demo", now)
	_ = modeles.DefinirServices(ctx, s.db, "linux", []string{"cron.service", "ssh.service"}, "demo", now)
	_ = auth.CreerUtilisateur(ctx, s.db, "admin", auth.RoleAdmin, "une longue phrase de passe", "demo", "", now)
	secret, _ := auth.NouveauSecretTOTP()
	ch, _ := secrets.Chiffrer(s.cle, "console.totp", secret)
	_ = auth.DefinirTOTP(ctx, s.db, "admin", ch, 0, "demo", "", now)
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
