// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"regexp"
	"strings"
)

// Requêtes toutes faites pour l'éditeur de panneau (demande du 10/11,
// SPEC § 18.18). « FILTRES » dans un sélecteur est remplacé par les filtres
// du tableau (ex. instance=~"$instance", site=~"$site") ; sans filtre, il
// disparaît. Les noms de métriques sont ceux de mirador-agent, du collecteur
// (SNMP Synology, unpoller) et du tableau Certificats.

// ModeleRequete : une requête prête, avec ce qu'il faut pour l'afficher.
type ModeleRequete struct {
	ID, Categorie, Titre, Description string
	Type, Unite, Expr, Legende        string
	Seuils, Correspondances           string // même format que le formulaire
	Icone, Teinte                     string
}

const exclusFS = `fstype!~"tmpfs|devtmpfs|overlay|squashfs|ramfs|nsfs|fuse.*"`
const exclusReseau = `device!~"lo|veth.*|docker.*|br-.*|virbr.*|tap.*|fwpr.*|fwln.*"`

var modelesRequetes = []ModeleRequete{
	// --- Système
	{ID: "cpu", Categorie: "Système", Titre: "CPU utilisé", Description: "Part du temps où le processeur travaille, par machine.",
		Type: "courbe", Unite: "percent", Expr: `100 * (1 - avg by (instance) (rate(node_cpu_seconds_total{mode="idle", FILTRES}[$__rate_interval])))`,
		Legende: "{{instance}}", Seuils: "ok\n80 attention\n95 erreur", Icone: "cpu", Teinte: "bleu"},
	{ID: "cpu-moyen", Categorie: "Système", Titre: "CPU moyen", Description: "Une seule valeur : moyenne de toutes les machines filtrées.",
		Type: "chiffre", Unite: "percent", Expr: `100 * (1 - avg(rate(node_cpu_seconds_total{mode="idle", FILTRES}[$__rate_interval])))`,
		Seuils: "ok\n80 attention\n95 erreur", Icone: "cpu", Teinte: "bleu"},
	{ID: "memoire", Categorie: "Système", Titre: "Mémoire utilisée", Description: "Mémoire réellement prise (hors cache), par machine.",
		Type: "courbe", Unite: "percent", Expr: `100 * (1 - node_memory_MemAvailable_bytes{FILTRES} / node_memory_MemTotal_bytes{FILTRES})`,
		Legende: "{{instance}}", Seuils: "ok\n85 attention\n95 erreur", Icone: "memoire", Teinte: "violet"},
	{ID: "swap", Categorie: "Système", Titre: "Swap utilisé", Description: "Machines qui ont un swap seulement.",
		Type: "barres", Unite: "percent", Expr: `100 * (1 - node_memory_SwapFree_bytes{FILTRES} / (node_memory_SwapTotal_bytes{FILTRES} > 0))`,
		Legende: "{{instance}}", Seuils: "ok\n50 attention\n80 erreur", Icone: "memoire", Teinte: "orange"},
	{ID: "charge", Categorie: "Système", Titre: "Charge par cœur (5 min)", Description: "Au-dessus de 1 : plus de travail que de cœurs.",
		Type: "courbe", Expr: `node_load5{FILTRES} / on (instance) count by (instance) (node_cpu_seconds_total{mode="idle", FILTRES})`,
		Legende: "{{instance}}", Seuils: "ok\n1 attention\n2 erreur", Icone: "jauge", Teinte: "orange"},
	{ID: "uptime", Categorie: "Système", Titre: "Durée depuis le démarrage", Description: "Par machine.",
		Type: "barres", Unite: "s", Expr: `node_time_seconds{FILTRES} - node_boot_time_seconds{FILTRES}`,
		Legende: "{{instance}}", Icone: "horloge", Teinte: "gris"},
	{ID: "redemarrage", Categorie: "Système", Titre: "Redémarrage nécessaire", Description: "Machines qui attendent un redémarrage (mises à jour du noyau…).",
		Type: "chiffre", Expr: `sum(node_reboot_required{FILTRES}) or vector(0)`, Seuils: "ok\n1 attention", Icone: "sauvegarde", Teinte: "orange"},
	{ID: "pression-cpu", Categorie: "Système", Titre: "Attente du processeur", Description: "Part du temps où des tâches attendent le CPU (pression, PSI).",
		Type: "courbe", Unite: "percent", Expr: `100 * rate(node_pressure_cpu_waiting_seconds_total{FILTRES}[$__rate_interval])`,
		Legende: "{{instance}}", Seuils: "ok\n10 attention\n30 erreur", Icone: "activite", Teinte: "orange"},
	{ID: "oom", Categorie: "Système", Titre: "Processus tués faute de mémoire (24 h)", Description: "Compteur du noyau (OOM killer).",
		Type: "chiffre", Expr: `sum(increase(node_vmstat_oom_kill{FILTRES}[1d])) or vector(0)`, Seuils: "ok\n1 erreur", Icone: "attention", Teinte: "rouge"},

	// --- Disques
	{ID: "disque", Categorie: "Disques", Titre: "Disque utilisé par partition", Description: "Systèmes de fichiers réels (sans tmpfs, overlay…).",
		Type: "barres", Unite: "percent", Expr: `100 * (1 - node_filesystem_avail_bytes{` + exclusFS + `, FILTRES} / node_filesystem_size_bytes{` + exclusFS + `, FILTRES})`,
		Legende: "{{instance}} {{mountpoint}}", Seuils: "ok\n80 attention\n90 erreur", Icone: "disque", Teinte: "violet"},
	{ID: "disque-max", Categorie: "Disques", Titre: "Partition la plus pleine", Description: "Une valeur : le pire de toutes les machines.",
		Type: "chiffre", Unite: "percent", Expr: `max(100 * (1 - node_filesystem_avail_bytes{` + exclusFS + `, FILTRES} / node_filesystem_size_bytes{` + exclusFS + `, FILTRES}))`,
		Seuils: "ok\n80 attention\n90 erreur", Icone: "disque", Teinte: "violet"},
	{ID: "disque-libre", Categorie: "Disques", Titre: "Espace libre par partition", Type: "barres", Unite: "bytes",
		Expr: `node_filesystem_avail_bytes{` + exclusFS + `, FILTRES}`, Legende: "{{instance}} {{mountpoint}}", Icone: "stockage", Teinte: "violet"},
	{ID: "inodes", Categorie: "Disques", Titre: "Inodes utilisés", Description: "Trop de petits fichiers : le disque peut être « plein » avec de la place.",
		Type: "barres", Unite: "percent", Expr: `100 * (1 - node_filesystem_files_free{` + exclusFS + `, FILTRES} / (node_filesystem_files{` + exclusFS + `, FILTRES} > 0))`,
		Legende: "{{instance}} {{mountpoint}}", Seuils: "ok\n80 attention\n90 erreur", Icone: "fichier", Teinte: "gris"},
	{ID: "disque-occupe", Categorie: "Disques", Titre: "Disque occupé (% du temps)", Description: "Proche de 100 % : le disque est le goulot.",
		Type: "courbe", Unite: "percent", Expr: `100 * rate(node_disk_io_time_seconds_total{FILTRES}[$__rate_interval])`,
		Legende: "{{instance}} {{device}}", Seuils: "ok\n80 attention", Icone: "activite", Teinte: "violet"},
	{ID: "disque-debit", Categorie: "Disques", Titre: "Lectures et écritures disque", Type: "courbe", Unite: "Bps",
		Expr: `sum by (instance) (rate(node_disk_read_bytes_total{FILTRES}[$__rate_interval]))`, Legende: "{{instance}} lecture", Icone: "disque", Teinte: "bleu"},
	{ID: "smart", Categorie: "Disques", Titre: "Santé SMART des disques", Description: "1 = bon, 0 = le disque annonce une panne.",
		Type: "tableau", Expr: `min by (instance, device) (smartctl_device_smart_status{FILTRES})`,
		Correspondances: "1 = OK ok\n0 = En panne erreur", Icone: "ok", Teinte: "vert"},

	// --- Températures
	{ID: "temp-max", Categorie: "Températures", Titre: "Température la plus haute", Description: "Le capteur le plus chaud de toutes les machines (hwmon).",
		Type: "chiffre", Unite: "celsius", Expr: `max(node_hwmon_temp_celsius{FILTRES})`, Seuils: "ok\n70 attention\n85 erreur", Icone: "temperature", Teinte: "rouge"},
	{ID: "temp-machines", Categorie: "Températures", Titre: "Température par machine", Description: "Capteur le plus chaud de chaque machine.",
		Type: "courbe", Unite: "celsius", Expr: `max by (instance) (node_hwmon_temp_celsius{FILTRES})`, Legende: "{{instance}}",
		Seuils: "ok\n70 attention\n85 erreur", Icone: "temperature", Teinte: "rouge"},
	{ID: "temp-capteurs", Categorie: "Températures", Titre: "Tous les capteurs", Description: "Processeur, carte mère, NVMe… (hwmon).",
		Type: "courbe", Unite: "celsius", Expr: `node_hwmon_temp_celsius{FILTRES}`, Legende: "{{instance}} {{chip}} {{sensor}}", Icone: "temperature", Teinte: "orange"},
	{ID: "temp-zones", Categorie: "Températures", Titre: "Zones thermiques", Description: "Capteurs du noyau (Raspberry Pi, portables…).",
		Type: "courbe", Unite: "celsius", Expr: `node_thermal_zone_temp{FILTRES}`, Legende: "{{instance}} {{type}}", Icone: "temperature", Teinte: "orange"},
	{ID: "temp-disques", Categorie: "Températures", Titre: "Température des disques (SMART)", Description: "Au-delà de 50 °C, un disque vieillit plus vite.",
		Type: "barres", Unite: "celsius", Expr: `smartctl_device_temperature{temperature_type="current", FILTRES}`, Legende: "{{instance}} {{device}}",
		Seuils: "ok\n50 attention\n60 erreur", Icone: "disque", Teinte: "rouge"},
	{ID: "temp-nas", Categorie: "Températures", Titre: "Température des NAS (Synology)", Type: "courbe", Unite: "celsius",
		Expr: `max by (instance) (temperature{job="snmp", FILTRES})`, Legende: "{{instance}}", Seuils: "ok\n55 attention\n65 erreur", Icone: "stockage", Teinte: "rouge"},
	{ID: "temp-nas-disques", Categorie: "Températures", Titre: "Disques des NAS (Synology)", Type: "barres", Unite: "celsius",
		Expr: `max by (instance, diskID) (diskTemperature{job="snmp", FILTRES})`, Legende: "{{instance}} {{diskID}}", Seuils: "ok\n50 attention\n60 erreur", Icone: "disque", Teinte: "rouge"},
	{ID: "temp-unifi", Categorie: "Températures", Titre: "Équipements UniFi", Type: "courbe", Unite: "celsius",
		Expr: `max by (name, temp_area) (unpoller_device_temperature_celsius{FILTRES})`, Legende: "{{name}} {{temp_area}}", Seuils: "ok\n70 attention\n85 erreur", Icone: "wifi", Teinte: "rouge"},

	// --- Réseau
	{ID: "reseau-entrant", Categorie: "Réseau", Titre: "Trafic entrant", Description: "Par machine, interfaces physiques et VLAN (sans Docker ni lo).",
		Type: "courbe", Unite: "Bps", Expr: `sum by (instance) (rate(node_network_receive_bytes_total{` + exclusReseau + `, FILTRES}[$__rate_interval]))`,
		Legende: "{{instance}}", Icone: "telechargement", Teinte: "bleu"},
	{ID: "reseau-sortant", Categorie: "Réseau", Titre: "Trafic sortant", Type: "courbe", Unite: "Bps",
		Expr: `sum by (instance) (rate(node_network_transmit_bytes_total{` + exclusReseau + `, FILTRES}[$__rate_interval]))`, Legende: "{{instance}}", Icone: "envoi", Teinte: "vert"},
	{ID: "reseau-interfaces", Categorie: "Réseau", Titre: "Trafic par interface", Type: "courbe", Unite: "Bps",
		Expr: `rate(node_network_receive_bytes_total{` + exclusReseau + `, FILTRES}[$__rate_interval])`, Legende: "{{instance}} {{device}}", Icone: "reseau", Teinte: "bleu"},
	{ID: "reseau-erreurs", Categorie: "Réseau", Titre: "Erreurs et pertes réseau", Description: "Paquets en erreur ou jetés, par seconde.",
		Type: "courbe", Unite: "pps", Expr: `sum by (instance) (rate(node_network_receive_errs_total{FILTRES}[$__rate_interval]) + rate(node_network_receive_drop_total{FILTRES}[$__rate_interval]))`,
		Legende: "{{instance}}", Seuils: "ok\n1 attention", Icone: "attention", Teinte: "orange"},

	// --- Services et Docker
	{ID: "services-echec", Categorie: "Services", Titre: "Services en échec", Type: "chiffre",
		Expr: `sum(node_systemd_unit_state{state="failed", FILTRES}) or vector(0)`, Seuils: "ok\n1 erreur", Icone: "alertes", Teinte: "rouge"},
	{ID: "services-liste", Categorie: "Services", Titre: "Liste des services en échec", Type: "tableau",
		Expr: `node_systemd_unit_state{state="failed", FILTRES} == 1`, Icone: "alertes", Teinte: "rouge"},
	{ID: "services-redemarrages", Categorie: "Services", Titre: "Services qui redémarrent (24 h)", Description: "Les 10 qui ont le plus redémarré.",
		Type: "barres", Expr: `topk(10, increase(node_systemd_service_restart_total{FILTRES}[1d]) > 0)`, Legende: "{{instance}} {{name}}", Icone: "sauvegarde", Teinte: "orange"},
	{ID: "conteneurs-arretes", Categorie: "Docker", Titre: "Conteneurs arrêtés", Description: "Conteneurs connus de Docker qui ne tournent pas.",
		Type: "chiffre", Expr: `count(node_docker_container_running{FILTRES} == 0) or vector(0)`, Seuils: "ok\n1 attention", Icone: "conteneur", Teinte: "orange"},
	{ID: "conteneurs-cpu", Categorie: "Docker", Titre: "CPU des conteneurs", Type: "courbe", Unite: "percent",
		Expr: `100 * rate(node_docker_container_cpu_seconds_total{FILTRES}[$__rate_interval])`, Legende: "{{instance}} {{name}}", Icone: "conteneur", Teinte: "bleu"},
	{ID: "conteneurs-memoire", Categorie: "Docker", Titre: "Mémoire des conteneurs", Type: "barres", Unite: "bytes",
		Expr: `topk(15, node_docker_container_memory_bytes{FILTRES})`, Legende: "{{instance}} {{name}}", Icone: "conteneur", Teinte: "violet"},
	{ID: "conteneurs-redemarrages", Categorie: "Docker", Titre: "Conteneurs qui redémarrent (24 h)", Type: "barres",
		Expr: `increase(node_docker_container_restart_count{FILTRES}[1d]) > 0`, Legende: "{{instance}} {{name}}", Icone: "sauvegarde", Teinte: "orange"},

	// --- Sécurité et certificats
	{ID: "pare-feu", Categorie: "Sécurité", Titre: "INPUT pas en DROP", Description: "Machines surveillées dont le pare-feu laisse tout entrer par défaut.",
		Type: "chiffre", Expr: `count(node_firewall_input_policy_drop{FILTRES} == 0) or vector(0)`, Seuils: "ok\n1 erreur", Icone: "mur", Teinte: "rouge"},
	{ID: "ssh-echecs", Categorie: "Sécurité", Titre: "Échecs de connexion SSH (24 h)", Description: "Comptés dans les logs par l'agent.",
		Type: "barres", Expr: `sum by (instance) (increase(mirador_logs_ssh_auth_failures_total{FILTRES}[1d])) > 0`, Legende: "{{instance}}", Icone: "cle", Teinte: "rouge"},
	{ID: "erreurs-disque-noyau", Categorie: "Sécurité", Titre: "Erreurs disque du noyau (24 h)", Description: "Lignes d'erreur d'E/S vues dans le journal.",
		Type: "barres", Expr: `sum by (instance) (increase(mirador_logs_kernel_disk_errors_total{FILTRES}[1d])) > 0`, Legende: "{{instance}}", Icone: "disque", Teinte: "rouge"},
	{ID: "certificats", Categorie: "Sécurité", Titre: "Jours avant expiration des certificats", Type: "barres", Unite: "d",
		Expr: `sort((ssl_cert_not_after_timestamp_seconds{FILTRES} - time()) / 86400)`, Legende: "{{cn}}", Seuils: "erreur\n7 attention\n21 ok", Icone: "cadenas", Teinte: "vert"},

	// --- Logs
	{ID: "logs-volume", Categorie: "Logs", Titre: "Volume des logs par machine", Description: "Lignes reçues par minute (compteur du serveur).",
		Type: "courbe", Unite: "lignes/min", Expr: `60 * sum by (instance) (rate(mirador_logs_recues_total{job="mirador", FILTRES}[$__rate_interval]))`,
		Legende: "{{instance}}", Icone: "logs", Teinte: "bleu"},
	{ID: "logs-palmares", Categorie: "Logs", Titre: "Machines les plus bavardes (24 h)", Type: "barres",
		Expr: `topk(10, sum by (instance) (increase(mirador_logs_recues_total{job="mirador", FILTRES}[1d])))`, Legende: "{{instance}}", Icone: "logs", Teinte: "orange"},

	// --- Agents
	{ID: "agents-file", Categorie: "Agents", Titre: "Données en attente sur les agents", Description: "File locale quand le serveur ne répond pas.",
		Type: "barres", Unite: "bytes", Expr: `mirador_agent_queue_bytes{FILTRES} > 0`, Legende: "{{instance}}", Icone: "envoi", Teinte: "orange"},
	{ID: "agents-memoire", Categorie: "Agents", Titre: "Mémoire de mirador-agent", Type: "barres", Unite: "bytes",
		Expr: `mirador_agent_resident_memory_bytes{FILTRES}`, Legende: "{{instance}}", Icone: "memoire", Teinte: "gris"},
}

