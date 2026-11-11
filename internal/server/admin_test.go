// SPDX-License-Identifier: GPL-3.0-or-later

package server

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mirador-stack/internal/agent/logs"
	"mirador-stack/internal/agent/metrics"
	"mirador-stack/internal/agent/remotewrite"
	"mirador-stack/internal/protocole"
	srvconfig "mirador-stack/internal/server/config"
	"mirador-stack/internal/server/flotte"
)

func TestConsultation(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	cle := filepath.Join(dir, "cle")
	_ = os.WriteFile(cle, []byte("AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=\n"), 0o400)
	cfg := srvconfig.Defaut()
	cfg.EcouteAgents, cfg.EcouteInterface = portLibre(t), portLibre(t)
	cfg.Donnees, cfg.FichierCle = filepath.Join(dir, "d"), cle
	s, err := Demarrer(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Arreter()
	if info, err := os.Stat(filepath.Join(cfg.Donnees, nomSocket)); err != nil || info.Mode().Perm() != 0o600 || info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("socket : %v %v", info, err)
	}
	token, _ := flotte.TokenManuel(ctx, s.db, "srv-test-mirador", "test", time.Now())
	envoyer := func(chemin, enc string, corps []byte) {
		req, _ := http.NewRequest(http.MethodPost, "http://"+cfg.EcouteAgents+chemin, bytes.NewReader(corps))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Encoding", enc)
		resp, err := http.DefaultClient.Do(req)
		if err != nil || resp.StatusCode != http.StatusNoContent {
			t.Fatalf("%s : %v %v", chemin, err, resp)
		}
		resp.Body.Close()
	}
	lot := metrics.NewBatch(2, nil)
	lot.Reset(time.Now().UnixMilli())
	lot.Add("node_load1", 1.5)
	lot.Add("node_filesystem_avail_bytes", 42, metrics.L("mountpoint", "/"))
	var enc remotewrite.Encoder
	envoyer(protocole.CheminMetriques, "snappy", enc.Encode(nil, lot))
	ll := logs.NewLot("x", "journal", "", "", 1<<20)
	ll.Ajouter(time.Now(), "Failed password for root", logs.Champs{{"unit", "ssh.service"}, {"level", "warning"}})
	ll.Ajouter(time.Now(), "session opened", logs.Champs{{"unit", "cron.service"}, {"level", "info"}})
	corps, _ := ll.Vider()
	envoyer(protocole.CheminLogs, "gzip", corps)

	commande := func(args ...string) (int, string) {
		var so, se bytes.Buffer
		code := Commande(ctx, cfg, args, &so, &se)
		return code, so.String() + se.String()
	}
	code, sortie := commande("voir-metriques", "-machine", "srv-test-mirador", "node_filesystem_avail_bytes")
	if code != 0 || !strings.Contains(sortie, `mountpoint="/"`) || !strings.Contains(sortie, "42") || !strings.Contains(sortie, "1 séries") {
		t.Fatalf("voir-metriques :\n%s", sortie)
	}
	code, sortie = commande("voir-logs", "-niveau", "warning")
	if code != 0 || !strings.Contains(sortie, "Failed password for root") || strings.Contains(sortie, "session opened") ||
		!strings.Contains(sortie, "srv-test-mirador ssh.service [warning]") {
		t.Fatalf("voir-logs :\n%s", sortie)
	}
	if code, _ := commande("voir-metriques", "nom invalide{}"); code == 0 {
		t.Error("nom invalide accepté")
	}
	if code, _ := commande("voir-logs", "-depuis", "9999h"); code == 0 {
		t.Error("période trop longue acceptée")
	}
}
