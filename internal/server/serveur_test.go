// SPDX-License-Identifier: GPL-3.0-or-later

package server

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/prometheus/prometheus/model/labels"

	"mirador-stack/internal/agent/envoi"
	"mirador-stack/internal/agent/logs"
	"mirador-stack/internal/agent/metrics"
	"mirador-stack/internal/agent/remotewrite"
	"mirador-stack/internal/protocole"
	"mirador-stack/internal/server/config"
	"mirador-stack/internal/server/ingestion"
	"mirador-stack/internal/server/stockage"
	"mirador-stack/internal/server/stockage/journaux"
	"mirador-stack/internal/server/stockage/metriques"
)

// portLibre réserve un port local libre.
func portLibre(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func TestDemarrerArreter(t *testing.T) {
	dir := t.TempDir()
	cle := filepath.Join(dir, "cle")
	if err := os.WriteFile(cle, []byte("AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaut()
	cfg.EcouteAgents, cfg.EcouteInterface = portLibre(t), portLibre(t)
	cfg.Donnees, cfg.FichierCle = filepath.Join(dir, "donnees"), cle

	s, err := Demarrer(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Arreter()

	client := &http.Client{Timeout: 3 * time.Second}
	for _, adr := range []string{cfg.EcouteAgents, cfg.EcouteInterface} {
		resp, err := client.Get("http://" + adr + "/sante")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s/sante : %d", adr, resp.StatusCode)
		}
	}
	// Réception sans token : refusée.
	resp, err := client.Post("http://"+cfg.EcouteAgents+"/api/agent/v1/metrics", "application/x-protobuf", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("métriques sans token : %d, attendu 401", resp.StatusCode)
	}
	// Chemin inconnu de l'API des agents : 501.
	resp, err = client.Post("http://"+cfg.EcouteAgents+"/api/agent/v1/inconnu", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("chemin inconnu : %d, attendu 501", resp.StatusCode)
	}
	// L'API des agents n'est pas servie sur l'écoute de l'interface : la
	// console demande une session (401), sans jamais traiter les données.
	resp, err = client.Post("http://"+cfg.EcouteInterface+"/api/agent/v1/metrics", "application/x-protobuf", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("l'interface ne doit pas servir l'API des agents : %d", resp.StatusCode)
	}
}

// TestAgentVersServeur : le code d'envoi de l'agent (mêmes en-têtes, même
// token) parle au vrai serveur, de bout en bout, jusqu'au stockage.
func TestAgentVersServeur(t *testing.T) {
	dir := t.TempDir()
	cle := filepath.Join(dir, "cle")
	if err := os.WriteFile(cle, []byte("AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaut()
	cfg.EcouteAgents, cfg.EcouteInterface = portLibre(t), portLibre(t)
	cfg.Donnees, cfg.FichierCle = filepath.Join(dir, "donnees"), cle
	journal := slog.New(slog.NewTextHandler(io.Discard, nil))
	s, err := Demarrer(context.Background(), cfg, journal)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Arreter()

	// Machine approuvée et son token (empreinte seulement en base).
	const token = "token-de-test-bout-en-bout-0123"
	e := ingestion.Empreinte(token)
	if _, err := s.db.Exec(`INSERT INTO machines(id, nom, statut, cree_le) VALUES (1, 'srv-test-mirador', 'approuvee', 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO tokens(machine_id, empreinte, cree_le) VALUES (1, ?, 1)`, e[:]); err != nil {
		t.Fatal(err)
	}

	lot := metrics.NewBatch(2, []metrics.Label{metrics.L("instance", "srv-test-mirador"), metrics.L("job", "integrations/unix")})
	lot.Reset(time.Now().UnixMilli())
	lot.Add("node_load1", 0.25)
	var enc remotewrite.Encoder
	fileM := envoi.NewFile(1 << 20)
	fileM.Ajouter(enc.Encode(nil, lot))
	lotL := logs.NewLot("srv-test-mirador", "journal", "", "", 1<<20)
	lotL.Ajouter(time.Now(), "bonjour", logs.Champs{{"unit", "ssh.service"}})
	corpsL, _ := lotL.Vider()
	fileL := envoi.NewFile(1 << 20)
	fileL.Ajouter(corpsL)

	ctx, annuler := context.WithTimeout(context.Background(), 10*time.Second)
	defer annuler()
	for _, x := range []struct {
		file    *envoi.File
		chemin  string
		entetes map[string]string
	}{
		{fileM, protocole.CheminMetriques, map[string]string{"Content-Type": "application/x-protobuf", "Content-Encoding": "snappy"}},
		{fileL, protocole.CheminLogs, map[string]string{"Content-Type": "application/stream+json", "Content-Encoding": "gzip"}},
	} {
		exp := envoi.NewExpediteur(x.file, envoi.Options{URL: "http://" + cfg.EcouteAgents + x.chemin, Token: token,
			Entetes: x.entetes, Journal: journal})
		go exp.Tourner(ctx)
	}
	// Lecture dans les vrais moteurs de stockage.
	lire := func() ([]stockage.Serie, []stockage.Ligne) {
		maintenant := time.Now()
		series, err := s.met.Lire(context.Background(), maintenant.Add(-time.Minute).UnixMilli(), maintenant.UnixMilli(),
			metriques.LimitesLecture{MaxSeries: 10, MaxPoints: 100}, labels.MustNewMatcher(labels.MatchEqual, "__name__", "node_load1"))
		if err != nil {
			t.Fatal(err)
		}
		res, err := s.jx.Chercher(context.Background(), journaux.Requete{Debut: maintenant.Add(-time.Minute).UnixMilli(),
			Fin: maintenant.UnixMilli(), Champs: map[string]string{"host": "srv-test-mirador"}}, journaux.LimitesRechercheDefaut())
		if err != nil {
			t.Fatal(err)
		}
		return series, res.Lignes
	}
	var series []stockage.Serie
	var lignes []stockage.Ligne
	for ctx.Err() == nil {
		if series, lignes = lire(); len(series) == 1 && len(lignes) == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	annuler()
	if len(series) != 1 || series[0].Valeur("instance") != "srv-test-mirador" || series[0].Points[0].V != 0.25 {
		t.Errorf("séries stockées : %+v", series)
	}
	if len(lignes) != 1 || lignes[0].Msg != "bonjour" || lignes[0].Champs["unit"] != "ssh.service" {
		t.Errorf("lignes stockées : %+v", lignes)
	}
	if d, ok := s.ing.Decalage(1); !ok || d > time.Second || d < -time.Second {
		t.Errorf("décalage d'horloge mesuré : %v (en-tête X-Mirador-Heure attendu)", d)
	}
}

func TestCleAbsente(t *testing.T) {
	cfg := config.Defaut()
	cfg.FichierCle = filepath.Join(t.TempDir(), "absente")
	if _, err := Demarrer(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil))); err == nil {
		t.Error("le serveur ne doit pas démarrer sans clé des secrets")
	}
}
