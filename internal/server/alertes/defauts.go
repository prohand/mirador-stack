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
		// Certificat du syslog d'un collecteur (SPEC § 23.14) : renouvelé
		// seul 30 jours avant l'échéance ; alerte s'il reste moins de 14 jours
		// ou si le dernier essai a échoué.
		m(Regle{Nom: "CertificatSyslogBientotExpire", Groupe: "collecteurs", Intervalle: Duree(15 * time.Minute),
			Expr:    `(mirador_collecteur_certificat_fin_timestamp_seconds - time()) / 86400 < 14`,
			Pour:    Duree(time.Hour),
			Gravite: Avertissement,
			Resume:  "Le certificat du syslog de {{ $labels.instance }} expire dans {{ $value | humanize }} jours",
			Description: "Le renouvellement automatique (30 jours avant) n'a pas réussi : voir la page du collecteur " +
				"(Certificat du syslog), puis « Renouveler maintenant ».",
			Note: "SPEC § 23.14"}),
		m(Regle{Nom: "CertificatSyslogRenouvellementEchoue", Groupe: "collecteurs", Intervalle: Duree(15 * time.Minute),
			Expr:    `mirador_collecteur_certificat_echec == 1`,
			Pour:    Duree(time.Hour),
			Gravite: Avertissement,
			Resume:  "{{ $labels.instance }} : certificat du syslog non obtenu (ACME / deSEC)",
			Description: "Dernier essai en échec (jeton deSEC, zone, nom du certificat, Let's Encrypt) : le message est sur " +
				"la page du collecteur. Nouvel essai toutes les 6 h, ou « Renouveler maintenant ».",
			Note: "SPEC § 23.14"}),
		// Services attendus par un modèle de surveillance (SPEC § 24.7) : un
		// service en échec ou arrêté est déjà signalé par les règles des
		// services ; ici, le service n'existe pas (pas installé, désinstallé,
		// ou nom faux dans le modèle).
		m(Regle{Nom: "ServiceDuModeleAbsent", Groupe: "modeles", Intervalle: Duree(5 * time.Minute),
			Expr: `max by (instance, name, modele) (mirador_modele_service) ` +
				`unless on (instance, name) node_systemd_unit_state`,
			Pour:    Duree(15 * time.Minute),
			Gravite: Avertissement,
			Resume:  "{{ $labels.name }} (modèle {{ $labels.modele }}) n'existe pas sur {{ $labels.instance }}",
			Description: "Le modèle attend ce service, mais l'agent ne le voit pas : service pas installé ou " +
				"désinstallé, ou nom faux dans la liste du modèle (Machines › Modèles). Aussi si la collecte systemd " +
				"est coupée pour cette machine.",
			Note: "SPEC § 24.7"}),
		// Sondes de services (SPEC § 48) : HTTP, TCP, ping, DNS, depuis le
		// collecteur ou le serveur. Deux essais ratés de suite (1 min).
		m(Regle{Nom: "SondeEnEchec", Groupe: "sondes", Intervalle: Duree(30 * time.Second),
			Expr:    `max by (instance, sonde, type, cible) (mirador_sonde_succes) == 0`,
			Pour:    Duree(time.Minute),
			Gravite: Critique,
			Resume:  "Sonde {{ $labels.sonde }} en échec ({{ $labels.cible }})",
			Description: "L'essai ne passe plus (pas de réponse, mauvais code, contenu attendu absent…). " +
				"La raison est sur la page Sondes. Faite depuis {{ $labels.instance }}."}),
		m(Regle{Nom: "SondeLente", Groupe: "sondes",
			Expr:        `max by (instance, sonde, type, cible) (mirador_sonde_lente) == 1`,
			Pour:        Duree(5 * time.Minute),
			Gravite:     Avertissement,
			Resume:      "Sonde {{ $labels.sonde }} lente depuis 5 min ({{ $labels.cible }})",
			Description: "Le service répond, mais plus lentement que le seuil réglé pour la sonde (page Sondes)."}),
		// Certificat du site d'une sonde HTTPS (SPEC § 81) : seulement si la
		// case est cochée sur la sonde (le nombre de jours vient d'elle).
		m(Regle{Nom: "SondeCertificatBientotExpire", Groupe: "sondes", Intervalle: Duree(15 * time.Minute),
			Expr: `(max by (instance, sonde, cible) (mirador_sonde_certificat_fin_timestamp_seconds) - time()) / 86400` +
				` < max by (instance, sonde, cible) (mirador_sonde_certificat_alerte_jours)`,
			Pour:    Duree(time.Hour),
			Gravite: Avertissement,
			Resume:  "Le certificat de {{ $labels.cible }} expire dans {{ $value | humanize }} jours",
			Description: "Certificat du site lu par la sonde {{ $labels.sonde }} : le renouveler (Let's Encrypt, " +
				"hébergeur…). Une valeur négative : déjà expiré.",
			Note: "SPEC § 81"}),
		// Applications (SPEC § 50, appelées « services » jusqu'au 19/11,
		// § 65) : état calculé chaque minute par le serveur. La série et son
		// étiquette gardent le nom « service » (interne).
		m(Regle{Nom: "ApplicationEnPanne", Groupe: "services",
			Expr:        `max by (service) (mirador_service_etat) == 2`,
			Pour:        Duree(2 * time.Minute),
			Gravite:     Critique,
			Resume:      textesApplication["ApplicationEnPanne"][0],
			Description: textesApplication["ApplicationEnPanne"][1]}),
		m(Regle{Nom: "ApplicationDegradee", Groupe: "services",
			Expr:        `max by (service) (mirador_service_etat) == 1`,
			Pour:        Duree(10 * time.Minute),
			Gravite:     Avertissement,
			Resume:      textesApplication["ApplicationDegradee"][0],
			Description: textesApplication["ApplicationDegradee"][1]}),
		// Tâches planifiées suivies (SPEC § 80) : timers systemd, conteneurs
		// ponctuels, sauvegardes Proxmox, signal « ping ». Gravité choisie
		// par tâche : une règle par gravité.
		m(Regle{Nom: "TacheEnEchec", Groupe: "taches",
			Expr:        `mirador_tache_code != 0 and on (instance, source, tache) mirador_tache_suivie{gravite="warning"}`,
			Gravite:     Avertissement,
			Resume:      "Tâche {{ $labels.tache }} en échec sur {{ $labels.instance }} (code {{ $value }})",
			Description: "La dernière exécution de la tâche planifiée a échoué. Détail et historique : Sondes › Tâches planifiées."}),
		m(Regle{Nom: "TacheEnEchecCritique", Groupe: "taches",
			Expr:        `mirador_tache_code != 0 and on (instance, source, tache) mirador_tache_suivie{gravite="critical"}`,
			Gravite:     Critique,
			Resume:      "Tâche {{ $labels.tache }} en échec sur {{ $labels.instance }} (code {{ $value }})",
			Description: "La dernière exécution de la tâche planifiée a échoué. Détail et historique : Sondes › Tâches planifiées."}),
		m(Regle{Nom: "TacheEnRetard", Groupe: "taches",
			Expr:        `mirador_tache_echeance_timestamp_seconds{gravite="warning"} < time()`,
			Gravite:     Avertissement,
			Resume:      "Tâche {{ $labels.tache }} en retard sur {{ $labels.instance }}",
			Description: "La tâche planifiée n'a pas tourné à temps (fréquence attendue et marge : Sondes › Tâches planifiées)."}),
		m(Regle{Nom: "TacheEnRetardCritique", Groupe: "taches",
			Expr:        `mirador_tache_echeance_timestamp_seconds{gravite="critical"} < time()`,
			Gravite:     Critique,
			Resume:      "Tâche {{ $labels.tache }} en retard sur {{ $labels.instance }}",
			Description: "La tâche planifiée n'a pas tourné à temps (fréquence attendue et marge : Sondes › Tâches planifiées)."}),
		// Anomalies des tâches suivies (SPEC § 95) : durée bien plus longue
		// que d'habitude, tâche qui tourne trop longtemps, sauvegarde d'une
		// VM bien plus petite que la fois d'avant.
		m(Regle{Nom: "TacheAnormale", Groupe: "taches",
			Expr:        `mirador_tache_anomalie == 1`,
			Gravite:     Avertissement,
			Resume:      "Tâche {{ $labels.tache }} sur {{ $labels.instance }} : {{ $labels.quoi }}",
			Description: "La dernière exécution sort de l'habitude (durée 3 fois plus longue, ou sauvegarde d'une VM 2 fois plus petite que la fois d'avant). Détail : Tâches planifiées."}),
		// VM ou conteneur d'un nœud Proxmox qu'aucun travail de sauvegarde
		// planifié ne couvre (SPEC § 95, lu par l'assistant root ; seulement
		// sur un nœud qui a au moins un travail de sauvegarde).
		m(Regle{Nom: "VMSansSauvegarde", Groupe: "taches",
			Expr:        ExprVMSansSauvegarde, // sauf les invités exclus dans l'interface (retour du 25/11)
			Pour:        Duree(2 * time.Hour),
			Gravite:     Avertissement,
			Resume:      "{{ $labels.id }} {{ $labels.nom }} n'est dans aucune sauvegarde planifiée ({{ $labels.instance }})",
			Description: "Aucun travail de Datacenter › Sauvegarde ne couvre cette VM ou ce conteneur (ni « tout », ni sa liste, ni son pool). L'ajouter à un travail ; s'il n'a pas à l'être (le PBS lui-même…), « Ne pas surveiller » dans Tâches planifiées."}),
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
			Pour:    Duree(30 * time.Minute), // SPEC § 61 : pas d'alerte pour une vérification ratée une fois
			Gravite: Avertissement,
			Resume:  "La mise à jour de l'agent a échoué sur {{ $labels.instance }} (version {{ $labels.version }})",
			Description: "L'agent est revenu à sa version précédente. Cette version ne sera pas retentée sur cette " +
				"machine."}),
		// Expiration des noms de domaine (demande du 27/11) : une alerte par
		// domaine principal, 30 jours puis 7 jours avant.
		m(Regle{Nom: "DomaineExpireBientot", Groupe: "securite", Intervalle: Duree(time.Hour),
			Expr:    `(max by (domaine) (last_over_time(mirador_domaine_expiration_timestamp_seconds[2h])) - time()) / 86400 < 30`,
			Gravite: Avertissement,
			Resume:  "Le nom de domaine {{ $labels.domaine }} expire dans {{ $value | humanize }} jours",
			Description: "Date lue chez le registre (RDAP) une fois par jour. Renouveler le domaine chez le bureau " +
				"d'enregistrement, ou vérifier le renouvellement automatique.",
			Note: "Demande du 27/11"}),
		m(Regle{Nom: "DomaineExpireTresBientot", Groupe: "securite", Intervalle: Duree(time.Hour),
			Expr:        `(max by (domaine) (last_over_time(mirador_domaine_expiration_timestamp_seconds[2h])) - time()) / 86400 < 7`,
			Gravite:     Critique,
			Resume:      "Le nom de domaine {{ $labels.domaine }} expire dans {{ $value | humanize }} jours",
			Description: "Tous les sous-domaines (sites, messagerie) s'arrêteront à l'expiration. Renouveler tout de suite.",
			Note:        "Demande du 27/11"}),
		// Connexion SSH depuis une adresse nouvelle (demande du 27/11) : série
		// écrite par le serveur 24 h, ou jusqu'à « C'est moi » sur l'alerte.
		m(Regle{Nom: "SshAdresseNouvelle", Groupe: "securite",
			Expr:    `max by (instance, adresse, utilisateur) (mirador_ssh_adresse_nouvelle) == 1`,
			Gravite: Avertissement,
			Resume:  "Connexion SSH depuis une adresse nouvelle sur {{ $labels.instance }} : {{ $labels.utilisateur }} depuis {{ $labels.adresse }}",
			Description: "Adresse publique jamais vue sur cette machine depuis 90 jours (lue par l'agent dans le journal de sshd). " +
				"Si c'est vous : « C'est moi » sur l'alerte. Sinon : vérifier les connexions (last, journalctl -u ssh).",
			Note: "Demande du 27/11 : adresses du LAN ignorées"}),
		// Nouveau port en écoute (demande du 27/11) : désactivée par défaut
		// (bavarde pendant une installation) ; le changement est noté dans
		// tous les cas (page machine › Changements). Machine qui n'avait aucun
		// port relevé la veille : ignorée.
		func() Regle {
			r := m(Regle{Nom: "NouveauPortEnEcoute", Groupe: "securite", Intervalle: Duree(5 * time.Minute),
				Expr: `(max by (instance, port, adresse) (last_over_time(mirador_port_ecoute[10m])) unless ` +
					`max by (instance, port, adresse) (last_over_time(mirador_port_ecoute[1d] offset 15m))) ` +
					`and on (instance) max by (instance) (last_over_time(mirador_port_ecoute[1d] offset 15m))`,
				Gravite: Info,
				Resume:  "Nouveau port en écoute sur {{ $labels.instance }} : tcp/{{ $labels.port }}",
				Description: "Relevé par l'agent dans /proc/net/tcp (toutes les 5 min). Si ce n'est pas une installation " +
					"voulue : voir quel programme écoute (ss -ltnp) sur la machine.",
				Note: "Demande du 27/11 : désactivée par défaut, le changement est toujours noté"})
			r.Active = false
			return r
		}(),
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
	return PoserRegles(ctx, db, maintenant, ReglesMirador())
}

// PoserRegles ajoute les règles fournies (mirador, modèles de surveillance)
// jamais posées : une règle supprimée par l'utilisateur ne revient pas.
func PoserRegles(ctx context.Context, db *sql.DB, maintenant time.Time, fournies []Regle) (int, error) {
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
	// Règle déjà posée sous son ancien nom (§ 65) : posée aussi sous le
	// nouveau (supprimée par l'utilisateur, elle ne revient pas).
	for _, x := range reglesRenommees {
		if poses[x.ancien] {
			poses[x.nouveau] = true
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
	for _, r := range fournies {
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
