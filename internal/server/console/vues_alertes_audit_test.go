// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/tsdb"

	"mirador-stack/internal/server/alertes"
)

// Partie gauche (valeur suivie) et droite (seuil) d'une règle, y compris
// une règle de modèle ; « a or b » : pas de courbe.
func TestDecomposerRegle(t *testing.T) {
	for expr, attendu := range map[string]string{
		`node_load1 > 2`: `node_load1|2`,
		`(node_docker_container_running{restart_policy=~"always"} == 0) and on (instance) mirador_machine_modele{modele="docker"}`: `node_docker_container_running{restart_policy=~"always"}|0`,
		`rate(x[5m]) * 8 > on (instance) group_left () mirador_modele_parametre{parametre="p"}`:                                    `rate(x[5m]) * 8|mirador_modele_parametre{parametre="p"}`,
	} {
		g, d, ok := decomposerRegle(expr)
		if !ok || g.String()+"|"+d.String() != attendu {
			t.Errorf("%s → %v %v", expr, g, d)
		}
	}
	if _, _, ok := decomposerRegle(`a == 11 or a == 12`); ok {
		t.Error("« or » découpé")
	}
}

// Mini-courbe tirée des vraies données de la machine de l'alerte, avec le
// seuil ; pas de courbe pour une autre machine.
func TestCourbeAlerte(t *testing.T) {
	b := nouveauBanc(t)
	db, err := tsdb.Open(t.TempDir(), nil, nil, tsdb.DefaultOptions(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	maintenant := b.h.maintenant()
	app := db.Appender(context.Background())
	for i, ts := 0, maintenant.Add(-6*time.Hour); !ts.After(maintenant); i, ts = i+1, ts.Add(5*time.Minute) {
		_, _ = app.Append(0, labels.FromStrings("__name__", "node_load1", "instance", "web1"), ts.UnixMilli(), float64(i%10))
	}
	_ = app.Commit()
	b.c.lecteur = alertes.NouveauLecteur(db, nil)
	ctx := context.Background()
	svg := string(b.c.courbeAlerte(ctx, "node_load1 > 4", labels.FromStrings("alertname", "Charge", "instance", "web1"), alertes.Critique, maintenant, 6*time.Hour, 120, 28))
	if !strings.Contains(svg, "<polyline") || !strings.Contains(svg, `class="seuil"`) || !strings.Contains(svg, "var(--erreur)") {
		t.Fatalf("mini-courbe : %s", svg)
	}
	if s := b.c.courbeAlerte(ctx, "node_load1 > 4", labels.FromStrings("alertname", "Charge", "instance", "autre"), alertes.Critique, maintenant, 6*time.Hour, 120, 28); s != "" {
		t.Error("courbe d'une autre machine")
	}
}

// Page Alertes et fragment rechargé : compteurs, choix de la vue.
func TestPageAlertesVue(t *testing.T) {
	b := nouveauBanc(t)
	c, _ := b.premierAdmin("admin", mdpAdmin)
	_, corps, _ := b.get(c, "/alertes?vue=regle")
	for _, attendu := range []string{`class="resume-alertes"`, `href="/alertes?vue=regle" class="actif"`, `data-rafraichir="/fragments/alertes?vue=regle"`, `data-nb-alertes="0"`} {
		if !strings.Contains(corps, attendu) {
			t.Errorf("page Alertes : %s absent", attendu)
		}
	}
	code, frag, _ := b.get(c, "/fragments/alertes?vue=nimporte")
	if code != http.StatusOK || !strings.Contains(frag, `data-nb-alertes="0"`) || strings.Contains(frag, "<html") {
		t.Errorf("fragment : %d", code)
	}
}

// Journal d'audit lisible : catégories, phrases, changements, sensibles,
// actions automatiques masquées, export CSV sans formule.
func TestAuditLisible(t *testing.T) {
	b := nouveauBanc(t)
	c, _ := b.premierAdmin("admin", mdpAdmin)
	maintenant := b.h.maintenant().Unix()
	for _, l := range [][]any{
		{maintenant, "admin", "regle", "CpuEleve", `{"nom":"CpuEleve","pour":"5m"}`, `{"nom":"CpuEleve","pour":"10m"}`, "192.0.2.1"},
		{maintenant, "admin", "utilisateur_passkey_ajoutee", "admin", "", "Téléphone", ""},
		{maintenant, "mirador", "regle", "Automatique", "", `{"nom":"Automatique"}`, ""},
		{maintenant, "admin", "reglages_interface", "", "", "=CMD()", ""},
	} {
		if _, err := b.c.db.Exec(`INSERT INTO audit(le, qui, action, objet, avant, apres, adresse) VALUES (?, ?, ?, ?, ?, ?, ?)`, l...); err != nil {
			t.Fatal(err)
		}
	}
	_, corps, _ := b.get(c, "/audit")
	for _, attendu := range []string{"Règle CpuEleve créée ou modifiée", `href="/regles/modifier/CpuEleve"`, "pour : <del>5m</del> → <ins>10m</ins>",
		`cat-audit regles`, "Passkey ajoutée au compte admin", `<span class="pastille erreur">sensible</span>`, "Aujourd&#39;hui", "action(s) automatique(s) masquée(s)"} {
		if !strings.Contains(corps, attendu) {
			t.Errorf("audit : %q absent", attendu)
		}
	}
	if strings.Contains(corps, "Règle Automatique") {
		t.Error("action automatique affichée par défaut")
	}
	if _, corps, _ = b.get(c, "/audit?auto=1&categorie=regles"); !strings.Contains(corps, "Règle Automatique") || strings.Contains(corps, "Passkey ajoutée") {
		t.Error("filtres catégorie / automatiques")
	}
	if _, corps, _ = b.get(c, "/audit?sensibles=1"); strings.Contains(corps, "Règle CpuEleve") || !strings.Contains(corps, "Passkey ajoutée") {
		t.Error("filtre sensibles")
	}
	code, csvTexte, ent := b.get(c, "/audit.csv?periode=tout")
	if code != http.StatusOK || !strings.HasPrefix(ent.Get("Content-Type"), "text/csv") || !strings.Contains(csvTexte, "Règle CpuEleve créée ou modifiée") ||
		!strings.Contains(csvTexte, "'=CMD()") {
		t.Errorf("export CSV : %d %q", code, csvTexte)
	}
}

// Bloc « en cours » avec des alertes : groupes, courbe, actions (admin).
func TestGabaritAlertesEnCours(t *testing.T) {
	b := nouveauBanc(t)
	d := donneesEnCours{Vue: "machine", Critiques: 1, Avertissements: 1, Total: 2, Retour: "/alertes?vue=machine",
		Groupes: []groupeAlertes{{Titre: "vm-docker", Lien: "/machines/vm-docker", Gravites: []string{"critical", "warning"}, Alertes: []vueAlerte{
			{resumeAlerte: resumeAlerte{ID: 7, Regle: "DockerConteneurArrete", Gravite: "critical", Machine: "vm-docker", Resume: "Conteneur postgres arrêté", Etat: "active", Depuis: 1},
				Courbe: svgCourbe([]int64{0, 1000}, []float64{1, 0}, time.UnixMilli(0), time.UnixMilli(1000), 0.5, true, "critical", 120, 28)},
			{resumeAlerte: resumeAlerte{ID: 8, Regle: "LinuxMemoireFaible", Gravite: "warning", Machine: "vm-docker", Acquittee: true, Depuis: 2}},
		}}}}
	w := httptest.NewRecorder()
	b.c.pages.executer(w, http.StatusOK, "alertes-fragment", Page{CSRF: "jeton", Admin: true, D: d})
	corps := w.Body.String()
	for _, attendu := range []string{`<a href="/machines/vm-docker">vm-docker</a>`, "2 alerte(s)", `action="/alertes/7"`, `value="acquitter"`, `name="duree" value="1h">1 h`,
		`href="/alertes/7#graphique"`, "<polyline", `data-depuis="1"`, `<span class="pastille ok">Acquittée</span>`, `name="retour" value="/alertes?vue=machine"`} {
		if !strings.Contains(corps, attendu) {
			t.Errorf("bloc en cours : %q absent", attendu)
		}
	}
	if strings.Contains(corps, `action="/alertes/8" class="enligne"`) {
		t.Error("« Acquitter » proposé sur une alerte déjà acquittée")
	}
}
