// SPDX-License-Identifier: GPL-3.0-or-later

package ingestion

import (
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

var reNomMirador = regexp.MustCompile(`mirador_[a-z0-9_]+`)

// nomsDansSources : noms mirador_* cités dans les fichiers Go (hors tests)
// des dossiers donnés, relatifs à ce paquet.
func nomsDansSources(t *testing.T, dossiers ...string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, d := range dossiers {
		err := filepath.WalkDir(d, func(chemin string, e fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if e.IsDir() || !strings.HasSuffix(chemin, ".go") || strings.HasSuffix(chemin, "_test.go") {
				return nil
			}
			b, err := os.ReadFile(chemin) // #nosec G304 -- sources du dépôt, en test
			if err != nil {
				return err
			}
			for _, n := range reNomMirador.FindAllString(string(b), -1) {
				out[n] = chemin
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return out
}

// Inventaire (revue du 28/11, H1) : aucun nom mirador_* émis par l'agent,
// le collecteur ou les sondes ne doit être refusé. Ce test échoue si un
// nouveau nom de l'agent tombe dans la liste réservée au serveur.
func TestNomsReservesHorsAgentEtCollecteur(t *testing.T) {
	noms := nomsDansSources(t, "../../agent", "../../collecteur", "../../sondes", "../../protocole")
	if len(noms) < 50 {
		t.Fatalf("inventaire trop petit (%d noms) : dossiers déplacés ?", len(noms))
	}
	for n, f := range noms {
		if nomReserve(n) {
			t.Errorf("%s (émis dans %s) est dans la liste réservée au serveur", n, f)
		}
	}
}

// Inventaire, dans l'autre sens : chaque nom écrit par le serveur (métriques
// internes, tâches suivies) est réservé, sauf ceux que l'agent écrit aussi.
func TestNomsEcritsParLeServeurReserves(t *testing.T) {
	reEcrit := regexp.MustCompile(`(?:ajout\(|Nom: )"(mirador_[a-z0-9_]+)"`)
	var ecrits []string
	for _, f := range []string{"../interne.go", "../taches/releves.go"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range reEcrit.FindAllStringSubmatch(string(b), -1) {
			ecrits = append(ecrits, m[1])
		}
	}
	if len(ecrits) < 25 {
		t.Fatalf("inventaire trop petit (%d noms) : interne.go a changé ?", len(ecrits))
	}
	// Constantes des autres paquets, écrites par interne.go.
	ecrits = append(ecrits, "mirador_pve_invite_exclue", "mirador_domaine_expiration_timestamp_seconds",
		"mirador_ssh_adresse_nouvelle", "mirador_machine_derniere_metrique_timestamp_seconds")
	// Écrits aussi par l'agent (tâches détectées) : le serveur ne les écrit
	// que pour les tâches « ping », qui n'ont pas d'agent.
	partages := map[string]bool{"mirador_tache_fin_timestamp_seconds": true, "mirador_tache_code": true,
		"mirador_tache_debut_timestamp_seconds": true}
	sort.Strings(ecrits)
	for _, n := range ecrits {
		if !partages[n] && !nomReserve(n) {
			t.Errorf("%s est écrit par le serveur mais pas réservé (reserves.go)", n)
		}
	}
}

// Coût du contrôle par série (mesure pour PROGRES.md) :
//
//	go test -run '^$' -bench NomReserve ./internal/server/ingestion/
func BenchmarkNomReserve(b *testing.B) {
	noms := []string{"node_load1", "mirador_agent_info", "mirador_tache_code", "mirador_modele_parametre"}
	for i := 0; i < b.N; i++ {
		nomReserve(noms[i%len(noms)])
	}
}

// Agent et collecteur : séries au nom réservé refusées et comptées, le
// reste du lot rangé (204).
func TestSeriesReserveesRefusees(t *testing.T) {
	b := nouveauBanc(t, nil)
	tps := reception.UnixMilli()
	corps := pbRequete(
		pbSerie([][2]string{{"__name__", "mirador_modele_parametre"}, {"modele", "linux"}, {"parametre", "disque_libre_pct"}, {"x", "1"}}, pbPoint(1, tps)),
		pbSerie([][2]string{{"__name__", "mirador_domaine_expiration_timestamp_seconds"}, {"domaine", "exemple.fr"}}, pbPoint(4e9, tps)),
		pbSerie([][2]string{{"__name__", "mirador_tache_suivie"}}, pbPoint(1, tps)),
		pbSerie([][2]string{{"__name__", "node_load1"}}, pbPoint(0.5, tps)),
		pbSerie([][2]string{{"__name__", "mirador_tache_code"}, {"tache", "a"}}, pbPoint(0, tps)), // émis par l'agent : gardé
	)
	if c := b.metriques(tokenPC, corps); c != http.StatusNoContent {
		t.Fatal(c)
	}
	if len(b.mem.Series) != 2 || b.g.Compteurs.SeriesReservees.Load() != 3 {
		t.Fatalf("%d séries rangées, %d refusées", len(b.mem.Series), b.g.Compteurs.SeriesReservees.Load())
	}
	for _, s := range b.mem.Series {
		if n := s.Valeur("__name__"); n != "node_load1" && n != "mirador_tache_code" {
			t.Errorf("série réservée rangée : %s", n)
		}
	}
	// Collecteur : même règle.
	corps = pbRequete(pbSerie([][2]string{{"__name__", "mirador_service_etat"}, {"service", "paie"}}, pbPoint(0, tps)))
	if c := b.metriques(tokenCollecteur, corps); c != http.StatusNoContent {
		t.Fatal(c)
	}
	if len(b.mem.Series) != 2 || b.g.Compteurs.SeriesReservees.Load() != 4 {
		t.Fatalf("collecteur : %d séries rangées", len(b.mem.Series))
	}
	// Un lot fait seulement de séries réservées ne compte pas comme une
	// réception (rien n'a été rangé).
	if _, ok := b.g.DerniereReception(4); ok {
		t.Error("collecteur noté « reçu » sans rien de rangé")
	}
}

// Points plus vieux que la fenêtre de retard (24 h) : écartés et comptés,
// sans faire passer la machine pour « vue ».
func TestPointsAnciens(t *testing.T) {
	b := nouveauBanc(t, nil)
	vieux := reception.Add(-25 * time.Hour).UnixMilli()
	corps := pbRequete(pbSerie([][2]string{{"__name__", "node_load1"}}, pbPoint(1, vieux)))
	if c := b.metriques(tokenPC, corps); c != http.StatusNoContent {
		t.Fatal(c)
	}
	if len(b.mem.Series) != 0 || b.g.Compteurs.PointsAnciens.Load() != 1 {
		t.Fatalf("point ancien : %d séries, %d comptés", len(b.mem.Series), b.g.Compteurs.PointsAnciens.Load())
	}
	if _, ok := b.g.DerniereReception(1); ok {
		t.Error("machine notée « reçue » avec un point trop ancien")
	}
	// 23 h : dans la fenêtre (file d'attente d'un agent), gardé.
	corps = pbRequete(pbSerie([][2]string{{"__name__", "node_load1"}},
		pbPoint(1, reception.Add(-23*time.Hour).UnixMilli()), pbPoint(2, vieux)))
	if c := b.metriques(tokenPC, corps); c != http.StatusNoContent {
		t.Fatal(c)
	}
	if len(b.mem.Series) != 1 || len(b.mem.Series[0].Points) != 1 || b.g.Compteurs.PointsAnciens.Load() != 2 {
		t.Fatalf("point de 23 h : %+v", b.mem.Series)
	}
}
