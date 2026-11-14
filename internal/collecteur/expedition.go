// SPDX-License-Identifier: GPL-3.0-or-later

package collecteur

import (
	"context"
	"log/slog"
	"sync"

	"mirador-stack/internal/agent/config"
	"mirador-stack/internal/agent/envoi"
	"mirador-stack/internal/agent/metrics"
	"mirador-stack/internal/agent/remotewrite"
	"mirador-stack/internal/collecteur/snmp"
	"mirador-stack/internal/collecteur/sondes"
	"mirador-stack/internal/collecteur/unifi"
)

// Expedition : un lot relevé pour un équipement (SNMP, sonde) est encodé
// et mis dans la file d'envoi du collecteur (même remote write).
func Expedition(file *envoi.File, journal *slog.Logger) func(*metrics.Batch) {
	var mu sync.Mutex
	var enc remotewrite.Encoder
	return func(b *metrics.Batch) {
		mu.Lock()
		defer mu.Unlock()
		if !file.Ajouter(enc.Encode(file.Prendre(), b)) {
			journal.Warn("lot d'un équipement trop gros pour le tampon, abandonné", "points", b.Len())
		}
	}
}

// TournerReleves : SNMP (une tâche par équipement), contrôleur UniFi et
// sondes de certificats, jusqu'à l'arrêt de ctx.
func TournerReleves(ctx context.Context, cfg config.Agent, envoyer func(*metrics.Batch), journal *slog.Logger) {
	var wg sync.WaitGroup
	for _, c := range cfg.Collecteur.SNMP {
		r := &snmp.Releveur{Cible: c, Envoyer: envoyer, Journal: journal}
		wg.Add(1)
		go func() { defer wg.Done(); r.Tourner(ctx) }()
	}
	if u := cfg.Collecteur.UniFi; u != nil {
		wg.Add(1)
		go func() { defer wg.Done(); unifi.Tourner(ctx, *u, envoyer, journal) }()
	}
	if len(cfg.Collecteur.Sondes) > 0 {
		wg.Add(1)
		go func() { defer wg.Done(); sondes.Tourner(ctx, cfg.Collecteur.Sondes, envoyer, journal) }()
	}
	wg.Wait()
}
