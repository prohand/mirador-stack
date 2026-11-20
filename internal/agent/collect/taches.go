// SPDX-License-Identifier: GPL-3.0-or-later

package collect

import (
	"time"

	"mirador-stack/internal/agent/metrics"
)

// Tâches planifiées (SPEC § 80) : dernière exécution de chaque tâche, quelle
// que soit sa source (timer systemd, conteneur Docker ponctuel, sauvegarde
// Proxmox VE / Proxmox Backup Server). Mêmes séries pour toutes :
//
//	mirador_tache_debut_timestamp_seconds{source,tache}
//	mirador_tache_fin_timestamp_seconds{source,tache}
//	mirador_tache_code{source,tache}          0 = réussie, sinon le code (ou 1)
//	mirador_tache_prochaine_timestamp_seconds  prochaine exécution prévue (timers)
//
// Le serveur choisit les tâches suivies et en tire les alertes « en échec »
// et « en retard ».

// AjouterTache ajoute les séries d'une tâche (dates nulles : série omise).
func AjouterTache(b *metrics.Batch, source, nom string, debut, fin time.Time, code int, prochaine time.Time) {
	l := []metrics.Label{metrics.L("source", source), metrics.L("tache", nom)}
	if !debut.IsZero() {
		b.Add("mirador_tache_debut_timestamp_seconds", float64(debut.Unix()), l...)
	}
	if !fin.IsZero() {
		b.Add("mirador_tache_fin_timestamp_seconds", float64(fin.Unix()), l...)
		b.Add("mirador_tache_code", float64(code), l...)
	}
	if !prochaine.IsZero() {
		b.Add("mirador_tache_prochaine_timestamp_seconds", float64(prochaine.Unix()), l...)
	}
}
