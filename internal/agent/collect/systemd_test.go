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
	unites     []uniteEtat
	redem      map[string]uint32
	actives    []string
	props      map[string]proprietesService
	timers     map[string]tacheTimer
	appelsRed  int
	errActives error // ServicesActivesAuDemarrage en échec (relecture du 21/11)
}

func (f *sourceFactice) Timer(_ context.Context, u uniteEtat) (tacheTimer, error) {
	t, ok := f.timers[u.Nom]
	if !ok {
		return t, errors.New("inconnu")
	}
	return t, nil
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
	return f.actives, f.errActives
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

// Tâches planifiées (SPEC § 80) : un timer, le résultat de son service.
func TestSystemdTimers(t *testing.T) {
	src := sourceExemple()
	fin := time.Date(2026, 11, 20, 4, 42, 0, 0, time.UTC)
	src.timers = map[string]tacheTimer{
		"apt-daily.timer": {Nom: "apt-daily", Debut: fin.Add(-42 * time.Minute), Fin: fin, Derniere: fin.Add(-42 * time.Minute),
			Prochaine: fin.Add(24 * time.Hour), Resultat: "success"},
	}
	src.unites = append(src.unites, uniteEtat{Nom: "sauvegarde.timer", Etat: "active", Charge: "loaded"})
	src.timers["sauvegarde.timer"] = tacheTimer{Nom: "sauvegarde", Debut: fin, Fin: fin.Add(time.Minute), Derniere: fin,
		Prochaine: fin.Add(time.Hour), Resultat: "exit-code", Code: 3}
	s := newSystemdAvecSource(src, nil)
	p := collecter(t, s)
	verifier(t, p, map[string]float64{
		"mirador_tache_fin_timestamp_seconds|source=systemd|tache=apt-daily":       float64(fin.Unix()),
		"mirador_tache_code|source=systemd|tache=apt-daily":                        0,
		"mirador_tache_prochaine_timestamp_seconds|source=systemd|tache=apt-daily": float64(fin.Add(24 * time.Hour).Unix()),
		"mirador_tache_code|source=systemd|tache=sauvegarde":                       3,
	})
	if codeTimer(tacheTimer{Resultat: "timeout"}) != 1 {
		t.Error("échec sans code de retour : 1 attendu")
	}
}

// Relecture du 21/11 : un passage où la liste des services échoue garde les
// valeurs d'avant (pas de « 0 service arrêté » inventé).
func TestSystemdEchecGardeValeurs(t *testing.T) {
	src := sourceExemple()
	s := newSystemdAvecSource(src, nil)
	t0 := time.Unix(1_000_000, 0)
	maintenant := t0
	s.maintenant = func() time.Time { return maintenant }
	avant := collecter(t, s)["node_systemd_enabled_units_total"]
	if avant == 0 {
		t.Fatal("aucun service suivi au départ")
	}
	src.errActives = errors.New("D-Bus : délai dépassé")
	maintenant = t0.Add(6 * time.Minute)
	if apres := collecter(t, s)["node_systemd_enabled_units_total"]; apres != avant {
		t.Errorf("services suivis : %v puis %v après un échec D-Bus", avant, apres)
	}
}
