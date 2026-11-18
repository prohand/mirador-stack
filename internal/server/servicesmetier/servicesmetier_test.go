// SPDX-License-Identifier: GPL-3.0-or-later

package servicesmetier

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/prometheus/model/labels"

	"mirador-stack/internal/server/alertes"
)

type faux map[string][]alertes.Echantillon

func (f faux) Instant(_ context.Context, expr string, _ time.Time) ([]alertes.Echantillon, error) {
	return f[expr], nil
}

func e(v float64, kv ...string) alertes.Echantillon {
	return alertes.Echantillon{Etiquettes: labels.FromStrings(kv...), Valeur: v}
}

func TestLignes(t *testing.T) {
	cs, err := LireLignes("clé sonde Home Assistant\n  conteneur docker01/mosquitto \n\nmachine pi-zigbee\n")
	if err != nil || len(cs) != 3 || !cs[0].Cle || cs[0].Nom != "Home Assistant" || cs[1].Nom != "docker01/mosquitto" || cs[2].Cle {
		t.Fatalf("%+v %v", cs, err)
	}
	if Lignes(Service{Composants: cs}) != "clé sonde Home Assistant\nconteneur docker01/mosquitto\nmachine pi-zigbee\n" {
		t.Error("aller-retour")
	}
	if err := Valider([]Service{{Nom: "Domotique", Composants: cs}}); err != nil {
		t.Error(err)
	}
	for _, mauvais := range [][]Service{
		{{Nom: "", Composants: cs}},
		{{Nom: "A", Composants: nil}},
		{{Nom: "A", Composants: []Composant{{Sorte: "conteneur", Nom: "sans-machine"}}}},
		{{Nom: "A", Composants: []Composant{{Sorte: "routeur", Nom: "x"}}}},
		{{Nom: "A", Composants: cs}, {Nom: "A", Composants: cs}},
	} {
		if Valider(mauvais) == nil {
			t.Errorf("accepté : %+v", mauvais)
		}
	}
}

func TestCalculer(t *testing.T) {
	l := faux{
		`max by (instance, name) (node_docker_container_running)`: {e(1, "instance", "docker01", "name", "ha"), e(1, "instance", "docker01", "name", "z2m"), e(0, "instance", "docker01", "name", "vieux")},
		`max by (instance, name) (node_docker_container_healthy)`: {e(1, "instance", "docker01", "name", "ha"), e(0, "instance", "docker01", "name", "z2m")},
		`max by (sonde) (mirador_sonde_succes)`:                   {e(1, "sonde", "HA"), e(0, "sonde", "Nextcloud")},
		`max by (sonde) (mirador_sonde_duree_secondes)`:           {e(0.041, "sonde", "HA")},
		`max by (sonde) (mirador_sonde_lente)`:                    {e(0, "sonde", "HA")},
	}
	muet := alertes.Alerte{Regle: "AgentMuet", Etat: alertes.Active, Gravite: alertes.Critique, Etiquettes: labels.FromStrings("instance", "nas")}
	disque := alertes.Alerte{Regle: "DisquePlein", Etat: alertes.Active, Gravite: alertes.Avertissement, Etiquettes: labels.FromStrings("instance", "pve1")}
	services := []Service{
		{Nom: "Domotique", Composants: []Composant{{Sorte: Sonde, Nom: "HA", Cle: true}, {Sorte: Conteneur, Nom: "docker01/ha", Cle: true}, {Sorte: Conteneur, Nom: "docker01/z2m"}}},
		{Nom: "Nextcloud", Composants: []Composant{{Sorte: Sonde, Nom: "Nextcloud", Cle: true}, {Sorte: Machine, Nom: "pve1"}}},
		{Nom: "Sauvegardes", Composants: []Composant{{Sorte: Machine, Nom: "pve1", Cle: true}}},
		{Nom: "Stockage", Composants: []Composant{{Sorte: Machine, Nom: "nas", Cle: true}}},
		{Nom: "Réseau", Composants: []Composant{{Sorte: Machine, Nom: "opnsense", Cle: true}, {Sorte: Conteneur, Nom: "docker01/inconnu"}}},
	}
	got := map[string]EtatService{}
	for _, x := range Calculer(context.Background(), l, []alertes.Alerte{muet, disque}, []string{"AgentMuet"}, services, time.Now()) {
		got[x.Nom] = x
	}
	attendus := map[string]int{"Domotique": Degrade, "Nextcloud": Panne, "Sauvegardes": Degrade, "Stockage": Panne, "Réseau": OK}
	for nom, etat := range attendus {
		if got[nom].Etat != etat {
			t.Errorf("%s : %d (%s), attendu %d — %+v", nom, got[nom].Etat, got[nom].Texte, etat, got[nom].Composants)
		}
	}
	if c := got["Domotique"].Composants; c[0].Texte != "OK · 41 ms" || c[2].Texte != "Non sain" {
		t.Errorf("composants : %+v", c)
	}
}
