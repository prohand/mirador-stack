// SPDX-License-Identifier: GPL-3.0-or-later

package collecteur

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"mirador-stack/internal/agent/config"
	"mirador-stack/internal/agent/envoi"
	"mirador-stack/internal/agent/metrics"
	"mirador-stack/internal/agent/remotewrite"
	"mirador-stack/internal/collecteur/snmp"
	"mirador-stack/internal/collecteur/sondes"
	"mirador-stack/internal/collecteur/unifi"
	essais "mirador-stack/internal/sondes"
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
	// Sondes de services (SPEC § 48) : une tâche par sonde, séries sous le
	// nom du collecteur (instance posée par le serveur), job="sondes".
	for _, s := range cfg.Collecteur.Services {
		wg.Add(1)
		go func() {
			defer wg.Done()
			essais.Tourner(ctx, s, essais.Options{}, func(s essais.Sonde, r essais.Resultat) {
				envoyer(LotSonde(s, r, time.Now()))
			})
		}()
	}
	wg.Wait()
}

// LotSonde : séries d'un essai (job="sondes", sonde, type, cible).
func LotSonde(s essais.Sonde, r essais.Resultat, t time.Time) *metrics.Batch {
	base := []metrics.Label{metrics.L("job", "sondes")}
	for _, e := range essais.Etiquettes(s) {
		base = append(base, metrics.L(e[0], e[1]))
	}
	b := metrics.NewBatch(4, base)
	b.Reset(t.UnixMilli())
	for nom, v := range essais.Valeurs(s, r) {
		b.Add(nom, v)
	}
	if !r.OK { // raison de l'échec (page Sondes)
		b.Add("mirador_sonde_erreur", 1, metrics.L("erreur", r.Erreur))
	}
	return b
}
