// SPDX-License-Identifier: GPL-3.0-or-later

package modeles

import (
	"strings"
	"testing"
)

func TestModelesFournis(t *testing.T) {
	ms := Fournis()
	if len(ms) < 12 {
		t.Fatalf("%d modèles fournis", len(ms))
	}
	noms := map[string]bool{}
	for _, m := range ms {
		if err := m.Valider(); err != nil {
			t.Errorf("%s : %v", m.Nom, err)
		}
		for _, r := range m.Regles {
			if noms[r.Nom] {
				t.Errorf("règle %s en double", r.Nom)
			}
			noms[r.Nom] = true
		}
	}
	if Trouver("linux") == nil || Trouver("inconnu") != nil {
		t.Error("Trouver")
	}
	if rs := Remplacees(); len(rs["DisqueFaible"]) != 1 || rs["DisqueFaible"][0] != "linux" {
		t.Errorf("remplacées : %v", rs)
	}
}

func TestExprEtRefs(t *testing.T) {
	m := &Modele{Version: 1, Nom: "test", Titre: "Test",
		Parametres: []Parametre{{Nom: "seuil", Titre: "Seuil", Defaut: 10, Min: 1, Max: 100}}}
	r := RegleModele{Nom: "TestRegle", Expr: "x > $seuil", Gravite: "warning"}
	m.Regles = []RegleModele{r}
	if err := m.Valider(); err != nil {
		t.Fatal(err)
	}
	if e := m.ExprRegle(r); e != `x > on (instance) group_left () mirador_modele_parametre{modele="test", parametre="seuil"}` {
		t.Errorf("expression : %s", e)
	}
	for _, mauvaise := range []string{"x > $autre", "$seuil < x", "x + $seuil > 1", "rate(x[5m]) * $seuil"} {
		m.Regles = []RegleModele{{Nom: "TestRegle", Expr: mauvaise, Gravite: "warning"}}
		if err := m.Valider(); err == nil {
			t.Errorf("%q accepté", mauvaise)
		}
	}
	// Configuration : clé inconnue de l'agent refusée.
	m.Regles = nil
	m.Config = []byte(`{"commande":"rm -rf /"}`)
	if err := m.Valider(); err == nil || !strings.Contains(err.Error(), "configuration") {
		t.Errorf("configuration inconnue acceptée : %v", err)
	}
	// Valeurs saisies : nombre, bornes.
	m.Config = nil
	if v, err := m.LireValeur("seuil", "12,5"); err != nil || v != 12.5 {
		t.Errorf("12,5 : %v %v", v, err)
	}
	for _, mauvais := range []string{"0", "101", "abc", "NaN", ""} {
		if _, err := m.LireValeur("seuil", mauvais); err == nil {
			t.Errorf("%q accepté", mauvais)
		}
	}
}

// Règle sans seuil : limitée aux machines du modèle ; étiquettes contrôlées ;
// modèles d'équipement appliqués automatiquement.
func TestRegleSansSeuilEtEquipements(t *testing.T) {
	d := Trouver("docker")
	var arrete RegleModele
	for _, r := range d.Regles {
		if r.Nom == "DockerConteneurArrete" {
			arrete = r
		}
	}
	if !strings.Contains(d.ExprRegle(arrete), `and on (instance) mirador_machine_modele{modele="docker"}`) {
		t.Errorf("règle sans seuil non limitée : %s", d.ExprRegle(arrete))
	}
	for _, n := range []string{"synology", "opnsense", "unifi"} {
		m := Trouver(n)
		if m == nil || !m.Auto || len(m.Regles) == 0 || m.Detection == "" {
			t.Errorf("modèle d'équipement %s incomplet", n)
		}
	}
	m := *Trouver("unifi")
	m.Regles = append([]RegleModele(nil), m.Regles...)
	m.Regles[0].Etiquettes = map[string]string{"severity": "x"}
	if m.Valider() == nil {
		t.Error("étiquette severity acceptée")
	}
}
