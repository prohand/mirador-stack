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
	"github.com/prometheus/prometheus/tsdb"

	"mirador-stack/internal/server/alertes"
)

func TestClasseChaleur(t *testing.T) {
	for v, attendu := range map[float64]string{0: "ch-0", 19.9: "ch-0", 20: "ch-1", 45: "ch-2", 60: "ch-3", 80: "ch-4", 90: "ch-5", 100: "ch-5", math.NaN(): "ch-vide"} {
		if g := classeChaleur(v); g != attendu {
			t.Errorf("classeChaleur(%v) = %s, attendu %s", v, g, attendu)
		}
	}
}

// Plus gourmands (page machine), carte de chaleur (vue d'ensemble) et
// écran mural (SPEC § 31.3).
func TestGourmandsChaleurMur(t *testing.T) {
	b := nouveauBanc(t)
	db, err := tsdb.Open(t.TempDir(), nil, nil, tsdb.DefaultOptions(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	m := b.h.maintenant()
	app := db.Appender(context.Background())
	k := 0.0
	for ts := m.Add(-25 * time.Hour); !ts.After(m); ts = ts.Add(time.Minute) {
		k += 60
		ajout := func(inst string, v float64, l ...string) {
			_, _ = app.Append(0, labels.FromStrings(append([]string{"instance", inst}, l...)...), ts.UnixMilli(), v)
		}
		// web1 : 2 cœurs, 50 % d'inactivité ; nas : 95 % d'inactivité.
		for _, cpu := range []string{"0", "1"} {
			ajout("web1", k*0.5, "__name__", "node_cpu_seconds_total", "cpu", cpu, "mode", "idle")
			ajout("nas", k*0.95, "__name__", "node_cpu_seconds_total", "cpu", cpu, "mode", "idle")
		}
		ajout("web1", 8e9, "__name__", "node_memory_MemTotal_bytes")
		ajout("web1", 2e9, "__name__", "node_memory_MemAvailable_bytes")
		ajout("nas", 8e9, "__name__", "node_memory_MemTotal_bytes")
		ajout("nas", 7e9, "__name__", "node_memory_MemAvailable_bytes")
		// Conteneurs de web1 : postgres prend un cœur (50 % de la machine).
		for nom, part := range map[string]float64{"postgres": 1, "traefik": 0.1, "appli": 0.4} {
			ajout("web1", k*part, "__name__", "node_docker_container_cpu_seconds_total", "name", nom)
			ajout("web1", part*2e9, "__name__", "node_docker_container_memory_bytes", "name", nom)
		}
	}
	_ = app.Commit()
	b.c.lecteur = alertes.NouveauLecteur(db, nil)
	for _, nom := range []string{"web1", "nas"} {
		if _, err := b.c.db.Exec(`INSERT INTO machines(nom, statut, type, cree_le, vu_le) VALUES (?, 'approuvee', 'agent', 1, ?)`, nom, m.Unix()); err != nil {
			t.Fatal(err)
		}
	}
	c, _ := b.premierAdmin("admin", mdpAdmin)

	_, corps, _ := b.get(c, "/machines/web1")
	g := section(corps, `id="gourmands"`)
	for _, attendu := range []string{"Les plus gourmands", "<td>postgres</td><td class=\"nombre\">50,0 %</td>", "<td>postgres</td><td class=\"nombre\">1,9 Gio · 25 %</td>"} {
		if !strings.Contains(g, attendu) {
			t.Errorf("plus gourmands : %q absent de %s", attendu, g)
		}
	}
	if strings.Index(g, "postgres") > strings.Index(g, "appli") || strings.Index(g, "appli") > strings.Index(g, "traefik") {
		t.Error("plus gourmands : ordre décroissant attendu")
	}
	if _, corps, _ = b.get(c, "/machines/nas"); strings.Contains(corps, `id="gourmands"`) {
		t.Error("plus gourmands sans conteneurs")
	}

	_, corps, _ = b.get(c, "/")
	ch := section(corps, `id="chaleur"`)
	for _, attendu := range []string{"Le parc sur 24 h", "<h3>Processeur</h3>", "<h3>Mémoire</h3>", `<a href="/machines/nas" title="nas">nas</a>`, `data-nom="web1" data-texte="web1   "`, "data-chaleur-plus",
		`class="ch-2" title="web1 · `, "· 50 %", `<td class="ch-max">75 %</td>`, `class="ch-0" title="nas · `, `href="/mur"`} {
		if !strings.Contains(ch+corps, attendu) {
			t.Errorf("carte de chaleur : %q absent de %s", attendu, ch)
		}
	}
	// Les plus chargées en haut (web1 : 50 % ; nas : 5 %), dans les deux cartes.
	if strings.Index(ch, `data-nom="web1"`) > strings.Index(ch, `data-nom="nas"`) {
		t.Error("carte de chaleur : web1, plus chargée, devrait être avant nas")
	}
	if n := strings.Count(ch, `title="web1 · `); n != 48 {
		t.Errorf("carte de chaleur : %d cases pour web1 (48 attendues : 24 h × 2 mesures)", n)
	}

	_, corps, _ = b.get(c, "/mur")
	for _, attendu := range []string{`<meta http-equiv="refresh" content="30">`, `data-theme="sombre"`, `class="ecran-mural"`, "<b>2</b><span>machines</span>",
		"Tout va bien", `<b>web1</b>`, "Quitter l'écran mural"} {
		if !strings.Contains(corps, attendu) {
			t.Errorf("écran mural : %q absent", attendu)
		}
	}
	if strings.Contains(corps, `class="barre"`) {
		t.Error("écran mural avec le menu")
	}
}

// section : de motif à la fin de sa carte.
func section(s, motif string) string {
	i := strings.Index(s, motif)
	if i < 0 {
		return "(absent)"
	}
	j := strings.Index(s[i:], "</section>")
	if j < 0 {
		return s[i:]
	}
	return s[i : i+j]
}

// Carte de chaleur : tri par pic, sans données en bas, coupure à 100 comptée.
func TestOrdonnerChaleur(t *testing.T) {
	v := vueChaleur{}
	for i := 0; i < 105; i++ {
		v.Lignes = append(v.Lignes, ligneChaleur{Nom: fmt.Sprintf("m%03d", i), pic: float64(i % 50)})
	}
	v.Lignes = append(v.Lignes, ligneChaleur{Nom: "a-sans-donnees", pic: math.NaN()})
	ordonnerChaleur(&v)
	if len(v.Lignes) != 100 || v.Cachees != 6 {
		t.Fatalf("%d lignes, %d cachées", len(v.Lignes), v.Cachees)
	}
	if v.Lignes[0].Nom != "m049" || v.Lignes[1].Nom != "m099" || v.Lignes[0].pic != 49 {
		t.Errorf("tête : %s, %s", v.Lignes[0].Nom, v.Lignes[1].Nom)
	}
	for _, l := range v.Lignes {
		if l.Nom == "a-sans-donnees" {
			t.Error("machine sans données gardée devant des machines chargées")
		}
	}
}
