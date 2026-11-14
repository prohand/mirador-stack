// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/tsdb"

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
	// Modèle porté par toutes les machines : rien à suggérer.
	if got := suggererServices("docker", nil, ms[:2], effs, vus); got != nil {
		t.Errorf("modèle partout : %v", got)
	}
}
