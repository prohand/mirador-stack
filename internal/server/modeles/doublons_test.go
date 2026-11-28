// SPDX-License-Identifier: GPL-3.0-or-later

package modeles

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"
	"github.com/prometheus/prometheus/tsdb"

	"mirador-stack/internal/server/alertes"
)

// Revue de sécurité du 28/11 (H1) : PromQL fait échouer TOUTE la requête
// dès que le côté « un » d'une jointure (on (…), group_left) a deux séries
// pour la même clé. Une seule machine (token volé, agent modifié) qui
// envoie deux fois la même série avec une étiquette en plus mettait donc
// en erreur une règle pour tout le parc.
//
// Ce test écrit, pour chaque métrique lue par les règles livrées (règles
// mirador et règles des modèles), DEUX séries de la même machine qui ne
// diffèrent que par une étiquette « doublon », puis évalue chaque règle :
// aucune ne doit être en erreur.

// seriesDoublees : deux séries par sélecteur des expressions, pour la
// machine « a ». Les égalités du sélecteur (modele="linux"…) sont reprises ;
// une expression régulière simple (a|b) prend sa première valeur.
func seriesDoublees(t *testing.T, exprs []string) []labels.Labels {
	t.Helper()
	vus := map[string]labels.Labels{}
	for _, expr := range exprs {
		e, err := alertes.AnalyserExpr(expr)
		if err != nil {
			t.Fatalf("%s : %v", expr, err)
		}
		parser.Inspect(e, func(n parser.Node, _ []parser.Node) error {
			vs, ok := n.(*parser.VectorSelector)
			if !ok {
				return nil
			}
			// Étiquettes de base : une machine, un invité Proxmox, un service.
			base := map[string]string{"instance": "a", "job": "node", "id": "100", "nom": "vm", "type": "lxc", "name": "x.service"}
			for _, m := range vs.LabelMatchers {
				switch m.Type {
				case labels.MatchEqual:
					base[m.Name] = m.Value
				case labels.MatchRegexp:
					v := strings.SplitN(strings.Trim(m.Value, "()"), "|", 2)[0]
					if m.Matches(v) {
						base[m.Name] = v
					}
				}
			}
			for _, d := range []string{"1", "2"} {
				b := labels.NewBuilder(labels.EmptyLabels())
				for k, v := range base {
					b.Set(k, v)
				}
				b.Set("doublon", d)
				ls := b.Labels()
				vus[ls.String()] = ls
			}
			return nil
		})
	}
	cles := make([]string, 0, len(vus))
	for k := range vus {
		cles = append(cles, k)
	}
	sort.Strings(cles)
	out := make([]labels.Labels, 0, len(cles))
	for _, k := range cles {
		out = append(out, vus[k])
	}
	return out
}

// lecteurDoublons : base de mesures remplie de ces séries (valeur 1, un
// point toutes les 2 min sur 26 h : fenêtres [1d], offset et rate servis).
func lecteurDoublons(t *testing.T, series []labels.Labels) *alertes.Lecteur {
	t.Helper()
	o := tsdb.DefaultOptions()
	db, err := tsdb.Open(t.TempDir(), nil, nil, o, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	app := db.Appender(context.Background())
	for _, ls := range series {
		for ts := t0.Add(-26 * time.Hour); !ts.After(t0); ts = ts.Add(2 * time.Minute) {
			if _, err := app.Append(0, ls, ts.UnixMilli(), 1); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := app.Commit(); err != nil {
		t.Fatal(err)
	}
	return alertes.NouveauLecteur(db, nil)
}

func TestReglesRobustesAuxDoublons(t *testing.T) {
	regles := append(alertes.ReglesMirador(), Regles()...)
	exprs := make([]string, 0, len(regles))
	for _, r := range regles {
		exprs = append(exprs, r.Expr)
	}
	lect := lecteurDoublons(t, seriesDoublees(t, exprs))
	for _, r := range regles {
		if _, err := lect.Instant(context.Background(), r.Expr, t0); err != nil {
			t.Errorf("règle %s en erreur avec une série en double : %v\n%s", r.Nom, err, r.Expr)
		}
	}
}

// Le cas du rapport : deux seuils pour la même machine (étiquette en plus).
// L'ancienne forme était en erreur ; la nouvelle donne le même résultat
// qu'avec un seul seuil.
func TestSeuilDuModeleEnDouble(t *testing.T) {
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
	for _, i := range []string{"a", "b"} {
		ecrire(25, "__name__", "node_filesystem_avail_bytes", "instance", i, "mountpoint", "/", "fstype", "ext4")
		ecrire(100, "__name__", "node_filesystem_size_bytes", "instance", i, "mountpoint", "/", "fstype", "ext4")
	}
	// Machine a : seuil normal. Machine b : seuil doublé par un faux envoi.
	ecrire(30, "__name__", "mirador_modele_parametre", "instance", "a", "modele", "linux", "parametre", "disque_libre_pct")
	ecrire(30, "__name__", "mirador_modele_parametre", "instance", "b", "modele", "linux", "parametre", "disque_libre_pct")
	ecrire(30, "__name__", "mirador_modele_parametre", "instance", "b", "modele", "linux", "parametre", "disque_libre_pct", "x", "1")
	if err := app.Commit(); err != nil {
		t.Fatal(err)
	}
	lect := alertes.NouveauLecteur(db, nil)
	m := Trouver("linux")
	var regle RegleModele
	for _, r := range m.Regles {
		if strings.Contains(r.Expr, "$disque_libre_pct") {
			regle = r
		}
	}
	if regle.Nom == "" {
		t.Fatal("règle du disque introuvable dans le modèle linux")
	}
	ancienne := reRefParametre.ReplaceAllStringFunc(regle.Expr, func(ref string) string {
		return `on (instance) group_left () mirador_modele_parametre{modele="linux", parametre="` + ref[1:] + `"}`
	})
	if _, err := lect.Instant(context.Background(), ancienne, t0); err == nil {
		t.Fatal("l'ancienne forme devait être en erreur (sinon le test ne prouve rien)")
	}
	ech, err := lect.Instant(context.Background(), m.ExprRegle(regle), t0)
	if err != nil {
		t.Fatalf("nouvelle forme en erreur : %v", err)
	}
	if len(ech) != 2 {
		t.Fatalf("attendu une alerte par machine : %v", ech)
	}
	for _, e := range ech {
		if e.Valeur != 25 || e.Etiquettes.Get("mountpoint") != "/" {
			t.Errorf("résultat changé : %v", e)
		}
	}
}
