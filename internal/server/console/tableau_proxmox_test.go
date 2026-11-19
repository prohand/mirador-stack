// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/tsdb"

	"mirador-stack/internal/agent/distant"
	"mirador-stack/internal/server/alertes"
	"mirador-stack/internal/server/flotte"
	"mirador-stack/internal/server/modeles"
)

// Tableau « Proxmox » (SPEC § 23.7) : chaque requête s'évalue sans erreur
// sur des données d'agent simulées, et celles des invités trouvent des
// séries (nom joint par mirador_pve_invite_info, invité arrêté compris).
func TestTableauProxmox(t *testing.T) {
	o := tsdb.DefaultOptions()
	db, err := tsdb.Open(t.TempDir(), nil, nil, o, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	fin := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	debut := fin.Add(-time.Hour)
	app := db.Appender(context.Background())
	ecrire := func(v func(min float64) float64, paires ...string) {
		etq := labels.FromStrings(append(paires, "instance", "pve1", "job", "mirador")...)
		for ts := debut; !ts.After(fin); ts = ts.Add(30 * time.Second) {
			if _, err := app.Append(0, etq, ts.UnixMilli(), v(ts.Sub(debut).Minutes())); err != nil {
				t.Fatal(err)
			}
		}
	}
	fixe := func(x float64) func(float64) float64 { return func(float64) float64 { return x } }
	monte := func(par float64) func(float64) float64 { return func(m float64) float64 { return m * 60 * par } }
	for _, inv := range []struct{ id, typ, nom string }{{"114", "qemu", "web"}, {"200", "lxc", "pbs"}, {"300", "qemu", "arret"}} {
		ecrire(fixe(1), "__name__", "mirador_pve_invite_info", "id", inv.id, "type", inv.typ, "nom", inv.nom, "noeud", "pve1")
		ecrire(fixe(2), "__name__", "mirador_pve_invite_coeurs", "id", inv.id, "type", inv.typ)
		ecrire(fixe(4<<30), "__name__", "mirador_pve_invite_memoire_max_octets", "id", inv.id, "type", inv.typ)
		if inv.id == "300" {
			continue // arrêté : seulement la configuration
		}
		ecrire(fixe(1), "__name__", "mirador_pve_invite_en_marche", "id", inv.id, "type", inv.typ)
		ecrire(monte(0.5), "__name__", "mirador_pve_invite_cpu_secondes_total", "id", inv.id, "type", inv.typ)
		ecrire(fixe(2<<30), "__name__", "mirador_pve_invite_memoire_octets", "id", inv.id, "type", inv.typ)
		ecrire(monte(1e6), "__name__", "mirador_pve_invite_disque_lus_octets_total", "id", inv.id, "type", inv.typ)
		ecrire(monte(2e6), "__name__", "mirador_pve_invite_disque_ecrits_octets_total", "id", inv.id, "type", inv.typ)
		ecrire(monte(1e5), "__name__", "mirador_pve_invite_reseau_recus_octets_total", "id", inv.id, "type", inv.typ)
		ecrire(monte(1e5), "__name__", "mirador_pve_invite_reseau_envoyes_octets_total", "id", inv.id, "type", inv.typ)
		for _, rn := range [][2]string{{"cpu", "some"}, {"io", "full"}, {"memory", "full"}} {
			ecrire(monte(0.01), "__name__", "mirador_pve_invite_pression_secondes_total", "id", inv.id, "type", inv.typ, "ressource", rn[0], "niveau", rn[1])
		}
	}
	ecrire(monte(3e6), "__name__", "mirador_pve_vms_disque_lus_octets_total")
	ecrire(monte(3e6), "__name__", "mirador_pve_vms_disque_ecrits_octets_total")
	if err := app.Commit(); err != nil {
		t.Fatal(err)
	}

	// Règles Proxmox fournies : elles s'évaluent ; rien ne dépasse ici
	// (CPU 25 %, mémoire 50 %, liste présente). Sans le seuil : une valeur
	// par invité, avec son nom.
	lect := alertes.NouveauLecteur(db, nil)
	for _, r := range alertes.ReglesMirador() {
		if r.Groupe != "proxmox" {
			continue
		}
		ech, err := lect.Instant(context.Background(), r.Expr, fin)
		if err != nil || len(ech) != 0 {
			t.Errorf("règle %s : %v %v", r.Nom, ech, err)
		}
		if i := strings.LastIndex(r.Expr, " > "); i > 0 {
			sans, _ := lect.Instant(context.Background(), r.Expr[:i], fin)
			if len(sans) == 0 || sans[0].Etiquettes.Get("nom") == "" {
				t.Errorf("règle %s sans seuil : %v", r.Nom, sans)
			}
		}
	}

	tab := tableauxDepart["proxmox"]
	if tab == nil {
		t.Fatal("tableau proxmox absent")
	}
	vals := map[string]string{"node": "pve1"}
	for _, r := range tab.Rangees {
		for _, p := range r.Panneaux {
			for _, rq := range p.Requetes {
				expr := substituer(rq.Expr, vals, 30*time.Second, 6*time.Hour)
				ech, err := lect.Instant(context.Background(), expr, fin)
				if err != nil {
					t.Errorf("%s / %s : %v\n%s", r.Titre, p.Titre, err, expr)
					continue
				}
				if strings.Contains(rq.Expr, "mirador_pve") && len(ech) == 0 {
					t.Errorf("%s / %s (%s) : aucune série\n%s", r.Titre, p.Titre, rq.ID, expr)
				}
				// Panneaux des invités : le nom est joint.
				if strings.Contains(rq.Expr, "group_left(nom)") {
					for _, e := range ech {
						if e.Etiquettes.Get("nom") == "" {
							t.Errorf("%s / %s : nom manquant : %v", r.Titre, p.Titre, e.Etiquettes)
						}
					}
				}
				// Inventaire : 3 invités, l'arrêté à 0.
				if p.Titre == "Inventaire" && rq.ID == "A" {
					etats := map[string]float64{}
					for _, e := range ech {
						etats[e.Etiquettes.Get("nom")] = e.Valeur
					}
					if len(etats) != 3 || etats["arret"] != 0 || etats["web"] != 1 || etats["pbs"] != 1 {
						t.Errorf("inventaire, état : %v", etats)
					}
				}
				if p.Titre == "Invités arrêtés" && (len(ech) != 1 || ech[0].Valeur != 1) {
					t.Errorf("invités arrêtés : %v", ech)
				}
				if p.Titre == "Inventaire" && rq.ID == "B" {
					for _, e := range ech { // 0,5 s de CPU par seconde sur 2 cœurs = 25 %
						if e.Valeur < 24 || e.Valeur > 26 {
							t.Errorf("CPU %% : %v = %v", e.Etiquettes, e.Valeur)
						}
					}
				}
			}
		}
	}
}

// Application automatique des modèles détectés (demande du 14/11) : appliqué
// une fois, signalé, marqué « auto » ; décoché ensuite, il ne revient pas.
func TestModelesAuto(t *testing.T) {
	b := nouveauBanc(t)
	recus := b.ecouter()
	db, err := tsdb.Open(t.TempDir(), nil, nil, tsdb.DefaultOptions(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	maintenant := b.h.maintenant()
	app := db.Appender(context.Background())
	for ts := maintenant.Add(-5 * time.Minute); !ts.After(maintenant); ts = ts.Add(30 * time.Second) {
		_, _ = app.Append(0, labels.FromStrings("__name__", "node_docker_up", "instance", "web1"), ts.UnixMilli(), 1)
	}
	_ = app.Commit()
	b.c.lecteur = alertes.NouveauLecteur(db, nil)
	ctx := context.Background()
	if _, err := b.c.db.Exec(`INSERT INTO machines(nom, statut, type, cree_le) VALUES ('web1', 'approuvee', 'agent', 1)`); err != nil {
		t.Fatal(err)
	}
	id, _ := flotte.IDMachine(ctx, b.c.db, "web1")
	// Désactivé : rien n'est appliqué, Docker est proposé.
	b.c.appliquerAuto(ctx, maintenant)
	if effs, _ := modeles.Effectifs(ctx, b.c.db, id, ""); len(effs) != 0 {
		t.Fatalf("appliqué sans le réglage : %+v", effs)
	}
	if n := b.c.nbModelesProposes(ctx, []flotte.InfoMachine{{ID: id, Nom: "web1", Statut: "approuvee"}}, maintenant); n["web1"] != 1 {
		t.Errorf("proposés : %v", n)
	}
	_ = modeles.DefinirAuto(ctx, b.c.db, true, "admin", maintenant)
	b.c.appliquerAuto(ctx, maintenant)
	effs, _ := modeles.Effectifs(ctx, b.c.db, id, "")
	if len(effs) != 1 || effs[0].Modele.Nom != "docker" || !effs[0].Auto {
		t.Fatalf("application automatique : %+v", effs)
	}
	if m := attendre(recus); !strings.Contains(m, "Docker") || !strings.Contains(m, "web1") {
		t.Errorf("message : %q", m)
	}
	// Retiré à la main (décoché) : ne revient pas.
	_ = modeles.Definir(ctx, b.c.db, modeles.PorteeMachine(id), "docker", modeles.EtatIgnore, nil, nil, "admin", maintenant)
	b.c.appliquerAuto(ctx, maintenant.Add(time.Minute))
	if effs, _ = modeles.Effectifs(ctx, b.c.db, id, ""); len(effs) != 0 {
		t.Errorf("revenu après retrait : %+v", effs)
	}
}

// Services suggérés pour un modèle : sur toutes ses machines, pas partout,
// hors services de base et déjà listés.
func TestSuggererServices(t *testing.T) {
	ms := []flotte.InfoMachine{{ID: 1, Nom: "a"}, {ID: 2, Nom: "b"}, {ID: 3, Nom: "c"}, {ID: 4, Nom: "muette"}}
	effs := map[int64]map[string]bool{1: {"docker": true}, 2: {"docker": true}, 3: {}, 4: {"docker": true}}
	vus := map[string]map[string]bool{
		"a": {"docker.service": true, "containerd.service": true, "nginx.service": true, "ssh.service": true, "systemd-journald.service": true, "commun.service": true},
		"b": {"docker.service": true, "containerd.service": true, "ssh.service": true, "commun.service": true},
		"c": {"ssh.service": true, "commun.service": true},
	}
	got := suggererServices("docker", []string{"docker.service"}, ms, effs, vus)
	if strings.Join(got, ",") != "containerd.service" {
		t.Errorf("suggérés : %v", got)
	}
	// Strict : actif aussi sur une machine sans le modèle → pas propre au
	// modèle ; service de distribution → jamais suggéré.
	vus2 := map[string]map[string]bool{
		"a": {"docker.service": true, "outil.service": true, "ModemManager.service": true},
		"b": {"docker.service": true, "outil.service": true, "ModemManager.service": true},
		"c": {"outil.service": true},
	}
	if got := suggererServices("docker", nil, ms, effs, vus2); strings.Join(got, ",") != "docker.service" {
		t.Errorf("suggérés (strict) : %v", got)
	}
	// Modèle porté par toutes les machines : rien à suggérer.
	if got := suggererServices("docker", nil, ms[:2], effs, vus); got != nil {
		t.Errorf("modèle partout : %v", got)
	}
}

// Machines déjà en place (retour du 14/11) : un modèle détecté est coché
// d'office sur la page de la machine, et la page Modèles propose de cocher
// d'un coup tous les modèles détectés.
func TestModelesDetectesExistants(t *testing.T) {
	b := nouveauBanc(t)
	db, err := tsdb.Open(t.TempDir(), nil, nil, tsdb.DefaultOptions(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	maintenant := b.h.maintenant()
	app := db.Appender(context.Background())
	for ts := maintenant.Add(-5 * time.Minute); !ts.After(maintenant); ts = ts.Add(30 * time.Second) {
		_, _ = app.Append(0, labels.FromStrings("__name__", "node_docker_up", "instance", "web1"), ts.UnixMilli(), 1)
	}
	_ = app.Commit()
	b.c.lecteur = alertes.NouveauLecteur(db, nil)
	ctx := context.Background()
	if _, err := b.c.db.Exec(`INSERT INTO machines(nom, statut, type, cree_le) VALUES ('web1', 'approuvee', 'agent', 1)`); err != nil {
		t.Fatal(err)
	}
	id, _ := flotte.IDMachine(ctx, b.c.db, "web1")
	c, _ := b.premierAdmin("admin", mdpAdmin)
	_, corps, _ := b.get(c, "/machines/web1")
	if !strings.Contains(corps, `name="m" value="docker" checked`) || !strings.Contains(corps, "à enregistrer") {
		t.Error("modèle détecté pas coché d'office sur la page de la machine")
	}
	_, corps, _ = b.get(c, "/modeles")
	if !strings.Contains(corps, `id="detectes"`) || !strings.Contains(corps, "<strong>web1</strong> : Docker") {
		t.Fatal("liste des modèles détectés absente")
	}
	// Mode administration ouvert à la connexion : pas de code à saisir.
	csrf := b.csrf(c, "/modeles")
	b.post(c, "/modeles", url.Values{"csrf": {csrf}, "action": {"detectes"}})
	effs, _ := modeles.Effectifs(ctx, b.c.db, id, "")
	if len(effs) != 1 || effs[0].Modele.Nom != "docker" || effs[0].Auto {
		t.Fatalf("modèles appliqués : %+v", effs)
	}
	if _, corps, _ = b.get(c, "/modeles"); strings.Contains(corps, `id="detectes"`) {
		t.Error("liste encore affichée après application")
	}
}

// Le modèle gère aussi les métriques (retour du 14/11) : décoché, il coupe
// ce qu'il faisait relever, même réglé avant les modèles ; recoché, il le
// rallume. Pareil par groupe.
func TestModelesGerentMetriques(t *testing.T) {
	b := nouveauBanc(t)
	ctx := context.Background()
	if _, err := b.c.db.Exec(`INSERT INTO machines(nom, statut, type, groupe, cree_le) VALUES ('web1', 'approuvee', 'agent', 'lan', 1)`); err != nil {
		t.Fatal(err)
	}
	id, _ := flotte.IDMachine(ctx, b.c.db, "web1")
	portee := "machine:" + strconv.FormatInt(id, 10)
	// Réglé à la main avant les modèles.
	if err := flotte.DefinirConfig(ctx, b.c.db, portee, []byte(`{"docker_metriques":true,"systemd":true}`), "admin", time.Now()); err != nil {
		t.Fatal(err)
	}
	_ = modeles.Definir(ctx, b.c.db, modeles.PorteeMachine(id), "docker", modeles.EtatActif, nil, nil, "admin", time.Now())
	c, _ := b.premierAdmin("admin", mdpAdmin)
	csrf := b.csrf(c, "/machines/web1")
	lire := func() map[string]any { m, _ := flotte.LireConfigPortee(ctx, b.c.db, portee); return m }
	effectif := func() *distant.ConfigDistante { cfg, _ := flotte.ConfigEffective(ctx, b.c.db, id); return cfg }
	// Décoché sur la machine : docker_metriques coupé, systemd (pas du modèle) gardé.
	b.post(c, "/machines/web1", url.Values{"csrf": {csrf}, "action": {"modeles"}})
	if m := lire(); m["docker_metriques"] != false || m["systemd"] != true {
		t.Fatalf("après retrait du modèle : %v", m)
	}
	if cfg := effectif(); cfg.DockerMetriques == nil || *cfg.DockerMetriques {
		t.Error("métriques Docker encore relevées après retrait du modèle")
	}
	// Recoché : la coupure est levée, le modèle rallume.
	b.post(c, "/machines/web1", url.Values{"csrf": {csrf}, "action": {"modeles"}, "m": {"docker"}})
	if _, ok := lire()["docker_metriques"]; ok {
		t.Errorf("coupure pas levée : %v", lire())
	}
	if cfg := effectif(); cfg.DockerMetriques == nil || !*cfg.DockerMetriques {
		t.Error("métriques Docker pas rallumées par le modèle")
	}
	// Par groupe : Linux coché puis décoché pour « lan » → systemd coupé.
	_ = modeles.Definir(ctx, b.c.db, modeles.PorteeMachine(id), "docker", "", nil, nil, "admin", time.Now())
	b.post(c, "/modeles", url.Values{"csrf": {csrf}, "action": {"groupes"}, "a": {"lan|linux"}})
	b.post(c, "/modeles", url.Values{"csrf": {csrf}, "action": {"groupes"}})
	if m := lire(); m["systemd"] != false || m["mises_a_jour"] != false {
		t.Errorf("après retrait du modèle du groupe : %v", m)
	}
}

// Modèles d'équipement (choix du 15/11 : « auto ») : appliqués dès la
// détection, même sans l'application automatique générale ; règle importée
// remplacée listée, puis retirée quand elle ne sert plus.
func TestModelesEquipementEtReglesCouvertes(t *testing.T) {
	b := nouveauBanc(t)
	db, err := tsdb.Open(t.TempDir(), nil, nil, tsdb.DefaultOptions(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	maintenant := b.h.maintenant()
	app := db.Appender(context.Background())
	for ts := maintenant.Add(-5 * time.Minute); !ts.After(maintenant); ts = ts.Add(30 * time.Second) {
		_, _ = app.Append(0, labels.FromStrings("__name__", "systemStatus", "instance", "nas1", "job", "snmp"), ts.UnixMilli(), 1)
	}
	_ = app.Commit()
	b.c.lecteur = alertes.NouveauLecteur(db, nil)
	ctx := context.Background()
	if _, err := b.c.db.Exec(`INSERT INTO machines(nom, statut, type, cree_le) VALUES ('nas1', 'approuvee', 'agent', 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := alertes.Enregistrer(ctx, b.c.db, []alertes.Regle{{Nom: "SynologyInjoignable", Groupe: "synology", Intervalle: alertes.Duree(time.Minute),
		Expr: `up{job="snmp"} == 0`, Gravite: alertes.Critique, Active: true, Origine: "vmalert:equipements.yml"}}, "admin", maintenant); err != nil {
		t.Fatal(err)
	}
	c, _ := b.premierAdmin("admin", mdpAdmin)
	// Avant : détecté, pas porté → la règle importée sert encore.
	if _, corps, _ := b.get(c, "/regles"); !strings.Contains(corps, `id="couvertes"`) || !strings.Contains(corps, "Sert encore") || !strings.Contains(corps, "<code>nas1</code>") {
		t.Fatal("règle importée remplacée : état « sert encore » absent")
	}
	// Application automatique générale coupée : le modèle d'équipement est appliqué quand même.
	b.c.appliquerAuto(ctx, maintenant)
	id, _ := flotte.IDMachine(ctx, b.c.db, "nas1")
	effs, _ := modeles.Effectifs(ctx, b.c.db, id, "")
	if len(effs) != 1 || effs[0].Modele.Nom != "synology" || !effs[0].Auto {
		t.Fatalf("modèle d'équipement non appliqué : %+v", effs)
	}
	_, corps, _ := b.get(c, "/regles")
	if !strings.Contains(corps, "ne sert plus") || !strings.Contains(corps, "Retirer la règle qui ne sert plus") {
		t.Fatal("règle couverte pas proposée au retrait")
	}
	csrf := b.csrf(c, "/regles")
	b.post(c, "/regles/action", url.Values{"csrf": {csrf}, "action": {"retirer_couvertes"}})
	rs, _ := alertes.ListerRegles(ctx, b.c.db)
	for _, r := range rs {
		if r.Nom == "SynologyInjoignable" {
			t.Fatal("règle importée couverte pas retirée")
		}
	}
}

// Page Règles (SPEC § 27) : une règle de modèle est marquée, ne se modifie
// ni ne se supprime ici (renvoi au modèle) ; elle se désactive.
func TestReglesDeModele(t *testing.T) {
	b := nouveauBanc(t)
	ctx := context.Background()
	if _, err := modeles.Synchroniser(ctx, b.c.db, "mirador", time.Now()); err != nil {
		t.Fatal(err)
	}
	c, _ := b.premierAdmin("admin", mdpAdmin)
	_, corps, _ := b.get(c, "/regles")
	if !strings.Contains(corps, `href="/modeles#docker"`) || strings.Contains(corps, `href="/regles/modifier/DockerConteneurArrete"`) {
		t.Fatal("règle de modèle pas marquée ou encore modifiable depuis la page Règles")
	}
	if code, _, ent := b.get(c, "/regles/modifier/DockerConteneurArrete"); code != http.StatusSeeOther || ent.Get("Location") != "/modeles#docker" {
		t.Errorf("modification : %d %q", code, ent.Get("Location"))
	}
	csrf := b.csrf(c, "/regles")
	b.post(c, "/regles/action", url.Values{"csrf": {csrf}, "action": {"supprimer"}, "nom": {"DockerConteneurArrete"}})
	b.post(c, "/regles/action", url.Values{"csrf": {csrf}, "action": {"desactiver"}, "nom": {"DockerConteneurArrete"}})
	rs, _ := alertes.ListerRegles(ctx, b.c.db)
	trouve := false
	for _, r := range rs {
		if r.Nom == "DockerConteneurArrete" {
			trouve = true
			if r.Active {
				t.Error("désactivation refusée")
			}
		}
	}
	if !trouve {
		t.Error("règle de modèle supprimée depuis la page Règles")
	}
}
