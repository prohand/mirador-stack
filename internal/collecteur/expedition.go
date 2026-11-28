// SPDX-License-Identifier: GPL-3.0-or-later

package collecteur

import (
	"context"
	"log/slog"
	"sync"
	"time"

	interdites "mirador-stack/internal/adresses" // nom court : « adresses » est déjà un type du paquet
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
func TournerReleves(ctx context.Context, cfg config.Agent, envoyer func(*metrics.Batch), journal *slog.Logger, decl *Declencheurs) {
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
	// Veto local (revue du 28/11, M4) : « MIRADOR_SONDES=non » dans le
	// fichier local du collecteur (fichier de root, le serveur ne peut pas
	// le changer) : aucune sonde demandée par le serveur n'est faite.
	if !cfg.SondesActives && (len(cfg.Collecteur.Sondes) > 0 || len(cfg.Collecteur.Services) > 0) && journal != nil {
		journal.Warn("sondes demandées par le serveur ignorées (MIRADOR_SONDES=non)",
			"certificats", len(cfg.Collecteur.Sondes), "services", len(cfg.Collecteur.Services))
	}
	if cfg.SondesActives && len(cfg.Collecteur.Sondes) > 0 {
		wg.Add(1)
		go func() { defer wg.Done(); sondes.Tourner(ctx, cfg.Collecteur.Sondes, envoyer, journal) }()
	}
	// Sondes de services (SPEC § 48) : une tâche par sonde, séries sous le
	// nom du collecteur (instance posée par le serveur), job="sondes".
	for _, s := range cfg.Collecteur.Services {
		if !cfg.SondesActives {
			break
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			essais.Tourner(ctx, s, OptionsSonde(decl.Canal(s.Nom)), func(s essais.Sonde, r essais.Resultat) {
				envoyer(LotSonde(s, r, time.Now()))
			})
		}()
	}
	wg.Wait()
}

// OptionsSonde : limites des sondes du collecteur (revue du 28/11, M4) :
// boucle locale, adresse non spécifiée et lien local (métadonnées des
// clouds : 169.254.169.254) toujours refusés, à chaque connexion et
// redirection comprise ; le LAN reste permis (c'est le but des sondes).
func OptionsSonde(maintenant <-chan struct{}) essais.Options {
	return essais.Options{Refuser: interdites.LocaleOuLienLocal, Maintenant: maintenant}
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
