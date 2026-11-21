// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"testing"

	"mirador-stack/internal/server/flotte"
)

// Page « Mises à jour des agents » (SPEC § 109) : états, étapes, cartes.
func TestVueMajParc(t *testing.T) {
	ms := []flotte.InfoMachine{
		{Nom: "a", Statut: "approuvee", Version: "0.6.2"},
		{Nom: "b", Statut: "approuvee", Version: "0.6.1"},
		{Nom: "c", Statut: "approuvee", Version: "0.6.1", MajEchec: "pas de battement"},
		{Nom: "test", Statut: "approuvee", Version: "0.6.2", VersionCible: "0.6.3"},
		{Nom: "col", Type: "collecteur", Statut: "approuvee", Version: "0.6.2"},
		{Nom: "box", Statut: "approuvee"},                      // équipement : ignoré
		{Nom: "neuve", Statut: "en_attente", Version: "0.6.2"}, // en attente : ignorée
	}
	versions := []flotte.VersionPubliee{
		{Produit: "mirador-agent", Version: "0.6.3", Architecture: "amd64"},
		{Produit: "mirador-collector", Version: "0.6.3", Architecture: "amd64"},
		{Produit: "mirador-agent", Version: "0.6.2", Architecture: "arm64"},
		{Produit: "mirador-agent", Version: "0.6.2", Architecture: "amd64"},
		{Produit: "mirador-agent", Version: "0.6.1", Architecture: "amd64"},
	}
	d := donneesMaj{Cible: "0.6.2", Machines: ms, Versions: versions, Choix: []string{"0.6.3", "0.6.2", "0.6.1"},
		Proposees: []versionProposee{{Tag: "v0.6.3", Importee: true}}}
	construireVueMaj(&d, "")
	v := d.Vue
	if v.Total != 5 || v.AJour != 2 || v.Retard != 2 || v.Echec != 1 {
		t.Fatalf("comptes : %d total, %d à jour, %d retard, %d échec", v.Total, v.AJour, v.Retard, v.Echec)
	}
	if v.Lignes[0].Nom != "c" || v.Lignes[0].Etat != majEchec {
		t.Errorf("l'échec doit venir en premier : %+v", v.Lignes[0].Nom)
	}
	// Essai en cours sur « test » (encore en 0.6.2) : étape 2.
	if v.Deployer != "0.6.3" || v.Etape != 2 || v.Essai == nil || v.Essai.Nom != "test" {
		t.Fatalf("étape : %q %d %+v", v.Deployer, v.Etape, v.Essai)
	}
	if len(v.Candidates) != 4 {
		t.Errorf("le collecteur n'est pas proposé pour l'essai : %v", v.Candidates)
	}
	// Essai réussi : étape 3.
	d.Machines[3].Version = "0.6.3"
	d.Vue = vueMajParc{}
	construireVueMaj(&d, "")
	if d.Vue.Etape != 3 {
		t.Errorf("essai réussi : étape %d", d.Vue.Etape)
	}
	// Déployée : étape 4 ; pas encore importée : étape 1.
	d.Cible, d.Vue = "0.6.3", vueMajParc{}
	construireVueMaj(&d, "")
	if d.Vue.Etape != 4 {
		t.Errorf("déployée : étape %d", d.Vue.Etape)
	}
	d.Cible, d.Proposees, d.Vue = "0.6.2", []versionProposee{{Tag: "v0.7.0"}}, vueMajParc{}
	construireVueMaj(&d, "")
	if d.Vue.Etape != 1 || d.Vue.DerniereImportee {
		t.Errorf("à importer : étape %d", d.Vue.Etape)
	}
	// Cartes : une par version, la plus récente d'abord, archs réunies ;
	// 0.6.1 est encore en service : pas supprimable.
	c := d.Vue.Cartes
	if len(c) != 3 || c[0].Version != "0.6.3" || !c[0].Collecteur || len(c[1].Archs) != 2 || !c[1].Parc || c[1].Supprimable || c[2].Supprimable {
		t.Errorf("cartes : %+v", c)
	}
	// Filtre « en échec ».
	d.Vue = vueMajParc{}
	construireVueMaj(&d, majEchec)
	if len(d.Vue.Lignes) != 1 || d.Vue.Total != 5 {
		t.Errorf("filtre : %d lignes sur %d", len(d.Vue.Lignes), d.Vue.Total)
	}
}
