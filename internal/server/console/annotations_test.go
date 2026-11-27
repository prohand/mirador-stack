// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/tsdb"

	"mirador-stack/internal/server/alertes"
)

// « plein dans X j » : seulement sous 30 jours, avec une pente qui descend.
func TestPleinDans(t *testing.T) {
	for j, attendu := range map[float64]string{
		0.4: "plein dans moins d'un jour", 12.4: "plein dans 12 j", 29.6: "plein dans 30 j",
		30: "", 200: "", -3: "", math.NaN(): "", math.Inf(1): "",
	} {
		if g := pleinDans(j); g != attendu {
			t.Errorf("pleinDans(%v) = %q, attendu %q", j, g, attendu)
		}
	}
	m := pleinDansMachine(map[string]float64{"web1|/": 20, "web1|/data": 5, "nas|/": 12})
	if m["web1"] != 5 || m["nas"] != 12 {
		t.Errorf("plus pressé par machine : %v", m)
	}
}

// Courbes d'une machine : alertes, redémarrage, nouvelle version de
// l'agent, changement fait dans l'interface, âge ; disque qui se remplit.
func TestAnnotationsEtDisques(t *testing.T) {
	b := nouveauBanc(t)
	db, err := tsdb.Open(t.TempDir(), nil, nil, tsdb.DefaultOptions(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	m := b.h.maintenant()
	boot := m.Add(-3 * time.Hour).Truncate(time.Minute)
	app := db.Appender(context.Background())
	// 8 jours, un point toutes les 10 min (un point toutes les 30 s la dernière heure).
	for ts := m.Add(-8 * 24 * time.Hour); !ts.After(m); {
		ajout := func(v float64, l ...string) {
			_, _ = app.Append(0, labels.FromStrings(append([]string{"instance", "web1"}, l...)...), ts.UnixMilli(), v)
		}
		age := m.Sub(ts).Hours()
		// Place libre : 50 Go il y a 8 jours, 2 Go perdus par jour.
		ajout(1e12, "__name__", "node_filesystem_size_bytes", "mountpoint", "/data", "fstype", "ext4")
		ajout(34e9+2e9*age/24, "__name__", "node_filesystem_avail_bytes", "mountpoint", "/data", "fstype", "ext4")
		// Disque stable : pas de prévision.
		ajout(100e9, "__name__", "node_filesystem_size_bytes", "mountpoint", "/", "fstype", "ext4")
		ajout(60e9, "__name__", "node_filesystem_avail_bytes", "mountpoint", "/", "fstype", "ext4")
		if ts.Before(boot) {
			ajout(float64(m.Add(-10*24*time.Hour).Unix()), "__name__", "node_boot_time_seconds")
		} else {
			ajout(float64(boot.Unix()), "__name__", "node_boot_time_seconds")
		}
		version := "1.4.0"
		if ts.After(m.Add(-2 * time.Hour)) {
			version = "1.5.0"
		}
		if ts.Before(m.Add(-90 * time.Second)) {
			ajout(1, "__name__", "mirador_agent_info", "version", version)
		}
		if age > 1 {
			ts = ts.Add(10 * time.Minute)
		} else {
			ts = ts.Add(30 * time.Second)
		}
	}
	_ = app.Commit()
	b.c.lecteur = alertes.NouveauLecteur(db, nil)
	if _, err := b.c.db.Exec(`INSERT INTO machines(nom, statut, type, cree_le, vu_le) VALUES ('web1', 'approuvee', 'agent', 1, ?)`, m.Unix()); err != nil {
		t.Fatal(err)
	}
	for _, a := range [][]any{
		{"CpuEleve", "web1", "warning", m.Add(-4 * time.Hour).UnixMilli(), m.Add(-3 * time.Hour).UnixMilli()},
		{"Autre", "web2", "critical", m.Add(-4 * time.Hour).UnixMilli(), int64(0)},
	} {
		if _, err := b.c.db.Exec(`INSERT INTO alertes(regle, etiquettes, gravite, etat, debut, active_le, fin) VALUES (?, ?, ?, 'resolue', ?, ?, ?)`,
			a[0], `{"alertname":"`+a[0].(string)+`","instance":"`+a[1].(string)+`"}`, a[2], a[3], a[3], a[4]); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := b.c.db.Exec(`INSERT INTO audit(le, qui, action, objet) VALUES (?, 'web:admin', 'machine_renommee', 'web1')`, m.Add(-time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	c, _ := b.premierAdmin("admin", mdpAdmin)

	// Page machine : courbes annotées, disque /data plein dans 17 j.
	_, corps, _ := b.get(c, "/machines/web1")
	if !strings.Contains(corps, "annoter=1") {
		t.Error("courbes de la page machine sans annotations")
	}
	if !strings.Contains(corps, `<span class="plein-dans" title="D'après la pente des 7 derniers jours">plein dans 17 j</span>`) {
		t.Errorf("prévision du disque absente : %s", extrait(corps, "plein-dans"))
	}
	if strings.Count(corps, `class="plein-dans`) != 1 {
		t.Error("prévision affichée pour un disque stable")
	}

	_, js, _ := b.get(c, "/donnees/courbe?instance=web1&panneau=1.0&periode=6h&tableau=serveurs-linux&annoter=1")
	for _, attendu := range []string{`"nom":"CpuEleve","gravite":"warning"`, `"nom":"Redémarrage"`, `"nom":"Agent 1.5.0"`,
		`"derniere":` + itoa(m.Add(-90*time.Second).Unix()-30)} {
		if !strings.Contains(js, attendu) {
			t.Errorf("annotations : %q absent de %.600s", attendu, js)
		}
	}
	if strings.Contains(js, "Autre") || strings.Contains(js, "Agent 1.4.0") {
		t.Errorf("annotation en trop : %.600s", js)
	}
	if !strings.Contains(js, `"t":`+itoa(boot.Unix())+`,"nom":"Redémarrage"`) {
		t.Errorf("heure du redémarrage : %.600s", js)
	}
	if !strings.Contains(js, `Machine`) {
		t.Errorf("changement de l'audit absent : %.600s", js)
	}
	// Sans « annoter » (tableaux) : rien.
	_, js, _ = b.get(c, "/donnees/courbe?instance=web1&panneau=1.0&periode=6h&tableau=serveurs-linux")
	if strings.Contains(js, "annotations") {
		t.Error("annotations hors page machine")
	}

	// Vue d'ensemble : le disque le plus pressé de la machine.
	_, corps, _ = b.get(c, "/")
	if !strings.Contains(corps, ">plein dans 17 j</span>") {
		t.Errorf("prévision absente du parc : %s", extrait(corps, "col-disque"))
	}
}

func extrait(s, motif string) string {
	i := strings.Index(s, motif)
	if i < 0 {
		return "(absent)"
	}
	return s[max(0, i-200):min(len(s), i+400)]
}

// Semaine dernière (une seule courbe) et aires empilées (SPEC § 31).
func TestComparaisonEtEmpile(t *testing.T) {
	b := nouveauBanc(t)
	db, err := tsdb.Open(t.TempDir(), nil, nil, tsdb.DefaultOptions(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	m := b.h.maintenant()
	app := db.Appender(context.Background())
	k := 0.0
	for ts := m.Add(-8 * 24 * time.Hour); !ts.After(m); ts = ts.Add(time.Minute) {
		k += 60
		ajout := func(v float64, l ...string) {
			_, _ = app.Append(0, labels.FromStrings(append([]string{"instance", "web1"}, l...)...), ts.UnixMilli(), v)
		}
		ajout(k*0.9, "__name__", "node_cpu_seconds_total", "cpu", "0", "mode", "idle")
		ajout(k*0.07, "__name__", "node_cpu_seconds_total", "cpu", "0", "mode", "user")
		ajout(k*0.03, "__name__", "node_cpu_seconds_total", "cpu", "0", "mode", "system")
		ajout(8e9, "__name__", "node_memory_MemTotal_bytes")
		ajout(5e9, "__name__", "node_memory_MemAvailable_bytes")
		ajout(3e9, "__name__", "node_memory_Cached_bytes")
		ajout(1e8, "__name__", "node_memory_Buffers_bytes")
	}
	_ = app.Commit()
	b.c.lecteur = alertes.NouveauLecteur(db, nil)
	c, _ := b.premierAdmin("admin", mdpAdmin)
	base := "/donnees/courbe?instance=web1&periode=6h&tableau=serveurs-linux&panneau="
	_, js, _ := b.get(c, base+"1.0&comparer=1")
	if !strings.Contains(js, `"precedent":[`) {
		t.Errorf("semaine dernière absente : %.300s", js)
	}
	_, js, _ = b.get(c, base+"1.0")
	if strings.Contains(js, "precedent") {
		t.Error("semaine dernière sans la demander")
	}
	// CPU par mode : empilé, une courbe par mode (hors idle).
	_, js, _ = b.get(c, base+"1.3&comparer=1")
	if !strings.Contains(js, `"empile":true`) || !strings.Contains(js, `"nom":"user"`) || !strings.Contains(js, `"nom":"system"`) ||
		strings.Contains(js, `"nom":"idle"`) || strings.Contains(js, "precedent") {
		t.Errorf("CPU par mode : %.400s", js)
	}
	// Mémoire : utilisée 3 Go + cache 3,1 Go + libre 1,9 Go = 8 Go.
	_, js, _ = b.get(c, base+"1.4")
	for _, attendu := range []string{`"nom":"Utilisée","v":[3000000000,`, `"nom":"Cache","v":[3100000000,`, `"nom":"Libre","v":[1900000000,`} {
		if !strings.Contains(js, attendu) {
			t.Errorf("répartition de la mémoire : %q absent de %s", attendu, js[strings.Index(js, `"series"`):min(len(js), strings.Index(js, `"series"`)+600)])
		}
	}
}

// Régression : une requête sur 7 jours est servie par la base agrégée (en
// retard) ; la place libre actuelle doit venir de la base brute, sinon
// « plein dans » n'apparaît jamais.
func TestPrevisionDeuxBases(t *testing.T) {
	ouvrir := func() *tsdb.DB {
		db, err := tsdb.Open(t.TempDir(), nil, nil, tsdb.DefaultOptions(), nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return db
	}
	brut, agrege := ouvrir(), ouvrir()
	m := time.Now()
	ecrire := func(db *tsdb.DB, debut, fin time.Time, pas time.Duration) {
		app := db.Appender(context.Background())
		for ts := debut; !ts.After(fin); ts = ts.Add(pas) {
			libre := 34e9 + 2e9*m.Sub(ts).Hours()/24 // 2 Go perdus par jour
			_, _ = app.Append(0, labels.FromStrings("__name__", "node_filesystem_avail_bytes", "instance", "web1", "mountpoint", "/data", "fstype", "ext4"), ts.UnixMilli(), libre)
		}
		_ = app.Commit()
	}
	ecrire(brut, m.Add(-2*time.Hour), m, 30*time.Second)                          // brute : récente seulement
	ecrire(agrege, m.Add(-8*24*time.Hour), m.Add(-20*time.Minute), 5*time.Minute) // agrégée : 20 min de retard
	// Comme metriques.Interroger : limite calculée au moment de la lecture.
	c := &Console{lecteur: alertes.NouveauLecteur(storage.QueryableFunc(func(mint, maxt int64) (storage.Querier, error) {
		if mint >= time.Now().Add(-7*24*time.Hour).UnixMilli() {
			return brut.Querier(mint, maxt)
		}
		return agrege.Querier(mint, maxt)
	}), nil), cache: cacheRequetes{tout: map[string]entreeRequete{}}}
	p := c.previsionsDisques(context.Background(), `instance="web1"`, m)
	if j := p["web1|/data"]; j < 16.5 || j > 17.5 {
		t.Errorf("plein dans %v j, 17 attendus (%v)", j, p)
	}
}

// Retour du 26/11 (sup) : place perdue dans la semaine, mais libérée le
// dernier jour : pas de prévision.
func TestPrevisionDisqueLibereHier(t *testing.T) {
	db, err := tsdb.Open(t.TempDir(), nil, nil, tsdb.DefaultOptions(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	m := time.Now()
	app := db.Appender(context.Background())
	for ts := m.Add(-7 * 24 * time.Hour); !ts.After(m); ts = ts.Add(5 * time.Minute) {
		age := m.Sub(ts).Hours()
		libre := 30e9 - 2e9*(7*24-age)/24 // 2 Go perdus par jour…
		if age < 20 {
			libre = 20e9 + 2.4e9 + 0.1e9*(20-age)/24 // … puis 2,4 Go libérés, et ça remonte
		}
		_, _ = app.Append(0, labels.FromStrings("__name__", "node_filesystem_avail_bytes", "instance", "sup", "mountpoint", "/", "fstype", "ext4"), ts.UnixMilli(), libre)
	}
	_ = app.Commit()
	c := &Console{lecteur: alertes.NouveauLecteur(db, nil), cache: cacheRequetes{tout: map[string]entreeRequete{}}}
	if p := c.previsionsDisques(context.Background(), `instance="sup"`, m); len(p) != 0 {
		t.Errorf("prévision malgré la place libérée : %v", p)
	}
}

// Grand parc (SPEC § 141) : 700 machines, 2 100 disques, plus que les 2 000 séries
// qu'une requête peut rendre. Avant : erreur, prévision absente sans bruit.
func TestPrevisionParcPlusDe2000Disques(t *testing.T) {
	db, err := tsdb.Open(t.TempDir(), nil, nil, tsdb.DefaultOptions(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	m := time.Now()
	app := db.Appender(context.Background())
	for i := 0; i < 2100; i++ { // 700 machines × 3 disques
		lb := labels.FromStrings("__name__", "node_filesystem_avail_bytes", "instance", fmt.Sprintf("m%04d", i/3), "mountpoint", fmt.Sprintf("/d%d", i%3), "fstype", "ext4")
		for _, h := range []float64{166, 100, 20, 2, 0} {
			ts := m.Add(-time.Duration(h * float64(time.Hour)))
			_, _ = app.Append(0, lb, ts.UnixMilli(), 34e9+2e9*h/24)
		}
	}
	if err := app.Commit(); err != nil {
		t.Fatal(err)
	}
	c := &Console{lecteur: alertes.NouveauLecteur(db, nil), cache: cacheRequetes{tout: map[string]entreeRequete{}}}
	p := c.previsionsDisques(context.Background(), `instance=~".*"`, m)
	if len(p) != 2100 {
		t.Fatalf("%d prévisions sur 2 100", len(p))
	}
	if j := p["m0007|/d1"]; j < 16.5 || j > 17.5 {
		t.Errorf("plein dans %v j, 17 attendus", j)
	}
}

// Découpage du parc : chaque paquet garde au plus seriesParPaquet disques.
func TestPaquetsDisques(t *testing.T) {
	if p := paquetsDisques(nil, nil); len(p) != 1 || p[0] != "" {
		t.Errorf("petit parc : %v", p)
	}
	var ms []alertes.Echantillon
	for i := 0; i < 1500; i++ {
		ms = append(ms, alertes.Echantillon{Etiquettes: labels.FromStrings("instance", fmt.Sprintf("srv-%04d.lan", i)), Valeur: 4})
	}
	p := paquetsDisques(ms, nil)
	if len(p) != 4 || !strings.Contains(p[0], `srv-0000\.lan|`) {
		t.Errorf("%d paquets : %.80s", len(p), p[0])
	}
}
