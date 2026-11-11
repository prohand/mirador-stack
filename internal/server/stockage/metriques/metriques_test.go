// SPDX-License-Identifier: GPL-3.0-or-later

package metriques

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/tsdb"
	"github.com/prometheus/prometheus/tsdb/chunkenc"

	"mirador-stack/internal/server/stockage"
)

var (
	t0      = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	ctx     = context.Background()
	toutLir = LimitesLecture{MaxSeries: 1000, MaxPoints: 1_000_000}
)

// horloge simulée, lue aussi par la tâche d'agrégation de fond.
type horloge struct{ ns atomic.Int64 }

func nouvelleHorloge(t time.Time) *horloge {
	h := &horloge{}
	h.regler(t)
	return h
}

func (h *horloge) regler(t time.Time)    { h.ns.Store(t.UnixNano()) }
func (h *horloge) maintenant() time.Time { return time.Unix(0, h.ns.Load()).UTC() }

func ouvrir(t *testing.T, dossier string, h *horloge) *Moteur {
	t.Helper()
	m, err := Ouvrir(Options{Dossier: dossier, Maintenant: h.maintenant, rechargement: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func serie(nom, instance string, points ...stockage.Point) stockage.Serie {
	return stockage.Serie{
		Labels: []stockage.Label{{Nom: "__name__", Valeur: nom}, {Nom: "instance", Valeur: instance}},
		Points: points,
	}
}

func pt(d time.Duration, v float64) stockage.Point {
	return stockage.Point{T: t0.Add(d).UnixMilli(), V: v}
}

func nom(n string) *labels.Matcher { return labels.MustNewMatcher(labels.MatchEqual, "__name__", n) }

func TestEcrireLireRedemarrer(t *testing.T) {
	dir := t.TempDir()
	h := nouvelleHorloge(t0.Add(time.Minute))
	m := ouvrir(t, dir, h)
	err := m.Ecrire(ctx, []stockage.Serie{
		serie("node_load1", "pc", pt(0, 0.5), pt(30*time.Second, 0.7)),
		serie("node_load1", "nas", pt(0, 2)),
		serie("autre", "pc", pt(0, 1)),
	})
	if err != nil {
		t.Fatal(err)
	}
	lire := func(m *Moteur) []stockage.Serie {
		t.Helper()
		ss, err := m.Lire(ctx, t0.Add(-time.Minute).UnixMilli(), t0.Add(time.Minute).UnixMilli(), toutLir, nom("node_load1"))
		if err != nil {
			t.Fatal(err)
		}
		return ss
	}
	ss := lire(m)
	if len(ss) != 2 || ss[1].Valeur("instance") != "pc" || len(ss[1].Points) != 2 || ss[1].Points[1].V != 0.7 {
		t.Fatalf("lu : %+v", ss)
	}
	if m.Points.Load() != 4 {
		t.Errorf("points comptés : %d", m.Points.Load())
	}
	// Arrêt puis redémarrage : tout est relu depuis le WAL.
	if err := m.Fermer(); err != nil {
		t.Fatal(err)
	}
	m = ouvrir(t, dir, h)
	defer m.Fermer()
	if ss := lire(m); len(ss) != 2 || len(ss[1].Points) != 2 {
		t.Fatalf("après redémarrage : %+v", ss)
	}
}

func TestPointsRejetes(t *testing.T) {
	h := nouvelleHorloge(t0)
	m := ouvrir(t, t.TempDir(), h)
	defer m.Fermer()
	if err := m.Ecrire(ctx, []stockage.Serie{serie("a", "pc", pt(0, 1))}); err != nil {
		t.Fatal(err)
	}
	// Même instant, autre valeur : refusé. Trop ancien (au-delà de 24 h) : refusé.
	// Ancien mais dans la fenêtre de retard (file d'un agent) : accepté.
	err := m.Ecrire(ctx, []stockage.Serie{serie("a", "pc", pt(0, 2), pt(-25*time.Hour, 1), pt(-20*time.Hour, 1))})
	if err != nil {
		t.Fatal(err)
	}
	if m.Rejetes.Load() != 2 || m.Points.Load() != 2 {
		t.Fatalf("rejetés %d, acceptés %d", m.Rejetes.Load(), m.Points.Load())
	}
}

func TestLimitesLecture(t *testing.T) {
	m := ouvrir(t, t.TempDir(), nouvelleHorloge(t0))
	defer m.Fermer()
	_ = m.Ecrire(ctx, []stockage.Serie{serie("a", "1", pt(0, 1), pt(time.Second, 1)), serie("a", "2", pt(0, 1))})
	if _, err := m.Lire(ctx, t0.UnixMilli(), t0.Add(time.Minute).UnixMilli(), LimitesLecture{MaxSeries: 1, MaxPoints: 100}, nom("a")); err != ErrTropDeDonnees {
		t.Errorf("séries : %v", err)
	}
	if _, err := m.Lire(ctx, t0.UnixMilli(), t0.Add(time.Minute).UnixMilli(), LimitesLecture{MaxSeries: 10, MaxPoints: 2}, nom("a")); err != ErrTropDeDonnees {
		t.Errorf("points : %v", err)
	}
}

func TestAgregation(t *testing.T) {
	dir := t.TempDir()
	h := nouvelleHorloge(t0)
	m := ouvrir(t, dir, h)
	// 15 min de points toutes les 30 s : une jauge (0, 1, 2…) et un compteur.
	var jauge, compteur []stockage.Point
	for i := 0; i < 30; i++ {
		jauge = append(jauge, pt(time.Duration(i)*30*time.Second, float64(i)))
		compteur = append(compteur, pt(time.Duration(i)*30*time.Second, float64(100+i)))
	}
	if err := m.Ecrire(ctx, []stockage.Serie{serie("node_load1", "pc", jauge...), serie("node_cpu_seconds_total", "pc", compteur...)}); err != nil {
		t.Fatal(err)
	}
	// Une fenêtre n'est agrégée qu'après le délai de retard (24 h).
	h.regler(t0.Add(24*time.Hour + 16*time.Minute))
	if err := m.Agreger(ctx); err != nil {
		t.Fatal(err)
	}
	lireAgrege := func(n string) []stockage.Point {
		t.Helper()
		q, err := m.agrege.Querier(t0.UnixMilli(), t0.Add(time.Hour).UnixMilli())
		if err != nil {
			t.Fatal(err)
		}
		defer q.Close()
		ss, err := lireTout(ctx, q, toutLir, nom(n))
		if err != nil || len(ss) != 1 {
			t.Fatalf("%s : %+v %v", n, ss, err)
		}
		return ss[0].Points
	}
	j := lireAgrege("node_load1")
	if len(j) != 3 || j[0].V != 4.5 || j[1].V != 14.5 || j[2].V != 24.5 || j[0].T != t0.Add(5*time.Minute).UnixMilli()-1 {
		t.Errorf("jauge (moyennes attendues 4,5 / 14,5 / 24,5) : %+v", j)
	}
	c := lireAgrege("node_cpu_seconds_total")
	if len(c) != 3 || c[0].V != 109 || c[2].V != 129 {
		t.Errorf("compteur (dernières valeurs 109 / 119 / 129) : %+v", c)
	}
	// Relancer ne double rien (progression gardée sur disque, même après redémarrage).
	if err := m.Fermer(); err != nil {
		t.Fatal(err)
	}
	m = ouvrir(t, dir, h)
	defer m.Fermer()
	if err := m.Agreger(ctx); err != nil {
		t.Fatal(err)
	}
	if j := lireAgrege("node_load1"); len(j) != 3 {
		t.Errorf("après reprise : %+v", j)
	}
	// Une lecture d'une période plus ancienne que la rétention brute passe par les agrégats.
	h.regler(t0.Add(8 * 24 * time.Hour))
	if _, agrege, _ := m.Interroger(t0.UnixMilli(), t0.Add(time.Hour).UnixMilli()); !agrege {
		t.Error("période ancienne : la base agrégée doit être lue")
	}
}

func TestRetentionAChaud(t *testing.T) {
	h := nouvelleHorloge(t0)
	m := ouvrir(t, t.TempDir(), h)
	defer m.Fermer()
	// 12 h de données en lots successifs, puis compaction en blocs de 2 h.
	for i := 0; i < 12*60; i++ {
		d := time.Duration(i) * time.Minute
		if err := m.Ecrire(ctx, []stockage.Serie{serie("a", "pc", pt(d, float64(i)))}); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.brut.Compact(ctx); err != nil {
		t.Fatal(err)
	}
	avant := len(m.brut.Blocks())
	if avant < 3 {
		t.Fatalf("%d blocs seulement", avant)
	}
	r := m.Reglages()
	r.RetentionBrut = 3 * time.Hour
	if err := m.Regler(r); err != nil {
		t.Fatal(err)
	}
	limite := time.Now().Add(5 * time.Second)
	for len(m.brut.Blocks()) >= avant && time.Now().Before(limite) {
		time.Sleep(50 * time.Millisecond)
	}
	if apres := len(m.brut.Blocks()); apres >= avant {
		t.Fatalf("aucun bloc supprimé (%d → %d)", avant, apres)
	}
	if err := m.Regler(Reglages{}); err == nil {
		t.Error("réglages nuls acceptés")
	}
}

// TestAncienEncodageLisible : une base écrite avec l'ancien encodage (XOR)
// reste lisible après le passage à XOR2.
func TestAncienEncodageLisible(t *testing.T) {
	dir := t.TempDir()
	o := tsdb.DefaultOptions()
	o.FloatChunkEncoding = chunkenc.EncXOR
	ancienne, err := tsdb.Open(filepath.Join(dir, "brut"), nil, nil, o, nil)
	if err != nil {
		t.Fatal(err)
	}
	app := ancienne.Appender(ctx)
	for i := 0; i < 600; i++ { // assez de points pour un bloc compacté
		if _, err := app.Append(0, labels.FromStrings("__name__", "a", "instance", "pc"), t0.Add(time.Duration(i)*time.Minute).UnixMilli(), float64(i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := app.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := ancienne.Compact(ctx); err != nil {
		t.Fatal(err)
	}
	if err := ancienne.Close(); err != nil {
		t.Fatal(err)
	}
	m := ouvrir(t, dir, nouvelleHorloge(t0.Add(10*time.Hour)))
	defer m.Fermer()
	_ = m.Ecrire(ctx, []stockage.Serie{serie("a", "pc", pt(10*time.Hour, 1))}) // nouveau point, en XOR2
	ss, err := m.Lire(ctx, t0.UnixMilli(), t0.Add(11*time.Hour).UnixMilli(), toutLir, nom("a"))
	if err != nil || len(ss) != 1 || len(ss[0].Points) != 601 {
		t.Fatalf("lecture mixte XOR / XOR2 : %v", err)
	}
}

func TestEstCompteur(t *testing.T) {
	for n, attendu := range map[string]bool{"node_cpu_seconds_total": true, "http_request_duration_seconds_bucket": true,
		"x_count": true, "node_load1": false, "node_memory_MemAvailable_bytes": false} {
		if estCompteur(n) != attendu {
			t.Errorf("%s", n)
		}
	}
}
