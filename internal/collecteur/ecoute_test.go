// SPDX-License-Identifier: GPL-3.0-or-later

package collecteur

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"mirador-stack/internal/agent/config"
	"mirador-stack/internal/agent/metrics"
)

// Relecture du 21/11 : écoute impossible (adresse pas encore posée au
// démarrage, DHCP…) : la mesure dit 0, nouvel essai, erreur écrite une fois.
func TestEcoutePerdueReessayee(t *testing.T) {
	var log tamponSur
	journal := slog.New(slog.NewTextHandler(&log, nil))
	cfg := config.DefautCollecteur()
	cfg.Nom, cfg.URLLogs = "collecteur-lan", "https://mirador.exemple.invalid/api/agent/v1/logs"
	cfg.FichierConfigDistante = filepath.Join(t.TempDir(), "config-distante.json")
	cfg.Collecteur = config.Collecteur{Equipements: []string{"nas"}, Syslog: &config.Syslog{Ecoute: "192.168.77.10:16514",
		NomCertificat: "syslog.lan.exemple.org", Emetteurs: []config.Emetteur{{Adresse: "192.168.1.20", Machine: "nas"}}}}
	s, _ := NouveauSyslog(cfg, "token", journal)
	cfg.Collecteur.Syslog.Certificat = signer(t, s.CSR)
	s, err := NouveauSyslog(cfg, "token", journal)
	if err != nil || s.serveur == nil {
		t.Fatal(err)
	}
	s.reessai = 200 * time.Millisecond
	ctx, fin := context.WithCancel(context.Background())
	defer fin()
	go s.Tourner(ctx, ctx)
	time.Sleep(1500 * time.Millisecond)
	b := metrics.NewBatch(16, nil)
	b.Reset(0)
	_ = s.Collecter(b)
	vu := false
	for _, e := range b.Samples[:b.Len()] {
		if e.Name == "mirador_collecteur_syslog_ecoute" {
			vu = true
			if e.Value != 0 {
				t.Errorf("écoute = %v alors qu'elle n'est pas ouverte", e.Value)
			}
		}
	}
	if !vu {
		t.Error("mesure de l'écoute absente")
	}
	if n := strings.Count(log.String(), "nouvel essai toutes les 30 s"); n != 1 {
		t.Errorf("erreur écrite %d fois (une seule attendue) : %s", n, log.String())
	}
}

// tamponSur : journal lu par le test pendant que la boucle d'écoute écrit.
type tamponSur struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (t *tamponSur) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.b.Write(p)
}

func (t *tamponSur) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.b.String()
}
