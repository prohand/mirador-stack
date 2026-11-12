// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/prometheus/prometheus/model/labels"

	"mirador-stack/internal/agent/distant"
	"mirador-stack/internal/server/alertes"
	"mirador-stack/internal/server/flotte"
)

// ech : un échantillon « nom{étiquettes} valeur ».
func ech(nom string, v float64, etq ...string) alertes.Echantillon {
	return alertes.Echantillon{Etiquettes: labels.FromStrings(append([]string{"__name__", nom}, etq...)...), Valeur: v}
}

func relevePhysique() []alertes.Echantillon {
	return []alertes.Echantillon{
		ech("mirador_agent_info", 1, "version", "0.3.0"),
		ech("node_hwmon_temp_celsius", 54, "chip", "coretemp", "sensor", "temp1"),
		ech("node_hwmon_temp_celsius", 43, "chip", "nct", "sensor", "temp2"),
		ech("smartctl_device_smart_status", 1, "device", "sda"),
		ech("smartctl_device_smart_status", 0, "device", "sdb"),
		ech("node_firewall_input_policy_drop", 1),
		ech("node_firewall_input_rules", 14),
		ech("node_systemd_enabled_units_total", 2),
		ech("node_systemd_enabled_units_down", 1),
		ech("node_systemd_unit_enabled_active", 1, "name", "ssh.service"),
		ech("node_systemd_unit_enabled_active", 0, "name", "nginx.service"),
		ech("mirador_agent_detecte", 3, "genre", "ventilateurs", "nom", "capteurs"),
		ech("mirador_agent_detecte", 1, "genre", "zfs", "nom", "rpool"),
		ech("mirador_agent_detecte", 1, "genre", "service", "nom", "postfix.service"),
	}
}

func etats(l []ligneSurv) string {
	var out []string
	for _, x := range l {
		out = append(out, x.Cle)
	}
	return strings.Join(out, ",")
}

func TestClasserSurveillance(t *testing.T) {
	vrai, faux := true, false
	// Rien de choisi : ce qui est collecté est « surveillé », les nouveautés
	// détectées sont « à valider », le reste est absent.
	v := classerSurveillance(&distant.ConfigDistante{}, relevePhysique(), true)
	if got := etats(v.AValider); got != "ventilateurs,zfs" {
		t.Errorf("à valider : %s", got)
	}
	if got := etats(v.Surveilles); got != "metriques,temperatures,envoyer_smart,envoyer_pare_feu,systemd" {
		t.Errorf("surveillés : %s", got)
	}
	if len(v.Ignores) != 0 || !strings.Contains(strings.Join(v.Absents, ","), "RAID logiciel") || !strings.Contains(strings.Join(v.Absents, ","), "Conteneurs Docker") {
		t.Errorf("ignorés / absents : %v %v", v.Ignores, v.Absents)
	}
	for _, l := range v.Surveilles {
		switch l.Cle {
		case "envoyer_smart":
			if !l.Alerte || l.Detail != "2 disques, 1 en échec" {
				t.Errorf("SMART : %+v", l)
			}
		case "temperatures":
			if l.Detail != "2 capteurs, 54 °C au plus" {
				t.Errorf("températures : %+v", l)
			}
		case "systemd":
			if !l.Alerte {
				t.Errorf("un service arrêté doit se voir : %+v", l)
			}
		}
	}
	// Sans validation des services : tous ceux que l'agent suit, aucun proposé
	// sauf ceux qu'il signale ; NbAValider les compte.
	if len(v.SvcSurveilles) != 2 || !v.SvcSurveilles[0].Arrete || len(v.SvcAValider) != 1 || v.NbAValider != 3 {
		t.Errorf("services : %+v / %+v / %d", v.SvcSurveilles, v.SvcAValider, v.NbAValider)
	}

	// Choix faits : ZFS validé, ventilateurs refusés, températures retirées,
	// services validés un par un.
	cfg := &distant.ConfigDistante{ZFS: &vrai, Ventilateurs: &faux, Temperatures: &faux, SystemdValidation: &vrai,
		SystemdSurveiller: &[]string{"ssh.service"}, SystemdIgnorer: &[]string{"fwupd-refresh.service"}}
	releve := append(relevePhysique(), ech("node_zfs_zpool_state", 0, "zpool", "rpool", "state", "online"),
		ech("node_zfs_zpool_state", 1, "zpool", "rpool", "state", "degraded"))
	v = classerSurveillance(cfg, releve, true)
	if len(v.AValider) != 0 || etats(v.Ignores) != "temperatures,ventilateurs" {
		t.Errorf("après choix : à valider %s, ignorés %s", etats(v.AValider), etats(v.Ignores))
	}
	trouve := false
	for _, l := range v.Surveilles {
		if l.Cle == "zfs" {
			trouve = l.Alerte && l.Detail == "rpool (DEGRADED)"
		}
	}
	if !trouve {
		t.Errorf("pool dégradé non signalé : %+v", v.Surveilles)
	}
	if !v.Validation || len(v.SvcSurveilles) != 1 || v.SvcSurveilles[0].Nom != "ssh.service" || len(v.SvcIgnores) != 1 {
		t.Errorf("services validés : %+v %+v", v.SvcSurveilles, v.SvcIgnores)
	}

	// Agent trop ancien : les réglages nouveaux sont bloqués, pas les anciens.
	v = classerSurveillance(&distant.ConfigDistante{}, relevePhysique(), false)
	for _, l := range append(v.AValider, v.Surveilles...) {
		if l.Bloque != flotte.CleSurveillance(l.Cle) {
			t.Errorf("%s : bloqué = %v", l.Cle, l.Bloque)
		}
	}
}

