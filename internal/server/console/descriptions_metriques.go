// SPDX-License-Identifier: GPL-3.0-or-later

package console

import "strings"

// Descriptions en français des métriques connues (mirador-agent, collecteur,
// tableaux fournis). Sert à l'assistant de requête et aux explications.
var descriptionsMetriques = map[string]string{
	// Système
	"node_cpu_seconds_total":                     "temps processeur par mode (idle = au repos)",
	"node_load1":                                 "charge sur 1 min",
	"node_load5":                                 "charge sur 5 min",
	"node_load15":                                "charge sur 15 min",
	"node_memory_MemTotal_bytes":                 "mémoire totale",
	"node_memory_MemAvailable_bytes":             "mémoire disponible",
	"node_memory_Buffers_bytes":                  "mémoire des tampons",
	"node_memory_Cached_bytes":                   "mémoire en cache",
	"node_memory_SwapTotal_bytes":                "swap total",
	"node_memory_SwapFree_bytes":                 "swap libre",
	"node_boot_time_seconds":                     "heure du démarrage",
	"node_time_seconds":                          "heure de la machine",
	"node_uname_info":                            "informations système (noyau, version)",
	"node_reboot_required":                       "redémarrage nécessaire (1 = oui)",
	"node_pressure_cpu_waiting_seconds_total":    "attente du processeur (pression)",
	"node_pressure_io_waiting_seconds_total":     "attente des disques (pression)",
	"node_pressure_io_stalled_seconds_total":     "blocage sur les disques (pression)",
	"node_pressure_memory_waiting_seconds_total": "attente de mémoire (pression)",
	"node_pressure_memory_stalled_seconds_total": "blocage faute de mémoire (pression)",
	"node_vmstat_oom_kill":                       "processus tués faute de mémoire",
	"node_vmstat_pswpin":                         "pages lues depuis le swap",
	"node_vmstat_pswpout":                        "pages écrites dans le swap",
	// Disques
	"node_filesystem_avail_bytes":      "espace libre d'une partition",
	"node_filesystem_size_bytes":       "taille d'une partition",
	"node_filesystem_files":            "inodes d'une partition",
	"node_filesystem_files_free":       "inodes libres d'une partition",
	"node_disk_io_time_seconds_total":  "temps où le disque travaille",
	"node_disk_read_bytes_total":       "octets lus sur le disque",
	"node_disk_written_bytes_total":    "octets écrits sur le disque",
	"smartctl_device_smart_status":     "santé SMART du disque (1 = bon)",
	"smartctl_device_temperature":      "température du disque (SMART)",
	"smartctl_device_power_on_seconds": "durée de fonctionnement du disque",
	"smartctl_device_available_spare":  "réserve restante du SSD (%)",
	"smartctl_device_attribute":        "attribut SMART",
	// Températures
	"node_hwmon_temp_celsius": "température d'un capteur (processeur, carte mère…)",
	"node_thermal_zone_temp":  "température d'une zone thermique",
	"temperature":             "température du NAS (Synology)",
	"diskTemperature":         "température d'un disque du NAS (Synology)",
	// Réseau
	"node_network_receive_bytes_total":  "octets reçus par interface",
	"node_network_transmit_bytes_total": "octets envoyés par interface",
	"node_network_receive_errs_total":   "erreurs en réception",
	"node_network_transmit_errs_total":  "erreurs en émission",
	"node_network_receive_drop_total":   "paquets reçus jetés",
	"node_network_transmit_drop_total":  "paquets envoyés jetés",
	// Services, Docker, pare-feu
	"node_systemd_unit_state":                  "état d'un service systemd",
	"node_systemd_service_restart_total":       "redémarrages d'un service",
	"node_systemd_enabled_units_down":          "services activés mais arrêtés",
	"node_systemd_enabled_units_total":         "services activés",
	"node_systemd_unit_enabled_active":         "service activé et en marche (1 = oui)",
	"node_docker_up":                           "Docker répond (1 = oui)",
	"node_docker_container_running":            "conteneur en marche (1 = oui)",
	"node_docker_container_healthy":            "conteneur en bonne santé (1 = oui)",
	"node_docker_container_cpu_seconds_total":  "temps processeur d'un conteneur",
	"node_docker_container_memory_bytes":       "mémoire d'un conteneur",
	"node_docker_container_memory_limit_bytes": "limite de mémoire d'un conteneur",
	"node_docker_container_restart_count":      "redémarrages d'un conteneur",
	"node_docker_container_oom_killed":         "conteneur tué faute de mémoire (1 = oui)",
	"node_firewall_input_policy_drop":          "pare-feu : INPUT en DROP (1 = oui)",
	"node_firewall_input_rules":                "pare-feu : nombre de règles INPUT",
	// Logs vus par l'agent
	"mirador_logs_ssh_auth_failures_total":     "échecs de connexion SSH",
	"mirador_logs_kernel_disk_errors_total":    "erreurs disque du noyau",
	"mirador_logs_oom_kills_total":             "lignes « out of memory » dans les logs",
	"mirador_logs_container_crash_lines_total": "plantages de conteneurs dans les logs",
	"mirador_logs_lines_total":                 "lignes de logs envoyées",
	"mirador_logs_recues_total":                "lignes de logs reçues par le serveur (par machine)",
	// Agent lui-même
	"mirador_agent_resident_memory_bytes": "mémoire de mirador-agent",
	"mirador_agent_cpu_seconds_total":     "processeur de mirador-agent",
	"mirador_agent_queue_bytes":           "données en attente sur l'agent",
	"mirador_agent_info":                  "version de mirador-agent",
	// Certificats, UniFi
	"ssl_cert_not_after_timestamp_seconds":     "date d'expiration d'un certificat",
	"unpoller_device_cpu_utilization_ratio":    "processeur d'un équipement UniFi",
	"unpoller_device_memory_utilization_ratio": "mémoire d'un équipement UniFi",
	"unpoller_device_temperature_celsius":      "température d'un équipement UniFi",
	"unpoller_device_uptime_seconds":           "durée depuis le démarrage (UniFi)",
	"unpoller_device_stations":                 "clients connectés (UniFi)",
	"unpoller_client_radio_signal_db":          "signal wifi d'un client (UniFi)",
	"unpoller_device_port_poe_watts":           "puissance PoE d'un port (UniFi)",
	"up":                                       "cible joignable (1 = oui)",
}

// descriptionMetrique : description connue, ou devinée pour quelques
// familles (vide si rien de sûr).
func descriptionMetrique(nom string) string {
	if d, ok := descriptionsMetriques[nom]; ok {
		return d
	}
	switch {
	case strings.HasPrefix(nom, "pve_"):
		return "Proxmox : " + strings.ReplaceAll(strings.TrimPrefix(nom, "pve_"), "_", " ")
	case strings.HasPrefix(nom, "unpoller_"):
		return "UniFi : " + strings.ReplaceAll(strings.TrimPrefix(nom, "unpoller_"), "_", " ")
	}
	return ""
}
