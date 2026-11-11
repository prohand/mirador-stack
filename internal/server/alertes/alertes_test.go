// SPDX-License-Identifier: GPL-3.0-or-later

package alertes

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/tsdb"

	"mirador-stack/internal/server/base"
)

// --- Aides ---------------------------------------------------------------------------

func baseTest(t testing.TB) *sql.DB {
	t.Helper()
	d := t.TempDir()
	db, err := base.Ouvrir(filepath.Join(d, "mirador.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := base.Migrer(context.Background(), db, filepath.Join(d, "copies")); err != nil {
		t.Fatal(err)
	}
	return db
}

func tsdbTest(t testing.TB) *tsdb.DB {
	t.Helper()
	o := tsdb.DefaultOptions()
	o.OutOfOrderTimeWindow = (24 * time.Hour).Milliseconds()
	db, err := tsdb.Open(t.TempDir(), nil, nil, o, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// ecrirePoints : une série, une valeur par pas sur [debut, fin].
func ecrirePoints(t testing.TB, db *tsdb.DB, etq labels.Labels, debut, fin time.Time, pas time.Duration, v func(time.Time) float64) {
	t.Helper()
	app := db.Appender(context.Background())
	for ts := debut; !ts.After(fin); ts = ts.Add(pas) {
		if _, err := app.Append(0, etq, ts.UnixMilli(), v(ts)); err != nil {
			t.Fatal(err)
		}
	}
	if err := app.Commit(); err != nil {
		t.Fatal(err)
	}
}

type recepteurTest struct {
	mu  sync.Mutex
	evs []Evenement
}

func (r *recepteurTest) Recevoir(evs []Evenement) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.evs = append(r.evs, evs...)
}

func (r *recepteurTest) vider() []Evenement {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.evs
	r.evs = nil
	return e
}

// --- Langage ---------------------------------------------------------------------------

func TestLangageRefus(t *testing.T) {
	cas := map[string]string{
		"fonction non permise":   `info(node_load5)`,
		"@":                      `node_load5 @ 1700000000`,
		"décalage négatif":       `node_load5 offset -5m`,
		"matrice":                `node_load5[5m]`,
		"syntaxe":                `rate(node_cpu_seconds_total[1m]`,
		"chaîne":                 `"texte"`,
		"histogramme non permis": `histogram_quantile(0.9, rate(x_bucket[5m]))`,
	}
	for nom, expr := range cas {
		if _, err := AnalyserExpr(expr); err == nil {
			t.Errorf("%s : %q accepté", nom, expr)
		}
	}
	if _, err := AnalyserExpr(strings.Repeat("a", TailleMaxExpr+1)); err == nil {
		t.Error("expression trop longue acceptée")
	}
	r := Regle{Nom: "Long", Expr: `max_over_time(x[8d]) > 0`, Gravite: Info}
	r.Defauts()
	if _, err := r.Valider(); err == nil || !strings.Contains(err.Error(), "7 jours") {
		t.Errorf("fenêtre de 8 j acceptée : %v", err)
	}
}

func TestValidationRegle(t *testing.T) {
	ok := Regle{Nom: "Ok", Expr: "up == 0", Gravite: Critique}
	ok.Defauts()
	if _, err := ok.Valider(); err != nil {
		t.Fatal(err)
	}
	mauvaises := []func(r *Regle){
		func(r *Regle) { r.Nom = "nom avec espace" },
		func(r *Regle) { r.Gravite = "none" },
		func(r *Regle) { r.Intervalle = Duree(time.Second) },
		func(r *Regle) { r.Pour = Duree(8 * 24 * time.Hour) },
		func(r *Regle) { r.Etiquettes = map[string]string{"severity": "x"} },
		func(r *Regle) { r.Etiquettes = map[string]string{"__x": "y"} },
		func(r *Regle) { r.Resume = "{{ $labels.instance " },
		func(r *Regle) { r.Resume = `{{ query "up" }}` }, // aucune requête depuis un modèle
	}
	for i, f := range mauvaises {
		r := ok
		f(&r)
		if _, err := r.Valider(); err == nil {
			t.Errorf("cas %d accepté", i)
		}
	}
}

func TestModeles(t *testing.T) {
	cas := []struct {
		modele  string
		valeur  float64
		attendu string
	}{
		{"{{ $labels.instance }} à {{ $value | humanize }}%", 12.3456, "pc1 à 12.35%"},
		{"{{ $value | humanizePercentage }}", 0.934, "93.4%"},
		{"{{ $value | humanizeDuration }}", 3725, "1h 2m 5s"},
		{"{{ $value | humanize }}", 1.5e9, "1.5G"},
		{"{{ $labels.absente }}x", 0, "x"},
	}
	for _, c := range cas {
		m, err := compilerModele(c.modele)
		if err != nil {
			t.Fatal(err)
		}
		if got := remplir(m, c.modele, map[string]string{"instance": "pc1"}, c.valeur); got != c.attendu {
			t.Errorf("%q : %q, attendu %q", c.modele, got, c.attendu)
		}
	}
	// Texte produit trop long : le modèle brut est rendu.
	m, _ := compilerModele(`{{ range $k, $v := $labels }}{{ $v }}{{ end }}`)
	long := map[string]string{"a": strings.Repeat("x", 3000)}
	if got := remplir(m, "brut", long, 0); got != "brut" {
		t.Errorf("texte trop long : %q", got[:20])
	}
}

// --- Import des règles actuelles ------------------------------------------------------

func TestImportRegLesActuelles(t *testing.T) {
	total := map[string]int{}
	var tout []Regle
	for _, f := range []string{"system.yml", "equipements.yml"} {
		fh, err := os.Open(filepath.Join("testdata", "regles", f))
		if err != nil {
			t.Fatal(err)
		}
		rap, err := Importer(fh, f)
		fh.Close()
		if err != nil {
			t.Fatalf("%s : %v", f, err)
		}
		var b bytes.Buffer
		rap.Ecrire(&b)
		t.Log("\n" + b.String())
		for k, v := range rap.Compter() {
			total[k] += v
		}
		tout = append(tout, rap.Regles...)
	}
	// 70 règles : Watchdog remplacée par le heartbeat, SyslogMuet importée
	// désactivée (règle d'enregistrement), les 68 autres exactes.
	if total[Exact] != 68 || total[Impossible] != 2 || total[Approche] != 0 {
		t.Errorf("classements : %v", total)
	}
	if len(tout) != 69 {
		t.Errorf("%d règles à enregistrer, 69 attendues", len(tout))
	}
	// Toutes s'évaluent sans erreur (base vide).
	db := baseTest(t)
	maintenant := time.Now()
	if _, err := Enregistrer(context.Background(), db, tout, "test", maintenant); err != nil {
		t.Fatal(err)
	}
	m, err := Nouveau(context.Background(), Options{DB: db, Donnees: tsdbTest(t)})
	if err != nil {
		t.Fatal(err)
	}
	m.EvaluerTout(context.Background(), maintenant)
	for _, s := range m.Sante() {
		if s.Erreur != "" {
			t.Errorf("%s : %s", s.Nom, s.Erreur)
		}
	}
	if n := len(m.Sante()); n != 68 {
		t.Errorf("%d règles actives chargées, 68 attendues", n)
	}
	// Export puis réimport au format mirador : identique.
	var b bytes.Buffer
	if err := Exporter(&b, tout); err != nil {
		t.Fatal(err)
	}
	rap, err := Importer(&b, "export.yml")
	if err != nil {
		t.Fatal(err)
	}
	if rap.Format != "mirador" || len(rap.Regles) != len(tout) {
		t.Fatalf("réimport : %s, %d règles", rap.Format, len(rap.Regles))
	}
	n, err := Enregistrer(context.Background(), db, rap.Regles, "test", maintenant)
	if err != nil || n != 0 {
		t.Errorf("réimport identique : %d changement(s), %v", n, err)
	}
}

func TestImportRefus(t *testing.T) {
	for nom, txt := range map[string]string{
		"inconnu":          "a: 1\n",
		"champ inconnu":    "groups:\n  - name: g\n    rules:\n      - alert: X\n        expr: up\n        inconnu: 1\n",
		"version":          "version: 2\nregles: []\n",
		"mirador invalide": "version: 1\nregles:\n  - nom: X\n    expr: 'info(up)'\n    gravite: warning\n",
	} {
		if _, err := Importer(strings.NewReader(txt), "f.yml"); err == nil {
			t.Errorf("%s accepté", nom)
		}
	}
	// Règle vmalert invalide : rapportée, le reste est importé.
	rap, err := Importer(strings.NewReader(`groups:
  - name: g
    rules:
      - alert: Bonne
        expr: up == 0
        labels: { severity: critical }
      - alert: Mauvaise
        expr: info(up)
      - record: x:y
        expr: up
`), "f.yml")
	if err != nil {
		t.Fatal(err)
	}
	if n := rap.Compter(); n[Exact] != 1 || n[Impossible] != 2 || len(rap.Regles) != 1 {
		t.Errorf("rapport : %v, %d règles", n, len(rap.Regles))
	}
}

// --- Moteur ----------------------------------------------------------------------------

func TestCycleDeVie(t *testing.T) {
	ctx := context.Background()
	db := baseTest(t)
	donnees := tsdbTest(t)
	t0 := time.Now().Truncate(time.Minute).Add(-time.Hour)
	regle := Regle{Nom: "CpuEleve", Expr: `cpu > 90`, Pour: Duree(2 * time.Minute), Gravite: Avertissement,
		Intervalle: Duree(30 * time.Second), Active: true, Resume: "CPU à {{ $value }} sur {{ $labels.instance }}"}
	if _, err := Enregistrer(ctx, db, []Regle{regle}, "test", t0); err != nil {
		t.Fatal(err)
	}
	// CPU à 95 de t0+5 min à t0+15 min, 10 sinon.
	pc1 := labels.FromStrings("__name__", "cpu", "instance", "pc1")
	ecrirePoints(t, donnees, pc1, t0, t0.Add(30*time.Minute), 30*time.Second, func(ts time.Time) float64 {
		if d := ts.Sub(t0); d >= 5*time.Minute && d < 15*time.Minute {
			return 95
		}
		return 10
	})
	rec := &recepteurTest{}
	horloge := t0
	opt := Options{DB: db, Donnees: donnees, Recepteur: rec, Maintenant: func() time.Time { return horloge }}
	m, err := Nouveau(ctx, opt)
	if err != nil {
		t.Fatal(err)
	}
	m.debut = t0.Add(-time.Hour) // pas de délai de démarrage dans ce test
	evaluer := func(d time.Duration) []Evenement {
		horloge = t0.Add(d)
		m.EvaluerTout(ctx, horloge)
		return rec.vider()
	}
	if evs := evaluer(4 * time.Minute); len(evs) != 0 || len(m.EnCours()) != 0 {
		t.Fatalf("avant : %v", evs)
	}
	if evs := evaluer(5 * time.Minute); len(evs) != 0 || len(m.EnCours()) != 1 || m.EnCours()[0].Etat != EnAttente {
		t.Fatalf("en attente : %v %v", evs, m.EnCours())
	}
	if evs := evaluer(6*time.Minute + 30*time.Second); len(evs) != 0 {
		t.Fatalf("trop tôt : %v", evs)
	}
	evs := evaluer(7 * time.Minute)
	if len(evs) != 1 || evs[0].Type != "debut" || evs[0].Alerte.Resume != "CPU à 95 sur pc1" ||
		evs[0].Alerte.Etiquettes.Get("severity") != "warning" || evs[0].Alerte.Machine() != "pc1" {
		t.Fatalf("début : %+v", evs)
	}
	// Redémarrage du serveur : l'alerte est relue, pas de nouvelle notification.
	m2, err := Nouveau(ctx, opt)
	if err != nil {
		t.Fatal(err)
	}
	m2.debut = m.debut
	m = m2
	if ec := m.EnCours(); len(ec) != 1 || ec[0].Etat != Active || !ec[0].Debut.Equal(t0.Add(5*time.Minute)) {
		t.Fatalf("après redémarrage : %+v", ec)
	}
	if evs := evaluer(10 * time.Minute); len(evs) != 0 {
		t.Fatalf("après redémarrage : %v", evs)
	}
	// Fin (lookback de 5 min : la dernière valeur à 95 est à t0+14m30).
	evs = evaluer(15 * time.Minute)
	if len(evs) != 1 || evs[0].Type != "fin" {
		t.Fatalf("fin : %+v", evs)
	}
	h, err := Historique(ctx, db, 10)
	if err != nil || len(h) != 1 || h[0].Etat != Resolue || h[0].ValeurMax != 95 || h[0].Valeur != 10 && h[0].Valeur != 95 {
		t.Fatalf("historique : %+v %v", h, err)
	}
	if d := h[0].Fin.Sub(h[0].ActiveLe); d != 8*time.Minute {
		t.Errorf("durée en alerte : %s", d)
	}
}

func TestEnAttenteEffaceeEtGrace(t *testing.T) {
	ctx := context.Background()
	db := baseTest(t)
	donnees := tsdbTest(t)
	t0 := time.Now().Truncate(time.Minute).Add(-time.Hour)
	if _, err := Enregistrer(ctx, db, []Regle{
		{Nom: "Bref", Expr: `x > 1`, Pour: Duree(5 * time.Minute), Gravite: Info, Active: true},
		{Nom: "Immediat", Expr: `x > 1`, Gravite: Critique, Active: true},
	}, "test", t0); err != nil {
		t.Fatal(err)
	}
	ecrirePoints(t, donnees, labels.FromStrings("__name__", "x"), t0, t0.Add(2*time.Minute), 30*time.Second,
		func(time.Time) float64 { return 2 })
	rec := &recepteurTest{}
	m, err := Nouveau(ctx, Options{DB: db, Donnees: donnees, Recepteur: rec, Maintenant: func() time.Time { return t0 }})
	if err != nil {
		t.Fatal(err)
	}
	m.debut = t0 // délai de démarrage de 3 min à partir de t0
	m.EvaluerTout(ctx, t0.Add(time.Minute))
	if evs := rec.vider(); len(evs) != 0 {
		t.Fatalf("pendant le délai de démarrage : %v", evs)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM alertes`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("%d ligne(s) en attente, %v", n, err)
	}
	m.EvaluerTout(ctx, t0.Add(3*time.Minute))
	if evs := rec.vider(); len(evs) != 1 || evs[0].Alerte.Regle != "Immediat" {
		t.Fatalf("après le délai : %v", evs)
	}
	// Plus de données : « Bref » (jamais en alerte) est effacée, « Immediat » résolue.
	m.EvaluerTout(ctx, t0.Add(10*time.Minute))
	if evs := rec.vider(); len(evs) != 1 || evs[0].Type != "fin" {
		t.Fatalf("fin : %v", evs)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM alertes`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("%d ligne(s), 1 attendue (%v)", n, err)
	}
}

func TestGarderEtRechargement(t *testing.T) {
	ctx := context.Background()
	db := baseTest(t)
	donnees := tsdbTest(t)
	t0 := time.Now().Truncate(time.Minute).Add(-time.Hour)
	if _, err := Enregistrer(ctx, db, []Regle{{Nom: "Clignote", Expr: `x > 1`, Garder: Duree(10 * time.Minute),
		Gravite: Info, Active: true}}, "test", t0); err != nil {
		t.Fatal(err)
	}
	ecrirePoints(t, donnees, labels.FromStrings("__name__", "x"), t0, t0.Add(time.Minute), 30*time.Second,
		func(time.Time) float64 { return 2 })
	rec := &recepteurTest{}
	m, err := Nouveau(ctx, Options{DB: db, Donnees: donnees, Recepteur: rec})
	if err != nil {
		t.Fatal(err)
	}
	m.debut = t0.Add(-time.Hour)
	m.EvaluerTout(ctx, t0.Add(time.Minute))
	m.EvaluerTout(ctx, t0.Add(8*time.Minute)) // plus de données, mais « garder » 10 min
	if evs := rec.vider(); len(evs) != 1 || len(m.EnCours()) != 1 {
		t.Fatalf("garder : %v", evs)
	}
	// Désactivation : alerte résolue au rechargement.
	if err := Activer(ctx, db, "Clignote", false, "test", t0); err != nil {
		t.Fatal(err)
	}
	if err := m.Recharger(ctx); err != nil {
		t.Fatal(err)
	}
	if evs := rec.vider(); len(evs) != 1 || evs[0].Type != "fin" || len(m.Sante()) != 0 {
		t.Fatalf("désactivation : %v", evs)
	}
}

func TestLag(t *testing.T) {
	ctx := context.Background()
	donnees := tsdbTest(t)
	t0 := time.Now().Truncate(time.Minute).Add(-2 * time.Hour)
	ecrirePoints(t, donnees, labels.FromStrings("__name__", "node_uname_info", "instance", "a"), t0, t0.Add(time.Hour),
		30*time.Second, func(time.Time) float64 { return 1 })
	ecrirePoints(t, donnees, labels.FromStrings("__name__", "node_uname_info", "instance", "b"), t0, t0.Add(90*time.Minute),
		30*time.Second, func(time.Time) float64 { return 1 })
	m, err := Nouveau(ctx, Options{DB: baseTest(t), Donnees: donnees})
	if err != nil {
		t.Fatal(err)
	}
	res, err := m.requete(ctx, `min by (instance) (lag(node_uname_info[1d])) > 60`, t0.Add(90*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].etq.Get("instance") != "a" || res[0].valeur != 1800 {
		t.Fatalf("lag : %+v", res)
	}
}

func TestDefautsPosesUneFois(t *testing.T) {
	ctx := context.Background()
	db := baseTest(t)
	n, err := PoserDefauts(ctx, db, time.Now())
	if err != nil || n != len(ReglesMirador()) {
		t.Fatalf("%d posées, %v", n, err)
	}
	if err := Supprimer(ctx, db, "OomKiller", "test", time.Now()); err != nil {
		t.Fatal(err)
	}
	if n, err := PoserDefauts(ctx, db, time.Now()); err != nil || n != 0 {
		t.Fatalf("2e fois : %d posées, %v", n, err)
	}
}

func TestComparaisonVmalert(t *testing.T) {
	ctx := context.Background()
	db := baseTest(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/alerts" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, `{"status":"success","data":{"alerts":[
			{"name":"CpuEleve","state":"firing","labels":{"instance":"srv-test"}},
			{"name":"DisqueFaible","state":"firing","labels":{"instance":"srv-test","mountpoint":"/"}},
			{"name":"MemoireFaible","state":"pending","labels":{"instance":"srv-test"}},
			{"name":"Watchdog","state":"firing","labels":{}},
			{"name":"CpuEleve","state":"firing","labels":{"instance":"autre-vm"}}]}}`)
	}))
	defer srv.Close()
	vm, err := LireVmalert(ctx, srv.URL)
	if err != nil || len(vm) != 5 {
		t.Fatalf("lecture : %v %d", err, len(vm))
	}
	mk := func(regle string) Alerte {
		return Alerte{Regle: regle, Etat: Active, Etiquettes: labels.FromStrings("instance", "srv-test-mirador")}
	}
	r := ReglagesVmalertDefaut()
	ecarts := Comparer(r, vm, []Alerte{mk("CpuEleve"), mk("OomKiller")}, map[string]bool{"srv-test-mirador": true})
	types := map[string]string{}
	for _, e := range ecarts {
		types[e.Regle] = e.Type
	}
	if len(ecarts) != 3 || types["CpuEleve"] != "commune" || types["DisqueFaible"] != "manquante" || types["OomKiller"] != "en_trop" {
		t.Fatalf("écarts : %+v", ecarts)
	}
	t0 := time.Now()
	if err := NoterEcarts(ctx, db, ecarts, t0); err != nil {
		t.Fatal(err)
	}
	if err := NoterEcarts(ctx, db, ecarts, t0.Add(5*time.Minute)); err != nil { // même écart : pas de doublon
		t.Fatal(err)
	}
	if err := NoterEcarts(ctx, db, nil, t0.Add(10*time.Minute)); err != nil { // fini
		t.Fatal(err)
	}
	eps, err := ListerEpisodes(ctx, db, t0.Add(-time.Hour))
	if err != nil || len(eps) != 2 || eps[0].Fin.Sub(eps[0].Debut) != 10*time.Minute {
		t.Fatalf("épisodes : %+v %v", eps, err)
	}
	if err := (&ReglagesVmalert{Actif: true}).Valider(); err == nil {
		t.Error("actif sans url accepté")
	}
}
