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

	"github.com/prometheus/prometheus/model/labels"

	"mirador-stack/internal/agent/logs"
	"mirador-stack/internal/agent/metrics"
	"mirador-stack/internal/agent/remotewrite"
	"mirador-stack/internal/protocole"
	srvconfig "mirador-stack/internal/server/config"
	"mirador-stack/internal/server/flotte"
	"mirador-stack/internal/server/sauvegarde"
	"mirador-stack/internal/server/stockage/journaux"
	"mirador-stack/internal/server/stockage/metriques"
)

// TestSauvegardeRestauration : procédure complète demandée par le brief
// (sauvegarde, restauration sur une instance vide, vérification), lancée par
// la CI à chaque envoi.
func TestSauvegardeRestauration(t *testing.T) {
	ancien := sauvegarde.RythmePlanificateur
	sauvegarde.RythmePlanificateur = 100 * time.Millisecond
	defer func() { sauvegarde.RythmePlanificateur = ancien }()
	ctx := context.Background()
	journal := slog.New(slog.NewTextHandler(io.Discard, nil))
	dir := t.TempDir()
	cle := filepath.Join(dir, "cle")
	_ = os.WriteFile(cle, []byte("AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=\n"), 0o400)
	conf := filepath.Join(dir, "server.conf")
	_ = os.WriteFile(conf, []byte("MIRADOR_DONNEES=/var/lib/mirador\n"), 0o600)
	nouvelleConfig := func(donnees string) srvconfig.Serveur {
		c := srvconfig.Defaut()
		c.EcouteAgents, c.EcouteInterface = portLibre(t), portLibre(t)
		c.Donnees, c.FichierCle, c.Fichier = donnees, cle, conf
		return c
	}
	commande := func(cfg srvconfig.Serveur, args ...string) (int, string) {
		var so, se bytes.Buffer
		code := Commande(ctx, cfg, args, &so, &se)
		return code, so.String() + se.String()
	}
	client := &http.Client{Timeout: 5 * time.Second}

	// --- Instance A : des données de toutes sortes.
	cfgA := nouvelleConfig(filepath.Join(dir, "a"))
	a, err := Demarrer(ctx, cfgA, journal)
	if err != nil {
		t.Fatal(err)
	}
	token, err := flotte.TokenManuel(ctx, a.db, "srv-test-mirador", "test", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := flotte.DefinirConfig(ctx, a.db, "defaut", []byte(`{"intervalle":"60s"}`), "test", time.Now()); err != nil {
		t.Fatal(err)
	}
	envoyer := func(base, chemin, encodage string, corps []byte) int {
		req, _ := http.NewRequest(http.MethodPost, "http://"+base+chemin, bytes.NewReader(corps))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Encoding", encodage)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	quand := time.Now().Add(-10 * time.Minute)
	lot := metrics.NewBatch(1, []metrics.Label{metrics.L("instance", "x"), metrics.L("job", "integrations/unix")})
	lot.Reset(quand.UnixMilli())
	lot.Add("node_load1", 0.42)
	var enc remotewrite.Encoder
	if c := envoyer(cfgA.EcouteAgents, protocole.CheminMetriques, "snappy", enc.Encode(nil, lot)); c != http.StatusNoContent {
		t.Fatalf("métriques : %d", c)
	}
	ll := logs.NewLot("x", "journal", "", "", 1<<20)
	ll.Ajouter(quand, "ligne d'avant la sauvegarde", logs.Champs{{"unit", "ssh.service"}})
	corps, _ := ll.Vider()
	if c := envoyer(cfgA.EcouteAgents, protocole.CheminLogs, "gzip", corps); c != http.StatusNoContent {
		t.Fatalf("logs : %d", c)
	}

	// Sauvegarde par la commande, faite par le serveur en marche.
	code, sortie := commande(cfgA, "sauvegarder", "-metriques", "-logs")
	if code != 0 || !strings.Contains(sortie, "Sauvegarde faite") {
		t.Fatalf("sauvegarder : %s", sortie)
	}
	archives, _ := sauvegarde.Lister(sauvegarde.DossierArchives(cfgA.Donnees))
	if len(archives) == 0 {
		t.Fatal("aucune archive")
	}
	archive := archives[0].Chemin
	if code, sortie := commande(cfgA, "sauvegardes"); code != 0 || !strings.Contains(sortie, "ok") || strings.Contains(sortie, "aucune sauvegarde réussie") {
		t.Fatalf("sauvegardes : %s", sortie)
	}
	a.Arreter()
	// L'archive quitte la machine (comme une copie SFTP) avant la « panne ».
	copie := filepath.Join(dir, "copie.sauvegarde")
	brut, _ := os.ReadFile(archive)
	_ = os.WriteFile(copie, brut, 0o600)

	// --- Instance B : neuve. Vérification, puis restauration.
	cfgB := nouvelleConfig(filepath.Join(dir, "b"))
	if code, sortie := commande(cfgB, "verifier-sauvegarde", copie); code != 0 || !strings.Contains(sortie, "métriques, logs") {
		t.Fatalf("verifier-sauvegarde : %s", sortie)
	}
	mauvaiseCle := filepath.Join(dir, "autre-cle")
	_ = os.WriteFile(mauvaiseCle, []byte("AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE=\n"), 0o400)
	cfgFaux := cfgB
	cfgFaux.FichierCle = mauvaiseCle
	if code, _ := commande(cfgFaux, "restaurer", copie); code == 0 {
		t.Fatal("restauration avec une autre clé acceptée")
	}
	if code, sortie := commande(cfgB, "restaurer", copie); code != 0 {
		t.Fatalf("restaurer : %s", sortie)
	}
	if code, _ := commande(cfgB, "restaurer", copie); code == 0 {
		t.Fatal("restauration par-dessus des données acceptée")
	}
	b, err := Demarrer(ctx, cfgB, journal)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Arreter()
	// La machine, sa configuration et son token sont là.
	if c := envoyer(cfgB.EcouteAgents, protocole.CheminMetriques, "snappy", enc.Encode(nil, lot)); c != http.StatusNoContent {
		t.Fatalf("token après restauration : %d", c)
	}
	if code, sortie := commande(cfgB, "voir-config", "srv-test-mirador"); code != 0 || !strings.Contains(sortie, `"intervalle":"60s"`) {
		t.Fatalf("configuration restaurée : %s", sortie)
	}
	// Les métriques et les logs d'avant la sauvegarde aussi.
	series, err := b.met.Lire(ctx, quand.Add(-time.Minute).UnixMilli(), time.Now().UnixMilli(),
		metriques.LimitesLecture{MaxSeries: 10, MaxPoints: 100}, labels.MustNewMatcher(labels.MatchEqual, "__name__", "node_load1"))
	if err != nil || len(series) != 1 || series[0].Points[0].V != 0.42 {
		t.Fatalf("métriques restaurées : %+v %v", series, err)
	}
	res, err := b.jx.Chercher(ctx, journaux.Requete{Debut: quand.Add(-time.Minute).UnixMilli(), Fin: time.Now().UnixMilli(),
		Mots: "avant la sauvegarde"}, journaux.LimitesRechercheDefaut())
	if err != nil || len(res.Lignes) != 1 {
		t.Fatalf("logs restaurés : %d %v", len(res.Lignes), err)
	}
	if _, err := os.Stat(filepath.Join(cfgB.Donnees, "restauration", "server.conf")); err != nil {
		t.Error("fichier de démarrage d'origine absent")
	}
}
