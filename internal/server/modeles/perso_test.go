// SPDX-License-Identifier: GPL-3.0-or-later

package modeles

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"mirador-stack/internal/server/alertes"
	"mirador-stack/internal/server/base"
)

func basePerso(t *testing.T) *sql.DB {
	t.Helper()
	ctx := context.Background()
	db, err := base.Ouvrir(filepath.Join(t.TempDir(), "mirador.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Close()
		muPerso.Lock()
		perso = map[string]*Modele{}
		muPerso.Unlock()
		majRemplacees()
	})
	if _, err := base.Migrer(ctx, db, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if _, err := alertes.PoserDefauts(ctx, db, t0); err != nil {
		t.Fatal(err)
	}
	if err := ChargerPerso(ctx, db); err != nil {
		t.Fatal(err)
	}
	return db
}

const appliJSON = `{"version": 1, "nom": "mon-appli", "titre": "Mon appli",
 "listes": [{"nom": "adresses", "titre": "Adresses", "type": "http"}],
 "parametres": [{"nom": "temps_max", "titre": "Temps max", "unite": "ms", "defaut": 500, "min": 10, "max": 10000}],
 "regles": [
  {"nom": "MonAppliEnPanne", "gravite": "critical", "pour": "2m", "expr": "mirador_check_ok{modele=\"mon-appli\"} == 0"},
  {"nom": "MonAppliLente", "gravite": "warning", "remplace": "RegleImporteeX", "expr": "mirador_check_duree_secondes{modele=\"mon-appli\"} * 1000 > $temps_max"}
 ]}`

func reglesDe(t *testing.T, db *sql.DB, origine string) map[string]alertes.Regle {
	t.Helper()
	rs, err := alertes.ListerRegles(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]alertes.Regle{}
	for _, r := range rs {
		if r.Origine == origine {
			out[r.Nom] = r
		}
	}
	return out
}

func TestModelePerso(t *testing.T) {
	ctx := context.Background()
	db := basePerso(t)
	m, err := DecoderModele([]byte(appliJSON))
	if err != nil {
		t.Fatal(err)
	}
	if err := EnregistrerPerso(ctx, db, m, "admin", t0); err != nil {
		t.Fatal(err)
	}
	if tr := Trouver("mon-appli"); tr == nil || !tr.Perso {
		t.Fatal("modèle personnel introuvable")
	}
	if !EstRemplacee("RegleImporteeX") {
		t.Error("remplacement du modèle personnel non pris en compte")
	}
	rs := reglesDe(t, db, "modele:mon-appli")
	if len(rs) != 2 || !strings.Contains(rs["MonAppliLente"].Expr, `mirador_modele_parametre{modele="mon-appli", parametre="temps_max"}`) {
		t.Fatalf("règles posées : %+v", rs)
	}
	// Règle désactivée dans « Règles » : le reste après un nouvel
	// enregistrement ; règle retirée du modèle : retirée de la base.
	if err := alertes.Activer(ctx, db, "MonAppliEnPanne", false, "admin", t0); err != nil {
		t.Fatal(err)
	}
	m.Regles = m.Regles[:1]
	m.Titre = "Mon appli (v2)"
	if err := EnregistrerPerso(ctx, db, m, "admin", t0); err != nil {
		t.Fatal(err)
	}
	rs = reglesDe(t, db, "modele:mon-appli")
	if len(rs) != 1 || rs["MonAppliEnPanne"].Active {
		t.Errorf("après modification : %+v", rs)
	}
	if EstRemplacee("RegleImporteeX") {
		t.Error("remplacement retiré mais encore actif")
	}
	// Refus : nom d'un modèle livré, règle au nom déjà pris.
	l := *Trouver("linux")
	l.Perso = false
	if err := EnregistrerPerso(ctx, db, &l, "admin", t0); err == nil {
		t.Error("modèle livré remplacé")
	}
	autre, _ := DecoderModele([]byte(strings.ReplaceAll(strings.ReplaceAll(appliJSON, `"mon-appli"`, `"autre"`), `modele=\"mon-appli\"`, `modele=\"autre\"`)))
	if err := EnregistrerPerso(ctx, db, autre, "admin", t0); err == nil || !strings.Contains(err.Error(), "déjà pris") {
		t.Errorf("règle en double acceptée : %v", err)
	}
	// Règle qui filtre les tests d'un autre modèle : refusée.
	faux, _ := DecoderModele([]byte(strings.NewReplacer(`"mon-appli"`, `"faux"`, "MonAppli", "Faux").Replace(appliJSON)))
	if err := EnregistrerPerso(ctx, db, faux, "admin", t0); err == nil || !strings.Contains(err.Error(), `modele="mon-appli"`) {
		t.Errorf("filtre d'un autre modèle accepté : %v", err)
	}
	// Copie d'un modèle livré : règles renommées, enregistrable.
	c := Copie(Trouver("serveur-web"), "web-perso", "Web perso")
	if c.Regles[0].Nom != "WebInjoignablePerso" || Trouver("serveur-web").Regles[0].Nom != "WebInjoignable" ||
		c.Regles[0].Expr != `mirador_check_ok{modele="web-perso"} == 0` || !strings.Contains(Trouver("serveur-web").Regles[0].Expr, `"serveur-web"`) {
		t.Errorf("copie : %+v", c.Regles)
	}
	if err := EnregistrerPerso(ctx, db, c, "admin", t0); err != nil {
		t.Fatal(err)
	}
	// Suppression : refusée tant qu'il est attribué.
	if err := Definir(ctx, db, PorteeGroupe("web"), "mon-appli", EtatActif, nil, map[string][]string{"adresses": {"https://a.exemple/"}}, "admin", t0); err != nil {
		t.Fatal(err)
	}
	if err := SupprimerPerso(ctx, db, "mon-appli", "admin", t0); err == nil || !strings.Contains(err.Error(), "groupe:web") {
		t.Errorf("suppression d'un modèle attribué : %v", err)
	}
	if err := SupprimerPerso(ctx, db, "linux", "admin", t0); err == nil {
		t.Error("modèle livré supprimé")
	}
	_ = Definir(ctx, db, PorteeGroupe("web"), "mon-appli", "", nil, nil, "admin", t0)
	if err := SupprimerPerso(ctx, db, "mon-appli", "admin", t0); err != nil {
		t.Fatal(err)
	}
	if Trouver("mon-appli") != nil || len(reglesDe(t, db, "modele:mon-appli")) != 0 {
		t.Error("modèle ou règles encore là après suppression")
	}
}

// Chargement : modèle abîmé ou portant le nom d'un modèle livré laissé de
// côté, les autres chargés.
func TestChargerPerso(t *testing.T) {
	ctx := context.Background()
	db := basePerso(t)
	m, _ := DecoderModele([]byte(appliJSON))
	brut, _ := json.Marshal(m)
	for _, l := range [][2]string{{"mon-appli", string(brut)}, {"abime", `{"version": 1, "nom": "abime"`}, {"linux", strings.ReplaceAll(string(brut), "mon-appli", "linux")}} {
		if _, err := db.Exec(`INSERT INTO modeles_perso(nom, definition, modifie_le) VALUES (?, ?, 1)`, l[0], l[1]); err != nil {
			t.Fatal(err)
		}
	}
	err := ChargerPerso(ctx, db)
	if err == nil || !strings.Contains(err.Error(), "abime") || !strings.Contains(err.Error(), "linux") {
		t.Errorf("erreur attendue : %v", err)
	}
	if Trouver("mon-appli") == nil || Trouver("abime") != nil || Trouver("linux").Perso {
		t.Error("chargement")
	}
	if _, err := DecoderModele([]byte(appliJSON + `{}`)); err == nil {
		t.Error("texte en trop accepté")
	}
	if _, err := DecoderModele([]byte(strings.Replace(appliJSON, `"version": 1,`, `"version": 1, "commande": "rm",`, 1))); err == nil {
		t.Error("champ inconnu accepté")
	}
	if _, err := DecoderModele([]byte(strings.Replace(appliJSON, `"titre": "Mon appli",`, `"titre": "Mon appli", "config": {"exec": "sh"},`, 1))); err == nil {
		t.Error("clé de configuration inconnue acceptée")
	}
}
