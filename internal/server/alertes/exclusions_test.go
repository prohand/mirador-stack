// SPDX-License-Identifier: GPL-3.0-or-later

package alertes

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/model/labels"
)

// Liste des invités exclus : ajout, mise à jour, retrait, refus, audit.
func TestInvitesExclus(t *testing.T) {
	ctx := context.Background()
	db := baseTest(t)
	t0 := time.Now()
	if l, err := LireInvitesExclus(ctx, db); err != nil || len(l) != 0 {
		t.Fatalf("liste vide : %v %v", l, err)
	}
	if err := ExclureInvite(ctx, db, InviteExclu{Machine: "proxmox", ID: "200", Nom: "pbs", Motif: "le PBS\nlui-même", Qui: "web:admin"}, t0); err != nil {
		t.Fatal(err)
	}
	// Deuxième fois : mise à jour, pas de doublon.
	if err := ExclureInvite(ctx, db, InviteExclu{Machine: "proxmox", ID: "200", Motif: "test", Qui: "web:admin"}, t0); err != nil {
		t.Fatal(err)
	}
	if err := ExclureInvite(ctx, db, InviteExclu{Machine: "proxmox", ID: "201", Qui: "web:admin"}, t0); err != nil {
		t.Fatal(err)
	}
	l, _ := LireInvitesExclus(ctx, db)
	if len(l) != 2 || l[0].ID != "200" || l[0].Motif != "test" || l[1].ID != "201" {
		t.Fatalf("liste : %+v", l)
	}
	for _, e := range []InviteExclu{{Machine: "proxmox", ID: "abc"}, {Machine: "../x", ID: "1"}, {Machine: "", ID: "1"}} {
		if ExclureInvite(ctx, db, e, t0) == nil {
			t.Errorf("accepté : %+v", e)
		}
	}
	if err := ReintegrerInvite(ctx, db, "proxmox", "200", "web:admin", t0); err != nil {
		t.Fatal(err)
	}
	if ReintegrerInvite(ctx, db, "proxmox", "200", "web:admin", t0) == nil {
		t.Error("retrait d'une VM non exclue accepté")
	}
	if l, _ := LireInvitesExclus(ctx, db); len(l) != 1 || l[0].ID != "201" {
		t.Fatalf("après retrait : %+v", l)
	}
	var n int
	_ = db.QueryRow(`SELECT COUNT(*) FROM audit WHERE action IN ('invite_exclu', 'invite_reintegre')`).Scan(&n)
	if n != 4 {
		t.Errorf("audit : %d lignes, 4 attendues", n)
	}
	if borner("  a\tb  ", 10) != "a b" || borner("éééé", 2) != "éé" {
		t.Error("borner")
	}
}

// La règle posée avant les exclusions reçoit la nouvelle expression ; une
// expression modifiée à la main est laissée.
func TestMettreAJourVMSansSauvegarde(t *testing.T) {
	ctx := context.Background()
	t0 := time.Now()
	for _, c := range []struct {
		avant   string
		attendu bool
	}{{exprVMSansSauvegardeAvant, true}, {`mirador_pve_invite_sans_sauvegarde{id!="200"} == 1`, false}, {ExprVMSansSauvegarde, false}} {
		db := baseTest(t)
		if _, err := PoserDefauts(ctx, db, t0); err != nil {
			t.Fatal(err)
		}
		rs, _ := ListerRegles(ctx, db)
		for _, r := range rs {
			if r.Nom == "VMSansSauvegarde" {
				r.Expr = c.avant
				if _, err := Enregistrer(ctx, db, []Regle{r}, "test", t0); err != nil {
					t.Fatal(err)
				}
			}
		}
		fait, err := MettreAJourVMSansSauvegarde(ctx, db, t0)
		if err != nil || fait != c.attendu {
			t.Errorf("%q : mise à jour %v (%v), attendu %v", c.avant, fait, err, c.attendu)
		}
		rs, _ = ListerRegles(ctx, db)
		for _, r := range rs {
			if r.Nom == "VMSansSauvegarde" && c.attendu && r.Expr != ExprVMSansSauvegarde {
				t.Errorf("expression : %q", r.Expr)
			}
		}
	}
}

// Effet de la règle : la VM exclue ne déclenche plus d'alerte, l'autre si.
func TestVMExclueSansAlerte(t *testing.T) {
	ctx := context.Background()
	db := baseTest(t)
	donnees := tsdbTest(t)
	t0 := time.Now().Truncate(time.Minute).Add(-time.Hour)
	r := Regle{Nom: "VMSansSauvegarde", Expr: ExprVMSansSauvegarde, Gravite: Avertissement, Intervalle: Duree(time.Minute), Active: true}
	if _, err := Enregistrer(ctx, db, []Regle{r}, "test", t0); err != nil {
		t.Fatal(err)
	}
	un := func(time.Time) float64 { return 1 }
	for _, id := range []string{"200", "201"} {
		ecrirePoints(t, donnees, labels.FromStrings("__name__", "mirador_pve_invite_sans_sauvegarde", "instance", "proxmox", "id", id, "nom", "vm"+id),
			t0, t0.Add(10*time.Minute), 30*time.Second, un)
	}
	ecrirePoints(t, donnees, labels.FromStrings("__name__", MetriqueInviteExclue, "instance", "proxmox", "id", "200"),
		t0, t0.Add(10*time.Minute), time.Minute, un)
	rec := &recepteurTest{}
	horloge := t0.Add(5 * time.Minute)
	m, err := Nouveau(ctx, Options{DB: db, Donnees: donnees, Recepteur: rec, Maintenant: func() time.Time { return horloge }})
	if err != nil {
		t.Fatal(err)
	}
	m.debut = t0.Add(-time.Hour)
	m.EvaluerTout(ctx, horloge)
	var ids []string
	for _, a := range m.EnCours() {
		ids = append(ids, a.Etiquettes.Get("id"))
	}
	if strings.Join(ids, ",") != "201" {
		t.Errorf("alertes pour les VM %v, attendu 201 seule", ids)
	}
}
