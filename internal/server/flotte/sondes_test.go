// SPDX-License-Identifier: GPL-3.0-or-later

package flotte

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	essais "mirador-stack/internal/sondes"
)

// Sondes de services (SPEC § 48) : liste vérifiée, part de chaque
// collecteur dans sa configuration, clé retirée pour un collecteur ancien.
func TestSondesCollecteur(t *testing.T) {
	b := nouveauBanc(t)
	jeton, err := CreerJetonEtiquettes(ctx, b.db, time.Hour, Etiquettes{Nom: "lan", TypeMachine: "collecteur"}, "admin", t0)
	if err != nil {
		t.Fatal(err)
	}
	b.enroler(t, jeton, idA, "lan", "198.51.100.4")
	if err := Approuver(ctx, b.db, "lan", false, "admin", t0); err != nil {
		t.Fatal(err)
	}
	liste := []essais.Sonde{
		{Nom: "HA", Type: essais.HTTP, Cible: "https://ha.lan:8123/", Depuis: "lan", NonVerifie: true, Machine: "ha"},
		{Nom: "Nextcloud", Type: essais.HTTP, Cible: "https://cloud.exemple.fr/status.php", Depuis: essais.ServeurLocal},
	}
	if err := EnregistrerSondes(ctx, b.db, append(liste, essais.Sonde{Nom: "X", Type: essais.TCP, Cible: "a:1", Depuis: "inconnu"}), "admin", t0); err == nil {
		t.Error("collecteur inconnu accepté")
	}
	if err := EnregistrerSondes(ctx, b.db, append(liste, liste[0]), "admin", t0); err == nil {
		t.Error("nom en double accepté")
	}
	if err := EnregistrerSondes(ctx, b.db, liste, "admin", t0); err != nil {
		t.Fatal(err)
	}
	id, _ := IDMachine(ctx, b.db, "lan")
	doc, _, err := ConfigMachine(ctx, b.db, id)
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Collecteur struct {
			Services []essais.Sonde `json:"services"`
		} `json:"collecteur"`
	}
	if err := json.Unmarshal(doc, &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Collecteur.Services) != 1 || cfg.Collecteur.Services[0].Nom != "HA" || !cfg.Collecteur.Services[0].NonVerifie {
		t.Fatalf("part du collecteur : %s", doc)
	}
	// La machine sondée reste au serveur (un ancien collecteur refuserait la clé).
	if strings.Contains(string(doc), `"machine"`) {
		t.Errorf("clé machine envoyée au collecteur : %s", doc)
	}
	if l, _ := LireSondes(ctx, b.db); len(l) != 2 || l[0].Machine != "ha" {
		t.Errorf("machine non gardée : %+v", l)
	}
	if cs, err := CollecteursSites(ctx, b.db); err != nil || len(cs) != 1 || cs[0].Nom != "lan" {
		t.Errorf("collecteurs et sites : %+v %v", cs, err)
	}
	// Collecteur ancien : clé retirée, le reste intact.
	sans, etag, err := sansServices(doc)
	if err != nil || strings.Contains(string(sans), `"services"`) || etag == "" || !strings.Contains(string(sans), `"collecteur"`) {
		t.Fatalf("sans services : %s %v", sans, err)
	}
	// Audit.
	var n int
	_ = b.db.QueryRow(`SELECT count(*) FROM audit WHERE action = 'sondes'`).Scan(&n)
	if n != 1 {
		t.Errorf("audit : %d lignes", n)
	}
}
