// SPDX-License-Identifier: GPL-3.0-or-later

package collect

import (
	"path/filepath"
	"testing"
	"time"

	"mirador-stack/internal/agent/racine"
)

func TestRacine(t *testing.T) {
	chemin := filepath.Join(t.TempDir(), "root.json")
	vrai, temp, heures, reserve := true, 38.0, 41000.0, 97.0
	t0 := time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)
	fin := time.Date(2027, 1, 15, 12, 0, 0, 0, time.UTC)
	res := &racine.Resultat{
		Version:    "test",
		Horodatage: t0,
		PareFeu:    &racine.PareFeu{Outil: "iptables", Politique: "DROP", Regles: 3},
		Smart: []racine.Disque{{Nom: "sda", SanteOK: &vrai, Temperature: &temp, HeuresMarche: &heures,
			ReserveNVMe: &reserve, Attributs: map[string]int64{"Reallocated_Sector_Ct": 8}}},
		Certificats: &racine.Certificats{Trouves: 1, Erreurs: 1, Liste: []racine.Certificat{
			{Chemin: "/etc/letsencrypt/live/exemple.org/cert.pem", CN: "exemple.org", Emetteur: "R11", Debut: fin.AddDate(0, -3, 0), Fin: fin}}},
	}
	if err := racine.Ecrire(chemin, res); err != nil {
		t.Fatal(err)
	}
	r := NewRacine(chemin)
	r.maintenant = func() time.Time { return t0.Add(10 * time.Minute) }
	p := collecter(t, r)
	verifier(t, p, map[string]float64{
		"node_firewall_input_policy_drop|tool=iptables|policy=DROP":                                                      1,
		"node_firewall_input_rules|tool=iptables|policy=DROP":                                                            3,
		"smartctl_device_smart_status|device=sda":                                                                        1,
		"smartctl_device_temperature|device=sda|temperature_type=current":                                                38,
		"smartctl_device_power_on_seconds|device=sda":                                                                    41000 * 3600,
		"smartctl_device_available_spare|device=sda":                                                                     97,
		"smartctl_device_attribute|device=sda|attribute_name=Reallocated_Sector_Ct|attribute_value_type=raw":             8,
		"ssl_cert_not_after_timestamp_seconds|path=/etc/letsencrypt/live/exemple.org/cert.pem|cn=exemple.org|issuer=R11": float64(fin.Unix()),
		"ssl_certs_found_total":          1,
		"ssl_certs_read_errors_total":    1,
		"mirador_agent_root_age_seconds": 600,
	})

	// Choix de l'interface : résultats de l'assistant non envoyés (SPEC § 19).
	sans := NewRacine(chemin)
	sans.maintenant = r.maintenant
	sans.SansPareFeu, sans.SansSmart, sans.SansCertificats = true, true, true
	if p := collecter(t, sans); len(p) != 4 || p["mirador_agent_root_info|version=test"] != 1 {
		t.Errorf("tout retiré : seuls l'âge, les erreurs et la version de l'assistant attendus, obtenu %v", p)
	}
	// Un certificat retiré dans l'interface (SPEC § 21) : plus envoyé, et
	// plus compté dans ssl_certs_found_total.
	ig := NewRacine(chemin)
	ig.maintenant = r.maintenant
	ig.IgnorerCertificats([]string{"/etc/letsencrypt/live/exemple.org/cert.pem"})
	p2 := collecter(t, ig)
	for k := range p2 {
		if contient(k, "ssl_cert_not_after") {
			t.Errorf("certificat retiré envoyé : %s", k)
		}
	}
	if v, ok := p2["ssl_certs_found_total"]; !ok || v != 0 {
		t.Errorf("ssl_certs_found_total = %v, attendu 0", v)
	}
	sans.SansSmart = false
	if p := collecter(t, sans); p["smartctl_device_smart_status|device=sda"] != 1 || len(p) != 9 {
		t.Errorf("SMART seul attendu, obtenu %v", p)
	}

	// Nœud Proxmox (SPEC § 23.7) : invités nommés par l'assistant ; un
	// assistant au format 2 n'est « à jour » que hors Proxmox.
	res.Format = 2
	res.Proxmox = &racine.Proxmox{Noeud: "pve1", Invites: []racine.Invite{
		{ID: "100", Type: "qemu", Nom: "web-01", Memoire: 8192, Coeurs: 8}, {ID: "101", Type: "lxc"}}}
	if err := racine.Ecrire(chemin, res); err != nil {
		t.Fatal(err)
	}
	pve := NewRacine(chemin)
	pve.maintenant = r.maintenant
	pve.Proxmox = true
	p3 := collecter(t, pve)
	verifier(t, p3, map[string]float64{
		"mirador_pve_invite_info|id=100|type=qemu|nom=web-01|noeud=pve1": 1,
		"mirador_pve_invite_info|id=101|type=lxc|nom=|noeud=pve1":        1,
		"mirador_pve_invite_memoire_max_octets|id=100|type=qemu":         8192 << 20,
		"mirador_pve_invite_coeurs|id=100|type=qemu":                     8,
		"mirador_agent_root_a_jour":                                      0,
	})
	horsPVE := NewRacine(chemin)
	horsPVE.maintenant = r.maintenant
	if p := collecter(t, horsPVE); p["mirador_agent_root_a_jour"] != 1 {
		t.Error("format 2 hors Proxmox : assistant à jour attendu")
	}

	// Assistant arrêté depuis 3 h : plus de valeurs, seulement l'âge.
	r.maintenant = func() time.Time { return t0.Add(3 * time.Hour) }
	p = collecter(t, r)
	if len(p) != 4 || p["mirador_agent_root_age_seconds"] != 3*3600 {
		t.Errorf("fichier trop vieux : seuls l'âge et les erreurs attendus, obtenu %v", p)
	}
}

func TestRacineAbsente(t *testing.T) {
	r := NewRacine(filepath.Join(t.TempDir(), "absent.json"))
	if p := collecter(t, r); len(p) != 0 {
		t.Errorf("sans assistant, aucun point attendu : %v", p)
	}
}
