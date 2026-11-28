// SPDX-License-Identifier: GPL-3.0-or-later

package ingestion

import "strings"

// Noms de métriques réservés au serveur (revue de sécurité du 28/11, H1).
//
// Le serveur écrit lui-même des séries mirador_* dans sa base (interne.go :
// seuils des modèles, expiration des domaines, état des applications,
// adresses SSH nouvelles, sauvegardes…). Les règles d'alerte les lisent.
// Une machine (token volé, agent modifié) qui envoyait une série du même
// nom pouvait fausser ces règles (fausse date d'expiration d'un domaine,
// faux état d'une application) ou les mettre en erreur pour tout le parc
// (seuil en double côté « un » d'un group_left).
//
// Ces noms sont donc refusés pour les agents et les collecteurs, comptés,
// et signalés dans le journal. La liste est vérifiée par un test contre
// tous les noms mirador_* émis par l'agent, le collecteur et les sondes :
// un nom légitime d'agent n'y tombe jamais.

// prefixesReserves : familles entières écrites par le serveur seul.
var prefixesReserves = []string{
	"mirador_alertes_",               // moteur d'alertes
	"mirador_canal_",                 // canaux de notification
	"mirador_collecteur_certificat_", // certificat du syslog (le collecteur envoie mirador_collecteur_syslog_*)
	"mirador_disque_",                // disque des données du serveur
	"mirador_domaine_",               // expiration des noms de domaine
	"mirador_element_",               // étiquettes libres
	"mirador_heartbeat_",             // battement de cœur des notifications
	"mirador_machine_",               // décalage d'horloge, mise à jour, modèles, dernière réception
	"mirador_modele_",                // seuils et services attendus des modèles
	"mirador_regles_",                // règles en erreur
	"mirador_sauvegarde_",            // sauvegardes du serveur
	"mirador_serveur_",               // charge du serveur
	"mirador_service_",               // état des applications (SPEC § 50)
	"mirador_ssh_",                   // adresses SSH nouvelles (l'agent envoie mirador_logs_ssh_*)
}

// nomsReservesServeur : noms isolés écrits par le serveur, dans des familles
// que l'agent utilise aussi (mirador_tache_*, mirador_logs_*,
// mirador_processus_*, mirador_pve_invite_*).
var nomsReservesServeur = map[string]bool{
	"mirador_base_octets":                      true,
	"mirador_file_notifications":               true,
	"mirador_logs_motifs":                      true,
	"mirador_logs_recues_total":                true,
	"mirador_metriques_octets":                 true,
	"mirador_points_recus_total":               true,
	"mirador_points_rejetes_total":             true,
	"mirador_processus_anon_octets":            true,
	"mirador_processus_rss_octets":             true,
	"mirador_pve_invite_exclue":                true,
	"mirador_tache_anomalie":                   true,
	"mirador_tache_echeance_timestamp_seconds": true,
	"mirador_tache_suivie":                     true,
}

// nomReserve : la métrique est-elle écrite par le serveur seul ? Coût : une
// comparaison de préfixe (les noms hors mirador_* sortent tout de suite).
func nomReserve(nom string) bool {
	if !strings.HasPrefix(nom, "mirador_") {
		return false
	}
	if nomsReservesServeur[nom] {
		return true
	}
	for _, p := range prefixesReserves {
		if strings.HasPrefix(nom, p) {
			return true
		}
	}
	return false
}
