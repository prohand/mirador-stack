// SPDX-License-Identifier: GPL-3.0-or-later

package modeles

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/tsdb"

	"mirador-stack/internal/server/alertes"
	"mirador-stack/internal/server/base"
)

var t0 = time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)

func TestAttribution(t *testing.T) {
	ctx := context.Background()
	db, err := base.Ouvrir(filepath.Join(t.TempDir(), "mirador.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := base.Migrer(ctx, db, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	// Groupe « web » : Linux (disque 20 %) et Docker ; machine 1 : Certificats,
	// retire Docker, disque à 30 % ; machine 2 du même groupe : héritage seul.
	for _, a := range []struct {
		portee, modele, etat string
		params               map[string]float64
	}{
		{PorteeGroupe("web"), "linux", EtatActif, map[string]float64{"disque_libre_pct": 20}},
		{PorteeGroupe("web"), "docker", EtatActif, nil},
		{PorteeMachine(1), "certificats", EtatActif, nil},
		{PorteeMachine(1), "docker", EtatIgnore, nil},
		{PorteeMachine(1), "linux", EtatActif, map[string]float64{"disque_libre_pct": 30}},
	} {
		if err := Definir(ctx, db, a.portee, a.modele, a.etat, a.params, nil, "admin", t0); err != nil {
			t.Fatal(err)
		}
	}
	e1, err := Effectifs(ctx, db, 1, "web")
	if err != nil {
		t.Fatal(err)
	}
	if len(e1) != 2 || e1[0].Modele.Nom != "certificats" || e1[1].Modele.Nom != "linux" {
		t.Fatalf("machine 1 : %+v", e1)
	}
	if e1[1].Parametres["disque_libre_pct"] != 30 || !e1[1].Surcharges["disque_libre_pct"] || e1[1].Parametres["cpu_pct"] != 90 || e1[1].DuGroupe {
		t.Errorf("machine 1, linux : %+v", e1[1])
	}
	e2, _ := Effectifs(ctx, db, 2, "web")
	if len(e2) != 2 || e2[0].Modele.Nom != "docker" || e2[1].Parametres["disque_libre_pct"] != 20 || !e2[1].DuGroupe {
		t.Errorf("machine 2 : %+v", e2)
	}
	if ign, _ := Ignores(ctx, db, 1); !ign["docker"] {
		t.Error("docker ignoré attendu")
	}
	// Configuration apportée : clés des modèles, listes réunies.
	cfg, err := ConfigEffective(e2)
	if err != nil {
		t.Fatal(err)
	}
	if string(cfg["systemd"]) != "true" || string(cfg["docker_metriques"]) != "true" {
		t.Errorf("configuration : %v", cfg)
	}
	if r := reunir(json.RawMessage(`["a","b"]`), json.RawMessage(`["b","c"]`)); string(r) != `["a","b","c"]` {
		t.Errorf("listes : %s", r)
	}
	// Refus : modèle inconnu, paramètre hors bornes ou inconnu, portée invalide.
	for _, cas := range []func() error{
		func() error { return Definir(ctx, db, PorteeMachine(1), "inconnu", EtatActif, nil, nil, "admin", t0) },
		func() error {
			return Definir(ctx, db, PorteeMachine(1), "linux", EtatActif, map[string]float64{"cpu_pct": 500}, nil, "admin", t0)
		},
		func() error {
			return Definir(ctx, db, PorteeMachine(1), "linux", EtatActif, map[string]float64{"x": 1}, nil, "admin", t0)
		},
		func() error { return Definir(ctx, db, "tout", "linux", EtatActif, nil, nil, "admin", t0) },
	} {
		if cas() == nil {
			t.Error("attribution invalide acceptée")
		}
	}
	// Retrait : la ligne de la machine disparaît, l'héritage du groupe revient.
	if err := Definir(ctx, db, PorteeMachine(1), "docker", "", nil, nil, "admin", t0); err != nil {
		t.Fatal(err)
	}
	if e1, _ = Effectifs(ctx, db, 1, "web"); len(e1) != 3 {
		t.Errorf("après reprise : %d modèles", len(e1))
	}
	var n int
	_ = db.QueryRow(`SELECT count(*) FROM audit WHERE action LIKE 'modele_%'`).Scan(&n)
	if n != 6 {
		t.Errorf("audit : %d lignes", n)
	}
	// Services du modèle : liste de départ, puis modifiée ; envoyés dans
	// systemd_surveiller (réunis avec ceux des autres modèles).
	if l, _ := Services(ctx, db, Trouver("docker")); len(l) != 2 {
		t.Errorf("services de départ de docker : %v", l)
	}
	if err := DefinirServices(ctx, db, "linux", []string{"ssh.service", "cron.service"}, "admin", t0); err != nil {
		t.Fatal(err)
	}
	for _, mauvais := range [][]string{{"rm -rf /"}, {"a.service", "a.service"}, {"nginx"}} {
		if DefinirServices(ctx, db, "linux", mauvais, "admin", t0) == nil {
			t.Errorf("services %v acceptés", mauvais)
		}
	}
	e2, _ = Effectifs(ctx, db, 2, "web")
	cfg, _ = ConfigEffective(e2)
	if string(cfg["systemd_surveiller"]) != `["containerd.service","docker.service","cron.service","ssh.service"]` {
		t.Errorf("systemd_surveiller : %s", cfg["systemd_surveiller"])
	}
}

// Règles des modèles sur des données : seuil par machine, machines sans le
// modèle ignorées ; règle importée remplacée pour les machines du modèle.
func TestReglesSurDonnees(t *testing.T) {
	o := tsdb.DefaultOptions()
	db, err := tsdb.Open(t.TempDir(), nil, nil, o, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	app := db.Appender(context.Background())
	ecrire := func(v float64, paires ...string) {
		for ts := t0.Add(-10 * time.Minute); !ts.After(t0); ts = ts.Add(30 * time.Second) {
			if _, err := app.Append(0, labels.FromStrings(paires...), ts.UnixMilli(), v); err != nil {
				t.Fatal(err)
			}
		}
	}
	// a, b, c : 25 % libre sur /. a porte Linux (seuil 30 → alerte), b porte
	// Linux (seuil par défaut 15 → rien), c ne porte rien.
	for _, i := range []string{"a", "b", "c"} {
		ecrire(25, "__name__", "node_filesystem_avail_bytes", "instance", i, "mountpoint", "/", "fstype", "ext4")
		ecrire(100, "__name__", "node_filesystem_size_bytes", "instance", i, "mountpoint", "/", "fstype", "ext4")
	}
	ecrire(30, "__name__", "mirador_modele_parametre", "instance", "a", "modele", "linux", "parametre", "disque_libre_pct")
	ecrire(15, "__name__", "mirador_modele_parametre", "instance", "b", "modele", "linux", "parametre", "disque_libre_pct")
	for _, i := range []string{"a", "b"} {
		ecrire(1, "__name__", "mirador_machine_regle_remplacee", "instance", i, "regle", "DisqueFaible")
	}
	if err := app.Commit(); err != nil {
		t.Fatal(err)
	}
	lect := alertes.NouveauLecteur(db, nil)
	m := Trouver("linux")
	var regle RegleModele
	for _, r := range m.Regles {
		if r.Remplace == "DisqueFaible" {
			regle = r
		}
	}
	ech, err := lect.Instant(context.Background(), m.ExprRegle(regle), t0)
	if err != nil {
		t.Fatal(err)
	}
	if len(ech) != 1 || ech[0].Etiquettes.Get("instance") != "a" || ech[0].Valeur != 25 {
		t.Errorf("règle du modèle : %v", ech)
	}
	// Service attendu par un modèle : absent sur a (aucune série systemd),
	// présent sur b.
	ecrireTard := func(v float64, paires ...string) {
		app := db.Appender(context.Background())
		for ts := t0.Add(-10 * time.Minute); !ts.After(t0); ts = ts.Add(30 * time.Second) {
			_, _ = app.Append(0, labels.FromStrings(paires...), ts.UnixMilli(), v)
		}
		_ = app.Commit()
	}
	for _, i := range []string{"a", "b"} {
		ecrireTard(1, "__name__", "mirador_modele_service", "instance", i, "modele", "docker", "name", "docker.service")
	}
	ecrireTard(0, "__name__", "node_systemd_unit_state", "instance", "b", "name", "docker.service", "state", "failed")
	for _, r := range alertes.ReglesMirador() {
		if r.Nom != "ServiceDuModeleAbsent" {
			continue
		}
		ech, err := lect.Instant(context.Background(), r.Expr, t0)
		if err != nil || len(ech) != 1 || ech[0].Etiquettes.Get("instance") != "a" || ech[0].Etiquettes.Get("name") != "docker.service" {
			t.Errorf("service absent : %v %v", ech, err)
		}
	}
	// Règle importée (seuil 30 pour l'exemple) : remplacée pour a et b, reste pour c.
	importee := `node_filesystem_avail_bytes / node_filesystem_size_bytes * 100 < 30`
	remplacee := "(" + importee + `) unless on (instance) mirador_machine_regle_remplacee{regle="DisqueFaible"}`
	ech, err = lect.Instant(context.Background(), remplacee, t0)
	if err != nil {
		t.Fatal(err)
	}
	if len(ech) != 1 || ech[0].Etiquettes.Get("instance") != "c" {
		t.Errorf("règle importée remplacée : %v", ech)
	}
}
