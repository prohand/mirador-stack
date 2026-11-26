// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"strings"
	"testing"
)

// Données d'un petit parc Linux (2 machines) pour les tests des tableaux.
func parcLinux(b *banc, t *testing.T) {
	cpu := func(m, mode string, pente float64) serieTest {
		return serieTest{[]string{"__name__", "node_cpu_seconds_total", "instance", m, "cpu", "0", "mode", mode}, func(k int) float64 { return float64(k) * pente }}
	}
	uname := func(m string) serieTest {
		return serieTest{[]string{"__name__", "node_uname_info", "instance", m, "site", "maison", "env", "vm"}, constante(1)}
	}
	b.metriquesSimulees(t, []serieTest{cpu("web1", "idle", 20), cpu("web1", "user", 10), cpu("db1", "idle", 25), cpu("db1", "user", 5), uname("web1"), uname("db1")})
}

// Liste en cartes (maquette du 26/11) : ce que couvre le tableau (le filtre
// qui a le plus de valeurs), petite courbe sur 24 h, proposés triés selon
// les données reçues.
func TestResumeTableau(t *testing.T) {
	b := nouveauBanc(t)
	parcLinux(b, t)
	tb := tableauxDepart["serveurs-linux"]
	r := b.c.calculerResume(context.Background(), tb, b.h.maintenant())
	if r.Couvre != "Machine : 2" || r.Apercu == "" || !r.AvecDonnees {
		t.Fatalf("résumé : %+v", r)
	}
	if p := strings.Fields(r.Apercu); len(p) < 40 {
		t.Errorf("petite courbe : %d points", len(p))
	}
	cl, _ := b.premierAdmin("admin", mdpAdmin)
	_, corps, _ := b.get(cl, "/tableaux")
	if !strings.Contains(corps, "Machine : 2 avec des données") || !strings.Contains(corps, "Sans données chez vous") {
		t.Error("proposés : données reçues absentes ou tableaux sans données non séparés")
	}
}

// Page d'un tableau (maquette du 26/11) : périodes en liens qui gardent les
// filtres, sommaire des rangées, envoi automatique des filtres.
func TestPageTableauPresentation(t *testing.T) {
	b := nouveauBanc(t)
	parcLinux(b, t)
	cl, _ := b.premierAdmin("admin", mdpAdmin)
	_, corps, _ := b.get(cl, "/tableaux/serveurs-linux?instance=web1&periode=6h")
	for _, attendu := range []string{
		`class="barre-filtres"`, `data-envoi-auto`,
		`href="/tableaux/serveurs-linux?instance=web1&amp;periode=24h"`, `class="actif" aria-current="true">6 h</a>`,
		`class="sommaire-tableau"`, `href="#r-ressources"`, `data-valeur-moyenne`,
	} {
		if !strings.Contains(corps, attendu) {
			t.Errorf("absent : %s", attendu)
		}
	}
}