// Parcours complet dans l'interface : page, choix, code 2FA, configuration
// de l'agent, audit, et refus pour un agent trop ancien.
func TestSurveillanceDansLInterface(t *testing.T) {
	b := nouveauBanc(t)
	c, secret := b.premierAdmin("admin", "une longue phrase de passe")
	ctx := context.Background()
	if _, err := b.db.Exec(`INSERT INTO machines(id, nom, statut, cree_le, vu_le, groupe, capacites) VALUES
		(1, 'pve1', 'approuvee', 1, ?, 'proxmox', 'surveillance'), (2, 'vieille', 'approuvee', 1, ?, '', '')`,
		b.h.maintenant().Unix(), b.h.maintenant().Unix()); err != nil {
		t.Fatal(err)
	}
	code, corps, _ := b.get(c, "/machines/pve1")
	if code != http.StatusOK || !strings.Contains(corps, `id="surveillance"`) || !strings.Contains(corps, "Enregistrer les choix") {
		t.Fatalf("bloc de surveillance absent : %d", code)
	}
	csrf := trouver(t, reCSRF, corps)
	poster := func(machine string, v url.Values) (int, string) {
		v.Set("csrf", csrf)
		v.Set("action", "surveillance")
		code, _, _ := b.post(c, "/machines/"+machine, v)
		_, page, _ := b.get(c, "/machines/"+machine)
		return code, strings.ToLower(page)
	}

	// Sans code 2FA : refusé, rien n'est écrit.
	if _, page := poster("pve1", url.Values{"g.zfs": {"oui"}, "code": {"000000"}}); !strings.Contains(page, "code 2fa faux") {
		t.Error("choix accepté sans code 2FA valable")
	}
	if cfg, _ := flotte.ConfigEffective(ctx, b.db, 1); cfg.ZFS != nil {
		t.Fatal("configuration modifiée malgré le refus")
	}

	// Choix sur la machine : ZFS validé, températures retirées, un service ignoré.
	poster("pve1", url.Values{"g.zfs": {"oui"}, "g.temperatures": {"non"}, "s.fwupd-refresh.service": {"ignorer"}, "code": {b.code(secret)}})
	cfg, err := flotte.ConfigEffective(ctx, b.db, 1)
	if err != nil || cfg.ZFS == nil || !*cfg.ZFS || cfg.Temperatures == nil || *cfg.Temperatures ||
		cfg.SystemdIgnorer == nil || len(*cfg.SystemdIgnorer) != 1 || cfg.SystemdValidation != nil {
		t.Fatalf("après les choix : %+v %v", cfg, err)
	}
	// Validation des services, puis un service validé et l'ignoré rétabli.
	poster("pve1", url.Values{"validation": {"oui"}, "code": {b.code(secret)}})
	poster("pve1", url.Values{"s.nginx.service": {"surveiller"}, "s.fwupd-refresh.service": {"retablir"}, "code": {b.code(secret)}})
	cfg, _ = flotte.ConfigEffective(ctx, b.db, 1)
	if cfg.SystemdValidation == nil || !*cfg.SystemdValidation || cfg.SystemdSurveiller == nil ||
		strings.Join(*cfg.SystemdSurveiller, ",") != "fwupd-refresh.service,nginx.service" || cfg.SystemdIgnorer != nil {
		t.Fatalf("services : %+v", cfg)
	}
	// Service supprimé de la machine : ignoré puis « oublié », il quitte la
	// configuration (plus dans « Ignoré ou absent »).
	poster("pve1", url.Values{"s.mirador-test.service": {"ignorer"}, "code": {b.code(secret)}})
	poster("pve1", url.Values{"s.mirador-test.service": {"oublier"}, "code": {b.code(secret)}})
	cfg, _ = flotte.ConfigEffective(ctx, b.db, 1)
	if (cfg.SystemdIgnorer != nil && slices.Contains(*cfg.SystemdIgnorer, "mirador-test.service")) ||
		slices.Contains(*cfg.SystemdSurveiller, "mirador-test.service") {
		t.Fatalf("service oublié encore présent : %+v", cfg)
	}
	// Rétablir les températures (valeur par défaut) ; choix pour tout le groupe.
	poster("pve1", url.Values{"g.temperatures": {"defaut"}, "g.mdraid": {"oui"}, "portee": {"groupe"}, "code": {b.code(secret)}})
	cfg, _ = flotte.ConfigEffective(ctx, b.db, 1)
	var groupe string
	_ = b.db.QueryRow(`SELECT valeur FROM reglages WHERE cle = 'config_agent:groupe:proxmox'`).Scan(&groupe)
	if cfg.Temperatures != nil || cfg.Mdraid == nil || !*cfg.Mdraid || groupe != `{"mdraid":true}` {
		t.Fatalf("portée groupe : %+v / %s", cfg, groupe)
	}

	// Entrées refusées : nom de service fantaisiste, choix inconnu, rien à faire.
	for nom, v := range map[string]url.Values{
		"service invalide": {"s.rm -rf /.service": {"surveiller"}},
		"choix inconnu":    {"g.zfs": {"peut-etre"}},
		"aucun choix":      {},
	} {
		v.Set("code", b.code(secret))
		if _, page := poster("pve1", v); !strings.Contains(page, `class="message erreur"`) {
			t.Errorf("%s : accepté", nom)
		}
	}
	// Agent trop ancien : un réglage nouveau est refusé, un ancien passe.
	if _, page := poster("vieille", url.Values{"g.zfs": {"oui"}, "code": {b.code(secret)}}); !strings.Contains(page, "agent plus récent") {
		t.Error("réglage nouveau accepté pour un agent trop ancien")
	}
	poster("vieille", url.Values{"g.mises_a_jour": {"non"}, "code": {b.code(secret)}})
	if cfg, _ := flotte.ConfigEffective(ctx, b.db, 2); cfg.ZFS != nil || cfg.MisesAJour == nil || *cfg.MisesAJour {
		t.Errorf("agent ancien : %+v", cfg)
	}

	// Éléments un par un (SPEC § 21) : refusés pour un agent sans la capacité.
	if _, page := poster("pve1", url.Values{"e.disque.sdb": {"retirer"}, "code": {b.code(secret)}}); !strings.Contains(page, "agent plus récent") {
		t.Error("élément accepté pour un agent sans la capacité « elements »")
	}
	if _, err := b.db.Exec(`UPDATE machines SET capacites = 'surveillance,elements' WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	poster("pve1", url.Values{"e.disque.sdb": {"retirer"}, "e.montage./mnt/sauvegarde": {"retirer"},
		"c./etc/ssl/local/vieux.pem": {"retirer"}, "certificats_nouveaux": {"/etc/nginx/ssl/*.crt\n\n/opt/appli/tls.pem"},
		"code": {b.code(secret)}})
	cfg, _ = flotte.ConfigEffective(ctx, b.db, 1)
	if cfg.IgnorerDisques == nil || (*cfg.IgnorerDisques)[0] != "sdb" || cfg.IgnorerMontages == nil ||
		cfg.CertificatsIgnorer == nil || cfg.CertificatsAjout == nil || len(*cfg.CertificatsAjout) != 2 {
		t.Fatalf("éléments : %+v", cfg)
	}
	poster("pve1", url.Values{"e.disque.sdb": {"retablir"}, "cm./opt/appli/tls.pem": {"retirer"}, "code": {b.code(secret)}})
	cfg, _ = flotte.ConfigEffective(ctx, b.db, 1)
	if cfg.IgnorerDisques != nil || cfg.CertificatsAjout == nil || strings.Join(*cfg.CertificatsAjout, ",") != "/etc/nginx/ssl/*.crt" ||
		cfg.IgnorerMontages == nil {
		t.Fatalf("après rétablissement : %+v", cfg)
	}
	// Chemin de certificat dangereux : refusé avec un message clair.
	if _, page := poster("pve1", url.Values{"certificats_nouveaux": {"/etc/shadow"}, "code": {b.code(secret)}}); !strings.Contains(page, "chemin de certificat refusé") {
		t.Error("/etc/shadow accepté")
	}
	if _, page := poster("pve1", url.Values{"e.disque.sdb": {"supprimer"}, "code": {b.code(secret)}}); !strings.Contains(page, `class="message erreur"`) {
		t.Error("choix d'élément inconnu accepté")
	}

	// La page « Configuration des agents » garde les choix de surveillance.
	_, page, _ := b.get(c, "/reglages/agents?portee=machine:1")
	b.post(c, "/reglages/agents", url.Values{"csrf": {trouver(t, reCSRF, page)}, "portee": {"machine:1"}, "intervalle": {"60s"}, "code": {b.code(secret)}})
	if cfg, _ := flotte.ConfigEffective(ctx, b.db, 1); cfg.ZFS == nil || cfg.Intervalle == nil || cfg.SystemdValidation == nil ||
		cfg.CertificatsAjout == nil || cfg.IgnorerMontages == nil {
		t.Errorf("choix perdus après « Configuration des agents » : %+v", cfg)
	}
	// Chaque changement est dans le journal d'audit.
	var n int
	_ = b.db.QueryRow(`SELECT count(*) FROM audit WHERE action = 'config_agent'`).Scan(&n)
	if n < 6 {
		t.Errorf("lignes d'audit : %d", n)
	}
	// Un compte en lecture seule ne peut pas enregistrer (pas de formulaire).
	if strings.Count(corps, `name="code"`) == 0 {
		t.Error("le formulaire admin doit demander le code 2FA")
	}
}

// Détail par élément (SPEC § 21) : disques, interfaces, montages, certificats.
func TestClasserElements(t *testing.T) {
	r := append(relevePhysique(),
		ech("node_disk_io_time_seconds_total", 1, "device", "sda"),
		ech("node_network_receive_bytes_total", 1, "device", "eth0"),
		ech("node_filesystem_size_bytes", 1<<30, "mountpoint", "/"),
		ech("ssl_cert_not_after_timestamp_seconds", 1.8e9, "path", "/etc/ssl/local/a.pem", "cn", "a.lan", "issuer", "CA"))
	ignores := []string{"sdb"}
	certs := []string{"/etc/ssl/local/vieux.pem"}
	ajout := []string{"/etc/nginx/ssl/*.crt"}
	v := classerSurveillance(&distant.ConfigDistante{IgnorerDisques: &ignores, CertificatsIgnorer: &certs, CertificatsAjout: &ajout}, r, true)
	if len(v.Composants) != 4 || v.NbRetires != 2 || len(v.Certs) != 2 || len(v.MotifsAjout) != 1 || !v.SystemeSuivi || !v.CertsSuivis {
		t.Fatalf("détail : %+v", v)
	}
	if c := v.Composants[1]; c.Genre != "disque" || c.Nom != "sdb" || !c.Retire {
		t.Errorf("disque retiré : %+v", c)
	}
	for _, l := range v.Surveilles {
		if l.Cle == "metriques" && l.Detail != "Processeur, mémoire, 1 disque, 1 interface, 1 montage" {
			t.Errorf("détail Système : %q", l.Detail)
		}
	}
}

// Coût du classement pour une machine (le reste est une requête en cache).
func BenchmarkClasserSurveillance(b *testing.B) {
	r := relevePhysique()
	cfg := &distant.ConfigDistante{}
	b.ReportAllocs()
	for b.Loop() {
		classerSurveillance(cfg, r, true)
	}
}

// Service ignoré mais encore signalé par l'agent (il n'a pas encore relu sa
// configuration) : plus « à valider », ni dans la fiche ni dans le compteur.
func TestServiceIgnoreAussitot(t *testing.T) {
	vrai := true
	ign := []string{"postfix.service"}
	cfg := &distant.ConfigDistante{SystemdValidation: &vrai, SystemdSurveiller: &[]string{}, SystemdIgnorer: &ign}
	v := classerSurveillance(cfg, relevePhysique(), true)
	for _, s := range v.SvcAValider {
		if s.Nom == "postfix.service" {
			t.Fatal("service ignoré encore « à valider »")
		}
	}
	if len(v.SvcIgnores) != 1 || v.SvcIgnores[0].Nom != "postfix.service" {
		t.Errorf("ignorés : %+v", v.SvcIgnores)
	}
	if !serviceChoisi(cfg, "postfix.service") || serviceChoisi(cfg, "autre.service") || serviceChoisi(nil, "x") {
		t.Error("serviceChoisi")
	}
}

// Chemins de certificats ajoutés : compte rendu de l'assistant root, et
// assistant plus ancien que l'agent signalé (12/11).
func TestCompteRenduChemins(t *testing.T) {
	ajout := []string{"/etc/app/*.pem", "/etc/vide/*.crt", "/etc/cles/*.pem", "/opt/neuf/*.pem"}
	cfg := &distant.ConfigDistante{CertificatsAjout: &ajout}
	echs := []alertes.Echantillon{
		ech("mirador_agent_info", 1, "version", "0.2.14"),
		ech("mirador_agent_root_info", 1, "version", "0.2.14"),
		ech("mirador_agent_certificats_chemin_fichiers", 3, "chemin", "/etc/app/*.pem"),
		ech("mirador_agent_certificats_chemin_lus", 2, "chemin", "/etc/app/*.pem"),
		ech("mirador_agent_certificats_chemin_fichiers", 0, "chemin", "/etc/vide/*.crt"),
		ech("mirador_agent_certificats_chemin_lus", 0, "chemin", "/etc/vide/*.crt"),
		ech("mirador_agent_certificats_chemin_fichiers", 1, "chemin", "/etc/cles/*.pem"),
		ech("mirador_agent_certificats_chemin_lus", 0, "chemin", "/etc/cles/*.pem"),
		ech("mirador_agent_certificats_chemin_ecartes", 1, "chemin", "/etc/cles/*.pem", "raison", "hors_dossier"),
	}
	v := classerSurveillance(cfg, echs, true)
	if v.RacineAncienne {
		t.Error("assistant à jour signalé ancien")
	}
	attendu := map[string]string{
		"/etc/app/*.pem":  "2 certificats lu(s), 1 fichier(s) écarté(s)",
		"/etc/vide/*.crt": "aucun fichier trouvé",
		"/etc/cles/*.pem": "aucun lu : lien vers un dossier non autorisé (1)",
		"/opt/neuf/*.pem": "pas encore lu",
	}
	for _, m := range v.MotifsAjout {
		if !strings.Contains(m.Resultat, attendu[m.Motif]) {
			t.Errorf("%s : %q", m.Motif, m.Resultat)
		}
	}
	// Assistant resté en 0.2.9 : signalé, et un chemin jamais lu le dit.
	echs[1] = ech("mirador_agent_root_info", 1, "version", "0.2.9")
	v = classerSurveillance(cfg, echs, true)
	if !v.RacineAncienne || v.VersionRacine != "0.2.9" {
		t.Errorf("assistant ancien non signalé : %+v", v)
	}
	for _, m := range v.MotifsAjout {
		if m.Motif == "/opt/neuf/*.pem" && !strings.Contains(m.Resultat, "trop ancien") {
			t.Errorf("chemin non lu : %q", m.Resultat)
		}
	}
}

// Élément retiré mais encore envoyé par l'agent (il n'a pas encore appliqué
// le retrait) : une seule ligne, « Retiré » (bug du 12/11 : deux lignes).
func TestElementRetireUneSeuleLigne(t *testing.T) {
	certs, disques := []string{"/etc/ssl/certs/ca.pem"}, []string{"sdb"}
	cfg := &distant.ConfigDistante{CertificatsIgnorer: &certs, IgnorerDisques: &disques}
	echs := []alertes.Echantillon{
		ech("mirador_agent_info", 1, "version", "0.2.15"),
		ech("ssl_cert_not_after_timestamp_seconds", 2147483647, "path", "/etc/ssl/certs/ca.pem", "cn", "GlobalSign", "issuer", "GlobalSign"),
		ech("node_disk_io_time_seconds_total", 1, "device", "sdb"),
		ech("node_disk_io_time_seconds_total", 1, "device", "sda"),
	}
	v := classerSurveillance(cfg, echs, true)
	n := 0
	for _, c := range v.Certs {
		if c.Chemin == "/etc/ssl/certs/ca.pem" {
			n++
			if !c.Retire || c.CN != "GlobalSign" {
				t.Errorf("certificat : %+v", c)
			}
		}
	}
	if n != 1 {
		t.Errorf("certificat retiré affiché %d fois", n)
	}
	n = 0
	for _, c := range v.Composants {
		if c.Nom == "sdb" {
			n++
			if !c.Retire {
				t.Errorf("disque : %+v", c)
			}
		}
	}
	if n != 1 {
		t.Errorf("disque retiré affiché %d fois", n)
	}
}
