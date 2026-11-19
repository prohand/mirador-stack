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
	"mirador-stack/internal/server/flotte"
	"mirador-stack/internal/server/modeles"
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

// Grande courbe d'une alerte de modèle (retour du 19/11) : seuil par
// machine (série job="mirador") trouvé malgré job="node" sur l'alerte,
// données pour graphes.js (axes, unité °C, seuil).
func TestCourbeAlerteModele(t *testing.T) {
	b := nouveauBanc(t)
	db, err := tsdb.Open(t.TempDir(), nil, nil, tsdb.DefaultOptions(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	maintenant := b.h.maintenant()
	app := db.Appender(context.Background())
	for i, ts := 0, maintenant.Add(-6*time.Hour); !ts.After(maintenant); i, ts = i+1, ts.Add(5*time.Minute) {
		_, _ = app.Append(0, labels.FromStrings("__name__", "smartctl_device_temperature", "instance", "nas", "job", "node", "device", "nvme0", "temperature_type", "current"), ts.UnixMilli(), float64(40+i%25))
		_, _ = app.Append(0, labels.FromStrings("__name__", "mirador_modele_parametre", "instance", "nas", "job", "mirador", "modele", "physique", "parametre", "disque_temp_max"), ts.UnixMilli(), 55)
	}
	_ = app.Commit()
	b.c.lecteur = alertes.NouveauLecteur(db, nil)
	expr := `smartctl_device_temperature{temperature_type="current"} > on (instance) group_left () mirador_modele_parametre{modele="physique", parametre="disque_temp_max"}`
	al := labels.FromStrings("alertname", "PhysiqueDisqueChaud", "instance", "nas", "job", "node", "device", "nvme0", "temperature_type", "current")
	j := b.c.courbeAlerteJSON(context.Background(), expr, "Physique disque chaud", al, maintenant, 6*time.Hour)
	if !strings.Contains(j, `"seuil":55`) || !strings.Contains(j, `"unite":"celsius"`) || !strings.Contains(j, `"series":[{"nom":"Physique disque chaud"`) {
		t.Fatalf("courbe JSON : %.300s", j)
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
		`cat-audit regles`, "Passkey ajoutée au compte admin", `<span class="pastille erreur">Sensible</span>`, "Aujourd&#39;hui · ", " masquée"} {
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
	for _, attendu := range []string{`<a href="/machines/vm-docker">vm-docker</a>`, "2 alertes", `action="/alertes/7"`, `value="acquitter"`, `name="duree" value="1h">1 h`,
		`href="/alertes/7#graphique"`, "<polyline", `data-depuis="1"`, `<span class="pastille ok">Acquittée</span>`, `name="retour" value="/alertes?vue=machine"`} {
		if !strings.Contains(corps, attendu) {
			t.Errorf("bloc en cours : %q absent", attendu)
		}
	}
	if strings.Contains(corps, `action="/alertes/8" class="enligne"`) {
		t.Error("« Acquitter » proposé sur une alerte déjà acquittée")
	}
}

// Frise 24 h / 7 jours et règles les plus bruyantes.
func TestFriseAlertes(t *testing.T) {
	b := nouveauBanc(t)
	c, _ := b.premierAdmin("admin", mdpAdmin)
	m := b.h.maintenant()
	ms := func(d time.Duration) int64 { return m.Add(-d).UnixMilli() }
	for _, a := range [][]any{
		// regle, instance, gravité, état, active_le, fin
		{"CpuEleve", "web1", "warning", "resolue", ms(5 * time.Hour), ms(4 * time.Hour)},
		{"CpuEleve", "web1", "warning", "resolue", ms(3 * time.Hour), ms(2 * time.Hour)},
		{"CpuEleve", "web2", "warning", "resolue", ms(90 * time.Minute), ms(80 * time.Minute)},
		{"DisqueFaible", "nas", "critical", "active", ms(30 * time.Hour), int64(0)},
		{"Ancienne", "web1", "info", "resolue", ms(72 * time.Hour), ms(71 * time.Hour)},
	} {
		if _, err := b.c.db.Exec(`INSERT INTO alertes(regle, etiquettes, gravite, etat, debut, active_le, fin) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			a[0], `{"alertname":"`+a[0].(string)+`","instance":"`+a[1].(string)+`"}`, a[2], a[3], a[4], a[4], a[5]); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := alertes.Enregistrer(context.Background(), b.c.db, []alertes.Regle{{Nom: "CpuEleve", Groupe: "hote", Intervalle: alertes.Duree(time.Minute),
		Expr: "up == 0", Gravite: alertes.Avertissement, Active: true, Origine: "vmalert:system.yml"}}, "admin", m); err != nil {
		t.Fatal(err)
	}
	_, corps, _ := b.get(c, "/alertes?hp=24h")
	for _, attendu := range []string{`class="frise-graduations g8"`, `href="/alertes?machine=web1#liste-historique">web1</a> <small>2</small>`,
		`class="g-critical"`, `x="0.0"`, `href="/regles/modifier/CpuEleve">Régler la règle</a>`, "<td class=\"nombre\">3</td><td class=\"nombre\">2</td>"} {
		if !strings.Contains(corps, attendu) {
			t.Errorf("frise 24 h : %q absent", attendu)
		}
	}
	frise := func(s string) string {
		return s[strings.Index(s, `id="frise"`):strings.Index(s, `id="liste-historique"`)]
	}
	if strings.Contains(frise(corps), "Ancienne") {
		t.Error("alerte hors période sur la frise 24 h")
	}
	if _, corps, _ = b.get(c, "/alertes?hp=7j"); !strings.Contains(corps, `class="frise-graduations g7"`) || !strings.Contains(frise(corps), "Ancienne") {
		t.Error("frise 7 jours")
	}
}

// Condition « en attente » (pas encore une alerte) : hors des compteurs et
// de la liste des alertes, dans un bloc à part avec le temps restant.
func TestAlertesEnAttente(t *testing.T) {
	actives, attente := separerAttente([]resumeAlerte{
		{ID: 1, Regle: "CpuEleve", Gravite: "warning", Etat: "active"},
		{ID: 2, Regle: "MisesAJourSecuriteEnAttente", Gravite: "warning", Etat: "en_attente", Machine: "pbs", Reste: "dans 28 h 43"},
	})
	if len(actives) != 1 || len(attente) != 1 || attente[0].ID != 2 {
		t.Fatalf("séparation : %v / %v", actives, attente)
	}
	b := nouveauBanc(t)
	w := httptest.NewRecorder()
	b.c.pages.executer(w, http.StatusOK, "alertes-fragment", Page{D: donneesEnCours{Vue: "machine", Attente: attente}})
	corps := w.Body.String()
	for _, attendu := range []string{"Aucune alerte en cours.", "En attente</strong> (1)", "MisesAJourSecuriteEnAttente", "dans 28 h 43", `data-nb-alertes="0"`} {
		if !strings.Contains(corps, attendu) {
			t.Errorf("bloc en attente : %q absent", attendu)
		}
	}
}

// Page d'une machine (SPEC § 29) : « En ce moment » avec les seuils du
// modèle, graphiques avant la surveillance, seuil du modèle dans la courbe.
func TestPageMachineRessources(t *testing.T) {
	b := nouveauBanc(t)
	db, err := tsdb.Open(t.TempDir(), nil, nil, tsdb.DefaultOptions(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	m := b.h.maintenant()
	app := db.Appender(context.Background())
	for k, ts := 0, m.Add(-30*time.Minute); !ts.After(m); k, ts = k+1, ts.Add(30*time.Second) {
		ajout := func(v float64, l ...string) {
			_, _ = app.Append(0, labels.FromStrings(append([]string{"instance", "web1"}, l...)...), ts.UnixMilli(), v)
		}
		ajout(float64(k)*30*0.05, "__name__", "node_cpu_seconds_total", "cpu", "0", "mode", "idle") // 5 % d'inactivité → 95 % utilisé
		ajout(8e9, "__name__", "node_memory_MemTotal_bytes")
		ajout(4e9, "__name__", "node_memory_MemAvailable_bytes")
		ajout(100e9, "__name__", "node_filesystem_size_bytes", "mountpoint", "/", "fstype", "ext4")
		ajout(20e9, "__name__", "node_filesystem_avail_bytes", "mountpoint", "/", "fstype", "ext4")
		ajout(0, "__name__", "node_memory_SwapTotal_bytes")
		ajout(0.5, "__name__", "node_load5")
		ajout(float64(m.Unix()), "__name__", "node_time_seconds")
		ajout(float64(m.Add(-5*24*time.Hour).Unix()), "__name__", "node_boot_time_seconds")
	}
	_ = app.Commit()
	b.c.lecteur = alertes.NouveauLecteur(db, nil)
	ctx := context.Background()
	if _, err := b.c.db.Exec(`INSERT INTO machines(nom, statut, type, cree_le, vu_le) VALUES ('web1', 'approuvee', 'agent', 1, ?)`, m.Unix()); err != nil {
		t.Fatal(err)
	}
	id, _ := flotte.IDMachine(ctx, b.c.db, "web1")
	_ = modeles.Definir(ctx, b.c.db, modeles.PorteeMachine(id), "linux", modeles.EtatActif, map[string]float64{"cpu_pct": 80}, nil, "admin", m)
	c, _ := b.premierAdmin("admin", mdpAdmin)
	_, corps, _ := b.get(c, "/machines/web1")
	for _, attendu := range []string{`id="maintenant"`, "<small>CPU</small><b>95 %</b>", `class="jauge erreur"`, "seuil 80 %",
		"<small>Mémoire</small><b>50 %</b>", "<small>Disque le plus plein</small><b>80 %</b>", "pas de swap", "<small>Démarrée depuis</small><b>5 j</b>",
		`data-regrouper-vides`, `href="#surveillance-modeles"`} {
		if !strings.Contains(corps, attendu) {
			t.Errorf("page machine : %q absent", attendu)
		}
	}
	if g, s := strings.Index(corps, `id="graphiques"`), strings.Index(corps, `id="surveillance-modeles"`); g < 0 || s < 0 || g > s {
		t.Error("graphiques pas avant la surveillance")
	}
	_, js, _ := b.get(c, "/donnees/courbe?instance=web1&panneau=1.0&periode=6h&tableau=serveurs-linux")
	if !strings.Contains(js, `"seuil":80`) {
		t.Errorf("seuil du modèle absent de la courbe : %.300s", js)
	}
	if ancre("Services et sécurité") != "r-services-et-securite" {
		t.Errorf("ancre : %s", ancre("Services et sécurité"))
	}
}

// Prises en charge (SPEC § 30) : hors des alertes à traiter et des
// compteurs, dans un bloc avec qui, quand, jusqu'à quand et « Reprendre ».
func TestPrisesEnCharge(t *testing.T) {
	aTraiter, prises := separerPrises([]resumeAlerte{
		{ID: 1, Regle: "CpuEleve", Gravite: "critical", Etat: "active"},
		{ID: 2, Regle: "DisqueFaible", Gravite: "warning", Etat: "active", Acquittee: true, AcquitteePar: "web:admin", AcquitteeLe: "14:05", AcquitteeJusqua: "15/11 18:05"},
	})
	if len(aTraiter) != 1 || len(prises) != 1 {
		t.Fatalf("séparation : %v / %v", aTraiter, prises)
	}
	b := nouveauBanc(t)
	w := httptest.NewRecorder()
	b.c.pages.executer(w, http.StatusOK, "alertes-fragment", Page{Admin: true, CSRF: "j", D: donneesEnCours{Vue: "machine", Prises: prises, Retour: "/alertes"}})
	corps := w.Body.String()
	for _, attendu := range []string{"Prises en charge</strong> (1)", "par <strong>web:admin</strong> à 14:05", "jusqu'au 15/11 18:05", `value="reprendre"`, `data-nb-alertes="0"`} {
		if !strings.Contains(corps, attendu) {
			t.Errorf("bloc prises en charge : %q absent", attendu)
		}
	}
}

// Écran mural (SPEC § 37) : rappel des alertes prises en charge ; plus de
// « Tout va bien » quand il en reste.
func TestMurPrisesEnCharge(t *testing.T) {
	b := nouveauBanc(t)
	prises := []resumeAlerte{{ID: 2, Regle: "DisqueFaible", Gravite: "warning", Machine: "nas", Etat: "active", Acquittee: true,
		AcquitteePar: "web:admin", AcquitteeLe: "14:05", AcquitteeJusqua: "15/11 18:05", Depuis: 1}}
	rendre := func(d donneesAccueil) string {
		w := httptest.NewRecorder()
		b.c.pages.executer(w, http.StatusOK, "ecran", Page{D: donneesMur{donneesAccueil: d, Heure: "12:00"}})
		return w.Body.String()
	}
	corps := rendre(donneesAccueil{Prises: prises})
	for _, attendu := range []string{"Rien à traiter", "Prises en charge (1)", "<b>nas</b> Disque faible", "par web:admin", "jusqu'au 15/11 18:05"} {
		if !strings.Contains(corps, attendu) {
			t.Errorf("écran mural : %q absent", attendu)
		}
	}
	if strings.Contains(corps, "Tout va bien") {
		t.Error("« Tout va bien » avec une alerte prise en charge")
	}
	// Avec une alerte à traiter : les deux blocs.
	corps = rendre(donneesAccueil{Prises: prises, Alertes: []resumeAlerte{{Regle: "CpuEleve", Gravite: "critical", Machine: "web1"}}})
	if !strings.Contains(corps, "Alertes en cours") || !strings.Contains(corps, "Prises en charge (1)") {
		t.Error("écran mural : alertes et prises en charge attendues ensemble")
	}
	if corps = rendre(donneesAccueil{}); !strings.Contains(corps, "Tout va bien") || strings.Contains(corps, "Prises en charge") {
		t.Error("écran mural calme")
	}
}
