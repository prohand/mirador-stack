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
)

// Santé de mirador (SPEC § 121, maquette validée le 22/11) : une carte par
// lieu, services root repérés, problèmes en bandeau, pastille du menu.
func TestPageSante(t *testing.T) {
	b := nouveauBanc(t)
	c, _ := b.premierAdmin("admin", mdpAdmin)
	ctx := context.Background()
	m := b.h.maintenant()
	if _, err := b.c.db.Exec(`INSERT INTO machines(nom, statut, type, cree_le, vu_le, version_agent) VALUES
		('collecteur-lan', 'approuvee', 'collecteur', 1, ?, '0.6.13'),
		('pc1', 'approuvee', 'agent', 1, ?, '0.6.13'),
		('fw', 'approuvee', 'agent', 1, ?, '')`, m.Unix(), m.Unix(), m.Unix()); err != nil {
		t.Fatal(err)
	}
	if _, err := flotte.TokenManuel(ctx, b.c.db, "fw", "admin", m); err != nil {
		t.Fatal(err)
	}
	db, err := tsdb.Open(t.TempDir(), nil, nil, tsdb.DefaultOptions(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	app := db.Appender(ctx)
	for ts := m.Add(-10 * time.Minute); !ts.After(m); ts = ts.Add(30 * time.Second) {
		_, _ = app.Append(0, labels.FromStrings("__name__", "mirador_collecteur_compagnon", "instance", "collecteur-lan", "job", "mirador-collector"), ts.UnixMilli(), 0)
		_, _ = app.Append(0, labels.FromStrings("__name__", "mirador_agent_root_age_seconds", "instance", "pc1"), ts.UnixMilli(), 300)
		_, _ = app.Append(0, labels.FromStrings("__name__", "mirador_agent_root_errors", "instance", "pc1"), ts.UnixMilli(), 0)
		_, _ = app.Append(0, labels.FromStrings("__name__", "mirador_agent_root_a_jour", "instance", "pc1"), ts.UnixMilli(), 1)
	}
	_ = app.Commit()
	b.c.lecteur = alertes.NouveauLecteur(db, nil)

	code, corps, _ := b.get(c, "/sante-mirador")
	if code != 200 {
		t.Fatalf("code %d", code)
	}
	for _, attendu := range []string{
		"<h1>Santé de mirador</h1>",
		"Serveur · VPS", "Collecteur · collecteur-lan", "Agents et envois des équipements",
		"mirador-server", "mirador-maj-serveur", "Réception des données", "Notifications", "Sauvegarde du serveur",
		// Compagnon absent : à regarder, avec la marche à suivre.
		"Compagnon des processus", "relancer une fois le script d&#39;installation",
		// Pas de sauvegarde : en panne.
		"aucune sauvegarde réussie",
		// Services root repérés, assistant root lu dans les séries des agents.
		`class="pastille root"`, "Assistant root des agents", "1 / 1",
		// Envoi direct de fw (token propre, sans agent).
		"Envoi direct · fw",
		// Filtres et repli sur téléphone.
		`data-filtre="root"`, `data-groupes="lan root"`, "data-deplier-sante",
	} {
		if !strings.Contains(corps, attendu) {
			t.Errorf("page Santé : %q absent", attendu)
		}
	}
	if !strings.Contains(corps, `data-groupes="lan probleme"`) {
		t.Error("ligne à regarder sans le groupe « probleme »")
	}
	// Pastille du menu : reprend le dernier calcul.
	if b.c.nbSante() == 0 {
		t.Error("pastille du menu à zéro malgré des services à regarder")
	}
	if _, corps, _ := b.get(c, "/"); !strings.Contains(corps, `href="/sante-mirador"`) {
		t.Error("entrée « Santé de mirador » absente du menu")
	}
}

// Calcul de fond : refait au plus toutes les 2 min (boucle de la console).
func TestTourSante(t *testing.T) {
	b := nouveauBanc(t)
	ctx := context.Background()
	b.c.tourSante(ctx, b.h.maintenant())
	premier := b.c.sante.le
	if premier.IsZero() {
		t.Fatal("pas de calcul au premier tour")
	}
	b.h.avancer(time.Minute)
	b.c.tourSante(ctx, b.h.maintenant())
	if b.c.sante.le != premier {
		t.Error("recalcul avant 2 min")
	}
	b.h.avancer(2 * time.Minute)
	b.c.tourSante(ctx, b.h.maintenant())
	if b.c.sante.le == premier {
		t.Error("pas de recalcul après 2 min")
	}
	// Sans sauvegarde : au moins un service à regarder.
	if b.c.nbSante() == 0 {
		t.Error("aucun problème compté")
	}
}

// /sante est le contrôle de vie du conteneur (serveur, healthcheck Docker) :
// aucune page de l'interface ne doit le prendre (la page s'ouvrait en JSON).
func TestSanteNePrendPasLeControleDeVie(t *testing.T) {
	for _, e := range entreesMenu {
		if e.Href == "/sante" {
			t.Errorf("entrée %q sur /sante", e.Cle)
		}
	}
}
