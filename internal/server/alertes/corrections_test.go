// SPDX-License-Identifier: GPL-3.0-or-later

package alertes

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/model/labels"
)

const exprAgentMuetAvant = `min by (instance) (lag(node_uname_info[1d])) > 60`

// Machine muette plus de 24 h (SPEC § 45) : l'ancienne règle se ferme
// seule, la règle corrigée reste vraie, avec les mêmes étiquettes ; une
// machine qui répond ne déclenche rien.
func TestAgentMuetAuDelaDe24h(t *testing.T) {
	ctx := context.Background()
	donnees := tsdbTest(t)
	t0 := time.Now().Truncate(time.Minute).Add(-72 * time.Hour)
	fin := t0.Add(71 * time.Hour)
	derniere := t0.Add(time.Hour) // « a » se tait ici ; « b » répond jusqu'au bout
	uname := func(m string) labels.Labels { return labels.FromStrings("__name__", "node_uname_info", "instance", m) }
	recue := func(m string) labels.Labels {
		return labels.FromStrings("__name__", MetriqueDerniereReception, "instance", m, "job", "mirador")
	}
	// Points écrits dans l'ordre du temps (la base refuse les retours en arrière).
	app := donnees.Appender(ctx)
	for ts := t0; !ts.After(fin); ts = ts.Add(30 * time.Second) {
		ms := ts.UnixMilli()
		if !ts.After(derniere) {
			_, _ = app.Append(0, uname("a"), ms, 1)
		}
		_, _ = app.Append(0, uname("b"), ms, 1)
		if ts.Second() == 0 { // série interne : une fois par minute
			_, _ = app.Append(0, recue("a"), ms, float64(min(ts.Unix(), derniere.Unix())))
			// Pire cas pour « b » : la valeur a presque une minute de retard.
			_, _ = app.Append(0, recue("b"), ms, float64(ts.Add(-59*time.Second).Unix()))
		}
	}
	if err := app.Commit(); err != nil {
		t.Fatal(err)
	}
	m, err := Nouveau(ctx, Options{DB: baseTest(t), Donnees: donnees})
	if err != nil {
		t.Fatal(err)
	}
	corrigee, _, ok := corrigerExpr(exprAgentMuetAvant)
	if !ok {
		t.Fatal("AgentMuet non reconnue")
	}
	for _, apres := range []time.Duration{2 * time.Minute, 30 * time.Minute, 23 * time.Hour, 23*time.Hour + 30*time.Minute,
		24*time.Hour - time.Minute, 24 * time.Hour, 24*time.Hour + time.Minute, 25 * time.Hour, 48 * time.Hour, 70 * time.Hour} {
		res, err := m.requete(ctx, corrigee, derniere.Add(apres))
		if err != nil {
			t.Fatal(err)
		}
		if len(res) != 1 || res[0].etq.String() != `{instance="a"}` {
			t.Fatalf("%v après la dernière donnée : %+v", apres, res)
		}
		// Valeur : secondes depuis la dernière donnée (à 30 s près).
		if d := res[0].valeur - apres.Seconds(); d < -1 || d > 31 {
			t.Errorf("%v : valeur %v", apres, res[0].valeur)
		}
	}
	// L'ancienne expression, elle, ne voit plus rien après 24 h (le défaut).
	if res, err := m.requete(ctx, exprAgentMuetAvant, derniere.Add(25*time.Hour)); err != nil || len(res) != 0 {
		t.Fatalf("ancienne expression : %+v %v", res, err)
	}
}

func TestCorrigerRegles(t *testing.T) {
	ctx := context.Background()
	db := baseTest(t)
	maintenant := time.Now()
	r := func(nom, expr string) Regle {
		return Regle{Nom: nom, Groupe: "hote", Intervalle: Duree(30 * time.Second), Expr: expr, Gravite: Critique,
			Active: true, Origine: "vmalert:system.yml"}
	}
	if _, err := Enregistrer(ctx, db, []Regle{
		r("AgentMuet", "min by (instance)\n  (lag(node_uname_info[1d])) >  60"), // espaces différents : reconnue
		r("AutreMuet", "min by (instance) (lag(node_uname_info[1d])) > 120"),    // changée par l'utilisateur : intacte
	}, "test", maintenant); err != nil {
		t.Fatal(err)
	}
	if n, err := CorrigerRegles(ctx, db, maintenant); err != nil || n != 1 {
		t.Fatalf("1re fois : %d, %v", n, err)
	}
	if n, err := CorrigerRegles(ctx, db, maintenant); err != nil || n != 0 {
		t.Fatalf("2e fois : %d, %v", n, err)
	}
	regles, err := ListerRegles(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range regles {
		switch x.Nom {
		case "AgentMuet":
			if !strings.Contains(x.Expr, MetriqueDerniereReception) || !strings.Contains(x.Note, "§ 45") {
				t.Errorf("AgentMuet : %q (%q)", x.Expr, x.Note)
			}
			if _, err := x.Valider(); err != nil {
				t.Error(err)
			}
		case "AutreMuet":
			if strings.Contains(x.Expr, MetriqueDerniereReception) {
				t.Errorf("AutreMuet modifiée : %q", x.Expr)
			}
		}
	}
	var qui string
	if err := db.QueryRowContext(ctx, `SELECT modifie_par FROM regles WHERE nom = 'AgentMuet'`).Scan(&qui); err != nil || qui != "mirador" {
		t.Errorf("modifiée par %q, %v", qui, err)
	}
}

// Coût de la règle corrigée comparé à l'ancienne : 20 machines, 24 h de
// node_uname_info (30 s) et de la série interne (1 min).
func BenchmarkAgentMuet(b *testing.B) {
	ctx := context.Background()
	donnees := tsdbTest(b)
	t0 := time.Now().Truncate(time.Minute).Add(-24 * time.Hour)
	app := donnees.Appender(ctx)
	for ts := t0; ts.Before(t0.Add(24 * time.Hour)); ts = ts.Add(30 * time.Second) {
		for i := range 20 {
			nom := string(rune('a' + i))
			_, _ = app.Append(0, labels.FromStrings("__name__", "node_uname_info", "instance", nom), ts.UnixMilli(), 1)
			if ts.Second() == 0 {
				_, _ = app.Append(0, labels.FromStrings("__name__", MetriqueDerniereReception, "instance", nom, "job", "mirador"),
					ts.UnixMilli(), float64(ts.Unix()))
			}
		}
	}
	if err := app.Commit(); err != nil {
		b.Fatal(err)
	}
	m, err := Nouveau(ctx, Options{DB: baseTest(b), Donnees: donnees})
	if err != nil {
		b.Fatal(err)
	}
	corrigee, _, _ := corrigerExpr(exprAgentMuetAvant)
	for _, c := range []struct{ nom, expr string }{{"avant", exprAgentMuetAvant}, {"corrigee", corrigee}} {
		b.Run(c.nom, func(b *testing.B) {
			for b.Loop() {
				if _, err := m.requete(ctx, c.expr, t0.Add(24*time.Hour)); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
