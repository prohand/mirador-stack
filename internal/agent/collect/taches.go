// SPDX-License-Identifier: GPL-3.0-or-later

package collect

import (
	"strconv"
	"time"

	"mirador-stack/internal/agent/metrics"
	"mirador-stack/internal/agent/racine"
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

// AjouterDetailSauvegarde : détail d'une sauvegarde Proxmox (SPEC § 89).
//
//	mirador_tache_info{source,tache,stockage,planning,commentaire} 1
//	mirador_tache_vm{source,tache,vm,type,nom}           1 réussie, 0 en échec
//	mirador_tache_vm_duree_secondes{source,tache,vm}
//	mirador_tache_vm_octets{source,tache,vm}
//	mirador_tache_vm_message{source,tache,vm,message}     1 (échec seulement)
//	mirador_tache_erreur{source,tache,rang,message}        1 (lignes « ERROR », § 93)
//	mirador_tache_resume{source,tache,rang,nom,valeur}     1 (tâches PBS, § 95)
func AjouterDetailSauvegarde(b *metrics.Batch, source, nom string, d *racine.DetailSauvegarde) {
	l := []metrics.Label{metrics.L("source", source), metrics.L("tache", nom)}
	if d.Stockage != "" || d.Planning != "" || d.Commentaire != "" {
		b.Add("mirador_tache_info", 1, append(l, metrics.L("stockage", d.Stockage), metrics.L("planning", d.Planning),
			metrics.L("commentaire", d.Commentaire))...)
	}
	for _, v := range d.VMs {
		lv := append(append([]metrics.Label{}, l...), metrics.L("vm", v.ID))
		ok := 0.0
		if v.OK {
			ok = 1
		}
		b.Add("mirador_tache_vm", ok, append(append([]metrics.Label{}, lv...), metrics.L("type", v.Type), metrics.L("nom", v.Nom))...)
		if v.Duree > 0 {
			b.Add("mirador_tache_vm_duree_secondes", float64(v.Duree), lv...)
		}
		if v.Octets > 0 {
			b.Add("mirador_tache_vm_octets", v.Octets, lv...)
		}
		if v.Message != "" {
			b.Add("mirador_tache_vm_message", 1, append(append([]metrics.Label{}, lv...), metrics.L("message", v.Message))...)
		}
	}
	for i, x := range d.Resume { // tâches PBS (SPEC § 95)
		b.Add("mirador_tache_resume", 1, append(append([]metrics.Label{}, l...), metrics.L("rang", strconv.Itoa(i+1)),
			metrics.L("nom", x.Nom), metrics.L("valeur", x.Valeur))...)
	}
	for i, e := range d.Erreurs {
		b.Add("mirador_tache_erreur", 1, append(append([]metrics.Label{}, l...), metrics.L("rang", strconv.Itoa(i+1)), metrics.L("message", e))...)
	}
}
