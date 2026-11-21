// SPDX-License-Identifier: GPL-3.0-or-later

package taches

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mirador-stack/internal/server/base"
)

// Historique : une exécution notée une fois, 60 au plus par tâche, lues
// de la plus récente à la plus ancienne.
func TestHistorique(t *testing.T) {
	db, err := base.Ouvrir(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if _, err := base.Migrer(ctx, db, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	var n Noteur
	m := time.Date(2026, 11, 20, 2, 0, 0, 0, time.UTC)
	for i := 0; i < 70; i++ {
		f := m.Add(time.Duration(i) * 24 * time.Hour)
		r := Releve{Debut: f.Add(-10 * time.Minute), Fin: f, Code: i % 7 / 6,
			VMs: []VMReleve{{ID: "101", OK: true, Octets: 2 << 30}, {ID: "102", OK: false, Octets: 0}}}
		for k := 0; k < 3; k++ { // même exécution vue 3 fois : une ligne
			if err := n.Noter(ctx, db, "pve1|pve|vzdump", r); err != nil {
				t.Fatal(err)
			}
		}
	}
	_ = n.Noter(ctx, db, "pve1|pve|vzdump", Releve{}) // jamais vue : rien
	var total int
	_ = db.QueryRow(`SELECT count(*) FROM taches_executions`).Scan(&total)
	if total != GardeesParTache {
		t.Fatalf("%d lignes, %d attendues", total, GardeesParTache)
	}
	h, err := Historiques(ctx, db, []string{"pve1|pve|vzdump", "autre"}, 10)
	if err != nil {
		t.Fatal(err)
	}
	l := h["pve1|pve|vzdump"]
	if len(l) != 10 || !l[0].Fin.After(l[1].Fin) || l[0].Duree() != 10*time.Minute || l[0].VMs["101"] != 2<<30 || len(l[0].VMs) != 1 {
		t.Fatalf("historique : %d, %+v", len(l), l[0])
	}
	if len(h["autre"]) != 0 {
		t.Error("tâche inconnue")
	}
}

// Anomalies : durée anormale, en cours trop longtemps, taille en baisse ;
// rien sans historique suffisant.
func TestAnomalies(t *testing.T) {
	m := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	var hist []Execution
	for i := 1; i <= 5; i++ {
		f := m.Add(-time.Duration(i) * 24 * time.Hour)
		hist = append(hist, Execution{Debut: f.Add(-20 * time.Minute), Fin: f, VMs: map[string]float64{"101": 2 << 30, "102": 50 << 20}})
	}
	// Normale.
	r := Releve{Debut: m.Add(-25 * time.Minute), Fin: m, VMs: []VMReleve{{ID: "101", OK: true, Octets: 1.9 * (1 << 30)}}}
	if a := Anomalies(r, hist, m); len(a) != 0 {
		t.Errorf("normale : %+v", a)
	}
	// Durée : 2 h au lieu de 20 min.
	r.Debut = m.Add(-2 * time.Hour)
	a := Anomalies(r, hist, m)
	if len(a) != 1 || a[0].Type != "duree" || a[0].Texte != "a duré 2 h, d'habitude 20 min" {
		t.Errorf("durée : %+v", a)
	}
	// Taille : 2 Go → 300 Mo (la petite VM de 50 Mo n'est pas comparée).
	r = Releve{Debut: m.Add(-20 * time.Minute), Fin: m, VMs: []VMReleve{{ID: "101", Nom: "web", OK: true, Octets: 300 << 20},
		{ID: "102", OK: true, Octets: 1 << 20}}}
	a = Anomalies(r, hist, m)
	if len(a) != 1 || a[0].Type != "taille" || !strings.Contains(a[0].Texte, "101 web : 2,0 Go → 300 Mo") {
		t.Errorf("taille : %+v", a)
	}
	// En cours depuis 3 h (début après la dernière fin).
	r = Releve{Debut: m.Add(-3 * time.Hour), Fin: m.Add(-24 * time.Hour)}
	a = Anomalies(r, hist, m)
	if len(a) != 1 || a[0].Type != "encours" {
		t.Errorf("en cours : %+v", a)
	}
	// Deux exécutions connues seulement : pas de durée habituelle.
	r = Releve{Debut: m.Add(-2 * time.Hour), Fin: m}
	if a := Anomalies(r, hist[:2], m); len(a) != 0 {
		t.Errorf("historique court : %+v", a)
	}
	// État : OK devient « à surveiller », un échec reste un échec.
	e := AvecAnomalies(Etat{Classe: "ok", Texte: "OK"}, []Anomalie{{Court: "Durée anormale"}})
	if e.Classe != "attention" || e.Texte != "Durée anormale" || !e.Probleme {
		t.Errorf("état : %+v", e)
	}
	if e := AvecAnomalies(Etat{Classe: "erreur", Texte: "Échec · code 1", Probleme: true}, []Anomalie{{Court: "Durée anormale"}}); e.Texte != "Échec · code 1" {
		t.Errorf("échec : %+v", e)
	}
	// Série pour la règle TacheAnormale.
	ts := []Tache{{Cle: "pve1|pve|vzdump", Machine: "pve1", Source: "pve", Nom: "vzdump", Gravite: "warning"}}
	trouve := false
	for _, s := range Series(ts, map[string][]Anomalie{"pve1|pve|vzdump": {{Court: "Durée anormale"}}}) {
		if s.Nom == "mirador_tache_anomalie" && strings.Join(s.Etiquettes, ",") == "instance,pve1,source,pve,tache,vzdump,quoi,Durée anormale" {
			trouve = true
		}
	}
	if !trouve {
		t.Error("série mirador_tache_anomalie absente")
	}
}

func TestTextes(t *testing.T) {
	for d, att := range map[time.Duration]string{40 * time.Second: "40 s", 25 * time.Minute: "25 min", 130 * time.Minute: "2 h 10", 3 * 24 * time.Hour: "3 j"} {
		if g := DureeTexte(d); g != att {
			t.Errorf("%v : %q", d, g)
		}
	}
	for o, att := range map[float64]string{500: "500 o", 300 << 20: "300 Mo", 2.1 * (1 << 30): "2,1 Go"} {
		if g := TailleTexte(o); g != att {
			t.Errorf("%v : %q", o, g)
		}
	}
}

// Relecture du 21/11 : une tâche disparue depuis plus de 180 jours est
// oubliée (base et mémoire) ; une tâche encore vivante garde son historique.
func TestOublierTachesDisparues(t *testing.T) {
	db, err := base.Ouvrir(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if _, err := base.Migrer(ctx, db, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	var n Noteur
	m := time.Now()
	_ = n.Noter(ctx, db, "pc1|systemd|jetable.timer", Releve{Fin: m.Add(-200 * 24 * time.Hour)})
	_ = n.Noter(ctx, db, "pc1|systemd|apt-daily.timer", Releve{Fin: m.Add(-time.Hour)})
	if err := n.Oublier(ctx, db, m); err != nil {
		t.Fatal(err)
	}
	var cles []string
	rows, _ := db.Query(`SELECT DISTINCT cle FROM taches_executions`)
	for rows.Next() {
		var c string
		_ = rows.Scan(&c)
		cles = append(cles, c)
	}
	_ = rows.Close()
	if strings.Join(cles, ",") != "pc1|systemd|apt-daily.timer" || len(n.vues) != 1 {
		t.Errorf("après le ménage : base %v, mémoire %d", cles, len(n.vues))
	}
}
