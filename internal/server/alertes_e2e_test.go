// SPDX-License-Identifier: GPL-3.0-or-later

package server

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mirador-stack/internal/agent/metrics"
	"mirador-stack/internal/agent/remotewrite"
	"mirador-stack/internal/protocole"
	srvconfig "mirador-stack/internal/server/config"
	"mirador-stack/internal/server/flotte"
	"mirador-stack/internal/server/notif"
)

// Bout en bout, partie 1 de l'étape 4 : import des règles actuelles par la
// commande, données d'un agent, évaluation, alerte visible par les commandes,
// métriques internes.
func TestAlertesDeBoutEnBout(t *testing.T) {
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
	commande := func(args ...string) (int, string) {
		var so, se bytes.Buffer
		code := Commande(ctx, cfg, args, &so, &se)
		return code, so.String() + se.String()
	}

	// Règles mirador posées au démarrage.
	code, sortie := commande("regles")
	if code != 0 || !strings.Contains(sortie, "OomKiller") || !strings.Contains(sortie, "SauvegardeTropAncienne") {
		t.Fatalf("regles :\n%s", sortie)
	}
	// Import des règles actuelles : essai, puis pour de bon.
	for _, f := range []string{"system.yml", "equipements.yml"} {
		chemin := filepath.Join("alertes", "testdata", "regles", f)
		code, sortie = commande("regles-importer", "-essai", chemin)
		if code != 0 || !strings.Contains(sortie, "rien n'a été enregistré") {
			t.Fatalf("essai %s :\n%s", f, sortie)
		}
		code, sortie = commande("regles-importer", chemin)
		if code != 0 || !strings.Contains(sortie, "enregistrée(s)") {
			t.Fatalf("import %s :\n%s", f, sortie)
		}
	}
	code, sortie = commande("regles")
	if code != 0 || !strings.Contains(sortie, "81 règle(s), 80 active(s)") {
		t.Fatalf("après import :\n%s", sortie)
	}
	if code, sortie = commande("regle-desactiver", "TemperatureElevee"); code != 0 {
		t.Fatal(sortie)
	}
	code, export := commande("regles-exporter")
	if code != 0 || !strings.Contains(export, "version: 1") || !strings.Contains(export, "nom: AgentMuet") {
		t.Fatalf("export :\n%s", export)
	}

	// Un agent envoie une machine en mémoire faible (MemoireFaible : < 10 %).
	token, _ := flotte.TokenManuel(ctx, s.db, "pc1", "test", time.Now())
	lot := metrics.NewBatch(4, nil)
	lot.Reset(time.Now().UnixMilli())
	lot.Add("node_memory_MemAvailable_bytes", 1e8)
	lot.Add("node_memory_MemTotal_bytes", 4e9)
	lot.Add("node_uname_info", 1)
	var enc remotewrite.Encoder
	req, _ := http.NewRequest(http.MethodPost, "http://"+cfg.EcouteAgents+protocole.CheminMetriques, bytes.NewReader(enc.Encode(nil, lot)))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Encoding", "snappy")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusNoContent {
		t.Fatalf("envoi : %v %v", err, resp)
	}
	resp.Body.Close()

	// Le serveur relit les règles (10 s au plus) ; on force ici, puis on
	// évalue deux fois : en attente (« pour » 10 min), puis en alerte
	// (après le délai de démarrage, dans la fenêtre de 5 min des données).
	if err := s.alertes.Recharger(ctx); err != nil {
		t.Fatal(err)
	}
	s.alertes.EvaluerTout(ctx, time.Now())
	// 4 min sans nouvelle donnée : seule AgentMuet part (lag > 60 s).
	evs := s.alertes.EvaluerTout(ctx, time.Now().Add(4*time.Minute))
	if len(evs) != 1 || evs[0].Alerte.Regle != "AgentMuet" || evs[0].Alerte.Machine() != "pc1" ||
		!strings.Contains(evs[0].Alerte.Resume, "pc1 ne remonte plus") {
		t.Fatalf("4 min plus tard : %+v", evs)
	}
	code, sortie = commande("alertes-en-cours")
	if code != 0 || !strings.Contains(sortie, "en_attente  warning   MemoireFaible") || !strings.Contains(sortie, "pc1") {
		t.Fatalf("alertes-en-cours :\n%s", sortie)
	}
	code, sortie = commande("etat-regles")
	if code != 0 || !strings.Contains(sortie, "MemoireFaible") || strings.Contains(sortie, "parse error") {
		t.Fatalf("etat-regles :\n%s", sortie)
	}

	// Métriques internes : écrites puis lisibles comme les autres.
	(&interne{s: s}).tour(ctx, time.Now())
	code, sortie = commande("voir-metriques", "-machine", InstanceServeur, "mirador_sauvegarde_derniere_reussie_timestamp_seconds")
	if code != 0 || !strings.Contains(sortie, "1 séries") {
		t.Fatalf("métriques internes :\n%s", sortie)
	}
	code, sortie = commande("voir-metriques", "-machine", InstanceServeur, "mirador_disque_libre_octets")
	if code != 0 || !strings.Contains(sortie, "1 séries") {
		t.Fatalf("disque :\n%s", sortie)
	}
	code, sortie = commande("alertes", "-limite", "5")
	if code != 0 || !strings.Contains(sortie, "MemoireFaible") {
		t.Fatalf("historique :\n%s", sortie)
	}

	// Canal Telegram (jeton factice), puis sourdine et notifications.
	reglages := filepath.Join(dir, "notif.json")
	_, notifJSON := commande("notif")
	notifJSON = strings.Replace(notifJSON, `"canaux": {`, `"canaux": {
    "telegram": {"actif": true, "jeton": "123456789:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", "chat": "-1001234"},`, 1)
	if err := os.WriteFile(reglages, []byte(notifJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, sortie := commande("mode", "actif"); code == 0 {
		t.Fatalf("mode actif sans canal accepté : %s", sortie)
	}
	if code, sortie := commande("notif-reglages", reglages); code != 0 {
		t.Fatalf("notif-reglages : %s", sortie)
	}
	// Le serveur relit les réglages à son tour suivant (5 s) ; forcé ici.
	// Aucun accès réseau : l'API Telegram pointe sur une adresse refusée.
	notif.APITelegram = "http://127.0.0.1:1"
	s.postier.Tour(ctx)
	for _, c := range []struct {
		args    []string
		attendu string
	}{
		{[]string{"silence", "-pendant", "3h", "-commentaire", "travaux", `{instance="pc1"}`}, "silence n° 1"},
		{[]string{"silences"}, `{instance="pc1"}`},
		{[]string{"maintenance", "-jours", "dim", "-heure", "03:00", "-pendant", "2h", "groupe:proxmox"}, "maintenance n° 1"},
		{[]string{"maintenances"}, "chaque dim à 03:00 pendant 2h"},
		{[]string{"inhibitions"}, `{alertname="SynologyVolumeDegrade"}`},
		{[]string{"dependances"}, "miradorcollector"},
		{[]string{"dependance", "groupe:vms", "pve1"}, "dépendance n° 2"},
		{[]string{"notif"}, `"mode": "ombre"`},
		{[]string{"mode", "actif"}, "fait"},
		{[]string{"notif"}, `"jeton": "***"`},
		{[]string{"notif-etat"}, "telegram"},
		{[]string{"silence-annuler", "1"}, "fait"},
		{[]string{"envois"}, "QUAND"},
		{[]string{"alertes-en-cours"}, "Mode des notifications"},
	} {
		code, sortie := commande(c.args...)
		if code != 0 || !strings.Contains(sortie, c.attendu) {
			t.Fatalf("%v :\n%s", c.args, sortie)
		}
	}
	for _, mauvais := range [][]string{
		{"silence", "instance=pc1"},
		{"maintenance", "-jours", "lundi", "-heure", "03:00", "machine:x"},
		{"mode", "fort"},
		{"acquitter", "999"},
		{"notif-tester", "email"}, // non configuré : échec rapporté par le serveur
	} {
		if code, sortie := commande(mauvais...); code == 0 {
			t.Errorf("%v accepté : %s", mauvais, sortie)
		}
	}

	// Comparaison avec un faux vmalert.
	vm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"data":{"alerts":[{"name":"AgentMuet","state":"firing","labels":{"instance":"pc1"}},
			{"name":"CpuEleve","state":"firing","labels":{"instance":"pc1"}}]}}`)
	}))
	defer vm.Close()
	fvm := filepath.Join(dir, "vmalert.json")
	_ = os.WriteFile(fvm, []byte(`{"actif": true, "url": "`+vm.URL+`", "suffixe": "-mirador"}`), 0o600)
	if code, sortie := commande("vmalert-reglages", fvm); code != 0 {
		t.Fatalf("vmalert-reglages : %s", sortie)
	}
	code, sortie = commande("comparer-vmalert")
	if code != 0 || !strings.Contains(sortie, "1 commune(s), 1 manquante(s)") {
		t.Fatalf("comparer-vmalert :\n%s", sortie)
	}
	(&interne{s: s}).comparerVmalert(ctx, time.Now())
	code, sortie = commande("comparer-vmalert", "-csv")
	if code != 0 || !strings.Contains(sortie, "CpuEleve,pc1,manquante") {
		t.Fatalf("csv :\n%s", sortie)
	}
}