// categoriesModeles : ordre d'affichage.
func categoriesModeles() []string {
	var out []string
	for _, m := range modelesRequetes {
		if len(out) == 0 || out[len(out)-1] != m.Categorie {
			out = append(out, m.Categorie)
		}
	}
	return out
}

func modeleParID(id string) (ModeleRequete, bool) {
	for _, m := range modelesRequetes {
		if m.ID == id {
			return m, true
		}
	}
	return ModeleRequete{}, false
}

// reLabelVariable : étiquette visée par une variable (label_values(…, etiq)).
var reVirgulesVides = regexp.MustCompile(`\{\s*,\s*|,\s*\}|,\s*,`)

// filtresTableau : « etiq=~"$var" » pour chaque filtre du tableau.
func filtresTableau(t *Tableau) []string {
	var out []string
	for _, v := range t.Variables {
		m := reLabelValues.FindStringSubmatch(v.Requete)
		if m == nil {
			continue
		}
		out = append(out, m[2]+`=~"$`+v.Nom+`"`)
	}
	return out
}

// appliquerFiltres : remplace FILTRES par les filtres du tableau.
func appliquerFiltres(expr string, filtres []string) string {
	expr = strings.ReplaceAll(expr, "FILTRES", strings.Join(filtres, ", "))
	// Nettoyage quand il n'y a rien à mettre : {, x} {x, } {}.
	for {
		n := reVirgulesVides.ReplaceAllStringFunc(expr, func(s string) string {
			switch {
			case strings.HasPrefix(s, "{"):
				return "{"
			case strings.HasSuffix(s, "}"):
				return "}"
			}
			return ","
		})
		if n == expr {
			break
		}
		expr = n
	}
	return strings.ReplaceAll(expr, "{}", "")
}
