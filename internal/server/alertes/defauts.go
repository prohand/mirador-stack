// SPDX-License-Identifier: GPL-3.0-or-later

package alertes

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"time"
)

// Règles fournies par mirador (SPEC.md § 4.6 bis et § 17.3), posées une seule
// fois : une règle que l'utilisateur supprime ne revient pas au redémarrage.
// Les métriques mirador_* sont écrites par le serveur lui-même chaque minute
// (paquet interne), sous instance="mirador-server".

// ReglesMirador : règles par défaut.
func ReglesMirador() []Regle {
	m := func(r Regle) Regle {
		r.Origine, r.Active = "mirador", true
		if r.Intervalle == 0 {
			r.Intervalle = Duree(time.Minute)
		}
		return r
	}
	return []Regle{
		m(Regle{Nom: "OomKiller", Groupe: "hote", Intervalle: Duree(30 * time.Second),
			Expr:    `increase(node_vmstat_oom_kill[5m]) > 0`,
			Gravite: Critique,
			Resume:  "Le noyau a tué {{ $value | humanize }} processus par manque de mémoire sur {{ $labels.instance }}",
			Description: "Compteur oom_kill du noyau (/proc/vmstat). Le processus tué est écrit dans le journal " +
				"(« Out of memory: Killed process »).",
			Note: "SPEC § 4.6 bis : activée par défaut"}),
		// Paquets (demande du 12/11) : mises à jour de sécurité qui attendent.
		// « Pour » de 3 jours : le temps normal de les appliquer.
		m(Regle{Nom: "MisesAJourSecuriteEnAttente", Groupe: "hote", Intervalle: Duree(5 * time.Minute),
			Expr:    `max by (instance) (apt_upgrades_security_pending) > 0`,
			Pour:    Duree(72 * time.Hour),
			Gravite: Avertissement,
			Resume:  "{{ $value | humanize }} mise(s) à jour de sécurité attendent depuis plus de 3 jours sur {{ $labels.instance }}",
			Description: "Compté par l'agent avec « apt-get -s upgrade » (simulation, rien n'est installé). " +
				"Appliquer les mises à jour (apt upgrade), puis redémarrer si demandé.",
			Note: "Demande du 12/11 : réglable (délai, gravité) dans Règles"}),
		// Matériel (SPEC § 19) : ces métriques n'existent que sur les machines où
		// l'élément a été validé dans l'interface ; ailleurs, la règle ne voit rien.
		m(Regle{Nom: "PoolZfsDegrade", Groupe: "materiel",
			Expr:        `node_zfs_zpool_state{state="online"} == 0`,
			Pour:        Duree(2 * time.Minute),
			Gravite:     Critique,
			Resume:      "Le pool ZFS {{ $labels.zpool }} de {{ $labels.instance }} n'est plus ONLINE",
			Description: "État lu dans /proc/spl/kstat/zfs. Voir « zpool status » sur la machine : disque en panne ou retiré.",
			Note:        "SPEC § 19"}),
		m(Regle{Nom: "RaidLogicielDegrade", Groupe: "materiel",
			Expr:        `node_md_degraded > 0`,
			Pour:        Duree(2 * time.Minute),
			Gravite:     Critique,
			Resume:      "La grappe RAID {{ $labels.device }} de {{ $labels.instance }} a {{ $value | humanize }} disque(s) manquant(s)",
			Description: "Valeur de /sys/block/mdX/md/degraded. Voir /proc/mdstat sur la machine.",
			Note:        "SPEC § 19"}),
		m(Regle{Nom: "VentilateurArrete", Groupe: "materiel",
			Expr:        `node_hwmon_fan_rpm == 0 and max_over_time(node_hwmon_fan_rpm[1d]) > 0`,
			Pour:        Duree(10 * time.Minute),
			Gravite:     Avertissement,
			Resume:      "Le ventilateur {{ $labels.sensor }} ({{ $labels.chip }}) de {{ $labels.instance }} est à l'arrêt",
			Description: "Il tournait dans les dernières 24 h. Un capteur non branché (toujours à 0) ne déclenche rien.",
			Note:        "SPEC § 19"}),
		// Proxmox par l'agent (SPEC § 23.7) : invités lus dans les cgroups,
		// noms par l'assistant root. Sans nœud Proxmox, ces règles ne voient rien.
		m(Regle{Nom: "ProxmoxInviteCpuSature", Groupe: "proxmox",
			Expr: `max by (instance, id, type) (rate(mirador_pve_invite_cpu_secondes_total[5m])) ` +
				`/ max by (instance, id, type) (mirador_pve_invite_coeurs) ` +
				`* on (instance, id) group_left (nom) mirador_pve_invite_info > 0.95`,
			Pour:    Duree(15 * time.Minute),
			Gravite: Avertissement,
			Resume:  "{{ $labels.nom }} ({{ $labels.id }}) sur {{ $labels.instance }} utilise {{ $value | humanizePercentage }} de ses cœurs depuis 15 min",
			Description: "Temps CPU de l'invité (cgroup) rapporté aux cœurs configurés. Ajouter des cœurs, ou chercher " +
				"le processus en cause dans l'invité.",
			Note: "SPEC § 23.7"}),
		m(Regle{Nom: "ProxmoxConteneurMemoireSaturee", Groupe: "proxmox",
			Expr: `max by (instance, id, type) (mirador_pve_invite_memoire_octets{type="lxc"}) ` +
				`/ max by (instance, id, type) (mirador_pve_invite_memoire_max_octets{type="lxc"}) ` +
				`* on (instance, id) group_left (nom) mirador_pve_invite_info > 0.95`,
			Pour:    Duree(15 * time.Minute),
			Gravite: Avertissement,
			Resume:  "Le conteneur {{ $labels.nom }} ({{ $labels.id }}) sur {{ $labels.instance }} utilise {{ $value | humanizePercentage }} de sa mémoire",
			Description: "Mémoire du cgroup sans le cache inactif, rapportée à la mémoire configurée : le noyau va " +
				"tuer des processus dans le conteneur (OOM). Seulement les LXC : pour une VM, la mémoire vue de " +
				"l'hôte ne dit pas si l'invité manque de mémoire (installer l'agent dans la VM).",
			Note: "SPEC § 23.7"}),
		m(Regle{Nom: "ProxmoxListeInvitesIndisponible", Groupe: "proxmox", Intervalle: Duree(5 * time.Minute),
			Expr: `count by (instance) (mirador_pve_invite_en_marche) ` +
				`unless on (instance) count by (instance) (mirador_pve_invite_info)`,
			Pour:    Duree(30 * time.Minute),
			Gravite: Avertissement,
			Resume:  "{{ $labels.instance }} : invités en marche, mais pas de liste ni de noms (assistant root)",
			Description: "L'assistant root ne lit pas /etc/pve : trop ancien (relancer le script, ou attendre sa mise " +
				"à jour automatique), ou erreur (voir root.json, champ « erreurs »). Les tableaux n'ont alors pas les noms.",
			Note: "SPEC § 23.7"}),
		m(Regle{Nom: "SauvegardeTropAncienne", Groupe: "mirador",
			Expr:    `time() - mirador_sauvegarde_derniere_reussie_timestamp_seconds > 26 * 3600`,
			Pour:    Duree(10 * time.Minute),
			Gravite: Avertissement,
			Resume:  "Dernière sauvegarde réussie il y a {{ $value | humanizeDuration }}",
			Description: "Aucune sauvegarde réussie depuis plus de 26 h (planifiée chaque nuit). Voir la commande " +
				"« sauvegardes » du serveur."}),
		m(Regle{Nom: "SauvegardeEnEchec", Groupe: "mirador",
			Expr:    `mirador_sauvegarde_derniere_en_echec == 1`,
			Gravite: Avertissement,
			Resume:  "La dernière sauvegarde a échoué",
			Description: "Le détail de l'erreur est dans l'historique (commande « sauvegardes »). L'alerte se termine " +
				"à la prochaine sauvegarde réussie."}),
		m(Regle{Nom: "ServeurDisqueSeRemplitVite", Groupe: "mirador", Intervalle: Duree(5 * time.Minute),
			Expr:    `predict_linear(mirador_disque_libre_octets[6h], 3 * 24 * 3600) < 0`,
			Pour:    Duree(30 * time.Minute),
			Gravite: Avertissement,
			Resume:  "Au rythme des 6 dernières heures, le disque du serveur sera plein dans moins de 3 jours",
			Description: "Le disque se remplit plus vite que prévu (brief, conservation des données). Vérifier la page " +
				"« Conservation des données » : plafonds de taille, volume de logs par jour."}),
		m(Regle{Nom: "HorlogeDecalee", Groupe: "mirador",
			Expr:    `abs(mirador_machine_decalage_horloge_secondes) > 60`,
			Pour:    Duree(10 * time.Minute),
			Gravite: Avertissement,
			Resume:  "L'horloge de {{ $labels.instance }} est décalée de {{ $value | humanizeDuration }}",
			Description: "Écart mesuré à chaque envoi de l'agent. Au-delà de 10 min, les données sont refusées. " +
				"Vérifier NTP (timedatectl) sur la machine."}),
		m(Regle{Nom: "MiseAJourAgentEchouee", Groupe: "mirador",
			Expr:    `mirador_machine_maj_echec == 1`,
			Gravite: Avertissement,
			Resume:  "La mise à jour de l'agent a échoué sur {{ $labels.instance }} (version {{ $labels.version }})",
			Description: "L'agent est revenu à sa version précédente. Cette version ne sera pas retentée sur cette " +
				"machine."}),
		m(Regle{Nom: "ServeurDisquePresquePlein", Groupe: "mirador",
			Expr:        `mirador_disque_libre_octets / mirador_disque_total_octets < 0.10`,
			Pour:        Duree(10 * time.Minute),
			Gravite:     Critique,
			Resume:      "Disque des données de mirador-server : moins de 10 % libre",
			Description: "Réduire la rétention ou le plafond de stockage, ou agrandir le disque du VPS."}),
		m(Regle{Nom: "EvaluationAlertesLente", Groupe: "mirador",
			Expr:    `mirador_alertes_duree_cycle_secondes > 10`,
			Pour:    Duree(15 * time.Minute),
			Gravite: Avertissement,
			Resume:  "L'évaluation des règles d'alerte prend {{ $value | humanize }} s par minute",
			Description: "Une règle lit trop de données (fenêtre longue sur beaucoup de séries). Voir la " +
				"commande « regles » (durée par règle)."}),
	}
}

