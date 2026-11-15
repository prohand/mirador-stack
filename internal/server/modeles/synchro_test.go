// SPDX-License-Identifier: GPL-3.0-or-later

package modeles

import (
	"context"
	"strings"
	"testing"
	"time"

	"mirador-stack/internal/server/alertes"
)

// Règles des modèles gérées par les modèles (SPEC § 27) : remises à jour au
// démarrage, choix « désactivée » gardé, anciennes retirées, imports refusés.
func TestSynchroniser(t *testing.T) {
	ctx := context.Background()
	db := basePerso(t)
	t0 := time.Now()
	if _, err := Synchroniser(ctx, db, "mirador", t0); err != nil {
		t.Fatal(err)
	}
	lire := func() map[string]alertes.Regle {
		rs, _ := alertes.ListerRegles(ctx, db)
		out := map[string]alertes.Regle{}
		for _, r := range rs {
			out[r.Nom] = r
		}
		return out
	}
	rs := lire()
	if r, ok := rs["DockerConteneurArrete"]; !ok || r.Origine != "modele:docker" || !r.Active {
		t.Fatalf("règle du modèle Docker absente : %+v", r)
	}
	// Désactivée par l'utilisateur, ancienne expression, règle disparue du modèle.
	_ = alertes.Activer(ctx, db, "DockerConteneurArrete", false, "admin", t0)
	r := rs["DockerDemonInjoignable"]
	r.Expr = "node_docker_up == 0"
	disparue := r
	disparue.Nom = "DockerRegleDisparue"
	if _, err := alertes.Enregistrer(ctx, db, []alertes.Regle{r, disparue}, "mirador", t0); err != nil {
		t.Fatal(err)
	}
	if _, err := Synchroniser(ctx, db, "mirador", t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	rs = lire()
	if rs["DockerConteneurArrete"].Active {
		t.Error("choix « désactivée » perdu")
	}
	if !strings.Contains(rs["DockerDemonInjoignable"].Expr, "mirador_machine_modele") {
		t.Errorf("expression pas remise à jour : %s", rs["DockerDemonInjoignable"].Expr)
	}
	if _, ok := rs["DockerRegleDisparue"]; ok {
		t.Error("règle disparue du modèle pas retirée")
	}
	// Un import ne peut pas écraser une règle de modèle.
	imp := rs["LinuxCpuEleve"]
	imp.Origine = "vmalert:system.yml"
	if _, err := alertes.Enregistrer(ctx, db, []alertes.Regle{imp}, "admin", t0); err == nil || !strings.Contains(err.Error(), "modèle") {
		t.Errorf("import par-dessus une règle de modèle : %v", err)
	}
}
