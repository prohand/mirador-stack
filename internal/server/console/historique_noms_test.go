// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/tsdb"

	"mirador-stack/internal/server/alertes"
)

// Une machine renommée : une seule courbe continue sous le nouveau nom.
func TestHistoriqueNomsMetriques(t *testing.T) {
	o := tsdb.DefaultOptions()
	db, err := tsdb.Open(t.TempDir(), nil, nil, o, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	debut := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	renomme := debut.Add(30 * time.Minute)
	fin := debut.Add(time.Hour)
	// Données plus anciennes (8 h avant), compactées en bloc sur disque plus bas.
	app := db.Appender(context.Background())
	for ts := debut.Add(-8 * time.Hour); ts.Before(debut.Add(-5 * time.Hour)); ts = ts.Add(time.Minute) {
		if _, err := app.Append(0, labels.FromStrings("__name__", "node_load1", "instance", "dns02", "job", "node"), ts.UnixMilli(), 1); err != nil {
			t.Fatal(err)
		}
	}
	if err := app.Commit(); err != nil {
		t.Fatal(err)
	}
	app = db.Appender(context.Background())
	for ts := debut; !ts.After(fin); ts = ts.Add(time.Minute) {
		nom := "dns02"
		if !ts.Before(renomme) {
			nom = "srv-test"
		}
		for _, e := range []labels.Labels{
			labels.FromStrings("__name__", "node_load1", "instance", nom, "job", "node"),
			labels.FromStrings("__name__", "node_load1", "instance", "autre", "job", "node"),
		} {
			if _, err := app.Append(0, e, ts.UnixMilli(), 1); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := app.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := db.Compact(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(db.Blocks()) == 0 {
		t.Fatal("aucun bloc sur disque")
	}
	anciens := map[string]string{}
	lect := alertes.NouveauLecteur(avecNomsAnciens(db, func() map[string]string { return anciens }), nil)
	plage := func(expr string) []alertes.Courbe {
		t.Helper()
		c, _, err := lect.Plage(context.Background(), expr, debut, fin, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	// Sans table : données telles quelles (moitié de la période).
	if c := plage(`node_load1{instance="srv-test"}`); len(c) != 1 || len(c[0].T) > 35 {
		t.Fatalf("sans rattachement : %d courbes", len(c))
	}
	anciens["dns02"] = "srv-test"
	c := plage(`node_load1{instance="srv-test"}`)
	if len(c) != 1 || len(c[0].T) != 61 || c[0].Etiquettes.Get("instance") != "srv-test" {
		t.Fatalf("rattachement : %d courbes, %v", len(c), c)
	}
	// Sans filtre : 2 machines, plus de « dns02 ».
	c = plage(`node_load1`)
	if len(c) != 2 {
		t.Fatalf("toutes : %d courbes", len(c))
	}
	for _, x := range c {
		if x.Etiquettes.Get("instance") == "dns02" {
			t.Error("ancien nom affiché")
		}
	}
	// Somme : pas de double compte.
	if c = plage(`sum(node_load1)`); len(c) != 1 || c[0].V[0] != 2 || c[0].V[60] != 2 {
		t.Errorf("somme : %v", c)
	}
	// Filtre par expression régulière et négation.
	if c = plage(`node_load1{instance=~"srv.*"}`); len(c) != 1 || len(c[0].T) != 61 {
		t.Errorf("regexp : %v", c)
	}
	if c = plage(`node_load1{instance!="srv-test"}`); len(c) != 1 || c[0].Etiquettes.Get("instance") != "autre" {
		t.Errorf("négation : %v", c)
	}
	// Bloc sur disque + données récentes : une seule courbe.
	c, _, err = lect.Plage(context.Background(), `node_load1{instance="srv-test"}`, debut.Add(-8*time.Hour), fin, 10*time.Minute)
	if err != nil || len(c) != 1 || c[0].Etiquettes.Get("instance") != "srv-test" {
		t.Errorf("avec bloc : %v %v", c, err)
	}
	// Ancien nom demandé : plus rien sous ce nom.
	if c = plage(`node_load1{instance="dns02"}`); len(c) != 0 {
		t.Errorf("ancien nom : %v", c)
	}
}