const cleDefautsPoses = "alertes.defauts_poses"

// PoserDefauts ajoute les règles mirador jamais posées (une seule fois chacune).
func PoserDefauts(ctx context.Context, db *sql.DB, maintenant time.Time) (int, error) {
	poses := map[string]bool{}
	var v string
	err := db.QueryRowContext(ctx, `SELECT valeur FROM reglages WHERE cle = ?`, cleDefautsPoses).Scan(&v)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	if v != "" {
		var noms []string
		if err := json.Unmarshal([]byte(v), &noms); err != nil {
			return 0, err
		}
		for _, n := range noms {
			poses[n] = true
		}
	}
	existantes, err := ListerRegles(ctx, db)
	if err != nil {
		return 0, err
	}
	deja := map[string]bool{}
	for _, r := range existantes {
		deja[r.Nom] = true
	}
	var nouvelles []Regle
	for _, r := range ReglesMirador() {
		if !poses[r.Nom] && !deja[r.Nom] {
			nouvelles = append(nouvelles, r)
		}
		poses[r.Nom] = true
	}
	n, err := Enregistrer(ctx, db, nouvelles, "mirador", maintenant)
	if err != nil {
		return 0, err
	}
	noms := make([]string, 0, len(poses))
	for k := range poses {
		noms = append(noms, k)
	}
	sort.Strings(noms)
	brut, _ := json.Marshal(noms)
	_, err = db.ExecContext(ctx, `INSERT INTO reglages(cle, valeur, modifie_le, modifie_par) VALUES (?, ?, ?, 'mirador')
		ON CONFLICT(cle) DO UPDATE SET valeur = excluded.valeur, modifie_le = excluded.modifie_le`,
		cleDefautsPoses, string(brut), maintenant.Unix())
	return n, err
}
