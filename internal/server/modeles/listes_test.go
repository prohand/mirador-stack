// SPDX-License-Identifier: GPL-3.0-or-later

package modeles

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"mirador-stack/internal/agent/config"
	"mirador-stack/internal/server/base"
)

func TestLireLignes(t *testing.T) {
	web := Trouver("serveur-web").Liste("adresses")
	lignes, err := web.LireLignes("https://exemple.org/\r\n\n# commentaire\n  https://192.0.2.1:8006/   non-verifie contient=Proxmox  VE\nhttps://exemple.org/\n")
	if err != nil || len(lignes) != 2 || lignes[1] != "https://192.0.2.1:8006/ non-verifie contient=Proxmox VE" {
		t.Fatalf("%q %v", lignes, err)
	}
	c, err := web.Check(lignes[1], "serveur-web")
	if err != nil || c.Contient != "Proxmox VE" || c.VerifierTLS == nil || *c.VerifierTLS || c.Nom != "192.0.2.1:8006" || c.Modele != "serveur-web" {
		t.Errorf("%+v %v", c, err)
	}
	for _, mauvais := range []string{"file:///etc/passwd", "https://exemple.org/ inconnu", "https://moi:mdp@exemple.org/", "https://h/ contient="} {
		if _, err := web.LireLignes(mauvais); err == nil {
			t.Errorf("accepté : %q", mauvais)
		}
	}
	ports := Trouver("ports").Liste("cibles")
	if _, err := ports.LireLignes("127.0.0.1:5432 non-verifie"); err == nil {
		t.Error("option http acceptée pour un port")
	}
	if _, err := ports.LireLignes(strings.Repeat("127.0.0.1:1\n", 3) + "h:2"); err != nil {
		t.Error(err)
	}
	var trop strings.Builder
	for i := 1; i <= MaxLignes+1; i++ {
		trop.WriteString("127.0.0.1:" + string(rune('0'+i/10)) + string(rune('0'+i%10)) + "\n")
	}
	if _, err := ports.LireLignes(trop.String()); err == nil {
		t.Error("plus de 50 lignes acceptées")
	}
}

// Listes : héritées du groupe, remplacées par la machine (même vidées),
// transformées en checks valides pour l'agent.
func TestListesEtChecks(t *testing.T) {
	ctx := context.Background()
	db, err := base.Ouvrir(filepath.Join(t.TempDir(), "mirador.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := base.Migrer(ctx, db, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	def := func(portee, modele string, listes map[string][]string) {
		t.Helper()
		if err := Definir(ctx, db, portee, modele, EtatActif, nil, listes, "admin", t0); err != nil {
			t.Fatal(err)
		}
	}
	def(PorteeGroupe("web"), "serveur-web", map[string][]string{"adresses": {"https://a.exemple/", "https://b.exemple/x?y=1"}})
	def(PorteeGroupe("web"), "ports", map[string][]string{"cibles": {"127.0.0.1:5432"}})
	def(PorteeMachine(2), "ports", map[string][]string{"cibles": {}}) // vidée pour la machine 2
	def(PorteeMachine(2), "sauvegarde", map[string][]string{"fichiers": {"/srv/s/*.tar"}, "dossiers": {"/srv/s"}})
	if err := Definir(ctx, db, PorteeMachine(2), "sauvegarde", EtatActif, nil, map[string][]string{"fichiers": {"relatif"}}, "admin", t0); err == nil {
		t.Error("chemin relatif accepté")
	}
	if err := Definir(ctx, db, PorteeMachine(2), "sauvegarde", EtatActif, nil, map[string][]string{"inconnue": {"/a"}}, "admin", t0); err == nil {
		t.Error("liste inconnue acceptée")
	}

	checksDe := func(machine int64) []config.Check {
		t.Helper()
		effs, err := Effectifs(ctx, db, machine, "web")
		if err != nil {
			t.Fatal(err)
		}
		cm, err := ConfigEffective(effs)
		if err != nil {
			t.Fatal(err)
		}
		var cs []config.Check
		if b := cm["checks"]; b != nil {
			if err := json.Unmarshal(b, &cs); err != nil {
				t.Fatal(err)
			}
		}
		if err := config.ValiderChecks(cs); err != nil {
			t.Errorf("checks refusés par l'agent : %v", err)
		}
		return cs
	}
	c1 := checksDe(1)
	if len(c1) != 3 || c1[0].Modele != "ports" || c1[1].Nom != "a.exemple" || c1[2].Nom != "b.exemple/x_y_1" {
		t.Errorf("machine 1 : %+v", c1)
	}
	c2 := checksDe(2)
	if len(c2) != 4 { // ports vidé, sauvegarde ×2, web ×2
		t.Errorf("machine 2 : %+v", c2)
	}
	for _, c := range c2 {
		if c.Modele == "ports" {
			t.Error("liste vidée pour la machine : encore testée")
		}
	}
	effs, _ := Effectifs(ctx, db, 2, "web")
	for _, e := range effs {
		if e.Modele.Nom == "ports" && (!e.ListesMach["cibles"] || len(e.Listes["cibles"]) != 0) {
			t.Errorf("ports machine 2 : %+v", e)
		}
	}
	// Noms en double dans un modèle : rendus uniques.
	def(PorteeMachine(3), "serveur-web", map[string][]string{"adresses": {"https://c.exemple/", "http://c.exemple/"}})
	c3 := checksDe(3)
	if len(c3) != 3 || c3[1].Nom != "c.exemple" || c3[2].Nom != "c.exemple-2" {
		t.Errorf("machine 3 : %+v", c3)
	}
	if n, e := NombreChecks(effs); n != 4 || e != 0 {
		t.Errorf("NombreChecks : %d %d", n, e)
	}
}
