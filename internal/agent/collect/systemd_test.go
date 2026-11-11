// SPDX-License-Identifier: GPL-3.0-or-later

package collect

import (
	"context"
	"errors"
	"testing"
	"time"
)

// sourceFactice simule systemd.
type sourceFactice struct {
	unites    []uniteEtat
	redem     map[string]uint32
	actives   []string
	props     map[string]proprietesService
	appelsRed int
}

func (f *sourceFactice) Unites(context.Context) ([]uniteEtat, error) { return f.unites, nil }
func (f *sourceFactice) Redemarrages(_ context.Context, u uniteEtat) (uint32, error) {
	f.appelsRed++
	n, ok := f.redem[u.Nom]
	if !ok {
		return 0, errors.New("inconnu")
	}
	return n, nil
}
func (f *sourceFactice) ServicesActivesAuDemarrage(context.Context) ([]string, error) {
	return f.actives, nil
}
func (f *sourceFactice) Proprietes(_ context.Context, nom string) (proprietesService, error) {
	p, ok := f.props[nom]
	if !ok {
		return p, errors.New("inconnu")
	}
	return p, nil
}

func sourceExemple() *sourceFactice {
	ok := func(etat string) proprietesService {
		return proprietesService{Type: "simple", ConditionOK: true, Etat: etat}
	}
	return &sourceFactice{
		unites: []uniteEtat{
			{Nom: "ssh.service", Etat: "active", Charge: "loaded"},
			{Nom: "cron.service", Etat: "failed", Charge: "loaded"},
			{Nom: "apt-daily.timer", Etat: "active", Charge: "loaded"},
			{Nom: "home.mount", Etat: "active", Charge: "loaded"},
			{Nom: "session-1.scope", Etat: "active", Charge: "loaded"}, // non suivi
			{Nom: "dev-sda.device", Etat: "active", Charge: "loaded"},  // non suivi
			{Nom: "dmesg.service", Etat: "inactive", Charge: "loaded"},
			{Nom: "fantome.service", Etat: "inactive", Charge: "not-found"}, // référencée, absente
		},
		redem: map[string]uint32{"ssh.service": 3, "cron.service": 0, "dmesg.service": 0},
		actives: []string{
			"ssh.service", "cron.service", "getty@.service", "zfs-import-cache.service",
			"corosync.service", "netavark.service", "dmesg.service", "nginx.service",
			"monappli.service", "inconnu.service",
		},
		props: map[string]proprietesService{
			"ssh.service":              ok("active"),
			"cron.service":             ok("failed"),
			"zfs-import-cache.service": {Type: "oneshot", ConditionOK: true, Etat: "inactive"},
			"corosync.service":         {Type: "simple", ConditionOK: false, Etat: "inactive"},
			"netavark.service":         {Type: "simple", ConditionOK: true, DeclencheePar: 1, Etat: "inactive"},
			// Tâche de démarrage : sortie code 0 après 3 secondes.
			"dmesg.service": {Type: "simple", ConditionOK: true, Etat: "inactive",
				CodeSortie: 1, StatutSortie: 0, DebutMono: 1_000_000, FinMono: 4_000_000},
			// Démon arrêté à la main après 2 heures : il devrait tourner.
			"nginx.service": {Type: "forking", ConditionOK: true, Etat: "inactive",
				CodeSortie: 1, StatutSortie: 0, DebutMono: 1_000_000, FinMono: 7_201_000_000},
			"monappli.service": ok("inactive"),
		},
	}
}

func TestSystemd(t *testing.T) {
	src := sourceExemple()
	s := newSystemdAvecSource(src, []string{"monappli.service"})
	p := collecter(t, s)

	verifier(t, p, map[string]float64{
		"node_systemd_unit_state|name=ssh.service|state=failed":     0,
		"node_systemd_unit_state|name=cron.service|state=failed":    1,
		"node_systemd_unit_state|name=apt-daily.timer|state=failed": 0,
		"node_systemd_unit_state|name=home.mount|state=failed":      0,
		"node_systemd_service_restart_total|name=ssh.service":       3,
		// Activés au démarrage : ssh (tourne), cron (arrêté), nginx (arrêté).
		"node_systemd_unit_enabled_active|name=ssh.service":   1,
		"node_systemd_unit_enabled_active|name=cron.service":  0,
		"node_systemd_unit_enabled_active|name=nginx.service": 0,
		"node_systemd_enabled_units_total":                    3,
		"node_systemd_enabled_units_down":                     2,
	})
	for k := range p {
		if contient(k, "state=active") {
			t.Errorf("la série state=active ne doit plus être envoyée : %s", k)
		}
		for _, exclu := range []string{"session-1.scope", "dev-sda.device", "getty@.service", "fantome.service",
			"zfs-import-cache", "corosync", "netavark", "enabled_active|name=dmesg", "monappli", "inconnu"} {
			if contient(k, exclu) {
				t.Errorf("élément exclu présent : %s", k)
			}
		}
	}
}

func TestSystemdRafraichissementEspace(t *testing.T) {
	src := sourceExemple()
	s := newSystemdAvecSource(src, nil)
	t0 := time.Unix(1_000_000, 0)
	maintenant := t0
	s.maintenant = func() time.Time { return maintenant }

	collecter(t, s)
	appels := src.appelsRed
	maintenant = t0.Add(time.Minute)
	p := collecter(t, s)
	if src.appelsRed != appels {
		t.Error("les redémarrages ne doivent être relus qu'après 5 minutes")
	}
	// La dernière valeur est quand même renvoyée (sinon la série disparaît).
	if p["node_systemd_service_restart_total|name=ssh.service"] != 3 {
		t.Error("la valeur en cache doit être renvoyée à chaque passage")
	}
	maintenant = t0.Add(6 * time.Minute)
	collecter(t, s)
	if src.appelsRed == appels {
		t.Error("les redémarrages auraient dû être relus après 5 minutes")
	}
}

func TestDevraitTourner(t *testing.T) {
	base := proprietesService{Type: "simple", ConditionOK: true, Etat: "inactive"}
	if !devraitTourner(base) {
		t.Error("un service simple arrêté devrait tourner")
	}
	// Code de sortie non nul : ce n'est pas une tâche de démarrage normale.
	echec := base
	echec.CodeSortie, echec.StatutSortie, echec.DebutMono, echec.FinMono = 1, 2, 1, 100
	if !devraitTourner(echec) {
		t.Error("une sortie en erreur doit être signalée")
	}
}
