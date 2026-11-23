// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"strings"
	"testing"
	"time"

	"mirador-stack/internal/server/servicesmetier"
	"mirador-stack/internal/server/stockage"
	"mirador-stack/internal/server/stockage/journaux"
)

// Page Conteneurs (SPEC § 42.1, refonte § 123, maquette validée le 23/11) :
// bandeau des problèmes avec leur raison, 5 cases, groupes par machine et
// projet Compose (problèmes d'abord, sains repliés), vue Liste, filtres,
// petite courbe du processeur, image changée, application, compteur du menu.
func TestPageConteneurs(t *testing.T) {
	b := nouveauBanc(t)
	ctx := context.Background()
	m := b.h.maintenant()
	c := func(inst, nom, projet string, extra ...string) []string {
		return append([]string{"instance", inst, "name", nom, "project", projet}, extra...)
	}
	serie := func(nom string, etq []string) []string { return append([]string{"__name__", nom}, etq...) }
	marche := func(inst, nom, projet, etat, politique string) []string {
		return serie("node_docker_container_running", c(inst, nom, projet, "state", etat, "restart_policy", politique))
	}
	series := []serieTest{
		{marche("web1", "postgres", "site", "running", "always"), constante(1)},
		{marche("web1", "appli", "site", "running", "always"), constante(1)},
		{marche("web1", "cache", "site", "exited", "unless-stopped"), constante(0)},
		{marche("nas", "sauvegarde", "", "exited", "no"), constante(0)},
		{serie("node_docker_container_healthy", c("web1", "appli", "site")), constante(0)},
		{serie("node_docker_container_healthy", c("web1", "postgres", "site")), constante(1)},
		{serie("node_docker_container_health_info", c("web1", "appli", "site", "sortie", "connexion refusée (postgres:5432)", "code", "1")), constante(1)},
		{serie("node_docker_container_exit_code", c("web1", "cache", "site")), constante(137)},
		{serie("node_docker_container_exit_code", c("nas", "sauvegarde", "")), constante(0)},
		{serie("node_docker_container_info", c("web1", "postgres", "site", "image", "postgres:16.4")), constante(1)},
		{serie("node_docker_container_port", c("web1", "postgres", "site", "public", "5433", "prive", "5432", "proto", "tcp")), constante(1)},
		// postgres : un cœur entier (30 s de processeur toutes les 30 s).
		{serie("node_docker_container_cpu_seconds_total", c("web1", "postgres", "site")), func(k int) float64 { return float64(k) * 30 }},
		{serie("node_docker_container_memory_bytes", c("web1", "postgres", "site")), constante(1 << 30)},
		{serie("node_docker_container_memory_limit_bytes", c("web1", "postgres", "site")), constante(4 << 30)},
		{serie("node_docker_container_restart_count", c("web1", "appli", "site")), func(k int) float64 { return float64(k / 1000) }},
		{serie("node_docker_container_started_timestamp_seconds", c("web1", "postgres", "site")), constante(float64(m.Add(-3 * 24 * time.Hour).Unix()))},
	}
	// Projet « media » : 4 conteneurs sains, 2 montrés, 2 repliés.
	for _, n := range []string{"m1", "m2", "m3", "m4"} {
		series = append(series, serieTest{marche("web1", n, "media", "running", "always"), constante(1)})
	}
	b.metriquesSimulees(t, series)
	b.machinesApprouvees(t, "web1", "nas")
	if _, err := b.c.db.Exec(`INSERT INTO changements(machine, le, type, detail) VALUES ('web1', ?, 'conteneur', 'postgres recréé : image postgres:16.3 → postgres:16.4')`,
		m.Add(-2*24*time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	if err := servicesmetier.Enregistrer(ctx, b.c.db, []servicesmetier.Service{{Nom: "Site", Composants: []servicesmetier.Composant{
		{Sorte: servicesmetier.Conteneur, Nom: "web1/postgres", Cle: true}}}}, "admin", "", m); err != nil {
		t.Fatal(err)
	}
	cl, _ := b.premierAdmin("admin", mdpAdmin)
	_, corps, _ := b.get(cl, "/conteneurs")
	for _, attendu := range []string{"<h1>Conteneurs</h1>",
		// Bandeau : raison de chaque problème.
		"2 conteneurs à regarder", "non sain, test de santé en échec : « connexion refusée (postgres:5432) »",
		"arrêté alors qu&#39;il doit redémarrer (politique « unless-stopped »), code de sortie 137 (tué : mémoire ou arrêt forcé)",
		// Cases.
		"En marche", "<span> / 8</span>", "sur 2 machines · 2 projets", `class="kpi kpi-erreur"`, "cache (unless-stopped)", "Mémoire pleine (OOM)",
		// Groupes : projet, problèmes, application, proposition.
		`<span class="doux">· projet</span> site`, "1 non sain", "1 arrêté", `href="/applications/Site">application Site</a>`,
		`href="/applications?machine=web1&amp;proposer=media#editeur">+ créer l'application</a>`, "· hors projet Compose",
		"data-en-trop hidden", "+ 2 autres conteneurs sains · voir",
		// Lignes : image, image changée, erreur, processeur, mémoire, démarré, logs.
		"postgres:16.4 · <span class=\"neuf\">image changée il y a 2 j (postgres:16.3 → postgres:16.4)</span>",
		`<small class="err">code de sortie 137`, `<small class="err">connexion refusée (postgres:5432)</small>`,
		"100 %", `<svg class="etincelle"`, "1,0 Go / 4,0 Go</small>", "il y a 3 j", `href="/logs?container=postgres&amp;host=web1"`,
		`<span class="sans-test" title="Pas de test de santé (healthcheck) défini pour ce conteneur">—</span>`,
		// Filtres et vue Liste.
		`data-filtre="pb"`, "Sans test de santé <b>", `data-vue-ct="liste"`, `data-vue="projet"`,
	} {
		if !strings.Contains(corps, attendu) {
			t.Errorf("page des conteneurs : %q absent", attendu)
		}
	}
	// Problèmes en premier : le groupe « site » avant « media », appli et cache avant postgres.
	vueProjet := corps[strings.Index(corps, `data-vue-ct="projet"`):strings.Index(corps, `data-vue-ct="liste"`)]
	if strings.Index(vueProjet, "· projet</span> site") > strings.Index(vueProjet, "· projet</span> media") {
		t.Error("groupe avec problème pas en premier")
	}
	if strings.Index(vueProjet, "<strong>appli</strong>") > strings.Index(vueProjet, "<strong>postgres</strong>") ||
		strings.Index(vueProjet, "<strong>cache</strong>") > strings.Index(vueProjet, "<strong>postgres</strong>") {
		t.Error("problèmes pas en premier dans le groupe")
	}
	// Compteur du menu : appli + cache.
	if !strings.Contains(corps, `title="Conteneurs non sains ou arrêtés alors qu&#39;ils devraient redémarrer">2</span>`) {
		t.Error("compteur du menu absent ou faux")
	}
	// Lien depuis une page machine : ?machine=.
	if _, corps, _ = b.get(cl, "/conteneurs?machine=nas"); !strings.Contains(corps, "sur nas") || strings.Contains(corps, "<strong>postgres</strong>") {
		t.Error("filtre par machine")
	}
	// ?etat=problemes : filtre « Problèmes » choisi, lignes saines cachées d'office.
	_, corps, _ = b.get(cl, "/conteneurs?etat=problemes")
	if !strings.Contains(corps, `data-filtre-depart="pb"`) || !strings.Contains(corps, `data-filtre="pb" class="actif"`) {
		t.Error("filtre « problèmes » pas choisi")
	}
	if i := strings.Index(corps, "<strong>postgres</strong>"); i < 0 || !strings.Contains(corps[strings.LastIndex(corps[:i], "<tr"):i], "data-onglet-cache") {
		t.Error("postgres pas caché par le filtre « problèmes »")
	}
}

// Raison et ligne d'erreur d'un conteneur.
func TestRaisonConteneur(t *testing.T) {
	l := ligneConteneur{Nom: "x", ClasseEtat: "erreur", Politique: "always", CodeSortie: "1", OOM: true}
	if r := l.Raison(); r != "arrêté alors qu'il doit redémarrer (politique « always »), code de sortie 1 ; tué faute de mémoire (OOM)" {
		t.Errorf("raison : %q", r)
	}
	if e := (ligneConteneur{CodeSortie: "0"}).LigneErreur(); e != "" {
		t.Errorf("code 0 : %q", e)
	}
	if f := (ligneConteneur{EnMarche: true, Sante: "Sain"}).Filtres(); f != "" {
		t.Errorf("filtres d'un conteneur sain : %q", f)
	}
}

// Fiche d'un conteneur (SPEC § 123) : pastilles, bandeau « pourquoi » avec
// la sortie du test de santé et les dernières erreurs de ses logs, 4 cases,
// graphiques du tableau « conteneur » avec le choix de période, même projet.
func TestFicheConteneur(t *testing.T) {
	b := nouveauBanc(t)
	ctx := context.Background()
	m := b.h.maintenant()
	c := func(nom string, extra ...string) []string {
		return append([]string{"instance", "web1", "name", nom, "project", "site"}, extra...)
	}
	serie := func(nom string, etq []string) []string { return append([]string{"__name__", nom}, etq...) }
	b.metriquesSimulees(t, []serieTest{
		{serie("node_docker_container_running", c("appli", "state", "running", "restart_policy", "unless-stopped")), constante(1)},
		{serie("node_docker_container_running", c("postgres", "state", "running", "restart_policy", "always")), constante(1)},
		{serie("node_docker_container_healthy", c("appli")), constante(0)},
		{serie("node_docker_container_health_info", c("appli", "sortie", "curl: (7) connexion refusée", "code", "7")), constante(1)},
		{serie("node_docker_container_info", c("appli", "image", "ghcr.io/exemple/appli:2.4.1")), constante(1)},
		{serie("node_docker_container_info", c("postgres", "image", "postgres:16.4")), constante(1)},
		{serie("node_docker_container_port", c("appli", "public", "8080", "prive", "80", "proto", "tcp")), constante(1)},
		{serie("node_docker_container_cpu_seconds_total", c("appli")), func(k int) float64 { return float64(k) * 3 }},
		{serie("node_docker_container_restart_count", c("appli")), constante(2)},
		{serie("node_docker_container_memory_bytes", c("appli")), constante(900 << 20)},
		{serie("node_docker_container_memory_limit_bytes", c("appli")), constante(1 << 30)},
		{serie("node_docker_container_started_timestamp_seconds", c("appli")), constante(float64(m.Add(-3 * 24 * time.Hour).Unix()))},
	})
	b.machinesApprouvees(t, "web1")
	jx, err := journaux.Ouvrir(journaux.Options{Dossier: t.TempDir(), Maintenant: b.h.maintenant})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = jx.Fermer() })
	b.c.opt.Logs = jx
	ch := map[string]string{"host": "web1", "container": "appli"}
	_ = jx.Ecrire(ctx, []stockage.Ligne{
		{T: m.Add(-9 * time.Minute).UnixMilli(), Msg: "démarrage normal", Champs: ch},
		{T: m.Add(-8 * time.Minute).UnixMilli(), Msg: "ERROR database connection refused (postgres:5432)", Champs: ch},
		{T: m.Add(-7 * time.Minute).UnixMilli(), Msg: "requête servie", Champs: ch},
		{T: m.Add(-6 * time.Minute).UnixMilli(), Msg: "retrying in 30 s", Champs: map[string]string{"host": "web1", "container": "appli", "level": "warning"}},
		{T: m.Add(-5 * time.Minute).UnixMilli(), Msg: "ERROR autre conteneur", Champs: map[string]string{"host": "web1", "container": "postgres"}},
	})
	cl, _ := b.premierAdmin("admin", mdpAdmin)
	code, corps, _ := b.get(cl, "/conteneurs/web1/appli?periode=6h")
	if code != 200 {
		t.Fatalf("code %d", code)
	}
	for _, attendu := range []string{"<h1>appli</h1>", "En marche · Non sain",
		// Pastilles.
		"ghcr.io/exemple/appli:2.4.1", `projet <a href="/conteneurs?machine=web1">site</a>`, "redémarrage : unless-stopped", "démarré il y a 3 j", "port 8080 → 80",
		// Pourquoi : sortie du test, dernières erreurs (sans les lignes normales ni l'autre conteneur).
		"Test de santé en échec", "Dernière sortie du test : « curl: (7) connexion refusée »",
		"database connection refused (postgres:5432)", "<b>WARNING</b><span>retrying in 30 s</span>", "Tous les logs",
		// Cases.
		"d&#39;un cœur · max 24 h", "88 % de la limite (1,0 Go)", `class="kpi kpi-attention"`, "sur 7 j",
		// Graphiques comme ceux d'une machine, période choisie.
		`id="graphiques"`, "tableau=conteneur", `href="/conteneurs/web1/appli?periode=24h#graphiques"`, `periode=6h#graphiques" class="actif"`, "Processeur", "Mémoire",
		// Même projet.
		"Même projet : site", `href="/conteneurs/web1/postgres"`, "postgres:16.4",
	} {
		if !strings.Contains(corps, attendu) {
			t.Errorf("fiche du conteneur : %q absent", attendu)
		}
	}
	for _, absent := range []string{"démarrage normal", "ERROR autre conteneur", "requête servie"} {
		if strings.Contains(corps, absent) {
			t.Errorf("fiche du conteneur : %q ne devrait pas apparaître", absent)
		}
	}
	// Ordre chronologique des erreurs.
	if strings.Index(corps, "database connection refused") > strings.Index(corps, "retrying in 30 s") {
		t.Error("erreurs des logs pas dans l'ordre")
	}
}
