// SPDX-License-Identifier: GPL-3.0-or-later

package alertes

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/prometheus/model/labels"
)

// Si les données disparaissent (SPEC § 46) : garder, fermer, ouvrir.
func TestAbsenceDeDonnees(t *testing.T) {
	ctx := context.Background()
	t0 := time.Now().Truncate(time.Minute).Add(-6 * time.Hour)
	silence := t0.Add(time.Hour) // « a » et « b » se taisent ici ; « c » continue
	donnees := tsdbTest(t)
	charge := func(m string) labels.Labels { return labels.FromStrings("__name__", "node_load1", "instance", m) }
	app := donnees.Appender(ctx)
	for ts := t0; ts.Before(t0.Add(5 * time.Hour)); ts = ts.Add(30 * time.Second) {
		if !ts.After(silence) {
			_, _ = app.Append(0, charge("a"), ts.UnixMilli(), 5) // en alerte
			_, _ = app.Append(0, charge("b"), ts.UnixMilli(), 0) // jamais en alerte
		}
		_, _ = app.Append(0, charge("c"), ts.UnixMilli(), 0)
	}
	if err := app.Commit(); err != nil {
		t.Fatal(err)
	}
	dernieres := map[string]time.Time{"a": silence, "b": silence, "c": t0.Add(5 * time.Hour)}
	essai := func(t *testing.T, absence string) (*Moteur, func(time.Time) []Evenement) {
		db := baseTest(t)
		r := Regle{Nom: "ChargeHaute", Groupe: "hote", Intervalle: Duree(time.Minute), Expr: "node_load1 > 1",
			Gravite: Critique, Active: true, Absence: absence}
		if _, err := Enregistrer(ctx, db, []Regle{r}, "test", t0); err != nil {
			t.Fatal(err)
		}
		m, err := Nouveau(ctx, Options{DB: db, Donnees: donnees, Maintenant: func() time.Time { return t0.Add(-time.Hour) },
			DerniereDonnee: func(i string) (time.Time, bool) { d, ok := dernieres[i]; return d, ok }})
		if err != nil {
			t.Fatal(err)
		}
		return m, func(ts time.Time) []Evenement { return m.EvaluerTout(ctx, ts) }
	}
	actives := func(m *Moteur) map[string]Alerte {
		out := map[string]Alerte{}
		for _, a := range m.EnCours() {
			if a.Etat == Active {
				out[a.Machine()] = a
			}
		}
		return out
	}

	t.Run("garder", func(t *testing.T) {
		m, eval := essai(t, AbsenceGarder)
		if evs := eval(t0.Add(10 * time.Minute)); len(evs) != 1 || evs[0].Type != "debut" {
			t.Fatalf("début : %+v", evs)
		}
		// Série disparue (5 min sans point), machine muette : l'alerte reste.
		for _, apres := range []time.Duration{10 * time.Minute, time.Hour, 3 * time.Hour} {
			if evs := eval(silence.Add(apres)); len(evs) != 0 {
				t.Fatalf("%v : %+v", apres, evs)
			}
			a, ok := actives(m)["a"]
			if !ok || !a.AbsenteDepuis.Equal(silence) {
				t.Fatalf("%v : %+v", apres, actives(m))
			}
		}
		// La machine revient (données à jour, condition fausse) : résolue.
		dernieres["a"] = silence.Add(3*time.Hour + 10*time.Minute)
		defer func() { dernieres["a"] = silence }()
		if evs := eval(silence.Add(3*time.Hour + 10*time.Minute)); len(evs) != 1 || evs[0].Type != "fin" {
			t.Fatalf("retour : %+v", evs)
		}
	})

	t.Run("fermer", func(t *testing.T) {
		_, eval := essai(t, AbsenceFermer)
		eval(t0.Add(10 * time.Minute))
		if evs := eval(silence.Add(10 * time.Minute)); len(evs) != 1 || evs[0].Type != "fin" {
			t.Fatalf("fermer : %+v", evs)
		}
	})

	t.Run("machine inconnue", func(t *testing.T) {
		_, eval := essai(t, AbsenceGarder)
		eval(t0.Add(10 * time.Minute))
		delete(dernieres, "a") // supprimée du parc
		defer func() { dernieres["a"] = silence }()
		if evs := eval(silence.Add(10 * time.Minute)); len(evs) != 1 || evs[0].Type != "fin" {
			t.Fatalf("inconnue : %+v", evs)
		}
	})

	t.Run("ouvrir", func(t *testing.T) {
		m, eval := essai(t, AbsenceOuvrir)
		eval(t0.Add(10 * time.Minute)) // « a » en alerte
		// 5 min de silence : trop tôt pour « b » (délai 10 min).
		eval(silence.Add(5 * time.Minute))
		if _, ok := actives(m)["b"]; ok {
			t.Fatal("b ouverte trop tôt")
		}
		evs := eval(silence.Add(12 * time.Minute))
		if len(evs) != 1 || evs[0].Type != "debut" || evs[0].Alerte.Machine() != "b" || evs[0].Alerte.AbsenteDepuis.IsZero() {
			t.Fatalf("ouvrir : %+v", evs)
		}
		if _, ok := actives(m)["c"]; ok {
			t.Fatal("c répond : pas d'alerte")
		}
		// Pas de doublon ensuite ; « a » (déjà en alerte) gardée.
		if evs := eval(silence.Add(20 * time.Minute)); len(evs) != 0 {
			t.Fatalf("doublon : %+v", evs)
		}
		if len(actives(m)) != 2 {
			t.Fatalf("actives : %+v", actives(m))
		}
	})
}
