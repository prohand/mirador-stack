// SPDX-License-Identifier: GPL-3.0-or-later

package sauvegarde

import (
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestPlanificateur(t *testing.T) {
	src := preparer(t)
	maintenant := time.Date(2026, 11, 10, 2, 0, 0, 0, time.UTC)
	p := &Planificateur{Sources: src, Cle: cle, Version: "test", Journal: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Maintenant: func() time.Time { return maintenant }}
	compter := func(statut string) int {
		var n int
		_ = src.DB.QueryRow(`SELECT count(*) FROM sauvegardes WHERE statut = ?`, statut).Scan(&n)
		return n
	}
	// Avant l'heure prévue (02:43) : rien.
	if err := p.Tour(ctx); err != nil || compter("ok") != 0 {
		t.Fatalf("avant l'heure : %v", err)
	}
	// Après : une sauvegarde, une seule même si on repasse.
	maintenant = maintenant.Add(time.Hour)
	for i := 0; i < 3; i++ {
		if err := p.Tour(ctx); err != nil {
			t.Fatal(err)
		}
		maintenant = maintenant.Add(time.Second)
	}
	if compter("ok") != 1 {
		t.Fatalf("%d sauvegardes réussies", compter("ok"))
	}
	if d, ok, _ := DerniereReussie(ctx, src.DB); !ok || d.Origine != "planifiee" || d.Destinations != "locale" {
		t.Fatalf("%+v", d)
	}
	// Le lendemain : une autre.
	maintenant = maintenant.Add(24 * time.Hour)
	_ = p.Tour(ctx)
	if compter("ok") != 2 {
		t.Fatalf("lendemain : %d", compter("ok"))
	}
	// Destination distante injoignable : échec noté, archive locale gardée.
	r := ReglagesDefaut()
	r, _ = PreparerSecrets(Reglages{Actif: false, Heure: "02:43", Conserver: 5,
		S3: &S3{Adresse: "https://127.0.0.1:1", Region: "x", Seau: "seau", CleAcces: "a", Secret: "s"}}, r, cle)
	if err := EcrireReglages(ctx, src.DB, r, "test", maintenant); err != nil {
		t.Fatal(err)
	}
	maintenant = maintenant.Add(time.Minute)
	if _, err := Demander(ctx, src.DB, "commande", false, false, maintenant); err != nil {
		t.Fatal(err)
	}
	_ = p.Tour(ctx)
	h, _ := Historique(ctx, src.DB, 1)
	if h[0].Statut != "echec" || !strings.Contains(h[0].Erreur, "s3 seau") || h[0].Fichier == "" {
		t.Fatalf("échec distant : %+v", h[0])
	}
	if a, _ := Lister(DossierArchives(src.Dossier)); len(a) != 3 {
		t.Fatalf("archives locales : %d", len(a))
	}
	// Audit sans secret.
	var apres string
	_ = src.DB.QueryRow(`SELECT apres FROM audit WHERE action = 'reglages_sauvegarde'`).Scan(&apres)
	if strings.Contains(apres, "chiffre:") || !strings.Contains(apres, `"secret":"***"`) {
		t.Fatalf("audit : %s", apres)
	}
}
