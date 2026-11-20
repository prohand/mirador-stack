// SPDX-License-Identifier: GPL-3.0-or-later

package taches

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/prometheus/prometheus/model/labels"

	"mirador-stack/internal/server/alertes"
	"mirador-stack/internal/server/base"
)

// Les règles des tâches sont acceptées par le langage restreint.
func TestReglesTaches(t *testing.T) {
	n := 0
	for _, r := range alertes.ReglesMirador() {
		if r.Groupe != "taches" {
			continue
		}
		n++
		if _, err := alertes.AnalyserExpr(r.Expr); err != nil {
			t.Errorf("%s : %v", r.Nom, err)
		}
	}
	if n != 4 {
		t.Fatalf("%d règles de tâches", n)
	}
}

// Échéance et état : période réglée, timer (prochaine exécution), échec.
func TestCalculer(t *testing.T) {
	m := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	jour := Tache{Periode: 24 * time.Hour, Marge: time.Hour, CreeLe: m.Add(-72 * time.Hour)}
	if e := Calculer(jour, Releve{Fin: m.Add(-10 * time.Hour)}, m); e.Probleme || e.Texte != "OK" {
		t.Errorf("à l'heure : %+v", e)
	}
	if e := Calculer(jour, Releve{Fin: m.Add(-3 * 24 * time.Hour)}, m); !e.Probleme || e.Texte != "En retard de 47 h" {
		t.Errorf("en retard : %+v", e)
	}
	if e := Calculer(jour, Releve{Fin: m.Add(-time.Hour), Code: 3}, m); !e.Probleme || e.Texte != "Échec · code 3" {
		t.Errorf("échec : %+v", e)
	}
	if e := Calculer(Tache{Marge: time.Hour, CreeLe: m}, Releve{Fin: m.Add(-time.Hour), Prochaine: m.Add(-2 * time.Hour)}, m); !e.Probleme {
		t.Errorf("timer qui n'a pas tourné à l'heure prévue : %+v", e)
	}
	if e := Calculer(jour, Releve{}, m); !e.Probleme || e.Texte != "Jamais reçue" {
		t.Errorf("jamais reçue après le délai : %+v", e)
	}
	if e := Calculer(Tache{Periode: time.Hour, CreeLe: m}, Releve{}, m); e.Probleme || e.Classe != "inconnu" {
		t.Errorf("tout juste créée : %+v", e)
	}
}

// Ping : création, signal réussi, échec par code, jeton inconnu ; séries.
func TestPingEtSeries(t *testing.T) {
	db, err := base.Ouvrir(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if _, err := base.Migrer(ctx, db, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	m := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	jeton, err := CreerPing(ctx, db, Tache{Nom: "Sauvegarde NAS", Machine: "nas", Periode: 24 * time.Hour, Marge: time.Hour, Gravite: "critical"}, m)
	if err != nil || len(jeton) < 20 {
		t.Fatalf("création : %q %v", jeton, err)
	}
	var e string
	_ = db.QueryRow(`SELECT jeton FROM taches`).Scan(&e)
	if e == jeton {
		t.Fatal("le jeton doit être gardé haché")
	}
	if err := Signaler(ctx, db, jeton, false, 0, 3*time.Minute, "", m.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := Signaler(ctx, db, "inconnu-inconnu-inconnu", false, 0, 0, "", m); err != ErrJetonInconnu {
		t.Fatalf("jeton inconnu : %v", err)
	}
	ts, _ := Lister(ctx, db)
	if len(ts) != 1 || ts[0].Releve.Fin.Unix() != m.Add(time.Hour).Unix() || ts[0].Releve.Debut.Unix() != m.Add(57*time.Minute).Unix() {
		t.Fatalf("relevé : %+v", ts)
	}
	_ = Signaler(ctx, db, jeton, false, 2, 0, "disque plein", m.Add(2*time.Hour))
	ts, _ = Lister(ctx, db)
	series := map[string]float64{}
	for _, s := range Series(ts) {
		series[s.Nom+"|"+s.Etiquettes[1]+"|"+s.Etiquettes[len(s.Etiquettes)-1]] = s.Valeur
	}
	if series["mirador_tache_code|nas|Sauvegarde NAS"] != 2 || series["mirador_tache_suivie|nas|critical"] != 1 ||
		series["mirador_tache_echeance_timestamp_seconds|nas|critical"] != float64(m.Add(2*time.Hour+25*time.Hour).Unix()) {
		t.Fatalf("séries : %v", series)
	}
	// Tâche d'agent : relevé lu dans les métriques.
	if err := Suivre(ctx, db, Tache{Nom: "certbot", Source: "systemd", Machine: "web1", Marge: time.Hour, Gravite: "warning"}, m); err != nil {
		t.Fatal(err)
	}
	if err := Suivre(ctx, db, Tache{Nom: "certbot", Source: "systemd", Machine: "web1", Marge: time.Hour, Gravite: "warning"}, m); err == nil {
		t.Fatal("suivie deux fois")
	}
	det := Detectees(func(expr string) ([]alertes.Echantillon, error) {
		l := labels.FromStrings("instance", "web1", "source", "systemd", "tache", "certbot")
		switch expr {
		case "mirador_tache_fin_timestamp_seconds":
			return []alertes.Echantillon{{Etiquettes: l, Valeur: float64(m.Unix())}}, nil
		case "mirador_tache_prochaine_timestamp_seconds":
			return []alertes.Echantillon{{Etiquettes: l, Valeur: float64(m.Add(12 * time.Hour).Unix())}}, nil
		}
		return nil, nil
	})
	ts, _ = Lister(ctx, db)
	for _, x := range AvecReleves(ts, det) {
		if x.Source == "systemd" && Echeance(x, x.Releve).Unix() != m.Add(13*time.Hour).Unix() {
			t.Fatalf("échéance d'un timer : %v", Echeance(x, x.Releve))
		}
	}
}
