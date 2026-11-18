# SPEC.md — cahier des charges de mirador-stack

- Version : 1.0 — **validée par l'utilisateur le 07/11/2026** (fin de l'étape 1).
- Toute modification ultérieure est notée ici avec sa date et sa raison.
- Référence complète : `docs/BRIEF.md`. Ce document ne le répète pas : il le
  traduit en décisions, en inventaire de l'existant et en points à trancher.
- Légende : **[Décidé]** = validé par l'utilisateur ou proposé dans cette PR ;
  **[À mesurer]** = choix provisoire, confirmé ou changé après mesure ;
  **[Ouvert]** = attend une réponse (voir `PROGRES.md`).

---

## 1. Objet et périmètre

- Outil de supervision perso en Go, mode **push uniquement**, pour remplacer à terme :
  Grafana Alloy + VictoriaMetrics + VictoriaLogs + vmalert + Alertmanager + Grafana.
- Quatre binaires :
  - `mirador-agent` : sur chaque machine Linux, collecte et envoie. **N'ouvre jamais de port.**
  - `mirador-server` : sur le VPS, derrière nginx. Stockage, alertes, notifications, interface web.
  - `mirador-release` : sur le poste de l'utilisateur, signe les versions de l'agent (Ed25519).
  - `mirador-collector` (étape 6, **[Décidé]** réponse du 07/11) : sur le collecteur LAN,
    reçoit ce qui ne sait pas pousser seul et pousse vers le serveur (§ 5.3).
- Hors périmètre :
  - mise à jour du serveur par le mécanisme de mise à jour des agents ;
  - agents Docker (mis à jour par leur image) ;
  - tout ce qui ressemble à de l'exécution à distance (voir § 12) ;
  - surveillance de disponibilité des services publics (outil externe déjà
    en place) : **elle reste**, indépendante, comme second filet.

---

## 2. Parc et contraintes chiffrées (réponses du 07/11)

| Élément | Valeur |
|---|---|
| Machines | 25 aujourd'hui, 25 dans 2 ans |
| Détail | 2 Proxmox physiques, 1 OPNsense, 2 NAS Synology, 1 contrôleur UniFi (1 borne, 1 switch), plusieurs VM Linux, plusieurs VPS Ubuntu 24.04, 1 collecteur LAN (LXC) |
| arm64 | aucune (compilation arm64 gardée, non testée sur du réel) |
| Agents Linux | Debian / Ubuntu uniquement |
| Sans agent | OPNsense, Synology, UniFi (+ metric server Proxmox) |
| VPS de supervision | 4 Go RAM (3,73 Gio vus par Docker), 2 vCPU, disque 38 Go : **17 Go utilisés, 22 Go libres** (`df -h /`) |
| Conservation actuelle | 30 jours (métriques et logs) |
| Docker sur le VPS | oui (stack obs et autres services) |

### 2.1 Mesures de la stack actuelle (07/11, sur le VPS)

**RAM et CPU (instantané `docker stats`)**

| Conteneur | RAM | CPU (instantané) |
|---|---|---|
| obs-victoriametrics | 248 Mio | 26,8 % (un seul relevé, sans doute une fusion en cours) |
| obs-victorialogs | 187 Mio | 1,4 % |
| obs-grafana | 291 Mio | 1,3 % |
| obs-alertmanager | 24 Mio | 0,1 % |
| obs-vmalert | 14 Mio | 1,1 % |
| obs-vmalert-logs | 5 Mio | 0,1 % |
| **Total stack obs** | **≈ 770 Mio** | — |
| docker-socket-proxy (déjà en place) | 17,6 Mio | 0,03 % |

→ L'objectif mirador (< 256 Mio) fait ~3 fois moins que la stack actuelle.
→ Le proxy de socket Docker coûte ~18 Mio : à ajouter au budget des machines
Docker (§ 6.5).

**Métriques**

- Taille sur disque de VictoriaMetrics : **1,1 Go** pour 30 jours.
- `series/count` : **94 953 séries**. Ce chiffre compte toutes les séries vues
  sur la période (y compris celles qui ont disparu : conteneurs recréés, etc.),
  pas seulement les séries actives. Le nombre de séries actives reste à mesurer.
- Soit ~3 800 séries par machine sur 30 jours, bien plus que les 500 par agent
  prévus par le brief. Cause probable : Alloy envoie tout ce que node_exporter
  produit, alors que les règles et dashboards n'utilisent qu'une petite partie
  (§ 4.1).

**Logs sur 24 h (champ message seul, sans les labels)**

| Source (`job`) | Lignes / jour | Volume / jour |
|---|---|---|
| docker | 862 461 | 172 Mo |
| journal | 482 015 | 87 Mo |
| syslog | 52 501 | 2,9 Mo |
| loki.source.journal.system | 5 302 | 0,3 Mo |
| unifi | 98 | 14 Ko |
| **Total** | **≈ 1,40 million (≈ 16 lignes/s en moyenne)** | **≈ 262 Mo** |

- Docker = 61 % des lignes : premier poste à filtrer (quels conteneurs ? mesure
  demandée dans `PROGRES.md`).
- `loki.source.journal.system` : nom de job par défaut d'Alloy sur une machine dont
  la config ne fixe pas `job` ; le dashboard Logs l'accepte déjà. mirador
  normalisera en `journal`.
- Sur 30 jours : ~7,9 Go de texte brut, soit de l'ordre de **1 à 1,5 Go
  compressé** (zstd, rapport 5 à 8 sur des logs). Taille réelle de VictoriaLogs
  sur disque : à mesurer (`du` absent de l'image, autre commande dans `PROGRES.md`).

**Conséquences**

- Les tests de charge gardent **50 agents** (marge × 2 sur le parc réel), avec le
  nombre de séries par agent **mesuré** à l'étape 2 (et non 500 supposé).
- Le jeu de logs simulé (étape 3) reprend ce profil : ~16 lignes/s en moyenne,
  pics à × 10, 60 % Docker, ~190 octets par ligne.
- Pendant la migration, la stack actuelle (~770 Mio) et mirador tournent ensemble :
  la RAM suffit.
- **Disque** : 22 Go libres. Plafond mirador par défaut : **10 Gio** (réponse du 10/11 ; 8 Go au départ) pendant la période
  en parallèle (il reste ~14 Go de marge). Ajustable dans l'interface.

---

## 3. Analyse du dossier `reference/`

### 3.1 Contenu (après ajout des fichiers manquants)

| Fichier | Contenu |
|---|---|
| `system.yml` | 31 règles vmalert (hôtes Linux, sécurité, matériel, Docker), dont `Watchdog` |
| `equipements.yml` | 39 règles vmalert (Synology, sources muettes, OPNsense, UniFi) |
| `syslog.yml` | 1 règle d'enregistrement LogsQL : `syslog_lignes_24h{env, src_ip}` (toutes les 15 min) |
| `alertmanager.yml` | routage Telegram + heartbeat HetrixTools + 3 inhibitions |
| `telegram.tmpl` | modèle de message Telegram (HTML) |
| `obs-agent-install.sh` | installeur Alloy + 4 collecteurs « textfile » (VM, VPS) |
| `config-proxmox.alloy` | config Alloy des 2 Proxmox (+ SMART via `smartctl_exporter`) |
| `obs-collecteur-install.sh` | collecteur LAN : syslog TLS, SNMP v3, sondes TLS distantes, unpoller |
| `supervision-push-v2.md` | document d'architecture complet (VPS, nginx, Proxmox, OPNsense, Synology, UniFi, règles, déploiement) |
| `grafana/*.json` | 8 dashboards, 181 panneaux au total |

### 3.2 Corrections et constats

- **70 règles d'alerte** (31 + 39, `Watchdog` compris) **+ 1 règle
  d'enregistrement** (`syslog_lignes_24h`). Pas 72.
- **Métriques `smartctl_*`** : viennent de `smartctl_exporter` installé à la main
  sur chaque Proxmox, en root, sur `127.0.0.1:9633`, lu toutes les 120 s
  (`supervision-push-v2.md` § 6.3, `config-proxmox.alloy`). Pas sur les VPS ni les VM.
- `supervision-push-v2.md` § 11.2 décrit aussi 5 règles d'alerte LogsQL
  (`logs.yml`). **[Décidé]** Ce sont des **ajouts possibles**, pas des règles
  existantes (réponse du 07/11, sauf indication contraire). Voir § 4.6 bis.
- Les règles et dashboards utilisent du **MetricsQL** (`lag()`) et du **LogsQL** :
  l'import doit traduire, pas copier (§ 9.2).
- Valeur figée dans une règle : limite de la table d'états OPNsense `605600` →
  paramètre modifiable dans l'interface.
- Label de la machine **différent selon la source** :
  - `instance` : métriques des agents (nom choisi), SNMP (nom `--nas`), sondes TLS, UniFi ;
  - `host` : logs, Telegraf (OPNsense), metric server Proxmox ;
  - `nodename` : invités Proxmox (VM/LXC) dans le metric server.
  Voir § 4.5.
- Aucun secret réel dans `reference/` : tokens d'exemple (`T0k3n_...`), IP de
  documentation (`192.0.2.x`, `203.0.113.x`), domaine `example.org`.

---

## 4. Ce qu'il faut garder

### 4.1 Métriques système des agents (format node_exporter)

Collecteurs actifs aujourd'hui, à reproduire avec **les mêmes noms et labels** à
l'étape 2 (pour que dashboards et règles actuels marchent) :

- `cpu`, `stat`, `loadavg`, `pressure` (PSI), `meminfo`, `vmstat`
- `filesystem`, `diskstats`, `filefd`
- `netdev`, `netstat`
- `systemd` (avec `node_systemd_service_restart_total`)
- `hwmon` (+ `thermal_zone` sur les Proxmox), `uname`, `time`, `os`

Filtres actuels, gardés comme **valeurs par défaut** (réglables plus tard) :

- Points de montage exclus : `/dev`, `/proc`, `/sys`, `/run/credentials/*`,
  `/var/lib/docker/*`, `/var/lib/containers/*`, `/var/lib/lxcfs/*`.
- Types de FS exclus : autofs, binfmt_misc, bpf, cgroup, cgroup2, configfs, debugfs,
  devpts, devtmpfs, fuse.lxcfs, fusectl, hugetlbfs, iso9660, mqueue, nsfs, overlay,
  proc, procfs, pstore, rpc_pipefs, securityfs, selinuxfs, squashfs, sysfs, tracefs.
- Disques exclus : `ram*`, `loop*`, `fd*`, `dm-*`, `sr*`, `nbd*`, `zd*`.
- Interfaces exclues : `veth*`, `docker*`, `br-*`, `lo`, `virbr*`, `tap*`, `fwbr*`, `fwln*`, `fwpr*`.
- Unités systemd : `*.service`, `*.timer`, `*.mount`.
- Métriques jetées : `go_*`, `promhttp_*`, `node_scrape_collector_*`, `node_textfile_*`,
  `node_systemd_unit_start_time_seconds`, et `node_systemd_unit_state` pour les
  états `inactive|activating|deactivating`.
- Intervalle : 30 s.

**[Décidé] Collecter ce qui sert, pas tout node_exporter.**
Les 70 règles et les 8 dashboards n'utilisent que **42 métriques `node_*`** côté
agents (liste ci-dessous). L'agent produit ces métriques, avec les mêmes noms et
labels, et pas les centaines d'autres de node_exporter. Gain attendu : beaucoup
moins de séries (RAM et disque du serveur, réseau, CPU de l'agent).

- CPU, charge : `node_cpu_seconds_total`, `node_load5`
- Mémoire : `node_memory_MemAvailable_bytes`, `node_memory_MemTotal_bytes`,
  `node_memory_SwapFree_bytes`, `node_memory_SwapTotal_bytes`, `node_vmstat_pswpin`,
  `node_vmstat_pswpout`
- PSI : `node_pressure_cpu_waiting_seconds_total`, `node_pressure_io_stalled_seconds_total`,
  `node_pressure_memory_stalled_seconds_total`
- Disques : `node_filesystem_avail_bytes`, `node_filesystem_size_bytes`,
  `node_filesystem_files`, `node_filesystem_files_free`, `node_disk_io_time_seconds_total`,
  `node_disk_read_bytes_total`, `node_disk_written_bytes_total`
- Réseau : `node_network_{receive,transmit}_bytes_total`,
  `node_network_{receive,transmit}_errs_total`, `node_network_{receive,transmit}_drop_total`
- systemd : `node_systemd_unit_state`, `node_systemd_service_restart_total`,
  `node_systemd_unit_enabled_active`, `node_systemd_enabled_units_total`,
  `node_systemd_enabled_units_down`
- Système : `node_uname_info`, `node_boot_time_seconds`, `node_time_seconds`,
  `node_hwmon_temp_celsius`
- Pare-feu : `node_firewall_input_policy_drop` (+ `node_firewall_input_rules`)
- Docker : `node_docker_up`, `node_docker_container_{running,restart_count,oom_killed,healthy,cpu_seconds_total,memory_bytes,memory_limit_bytes}`
- Certificats : `ssl_cert_not_after_timestamp_seconds`, `ssl_certs_found_total`,
  `ssl_certs_read_errors_total`
- SMART : les 5 `smartctl_*` du § 4.2

Ajouts proposés (peu de séries, utiles au détail machine ou aux nouvelles alertes) :
`node_vmstat_oom_kill` (OOM, § 4.6 bis), `node_load1`, `node_load15`,
`node_memory_Cached_bytes`, `node_memory_Buffers_bytes`, `node_thermal_zone_temp`
(Proxmox), métriques de mises à jour en attente, métriques internes de l'agent.
Les autres métriques Docker d'`obs-docker.py` (réseau, blkio, nb par état) restent
disponibles mais désactivées par défaut.

Nombre de séries par machine : **mesuré à l'étape 2** et comparé à Alloy.

### 4.2 Métriques « maison » (collecteurs textfile et exporters actuels)

À reproduire **dans l'agent Go**, sans script, sans Python, sans exporter séparé :

| Origine actuelle | Métriques | Fréquence actuelle |
|---|---|---|
| `obs-systemd.sh` | `node_systemd_unit_enabled_active{name}`, `node_systemd_enabled_units_total`, `node_systemd_enabled_units_down` | 15 min |
| `obs-certs.sh` | `ssl_cert_not_after_timestamp_seconds{path,cn,issuer}`, `ssl_cert_not_before_timestamp_seconds`, `ssl_certs_found_total`, `ssl_certs_read_errors_total` | 15 min |
| `obs-firewall.sh` | `node_firewall_input_policy_drop{tool,policy}`, `node_firewall_input_rules` | 15 min |
| `obs-docker.py` | `node_docker_up`, `node_docker_containers{state}`, `node_docker_container_running{name,project,state,restart_policy}`, `..._restart_count`, `..._oom_killed`, `..._healthy`, `..._cpu_seconds_total`, `..._memory_bytes`, `..._memory_limit_bytes`, `..._network_{receive,transmit}_bytes_total`, `..._blkio_{read,write}_bytes_total` | 1 min |
| `smartctl_exporter` (Proxmox) | utilisées : `smartctl_device_smart_status`, `smartctl_device_temperature{temperature_type}`, `smartctl_device_attribute{attribute_name,attribute_value_type}` (`Reallocated_Sector_Ct`, `Current_Pending_Sector`), `smartctl_device_power_on_seconds`, `smartctl_device_available_spare` | 2 min |

Logique fine à conserver (elle évite des fausses alertes) :

- `obs-systemd` : exclure unités modèles (`@.service`), `Type=oneshot`,
  `ConditionResult=no`, unités activées par socket/timer/path, tâches de démarrage
  terminées avec code 0 en moins de 2 min, et une liste d'exclusions (aujourd'hui
  `/etc/obs-systemd-ignore`, demain un réglage de l'interface).
- `obs-certs` : chemins `/etc/letsencrypt/live/*/cert.pem`, `/etc/ssl/local/*.crt|*.pem`,
  `/etc/pve/local/pveproxy-ssl.pem`, `/etc/proxmox-backup/proxy.pem` ; dédoublonnage
  par inode ; pas de `pve-ssl.pem` ni `pve-root-ca.pem`.
- `obs-firewall` : iptables (y compris ufw, iptables-nft) puis nftables natif.
- `obs-docker` : mémoire = usage − `inactive_file` ; `healthy` absent sans healthcheck.
- SMART : seulement sur machines physiques (pas sur VPS ni VM). Contrôleur RAID
  matériel : options `-d megaraid,N` à prévoir (réglage typé, pas de commande libre).

Nouveau (demandé par le brief) : mises à jour en attente.

### 4.3 Logs

- journald : labels `unit`, `level`, `ident`, `job=journal`, `host`. Reprise au
  maximum 12 h en arrière au démarrage.
- Docker : labels `container`, `stream`, `job=docker`, `host`.
- Filtrage à la source (valeurs par défaut) :
  - niveau `debug` jeté ;
  - bruit systemd jeté : `session opened/closed for user`, `pam_unix(cron`,
    `Started/Starting Session`, `Removed/Created slice` ;
  - limite 200 lignes/s, rafale 500, par machine.
- Flux entrants bloqués (VPS) : lignes noyau iptables `IN=... OUT= ... SRC=...`,
  avec nom de la règle (préfixe `regle: IN=`), IP source, port visé, pays (GeoIP).
  **[Décidé]** gardé (réponse 4). Voir § 6.6.
- Syslog des appliances (collecteur LAN) : labels `job=syslog`, `host`, `app`,
  `level`, `src_ip`, `env` (rôle : `firewall`, `nas`) ; `filterlog` d'OPNsense
  jeté par défaut (plusieurs Go/jour) ; limite 500 lignes/s, rafale 1000.
- UniFi (unpoller → logs) : `job=unifi`, `env=reseau`, `app` (`unifi_event`,
  `unifi_alarm`, `unifi_ids`, `unifi_anomaly`), `level`, `key`.

### 4.4 Sources sans agent (étape 6)

| Source | Aujourd'hui | Métriques / labels clés |
|---|---|---|
| Proxmox | metric server natif, protocole InfluxDB v2, `Authorization: Token ...`, 30 s ; Proxmox appelle `GET /health` avant d'accepter la cible | `cpustat_*`, `memory_*`, `nics_*`, `system_*`, `blockstat_*` ; labels `object` (nodes, qemu, lxc, storages), `host`, `nodename` |
| OPNsense | Telegraf (`os-telegraf`), InfluxDB v2, `Token`, 30 s | `cpu_usage_*`, `mem_used_percent`, `swap_used_percent`, `disk_used_percent`, `system_*`, `processes_*`, `net_*`, `pf_*`, `ping_*`, `unbound_*` ; label `host` |
| Synology ×2 | SNMP v3 (SHA + AES) depuis le collecteur, 60 s ; modules `synology`, `if_mib`, `system`, `ucd_system_stats`, `ucd_memory` | MIB Synology (system, power, fans, raid, disk, ups, upgrade, storageIO, services), `ifHC*`, `sysUpTime` ; `job=snmp`, `instance`, `env=nas` |
| UniFi | unpoller (API HTTPS du contrôleur, compte lecture seule) sur le collecteur, 60 s | `unpoller_device_*`, `unpoller_client_*`, `unpoller_site_*` ; `job=unifi`, `env=reseau` |
| Certificats distants | sonde HTTPS (blackbox) depuis le collecteur, 5 min, certificat lu sans vérification | renommé en `ssl_cert_not_after_timestamp_seconds{job="certs", instance=NOM, path=https://HOTE:PORT}` + `probe_ssl_last_chain_info`, `probe_success` |
| Syslog | TLS sur 6514 (TLS 1.2 min.), RFC 5424, certificat Let's Encrypt (défi DNS deSEC), liste d'IP autorisées avec rôle | voir § 4.3 |

### 4.5 Labels

- **[Décidé]** Étape 2 : on garde **exactement** les labels actuels (`instance` pour
  les métriques, `host` pour les logs) — réponse 4.
- **[Décidé]** Stockage natif (étape 3) : un seul identifiant interne de machine,
  présenté sous `host` **et** `instance` pour rester compatible avec les règles
  importées. Les invités Proxmox (`nodename`) sont rattachés à leur nœud.
- Nom de machine : mêmes règles que le script (refus des noms génériques,
  format `^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`).

### 4.6 Les 70 règles d'alerte

Source mirador prévue : **A** = agent, **R** = assistant root de l'agent (§ 6.4),
**S** = SNMP, **T** = Telegraf OPNsense, **P** = Proxmox, **U** = UniFi,
**L** = syslog / logs, **I** = interne au serveur. S, T, P, U, L arrivent à l'étape 6.

| # | Règle | Gravité | Durée | Source | Remarque pour la traduction |
|---|---|---|---|---|---|
| 1 | Watchdog | none | — | I | devient le heartbeat HetrixTools interne (§ 10) |
| 2 | AgentMuet | critical | — | I | « dernière donnée > 60 s », détecté par le serveur |
| 3 | CpuEleve | warning | 1m | A | |
| 4 | LoadAverageEleve | warning | 10m | A | load5 / nb de cœurs > 2 |
| 5 | MemoireFaible | warning | 10m | A | |
| 6 | SwapIntensif | warning | 15m | A | |
| 7 | DisqueFaible | warning | 5m | A | |
| 8 | DisqueSaturationPrevue | warning | 1h | A | tendance (`predict_linear` 12 h → 4 j) |
| 9 | InodesFaibles | warning | 15m | A | |
| 10 | ServiceSystemdEnEchec | critical | 5m | A | exclusion `fwupd-refresh.service` |
| 11 | ServiceAutomatiqueArrete | warning | 10m | A | |
| 12 | ServiceRedemarreEnBoucle | warning | 10m | A | |
| 13 | IoDisqueSature | warning | 15m | A | |
| 14 | TraficAnormal | warning | 10m | A | > 800 Mbit/s entrant |
| 15 | ErreursReseau | warning | 15m | A | |
| 16 | TemperatureElevee | warning | 10m | A | |
| 17 | RedemarrageInattendu | info | — | A | |
| 18 | PareFeuNonRestrictif | critical | 5m | R | |
| 19 | CertificatExpireBientot | warning | 10m | A | < 21 j (agents **et** sondes distantes) |
| 20 | CertificatExpireTresBientot | critical | 10m | A | < 7 j |
| 21 | CollecteCertificatsMuette | warning | 1h | A | « en avait sur 7 j, n'en a plus » |
| 22 | CertificatsIllisibles | info | 1h | A | |
| 23 | SmartEnEchec | critical | 5m | R | Proxmox uniquement |
| 24 | SecteursRealloues | warning | — | R | hausse sur 24 h |
| 25 | DisqueChaud | warning | 20m | R | |
| 26 | DockerInjoignable | critical | 3m | A | via proxy de socket |
| 27 | ConteneurArrete | critical | 3m | A | seulement `always`/`unless-stopped` |
| 28 | ConteneurRedemarreEnBoucle | warning | — | A | |
| 29 | ConteneurNonSain | warning | 5m | A | |
| 30 | ConteneurTueParOOM | warning | — | A | |
| 31 | ConteneurMemoireSaturee | warning | 10m | A | |
| 32 | SynologyInjoignable | critical | 10m | S | `via=collecteur` |
| 33 | SynologySystemeEnPanne | critical | 5m | S | |
| 34 | SynologyAlimentationEnPanne | critical | 5m | S | |
| 35 | SynologyVentilateurHS | critical | 5m | S | |
| 36 | SynologyVolumeDegrade | critical | 2m | S | inhibe #37 |
| 37 | SynologyVolumeEnReparation | warning | — | S | |
| 38 | SynologyDisqueAvertissement | warning | 10m | S | |
| 39 | SynologyDisqueCritique | critical | 5m | S | |
| 40 | SynologyNouveauxSecteursDefectueux | warning | — | S | hausse sur 24 h |
| 41 | SynologyVolumePresquePlein | warning | 30m | S | > 90 % |
| 42 | SynologyVolumePlein | critical | 10m | S | > 97 % |
| 43 | SynologyTemperatureElevee | warning | 15m | S | |
| 44 | SynologyDisqueChaud | warning | 20m | S | |
| 45 | SynologyOnduleurSurBatterie | critical | 1m | S | état onduleur en label (`upsInfoStatus=~"OB.*"`) |
| 46 | SynologyMiseAJourDSM | info | 1d | S | |
| 47 | OpnsenseMuet | critical | 2m | I | source muette > 5 min |
| 48 | ProxmoxMuet | critical | 2m | I | idem |
| 49 | UnifiMuet | critical | 2m | I | idem, `via=collecteur` |
| 50 | CertificatIllisible | warning | 30m | S | sonde HTTPS distante, `via=collecteur` |
| 51 | SyslogMuet | warning | — | I | remplace la règle d'enregistrement `syslog_lignes_24h` : « dernière ligne reçue de cette IP » tenu à l'ingestion |
| 52 | OpnsenseCpuEleve | warning | 5m | T | |
| 53 | OpnsenseMemoireFaible | warning | 10m | T | |
| 54 | OpnsenseSwapUtilise | warning | 15m | T | |
| 55 | OpnsenseDisquePresquePlein | warning | 15m | T | |
| 56 | OpnsenseDisquePlein | critical | 5m | T | |
| 57 | OpnsenseRedemarre | warning | — | T | |
| 58 | OpnsenseTableEtatsPresquePleine | warning | 10m | T | limite 605600 → paramètre |
| 59 | OpnsenseTableEtatsPleine | critical | 2m | T | idem |
| 60 | OpnsenseEtatsAnormaux | warning | 10m | T | 5 × moyenne 7 j et > 20 000 |
| 61 | OpnsensePertesPaquets | warning | 10m | T | |
| 62 | OpnsenseLatenceElevee | warning | 15m | T | |
| 63 | OpnsenseErreursInterface | warning | 15m | T | |
| 64 | UnifiEquipementDeconnecte | critical | 5m | U | `via=collecteur` |
| 65 | UnifiEquipementRedemarre | warning | — | U | idem |
| 66 | UnifiCpuEleve | warning | 15m | U | idem |
| 67 | UnifiMemoireSaturee | warning | 15m | U | idem |
| 68 | UnifiTemperatureElevee | warning | 15m | U | idem |
| 69 | UnifiErreursPort | warning | 15m | U | idem |
| 70 | UnifiMiseAJourDisponible | info | 1d | U | idem |

Fonctions utilisées (à couvrir par le langage d'alertes, § 9.2) : seuil, rapport de
deux métriques, `rate`, `increase`, `resets`, `lag`, `predict_linear`,
`avg_over_time` (7 j), `max_over_time` (7 j), agrégations `min/max/avg/count by`,
filtres de labels (égalité, différence, regex), `and`, `or`, `time()`.

### 4.6 bis Ajouts possibles (règles de logs du document, § 11.2)

Pas en service aujourd'hui (réponse du 07/11). Proposés comme **modèles prêts à
l'emploi, désactivés par défaut**, sauf l'OOM.

| Règle | Gravité | Proposition mirador | Coût |
|---|---|---|---|
| **OomKiller** | critical | **[Décidé] activée par défaut.** Basée sur le compteur noyau `oom_kill` de `/proc/vmstat` (métrique `node_vmstat_oom_kill`) : hausse sur 5 min > 0 → alerte. Le message ajoute le processus tué, lu dans la ligne noyau `Out of memory: Killed process ...` du journal (déjà lu par l'agent). | quasi nul : un compteur lu avec les autres, pas de recherche dans les logs |
| PicErreursSysteme | warning | > 50 lignes de niveau err/crit/alert/emerg en 5 min par machine : compteur tenu par l'agent pendant le filtrage | faible |
| EchecsAuthSsh | warning | > 20 `Failed password` de `sshd` en 10 min : compteur tenu par l'agent | faible |
| ErreursKernelDisque | critical | `I/O error`, `medium error`, `EXT4-fs error` dans les lignes noyau : compteur tenu par l'agent | faible |
| ConteneurEnBoucle | warning | `panic:`, `FATAL`, `Segmentation fault` > 5 en 15 min par conteneur | faible ; recoupe ConteneurRedemarreEnBoucle |

- **[Décidé]** Principe : les alertes « sur les logs » deviennent des **compteurs
  calculés par l'agent au fil de l'eau** (motifs figés et typés, pas d'expression
  libre envoyée par le serveur), envoyés comme des métriques. Le serveur ne relit
  jamais les logs stockés pour évaluer une alerte.
- L'OOM des conteneurs reste couvert par `ConteneurTueParOOM` (# 30).

### 4.7 Routage, modèle de message et inhibitions

Réglages par défaut de mirador, repris d'Alertmanager :

- `critical` → Telegram **avec son**, regroupement 10 s, rappel toutes les 2 h.
- `warning` / `info` → Telegram **silencieux**, regroupement 30 s, rappel toutes les 12 h.
- Regroupement par `alertname` + `instance`, intervalle de groupe 5 min.
- Messages de résolution envoyés.
- Modèle (`telegram.tmpl`), à garder comme modèle par défaut :
  - pastille de gravité 🔴 critical / 🟠 warning / 🔵 autre ;
  - nom de l'alerte en gras, résumé, description en italique ;
  - machine (`instance`, sinon `host`, sinon `src_ip`) + « depuis JJ/MM HH:MM » ;
  - résolution : ✅ « Résolu », nom, résumé, machine, « fin JJ/MM HH:MM » ;
  - **ajouts mirador** : valeur, durée, lien direct vers l'alerte, canal utilisé si secours.
- Heartbeat HetrixTools : toutes les minutes (indépendant des canaux).
- Inhibitions :
  - machine muette → ses alertes `warning`/`info` sont tues ;
  - collecteur LAN muet → toutes les alertes `via=collecteur` sont tues
    (dans mirador : **dépendance** « équipements → collecteur ») ;
  - `SynologyVolumeDegrade` tue `SynologyVolumeEnReparation` sur la même machine.

### 4.8 Dashboards (8 tableaux, 181 panneaux)

Rafraîchissement actuel : 1 min (flux bloqués : 10 s, certificats : 5 min).
Périodes par défaut : 6 h (certificats : 30 j, flux bloqués : 15 min).

**Serveurs Linux** (24 panneaux ; filtres site / env / machine, hors Proxmox)

- Vue d'ensemble : nb de machines, machines muettes (> 5 min), services en échec,
  services auto arrêtés, conteneurs arrêtés, INPUT pas en DROP, disques > 85 %.
- Tableau « État du parc » : dernière donnée, CPU, mémoire, disque le plus plein,
  load/cœur, uptime, services en échec, auto arrêtés, pare-feu, Docker.
- Ressources : CPU, load/cœur, mémoire, swap.
- Disques : occupation des FS, saturation (io_time), débit lecture/écriture.
- Réseau : trafic (hors interfaces virtuelles), erreurs et pertes.
- Services et sécurité : unités en échec, activées mais arrêtées, pare-feu.
- Docker : état des conteneurs, consommation (CPU, mémoire, redémarrages, santé),
  top 10 CPU, top 10 mémoire.

**Proxmox** (31 ; filtre nœud)

- Vue d'ensemble : nœuds actifs, VM en marche, LXC en marche, uptime le plus court.
- Nœuds (metric server) : CPU, load, mémoire, iowait et swap, trafic, ARC ZFS.
- Nœuds (agent) : débit des disques physiques, PSI.
- Invités : inventaire (CPU, mémoire, uptime), top CPU / mémoire / disque / réseau,
  I/O par disque virtuel, PSI par invité.
- Stockages : occupation, évolution de l'espace libre.
- Matériel : températures (hwmon), température des disques, état SMART
  (statut, heures, température), secteurs réalloués / en attente, réserve SSD.
- systemd : en échec, activées mais arrêtées, nb de services, redémarrages sur 24 h.

**OPNsense** (30 ; filtres hôte, interface)

- État : dernière remontée, uptime, CPU, mémoire, load/cœur, disque /, swap, états pf.
- Système : CPU (user/system/irq), mémoire et swap, load, disques, processus (total, en cours, zombies).
- Réseau : trafic, paquets/s, erreurs et pertes par interface.
- pf : table d'états, créations/suppressions, règles et recherches.
- Latence et DNS : ping (latence, pertes) par cible, Unbound (requêtes, cache).
- Logs (syslog) : lignes, erreurs, avertissements, applications, volume par
  application, programmes en erreur, erreurs/avertissements, recherche.

**Synology** (31 ; filtre NAS)

- État : joignable, système, alimentation, ventilateurs, volumes, santé disques,
  température, mise à jour DSM.
- NAS : modèle, version, uptime, CPU, mémoire, température (avec repli sur la MIB UCD
  si le module Synology ne donne pas CPU/mémoire).
- Stockage : volumes (état, taille, libre, %, spare), disques (modèle, état, santé,
  température, secteurs défectueux, retries, durée de vie restante), I/O et charge par disque.
- Réseau et services : trafic, erreurs, utilisateurs connectés par service.
- Onduleur : état, charge batterie, autonomie, charge.
- Logs (syslog) : comme OPNsense.

**UniFi** (30 ; filtres équipement, ESSID)

- État : dernière collecte, équipements, déconnectés, clients Wi-Fi / filaires,
  mises à jour, satisfaction Wi-Fi, signal le plus faible.
- Équipements : inventaire (type, modèle, version, IP, uptime, CPU, mémoire, clients),
  CPU, mémoire, température.
- Wi-Fi : clients par borne et par réseau, occupation des canaux, 15 clients au
  signal le plus faible, satisfaction par borne.
- Trafic : par équipement, top 10 clients.
- Ports des switchs : top 10 trafic, PoE, erreurs/pertes, vitesse négociée.
- Événements du contrôleur (logs) : événements, alarmes, IDS/IPS, anomalies,
  volume par type, top événements, liste.

**Logs** (13 ; filtres site, env, machine, source, niveau, recherche libre)

- Vue d'ensemble : lignes, machines émettrices, erreurs système, erreurs conteneurs.
- Volume par machine, logs système par niveau, volume d'erreurs par machine.
- Palmarès : machines, programmes, conteneurs les plus en erreur.
- Listes : erreurs système, erreurs conteneurs, recherche dans tous les logs.
- « Erreur conteneur » = recherche insensible à la casse de `error`, `err`, `fatal`,
  `panic`, `critical`, `traceback`, `segmentation fault`, `...Exception`, y compris
  avec codes couleur ANSI.

**Certificats TLS** (9 ; filtre machine)

- Certificats suivis, expirés, < 7 j, < 21 j, échéance la plus proche.
- Inventaire (machine, chemin, CN, émetteur, jours restants) fusionnant certificats
  locaux (agents) et distants (sondes, CN/émetteur via `probe_ssl_last_chain_info`).
- Jours restants, évolution des échéances, état de la collecte par machine.

**Flux entrants bloqués** (13 ; filtres env, machine, règle)

- Flux, IP sources distinctes, pays, ports visés.
- Carte du monde par pays, top pays, flux par machine, flux par règle, évolution.
- Top IP (avec pays), adresses par règle, top ports, derniers flux.
- Filtre de base : lignes noyau `IN=x OUT=` avec `SRC=`, sans interfaces virtuelles,
  sans broadcast/multicast, sans `0.0.0.0` ni `fe80:`.
- **[Décidé]** la même carte pour OPNsense, plus tard (réponse 4, voir § 6.6).

**Ce qu'il faudra traduire**

| Langage | Où | Éléments utilisés |
|---|---|---|
| PromQL standard | tous les tableaux métriques | `rate`, `increase`, `topk`, `bottomk`, `min/max/avg/sum/count by`, `or vector(0)`, `> bool`, `timestamp()`, `time()`, `label_replace`, `group_left`, sélection par regex sur `__name__`, `label_values()` pour les filtres |
| MetricsQL (VictoriaMetrics) | Serveurs Linux (machines, muettes, état du parc) | `lag()` → dans mirador : « dernière donnée reçue », tenu à l'ingestion |
| Macro Grafana | partout | `$__rate_interval` → pas automatique selon la période affichée |
| LogsQL (VictoriaLogs) | Logs, Flux bloqués, OPNsense, Synology, UniFi, `syslog.yml` | filtres de champs (`=`, `:~` regex, `-~` négation, `i()` casse), `stats count() / count_uniq() by`, `extract_regexp`, `extract`, `filter`, `sort`, `limit`, `_time:` |

**[Décidé]** Pas de moteur PromQL/LogsQL complet côté interface : chaque panneau
devient une **requête nommée** écrite en Go (paramètres : machine, période, filtres),
calculée côté serveur, mise en cache bornée. Moins de RAM et de CPU qu'un moteur
générique, et rien d'arbitraire n'est exécuté. Les vues perso (étape 5) réutilisent
le petit langage des alertes (§ 9.2).

---

## 5. Architecture

```
 machines (mirador-agent)             VPS OVH
 ┌──────────────────────┐   HTTPS    ┌──────────┐   ┌───────────────────────────┐
 │ collecte /proc /sys  │──(sortant)─▶  nginx   │──▶│ mirador-server            │
 │ journald, Docker     │  token     │ TLS      │   │  - écoute agents (token)  │
 │ tampon mémoire borné │            │ liste IP │   │  - écoute interface web   │
 └──────────────────────┘            │ filtre IP│   │  - SQLite / TSDB / logs   │
 collecteur LAN (Alloy → étape 6)    └──────────┘   │  - alertes, notifications │
 Proxmox / OPNsense (InfluxDB push)                 └───────────────────────────┘
 poste admin : mirador-release (signe)              daemon Olvid (127.0.0.1)
```

- **[Décidé]** Deux écoutes séparées dans `mirador-server` : agents (token) et
  interface (session + 2FA). Par défaut sur `127.0.0.1`, nginx devant.
- **[Décidé]** Fichier de démarrage limité à : adresses d'écoute, TLS, chemin de la
  base, clé de chiffrement des secrets. Tout le reste dans l'interface.
- **[Décidé]** nginx garde : TLS, liste d'IP, limites de débit, filtrage
  géographique pour l'interface. Plus de correspondance token → nom (gérée par l'enrôlement).
- **[Décidé]** mirador-server ne fait confiance à `X-Real-IP` / `X-Forwarded-For`
  que si la connexion vient de l'adresse du proxy déclarée dans le fichier de démarrage.

### 5.1 Accès réseau (d'après `supervision-push-v2.md` § 4)

| Vhost | Aujourd'hui | Avec mirador |
|---|---|---|
| ingestion | liste blanche d'IP **+** token par machine (map nginx), écriture seule | liste blanche d'IP dans nginx ; tokens vérifiés par mirador (hachés, révocables) |
| console | filtrage par adresses dans nginx, Grafana derrière | **sous-domaine dédié**, même filtrage + liste d'IP (réponse 5) ; mirador ajoute 2FA et sa propre liste d'IP (réglage interface) |
| backends | non exposés, accès par tunnel SSH | idem : seules les deux écoutes de mirador sont derrière nginx |

### 5.2 Protocole agent ↔ serveur

- **[Décidé]** HTTPS sortant uniquement, l'agent n'ouvre aucun port.
- **[Décidé]** Versionné dans l'URL : `/api/agent/v1/...`. Le serveur accepte au
  moins N et N-1 et signale les agents trop anciens.
- Points d'entrée prévus : `enroll`, `metrics`, `logs`, `config` (GET, ETag),
  `update` (GET, ETag), `heartbeat`.
- **[Décidé]** Métriques : Prometheus *remote write* (protobuf + snappy), encodeur
  écrit à la main (peu de champs) → même code pour VictoriaMetrics (étape 2) et
  mirador-server (étape 3), sans grosse dépendance.
- **[Décidé]** Logs : JSON par ligne, compressé (zstd si accepté, sinon gzip).
  À l'étape 2 : format `jsonline` de VictoriaLogs.
- **[Décidé]** Compatibilité InfluxDB v2 côté serveur (étape 6) pour Proxmox et
  Telegraf : `POST /api/v2/write` (line protocol), `Authorization: Token ...` accepté
  en plus de `Bearer`, `GET /health` qui répond au format InfluxDB. Noms produits :
  `mesure_champ` + tags en labels (comme VictoriaMetrics aujourd'hui).
- Configuration distante : JSON **strictement typé** (champs inconnus refusés,
  bornes vérifiées). Jamais de commande, de chemin d'exécutable ni de script.

### 5.3 Collecteur LAN **[Décidé, réponse du 07/11]**

- Aujourd'hui : un LXC Debian (512 Mo conseillés) avec Alloy + unpoller, qui
  **écoute** le syslog TLS sur 6514 côté LAN, interroge SNMP v3 et l'API UniFi,
  sonde des certificats, puis pousse tout en HTTPS.
- **Jusqu'à l'étape 6 : Alloy reste en place sur le collecteur**, inchangé.
  `mirador-agent` peut y être installé comme sur toute VM (métriques et journal de
  la machine), sans toucher à la partie collecteur.
- **À l'étape 6 : binaire séparé `mirador-collector`** (pas un rôle de l'agent) :
  - c'est le **seul** composant mirador qui écoute un port : syslog TLS (6514 par
    défaut), **sur l'adresse LAN uniquement**, TLS 1.2 minimum, liste d'IP émettrices
    avec leur rôle (`firewall`, `nas`), limite de débit ;
  - SNMP v3, sondes TLS et API UniFi : connexions **sortantes** depuis le collecteur ;
  - pousse vers le serveur en HTTPS avec son propre token (même protocole que l'agent) ;
  - cibles et secrets SNMP/UniFi saisis dans l'interface, transmis dans sa
    configuration typée, chiffrés ; jamais de commande ;
  - unpoller : d'abord lu en local (`127.0.0.1:9130`), client Go natif étudié à
    l'étape 6 (mêmes noms de métriques) ;
  - même durcissement que l'agent (utilisateur dédié, systemd strict, mise à jour signée).
- `mirador-agent` **n'ouvre jamais de port**, sur aucune machine.

#### 5.3.1 Gestion de `mirador-collector` depuis l'interface **[Décidé, demande du 07/11]**

Même principe que l'agent : le collecteur **interroge** le serveur (configuration
typée, ETag, toutes les 1 à 5 min + délai aléatoire). Le serveur ne lui envoie
jamais rien de lui-même, et jamais de commande.

- **Installation et enrôlement** : script d'installation avec un token d'enrôlement
  à usage unique ; le collecteur apparaît « en attente d'approbation » ; approbation,
  nom, site, token définitif (haché, révocable). Plusieurs collecteurs possibles
  (un par site).
- **Réglages dans l'interface** (valeurs typées, bornes, défaut, « rétablir »,
  audit, application à chaud) :
  - **syslog** : activé ou non, adresse d'écoute (choisie parmi les adresses LAN que
    le collecteur déclare ; jamais `0.0.0.0` ni une adresse publique), port, liste
    d'IP émettrices avec rôle (`firewall`, `nas`…), `filterlog` gardé ou jeté,
    limites de débit ;
  - **SNMP v3** : équipements (nom, adresse, préréglage — Synology, onduleur,
    switch…), utilisateur, protocoles d'authentification et de chiffrement,
    intervalle ;
  - **UniFi** : adresse du contrôleur, compte lecture seule ou clé API, vérification
    du certificat, intervalle, types d'événements gardés ;
  - **sondes de certificats** : liste `NOM = hôte:port`, intervalle ;
  - **général** : intervalles, tailles de tampon, niveau de log, mode debug temporaire.
- **Secrets** (mots de passe SNMP, compte ou clé UniFi) : saisis dans l'interface,
  chiffrés au repos sur le serveur, jamais ré-affichés ni journalisés ; transmis au
  collecteur dans sa configuration (HTTPS) ; gardés par le collecteur dans un
  fichier de cache `0600` (utilisateur dédié) pour redémarrer même si le serveur est
  injoignable — comme `/etc/alloy/snmp-auth.yml` aujourd'hui.
- **État affiché** : en ligne / muet, version, consommation (RAM, CPU), pour chaque
  source la dernière donnée reçue et les erreurs (SNMP injoignable, mot de passe
  refusé, contrôleur UniFi en échec), lignes syslog reçues et rejetées par motif
  (IP non autorisée, débit, `filterlog`), échéance de son certificat TLS.
- **Mise à jour** : même mécanisme signé que l'agent (version cible, déploiement,
  retour arrière automatique), même page « Mises à jour ».
- **Tests depuis l'interface** — **[Décidé]** : bouton « tester » sur une cible
  SNMP ou UniFi. Ce n'est pas une commande : le serveur ajoute à la configuration
  une **demande de test typée** (identifiant de la cible déjà configurée, rien
  d'autre) ; le collecteur la voit à sa prochaine interrogation (« accélérée » à
  quelques secondes tant qu'un test est en attente), lit quelques OID fixes ou fait
  une connexion de lecture, et renvoie le résultat. Aucun paramètre libre, aucune
  adresse qui ne soit pas déjà dans la configuration.
- **Certificat TLS du syslog** — **[à trancher à l'étape 6]** : aujourd'hui certbot +
  défi DNS deSEC sur le collecteur. Options :
  - (a) le collecteur obtient lui-même le certificat (ACME, défi DNS deSEC) ; jeton
    deSEC saisi dans l'interface comme secret, limité à l'enregistrement
    `_acme-challenge` du collecteur ; renouvellement automatique, échéance affichée ;
  - (b) garder certbot sur la machine, le collecteur relit le certificat quand il change.
  (a) est plus simple à gérer depuis l'interface ; (b) ajoute moins de code.
- **Ce qui reste hors interface** (posé par le script d'installation, local) :
  adresse du serveur, token, utilisateur système, unité systemd.

---

## 6. Agent (`mirador-agent`)

### 6.1 Objectifs de consommation

| Mesure | Objectif | Plafond dur |
|---|---|---|
| RAM résidente | < 30 Mio | 150 Mio (`MemoryMax`) |
| CPU moyen | < 1 % d'un cœur | — |
| Écritures disque en régime normal | ~0 (curseur journald : 1 écriture / 5 min max) | — |

Pour comparaison : Alloy consomme 80 à 150 Mo par machine aujourd'hui
(`supervision-push-v2.md` § 1).

Unité systemd : `Nice=19`, `CPUWeight=10`, `IOSchedulingClass=idle`,
`MemoryMax=150M`, `GOMEMLIMIT` ≈ 25 Mio.

### 6.2 Collecteurs et intervalles par défaut

| Collecteur | Méthode | Intervalle | Justification coût |
|---|---|---|---|
| CPU, mémoire, swap, load, PSI, vmstat, fd, uptime, time, uname, os | lecture `/proc` | 30 s | lecture directe, tampons réutilisés |
| Disques, FS, inodes | `/proc/diskstats`, `/proc/self/mountinfo`, `statfs` | 30 s | pas de `df` |
| Réseau | `/proc/net/dev`, `/proc/net/snmp`, `/proc/net/netstat` | 30 s | |
| hwmon, thermal_zone | `/sys/class/hwmon`, `/sys/class/thermal` | 60 s | change peu |
| systemd (état des unités) | D-Bus (une connexion permanente) | 60 s | pas de `systemctl` lancé à chaque cycle |
| systemd « activé mais arrêté » | D-Bus | 5 min | |
| Docker | API du **proxy de socket** en lecture seule (HTTP stdlib) | 60 s | `stats` en « one-shot » |
| Certificats locaux | lecture des fichiers + `crypto/x509` | 6 h | pas d'`openssl` |
| SMART | assistant root (§ 6.4) | 30 min | fork rare, root isolé |
| Pare-feu | assistant root (§ 6.4) | 15 min | idem |
| Mises à jour en attente | `apt-get --simulate upgrade` (chemin figé) | 6 h | fork rare accepté |
| Logs journald | un seul `journalctl --follow -o export` permanent (chemin figé) | flux | voir ci-dessous |
| Logs Docker | API du proxy (`/logs?follow`) | flux | |

- **[À mesurer]** journald : la lecture native demande `libsystemd` (cgo, pas de
  binaire statique). Proposition : **un seul** `journalctl` permanent, relancé avec
  délai croissant s'il meurt, curseur en mémoire écrit au plus toutes les 5 min.
  Sa RAM compte dans le budget. Alternative étudiée en étape 2 : lecteur Go pur.
- Masquage des données sensibles **dans l'agent avant envoi** : modèles prêts à
  l'emploi + expressions perso ; moteur `regexp` de Go (temps linéaire) + limites
  (longueur d'expression, nombre de règles, longueur de ligne).

### 6.3 Envoi et tampon

- Envoi groupé toutes les 10 s (métriques) / 5 s (logs), compressé.
- Tampon **en mémoire, borné** (ex. 8 Mio métriques, 4 Mio logs). Plein → on jette
  les plus anciens et on compte les pertes. Jamais d'écriture disque intensive.
  *(Différence avec Alloy, qui garde 4 h de WAL sur disque.)*
- Reprise avec délai croissant + aléa quand le serveur est injoignable.
- Tampons réutilisés, pas de réflexion ni de `fmt` dans les boucles de collecte.
  Objectif mesuré par benchmark : 0 allocation par point en régime établi.

### 6.4 Droits : agent sans root + assistant root **[Décidé, réponse 3]**

- `mirador-agent` tourne sous un utilisateur dédié `mirador-agent`, groupe
  `systemd-journal` (lecture du journal). **Pas** dans le groupe `docker`.
- **Assistant root** :
  - une copie du binaire, posée par le script d'installation dans un dossier
    appartenant à root (ex. `/usr/local/lib/mirador-agent/`), lancée par un
    **timer systemd** (`mirador-agent-root.timer`, toutes les 15 min) avec une
    sous-commande fixe ;
  - fait uniquement : lecture du pare-feu (`iptables -S INPUT`, `nft list ruleset`)
    et SMART (`smartctl --json`, toutes les 30 min, machines physiques), chemins et
    arguments figés dans le code ;
  - écrit un petit fichier JSON dans `/run/mirador-agent/` (mémoire, pas de disque),
    lisible par le groupe de l'agent ; l'agent le lit et l'envoie ;
  - **jamais** mis à jour par la mise à jour automatique : seulement par le script
    d'installation (lancé à la main) ;
  - aucune entrée réseau, aucune lecture de la configuration distante.
- Remplace `smartctl_exporter` (root, écoute sur 127.0.0.1:9633) et les scripts
  `obs-firewall.sh` / `obs-certs.sh` / `obs-systemd.sh`.

### 6.5 Docker par proxy de socket **[Décidé, réponse 3]**

- `tecnativa/docker-socket-proxy`, déjà utilisé sur le VPS : l'agent ne touche
  jamais `/var/run/docker.sock`.
- Autorisations minimales : `CONTAINERS=1` (liste, inspect, stats, logs),
  `POST=0` (aucune écriture). Tout le reste à 0. Image figée par version.
- Écoute du proxy : `127.0.0.1` uniquement (ou socket Unix partagé).
- Coût mesuré sur le VPS (proxy déjà en place) : **~18 Mio de RAM**, CPU
  négligeable. Sur une machine Docker, le budget total devient donc
  agent (< 30 Mio) + proxy (~18 Mio). Un proxy déjà présent
  peut être réutilisé s'il a les mêmes droits en lecture seule.
- Limite connue : `inspect` renvoie les variables d'environnement des conteneurs
  (qui peuvent contenir des secrets). L'agent ne lit pas ce champ, mais un agent
  compromis le pourrait → noté dans le modèle de menaces.

### 6.6 Flux entrants bloqués et GéoIP **[Décidé]**

- L'agent extrait des lignes noyau (déjà lues dans le journal) : règle, IP source,
  port, protocole, interface. Mêmes filtres que le dashboard (§ 4.8).
- **GéoIP côté serveur**, pas dans l'agent : la base DB-IP (~8 Mo) n'est chargée
  qu'une fois sur le VPS au lieu de chaque VPS supervisé → économise ~8 Mo de RAM
  par agent et un téléchargement mensuel par machine.
- À l'étape 2 (envoi direct vers VictoriaLogs), la carte actuelle reste alimentée
  par Alloy, qui tourne en parallèle.
- **[Décidé]** Économie d'I/O (étape 3) : en plus des lignes brutes, l'agent envoie
  des compteurs par minute (règle, pays, port, IP) ; la carte se calcule sur ces
  compteurs, et la rétention des lignes brutes peut être plus courte.
- OPNsense (plus tard, étape 6) : `filterlog` est jeté aujourd'hui (plusieurs Go/jour).
  Pour la carte, le collecteur ne garderait que les **blocages entrants sur WAN**,
  agrégés en compteurs par minute, sans stocker chaque ligne.

### 6.7 Configuration distante (prévue dès l'étape 2, désactivée)

- Récupérée toutes les 5 à 10 min (+ délai aléatoire), requête conditionnelle ETag.
- Défaut global → surcharge par groupe → surcharge par machine.
- Contenu : intervalles, collecteurs actifs, exclusions, filtres de logs, règles de
  masquage, tailles de tampon, intervalle de récupération. Que des valeurs typées.

### 6.8 Mise à jour signée (prévue dès l'étape 2, désactivée)

- Activée seulement avec `--auto-update` à l'installation.
- Version assignée plus récente → téléchargement dans un dossier temporaire →
  SHA-256 → signature Ed25519 (clé publique intégrée + 1 clé de rotation) → refus
  d'une version plus ancienne (sauf instruction signée) → `--self-test` → lien
  `current` remplacé atomiquement → redémarrage par systemd → retour arrière
  automatique si le nouvel agent ne se signale pas dans les minutes qui suivent.
- **[Décidé]** Compromis : `/var/lib/mirador-agent/bin/` appartient à l'utilisateur
  du service, lien `current` utilisé par l'unité → mise à jour **sans root**.
  Un agent compromis peut remplacer son propre binaire, mais n'obtient rien de plus
  que ses propres droits : l'assistant root (§ 6.4) est hors de ce dossier.

---

## 7. Serveur (`mirador-server`)

### 7.1 Objectifs de consommation **[Décidé, à confirmer par mesure]**

| Mesure | Objectif (50 agents × ~500 séries, 30 s) |
|---|---|
| RAM résidente (hors daemon Olvid) | < 256 Mio |
| CPU moyen | < 5 % d'un cœur |
| Écritures disque moyennes | < 1 Mo/s |

Pour comparaison, mesuré le 07/11 : stack actuelle ≈ 770 Mio (dont Grafana 291,
VictoriaMetrics 248, VictoriaLogs 187), § 2.1.

### 7.2 Stockage (tout derrière des interfaces Go `Écrire / Lire / Purger`)

- **SQLite** (`modernc.org/sqlite`, pur Go, WAL) : configuration, règles, silences,
  utilisateurs, tokens (hachés), historique d'alertes, audit, envois de notifications.
  Pas de cgo, un fichier, sauvegarde à chaud simple ; un peu plus de CPU que la
  version cgo, sans importance à ce volume. Transactions groupées, index limités.
- **Métriques — [À mesurer]** bibliothèque Prometheus TSDB en première option.
  - Pour : éprouvée, blocs de 2 h compactés, suppression **par blocs entiers**,
    rétention par durée et par taille.
  - Contre : dépendances lourdes ; pas d'agrégation native → agrégats 30 j dans une
    seconde base (pas de 5 min) alimentée par mirador.
  - À vérifier : changement de rétention **à chaud** ; sinon, mirador supprime
    lui-même les blocs trop anciens.
  - Repère actuel : VictoriaMetrics occupe 1,1 Go pour 30 j ; 94 953 séries vues
    sur la période (séries actives : à mesurer).
  - Risque : la RAM de la TSDB Prometheus croît avec le nombre de **séries actives**.
    Avec ~95 000 séries, l'objectif de 256 Mio serait très probablement dépassé.
    Parade : la collecte limitée aux métriques utiles (§ 4.1) ; la mesure de
    l'étape 3 tranchera. Si l'objectif n'est pas tenu : moteur plus simple écrit
    pour mirador (blocs par période compressés), derrière la même interface.
- **Logs — [À mesurer]** comparatif chiffré avant choix :
  (a) segments zstd par période + petit index (machine, unité, niveau, temps) ;
  (b) SQLite FTS5. Mesures : RAM, I/O, taille disque, vitesse de recherche.
  - Estimation à partir des mesures du 07/11 (262 Mo de texte par jour) :
    (a) ≈ 1 à 1,5 Go pour 30 j ; (b) SQLite ne compresse pas → ≈ 8 Go de texte
    + l'index plein texte, soit **au-delà du plafond de 8 Go**. La comparaison
    reste faite (brief), mais (a) part favori.
- Possibilité de brancher VictoriaMetrics / VictoriaLogs comme stockage.
- Rétention réglable dans l'interface. **Défauts** : brut 7 j, agrégé 30 j, logs
  30 j, plafond total **10 Gio** (réponse du 10/11) pendant la période en parallèle (22 Go libres
  mesurés, l'ancienne stack garde sa part). Ajustable dans l'interface.
- Purge par tâche de fond à faible priorité, `VACUUM` hors heures de pointe.

### 7.3 Autres règles serveur

- Cache mémoire borné, lectures paginées, jamais de chargement complet.
- Quotas par agent, limite de cardinalité, limite de débit, décalage d'horloge.
- Migrations SQLite versionnées, sauvegarde automatique avant migration.
- Sauvegarde à chaud chiffrée (local, SFTP, S3), restauration testée en CI.
- Métriques internes protégées par authentification.

---

## 8. Interface web

- **[Décidé]** Rendu côté serveur (`html/template`, échappement automatique) + un peu
  de JavaScript sans framework. Fichiers statiques embarqués (`embed`), pré-compressés.
- **[Décidé]** Graphiques : petite bibliothèque embarquée (proposition : uPlot,
  MIT, ~50 Ko). Carte du monde (flux bloqués) : fond SVG embarqué, pas de tuiles externes.
- Mise à jour par requêtes légères ; pause quand l'onglet est caché.
- Adaptée au téléphone.
- Pages de départ = les 8 tableaux du § 4.8 + vue d'ensemble, détail machine,
  alertes, logs, configuration.
- Page **« Collecteurs »** (étape 6, quand le binaire existe) : liste, état,
  réglages et secrets des sources sans agent, tests, mises à jour (§ 5.3.1).
- Chaque réglage : défaut, bornes, « rétablir », ligne d'audit, application à chaud.

---

## 9. Alertes

### 9.1 Principes

- Règles en base, rechargées sans redémarrage, import/export YAML versionné.
- Langage **restreint**, sans exécution de code.
- Dépendances, maintenance, ack, silences, inhibitions, rappels, historique.
- Mode « ombre » + comparaison avec vmalert.

### 9.2 Évaluation incrémentale **[Remplacé le 10/11 : voir § 17.1]**

> Étape 4 : le « sous-ensemble maison » ci-dessous est remplacé par le moteur
> PromQL de la bibliothèque Prometheus (déjà présente pour la TSDB), restreint
> et mesuré (§ 17.1). Le texte d'origine est gardé pour l'historique.

- Pas de moteur PromQL complet. Un **sous-ensemble** couvrant les fonctions du
  § 4.6, avec un état en mémoire par série :
  - `lag` / « source muette » → « dernière donnée reçue à », tenu à l'ingestion
    (remplace aussi la règle d'enregistrement `syslog_lignes_24h`) ;
  - `rate` / `increase` / `resets` → fenêtre glissante courte en mémoire ;
  - `avg/max_over_time` sur 7 j, `predict_linear` → calculés sur les agrégats.
- Import des 70 règles avec rapport « équivalent exact / approché / impossible ».

---

## 10. Notifications

- Canaux = modules derrière une interface Go commune.
- **[Décidé]** **Telegram principal.** **Olvid optionnel** (secours, puis principal
  si le test est concluant). ntfy et e-mail : non utilisés aujourd'hui → l'interface
  Go les prévoit, modules écrits plus tard, désactivés par défaut.
- **Sans Olvid, il n'y a qu'un canal** : la bascule n'a alors pas de secours. Le
  heartbeat HetrixTools reste le filet indépendant (il prévient si le serveur ne
  notifie plus), et la panne de Telegram est affichée dans l'interface et gardée en
  file d'attente bornée jusqu'au retour.
- Surveillance continue de chaque canal, bascule automatique avec anti-rebond,
  message « canal principal en panne » sur le secours, retour à la normale, file
  d'attente bornée sur disque si tout est en panne.
- Telegram : test de joignabilité sans message (`getMe`, toutes les 5 min).
- Heartbeat HetrixTools indépendant, **arrêté si le serveur ne peut plus évaluer
  ses règles**.
- **Olvid — référence : `prohand/gladys-olvid`** (intégration Olvid de Gladys,
  v1.4.0, Node.js, licence AGPL-3.0). **L'utilisateur en est l'auteur** : sa
  logique, ses choix et ses textes peuvent être repris librement, réécrits en Go,
  avec la provenance notée en commentaire. Ce qu'il montre en pratique :
  - **profil Olvid particulier, sans abonnement Enterprise ni Keycloak** : un profil
    normal, rencontré comme n'importe quel utilisateur (lien d'invitation + code à
    4 chiffres échangé des deux côtés). La FAQ Olvid parle d'« Olvid Enterprise »,
    mais l'usage réel de gladys-olvid montre que le profil perso fonctionne ;
  - daemon `olvid/bot-daemon:2.0.1`, API gRPC sur 50051, **aucun port publié**
    (réseau Docker privé) ; données dans `/daemon/data` (identité, contacts,
    messages) et `/daemon/backups` ;
  - le daemon est une **application Java** : gladys-olvid le lance avec
    `-Xmx512m` (tas Java limité à 512 Mo) et une limite conteneur de 2 Go, 1 CPU ;
  - clé **admin** (aléatoire, `openssl rand -hex 32`) utilisée seulement pour le
    provisionnement : créer le profil, puis une **clé client limitée à ce profil**
    que l'intégration utilise ensuite ;
  - réglages du profil par l'API : acceptation automatique des invitations
    (désactivable), **conservation des messages** (30 j par défaut, pour la place
    disque et la confidentialité) ;
  - santé : `ping` toutes les 60 s ; **chaque appel gRPC borné à 15 s** (la
    bibliothèque officielle Node ne met ni délai ni keepalive : un daemon disparu
    sans fermer la connexion bloquerait tout) ; reconnexion 5 s pendant 12 essais
    puis délai croissant jusqu'à 5 min ;
  - file d'envoi bornée (20 messages, 5 min max) quand le daemon est coupé ;
  - envoi : texte découpé en morceaux si trop long, image en pièce jointe ;
    discussions de **groupe ignorées en réception** (un message reçu agit avec les
    droits de l'utilisateur lié : logique seulement en tête-à-tête) ;
  - contacts non liés limités à 5 messages / 15 min, puis silence.

- **[Décidé]** Conception Olvid pour mirador (module écrit à l'étape 4, après le test) :
  - le daemon tourne dans **son propre `docker compose`** sur le VPS, lancé par
    l'utilisateur (mirador ne pilote pas Docker : il lui faudrait le socket Docker,
    équivalent root). Fichier compose fourni, image figée par version, gRPC sur
    `127.0.0.1:50051` ou réseau Docker interne uniquement ;
  - client gRPC **Go généré** depuis la description protobuf officielle
    (Olvid-Bot-Protobuf) ; dépendance `google.golang.org/grpc` côté **serveur
    seulement** (jamais dans l'agent) ;
  - clé admin saisie une fois dans l'interface (secret, écriture seule, chiffré),
    utilisée pour créer la clé client du profil ; ensuite seule la clé client sert ;
  - **chaque appel gRPC avec délai** (15 s) **et keepalive HTTP/2** (le client Go le
    permet) ; ping toutes les 60 s ; même schéma de reconnexion ;
  - envoi des alertes en **tête-à-tête** au(x) contact(s) choisi(s) ; groupe en option
    si le test le valide ;
  - invitations : **acceptation automatique désactivée par défaut** ; l'interface
    affiche le lien d'invitation, les invitations en cours, le code à 4 chiffres, et
    permet de valider ;
  - réception (étape 4, optionnelle) : seul un contact lié peut répondre, et
    seulement pour **acquitter une alerte** (pas d'autre action), avec limite de débit ;
  - conservation des messages dans le daemon : 30 j par défaut, réglable ;
  - sauvegarde : le volume `/daemon/data` contient l'identité → inclus dans la
    procédure de sauvegarde documentée (sinon les contacts devront réinviter).

- **Coût — [À mesurer]** : un daemon Java pèse probablement plus que mirador-server
  lui-même. Test de l'étape 4 avec `-Xmx256m` puis `-Xmx512m`, mesure de la RAM
  réelle, du CPU et du disque. Budget à confirmer : il est **compté à part** de
  l'objectif < 256 Mio du serveur. VPS : ~3,7 Gio, ~1,4 Gio utilisés aujourd'hui
  par les conteneurs → la place existe, mais le chiffre sera reporté dans `PROGRES.md`.

- **Point à vérifier à l'étape 4** : le brief demande de savoir si le bot est
  connecté aux **serveurs Olvid** (pas seulement si le daemon répond). `ping` ne
  teste que le daemon, et un envoi peut réussir localement puis attendre le réseau.
  Pistes : statut d'envoi/réception des messages dans l'API, ou message de contrôle
  périodique vers un contact technique. Si aucune piste ne marche : un envoi non
  confirmé après N minutes compte comme une panne.

- **[Décidé]** Étape 4 : **test du daemon** avant d'écrire le module : profil perso,
  envoi à un contact (et à un groupe), mesure RAM/CPU/disque, comportement quand le
  réseau Olvid est coupé. Je fournis les commandes, l'utilisateur les lance et colle
  les sorties. Si le test échoue : Olvid abandonné, autre secours proposé.

---

## 11. Sécurité

### 11.1 Exigences clés (rappel du brief)

- Agent : aucune commande reçue, **aucun port ouvert**, binaire signé uniquement.
  Seul `mirador-collector` (étape 6) écoute, sur le LAN uniquement (§ 5.3).
- Interface : Argon2id + 2FA obligatoire, pas de compte par défaut, sessions serveur
  courtes, cookies `HttpOnly`/`Secure`/`SameSite=Strict`, CSRF, CSP stricte, journal
  lisible par CrowdSec, rôles lecture / admin, ré-saisie 2FA pour les actions
  sensibles, annulation auto au bout de 2 min des réglages qui peuvent enfermer dehors.
- Secrets chiffrés au repos, jamais ré-affichés ni journalisés.
- Processus non-root, durcissement systemd.
- CI : `go vet`, `gosec`, `govulncheck`, tests de sécurité, dépendances figées, SBOM.

### 11.2 Modèle de menaces global

| Menace | Conséquence | Parade principale |
|---|---|---|
| Serveur compromis | prise de contrôle des agents | agents sans commande, binaires signés hors serveur, config typée |
| Token d'agent volé | fausses données, saturation | token par machine, haché, révocable ; quotas ; liste d'IP nginx |
| Attaque de l'interface depuis Internet | accès aux données et réglages | filtrage par adresses (nginx), 2FA, limitation des tentatives, CrowdSec, CSP, CSRF |
| Logs malveillants (contenu non fiable) | XSS | échappement systématique, CSP sans inline |
| Expression régulière abusive | CPU à 100 % | `regexp` linéaire + limites |
| Webhook vers le réseau interne | SSRF | liste de destinations, IP privées bloquées par défaut |
| Canal de notification bloqué (vécu) | alertes perdues | surveillance des canaux, bascule, heartbeat externe |
| Disque plein sur le VPS | perte de données | quotas, plafond de stockage, alerte de remplissage |
| Agent compromis | se remplace lui-même, lit les variables d'env. des conteneurs via le proxy | droits limités à l'utilisateur de l'agent ; assistant root hors de portée ; proxy en lecture seule |
| `mirador-collector` compromis (étape 6) | faux syslog, accès SNMP/UniFi | binaire séparé de l'agent, écoute LAN seule, TLS + liste d'IP, secrets limités (SNMP lecture, compte UniFi lecture seule) |
| Serveur compromis → collecteur | écoute détournée, secrets lus | adresse d'écoute limitée aux adresses LAN déclarées par le collecteur ; config typée sans commande ; tests limités aux cibles déjà configurées ; comptes SNMP/UniFi en lecture seule |
| `X-Forwarded-For` falsifié | contournement liste d'IP / limites | en-tête cru seulement depuis l'adresse du proxy |

### 11.3 Modèle de menaces de l'étape 1

- Pas de code. Risques : secret copié dans le dépôt, données privées dans `reference/`.
- Vérifié : pas de secret réel dans `reference/` (tokens d'exemple, IP de doc,
  `example.org`).
- À l'étape 2 : `.gitignore` des fichiers de clé (`*.key`, clés privées).

---

## 12. Nyx

- Source : `prohand/nyx-stack`, cloné en lecture seule (commit `5a54cdc`, v0.1.0,
  Rust, GPL-3.0). Code, docs et interface lus. **Aucun code repris** : seulement
  des idées, réécrites en Go avec ma propre conception.
- Architecture Nyx en bref : agent Rust en root, rapport JSON en POST toutes les
  15 s, ordres du serveur renvoyés dans la réponse, signature HMAC (clé partagée par
  hôte), PocketBase embarqué, interface React, notifications via Shoutrrr.

### 12.1 Ce que je retiens

| Idée Nyx | Ce que ça devient dans mirador | Pourquoi |
|---|---|---|
| Données lentes (conteneurs, SMART, mises à jour) envoyées **seulement si elles changent** + drapeau « resync » quand le serveur redémarre | même principe dans le protocole v1 | rapports minuscules : moins de CPU, réseau et écritures |
| Intervalle de rapport fixé par le serveur dans la réponse (borné) | idem, mais comme **valeur typée** de la config distante | pas de nouvelle connexion entrante, réglage depuis l'interface |
| Logs « en direct » à la demande : le serveur répond « quelqu'un regarde », l'agent envoie toutes les 2 s, s'arrête seul | idem, sans rien stocker en plus | zéro coût quand personne ne regarde |
| Alertes par **réconciliation** : ensemble « voulu » avec clés stables, on n'écrit que les différences ; grâce au redémarrage du serveur | base du moteur incrémental (§ 9.2) | pas de rescan, écritures minimales |
| Hystérésis (plusieurs points de marge avant de redescendre) | option par règle, en plus de la durée « pendant X min » | moins d'alertes qui clignotent |
| Battement de cœur du serveur écrit en base | distinguer « machine muette » et « serveur arrêté » après coupure | pas de fausse alerte au redémarrage |
| Détection séparée de l'envoi : événements → règles avec cadence (immédiat groupé, quotidien, hebdo, mensuel) | regroupement + **condensé** possible pour les `info` (ex. mises à jour DSM / UniFi) | moins de bruit sur Telegram |
| Modèles de messages avec **aperçu** et bouton « tester le canal » | idem pour Telegram et Olvid | demandé par le brief |
| Secret passé sur l'entrée standard et non en argument | règle générale pour tout appel externe éventuel | pas de fuite dans `ps` |
| SMART résumé en mots simples (vie restante, secteurs, octets écrits, n° de série) ; lecture **sans réveiller** les disques en veille (`-n standby`) | dans l'assistant root et le détail machine | moins d'usure, lecture plus claire |
| SNMP **100 % déclaratif** : une sonde = liste d'éléments (OID, agrégation, débit, échelle, seuils) ; préréglages = simples données | préréglages Synology (repris de la MIB actuelle), UPS, switch | ajout d'un équipement sans code |
| Sonde HTTPS qui lit le certificat sans le vérifier | sondes TLS du collecteur (§ 5.3) | comme aujourd'hui (blackbox) |
| Pages en **blocs réordonnables**, masquables, 1 à 4 colonnes, mise en page séparée ordinateur / téléphone, blocs sans objet masqués (ex. SMART sur une VM) | « vues personnalisables » de l'étape 5 | demandé par le brief, utile sur mobile |
| Arbre des hôtes : invités sous leur nœud Proxmox | vue Proxmox et dépendances (§ 9) | lecture naturelle, sert aussi aux dépendances |
| Logs repliés (compteur de répétitions) + « services les plus bavards » | page Logs | lisibilité, et aide à régler les filtres |
| Rafraîchissement suspendu quand l'onglet est caché ; service worker qui ne met jamais `/api` en cache | interface web | moins de requêtes, pas de données périmées |
| Type d'hôte (VM, physique, LXC, VPS, Proxmox) modifiable, recoupé avec la détection | champ machine dans l'interface | adapte les collecteurs (pas de SMART sur VPS) |
| Lots d'écriture toutes les N min + fusion horaire après 7 j, en transactions qui reprennent proprement | agrégation des métriques (§ 7.2) | I/O groupées, purge simple |
| Unité systemd en priorité basse (`Nice`, `IOSchedulingClass=idle`, `CPUWeight`) | déjà prévu, réglages plus bas encore (§ 6.1) | brief |
| Tests unitaires collés à chaque module ; commentaires « champ absent des anciens agents » | règle de code mirador | compatibilité du protocole suivie dans le code |

### 12.2 Ce que j'écarte

| Élément Nyx | Raison |
|---|---|
| Ordres envoyés à l'agent (mise à jour système `apt upgrade`, déploiement Git avec commandes install/build/start) | exécution à distance : **interdite** par le brief |
| Agent en root (et conteneur `privileged`, socket Docker monté) | mirador : agent sans root + assistant root figé + proxy de socket (§ 6.4, § 6.5) |
| Signature HMAC avec clé partagée stockée **en clair** sur le serveur ; binaire d'auto-mise à jour vérifié par cette même clé ; install initiale sans vérification | « qui contrôle le serveur contrôle les agents » (Nyx le dit lui-même) → mirador : Ed25519, clé privée hors serveur, token haché |
| Pas de nonce (rejeu possible 5 min) | mirador : TLS obligatoire + horodatage + identifiant de requête unique |
| Pas de file d'approbation des machines | mirador : enrôlement à usage unique + approbation dans l'interface |
| Premier compte créé par le premier visiteur ; tous admins ; sessions sans état non révocables ; pas de TOTP ni de HSTS | mirador : jeton console, rôles, sessions côté serveur, 2FA obligatoire, HSTS |
| CSP qui autorise `style-src 'unsafe-inline'` | mirador : CSP stricte sans inline |
| PocketBase (binaire embarqué lancé sur un port local) | SQLite pur Go dans le même processus, stockage derrière interfaces |
| Logs et métriques récentes seulement en RAM (perdus au redémarrage) | mirador stocke les logs (recherche sur 30 j) |
| Pas de tampon si le serveur est injoignable (métriques perdues) | mirador : tampon mémoire borné (§ 6.3) |
| Shoutrrr (URL de canal en clair, un processus par envoi), pas de bascule entre canaux | modules Go internes avec surveillance d'état et bascule (§ 10) |
| Interface React + Vite (build Node) | rendu serveur Go + peu de JS : rien à compiler côté front, maintenable seul |
| Graphiques en SVG fait main | gardé en tête comme alternative ; uPlot (canvas) préféré pour les longues séries (moins de nœuds DOM, plus fluide sur téléphone) |
| Partie « Manager » (IPAM, inventaire, Kanban, tâches), Nextcloud, EDF Tempo, prises Meross, lien noVNC, serveur MCP | hors périmètre supervision, surface d'attaque en plus |
| Pages de statut publiques, murs d'écran | exposition publique ; murs d'écran éventuellement plus tard (clé dans le `#` de l'URL : bonne idée à garder) |
| Suivi de tunnels VPN | pas de VPN dans cette infrastructure |
| Sondes ping / HTTP / DNS depuis le serveur | un outil externe s'en charge déjà |
| Proxmox via `pvesh` lancé par l'agent | mirador garde le metric server natif (InfluxDB) + agent sur le nœud |
| Rôle Ansible, paquets .deb/.rpm | déploiement à la main par script (brief) ; paquets éventuellement plus tard |

---

## 13. Plan et critères de fin d'étape

| Étape | Livrable | Fin d'étape quand… |
|---|---|---|
| 1 | `SPEC.md`, `CLAUDE.md`, `PROGRES.md` | SPEC validée |
| 2 | `mirador-agent` → VictoriaMetrics / VictoriaLogs, assistant root, accès Docker par proxy | métriques et logs identiques à Alloy sur 1 machine en parallèle, 31 règles de `system.yml` OK, RAM/CPU/I/O < objectifs, benchmarks + fuzzing |
| 3 | `mirador-server` + stockage + enrôlement + config + mises à jour signées + `mirador-release` | test de charge 50 agents avec rapport, choix TSDB et logs chiffrés, sauvegarde/restauration testée en CI |
| 4 | Alertes + notifications (Telegram ; Olvid après test du daemon) | 70 règles importées avec rapport + alerte OOM, mode ombre, bascule testée de bout en bout |
| 5 | Interface web | 8 tableaux + pages et réglages du brief, tests de sécurité |
| 6 | Sources sans agent + `mirador-collector` **géré depuis l'interface** | InfluxDB (Proxmox, Telegraf), SNMP v3, UniFi, syslog TLS, sondes TLS, fuzzing des formats ; enrôlement, réglages, secrets, état, tests et mises à jour du collecteur dans l'interface ; Alloy retiré du collecteur seulement après validation |
| 7 | Bascule | comparaison avec l'ancienne stack, arrêt sur validation explicite |

---

## 14. Licence **[Décidé, réponse du 07/11]**

- **GPL-3.0-or-later**, comme Nyx (« version 3 de la licence, ou toute version
  ultérieure »). Texte officiel dans `LICENSE` à la racine.
- Chaque fichier Go porte un en-tête court :
  `// SPDX-License-Identifier: GPL-3.0-or-later`.
- Dépendances : licences **compatibles GPL-3.0** uniquement (MIT, BSD, ISC,
  Apache-2.0, MPL-2.0…). Vérifiées en CI à partir du SBOM, liste dans
  `THIRD-PARTY.md` à chaque version.
- Conséquences :
  - Nyx est aussi en GPL-3.0-or-later : reprendre un passage serait possible côté
    licence, mais la règle reste **demander d'abord** (brief) et citer la source ;
  - gladys-olvid (AGPL-3.0) : l'utilisateur en est l'auteur, la reprise en Go sous
    GPL-3.0 est donc possible ;
  - la base GéoIP DB-IP (CC BY 4.0) n'est pas du code : citée dans l'interface
    (« IP Geolocation by DB-IP »), comme aujourd'hui.

---

## 15. Étape 2 — choix techniques et modèle de menaces (en cours)

### 15.1 Test en parallèle d'Alloy (réponses du 07/11)

- Machine de test : **srv-test**.
- mirador envoie sous **`instance` / `host` = `srv-test-mirador`** (mêmes `job`,
  `site`, `env`) : pas de mélange avec les séries d'Alloy, comparaison directe dans
  les dashboards.
- Sur cette machine, **Alloy envoie sous le nom `srvtest`** (`OBS_NAME`, sans
  tiret) : c'est ce nom qu'il faut comparer à `srv-test-mirador` (précision du
  08/11). Le script d'installation refuse un nom identique à celui d'Alloy
  en mode étape 2 seulement (même base) ; vers mirador-server (base séparée),
  le même nom est permis (correction du 11/11, à l'enrôlement de srv-test).
- Les alertes déclenchées pour `srv-test-mirador` **partent normalement** (utile
  pour comparer).
- Accès d'ingestion : une ligne de token en plus dans la `map` nginx actuelle
  (essai vers VictoriaMetrics seulement ; avec mirador-server, plus de `map` : § 16.5).

### 15.2 Choix (avec leur coût)

| Choix | Raison | Coût mesuré ou attendu |
|---|---|---|
| Go 1.27.4 (dernière version disponible ; 1.27.3 → 1.27.4 le 11/11 pour GO-2026-6859), fixée dans `go.mod` (`toolchain`) | versions maintenues = correctifs de sécurité ; `govulncheck` propre | aucun |
| Module Go `mirador-stack` (nom du brief) | demandé tel quel | aucun |
| Une seule dépendance pour l'instant : `klauspost/compress` (snappy, zstd) | plus rapide que la bibliothèque standard, sans cgo, licence BSD | +~0,3 Mo de binaire |
| Encodeur protobuf *remote write* écrit à la main | 4 messages simples ; évite une grosse bibliothèque et ses allocations | **0 allocation** pour 576 points, 0,17 ms |
| Lot réutilisé + étiquettes en tableau fixe + « interneur » de chaînes | pas d'allocation par point | **0 allocation** par point |
| Lecture de `/proc` dans un tampon réutilisé, analyse en octets | pas de découpage en chaînes | 43 petites allocations par cycle (ouverture des fichiers), cycle complet 0,18 à 0,22 ms |
| Chemins de fichiers calculés une fois au démarrage | évite une allocation par fichier et par cycle | — |
| `statfs` avec délai (5 s) pour NFS/CIFS/sshfs, montage bloqué sauté ensuite | un serveur NFS mort bloquait toute la collecte (problème connu de node_exporter) | une goroutine par montage réseau et par cycle |
| Liste des capteurs de température relue toutes les 10 min seulement | lire les dossiers à chaque cycle coûte des allocations | — |
| Collecte et envoi dans deux goroutines séparées | un serveur lent ne retarde pas la collecte ; à l'arrêt, 5 s pour vider la file | — |
| File d'envoi en mémoire, bornée (8 Mio par défaut), les plus anciens jetés | jamais d'écriture disque, jamais de croissance sans limite | 8 Mio ≈ 2 300 cycles (≈ 19 h à 30 s) à ~3,5 Ko par lot compressé |
| Reprise : délai doublé de 1 s à 5 min, ± 20 % d'aléa | 25 agents ne reviennent pas tous en même temps après une coupure | — |
| HTTP 400 (et 413 depuis l'étape 3) → lot abandonné ; 401/403/429/5xx → on garde et on réessaie | un lot mal formé ne bloque pas la file ; un token pas encore ajouté dans nginx ne fait rien perdre | — |
| `GOMEMLIMIT` à 25 Mio par défaut (si systemd ne le fixe pas) | le ramasse-miettes travaille plus tôt, la RAM reste basse | un peu plus de CPU de ramassage, négligeable à ce volume |
| Configuration locale au format `CLE=valeur` (comme `/etc/default`) | lisible et modifiable à la main, sans dépendance YAML ; clé inconnue = erreur | — |
| Token dans un fichier séparé, refusé s'il est lisible par tous | le fichier de configuration peut être lu, copié ou affiché sans exposer le token | — |
| Compilation `CGO_ENABLED=0`, `-trimpath`, `-buildid=` | binaire statique et **reproductible** (vérifié : mêmes empreintes après vidage du cache) | binaire 7,3 Mo (amd64), 6,7 Mo (arm64) |
| Métriques internes `mirador_agent_*` (RAM, CPU, file, envois, durée et erreurs par collecteur) | supervision de la supervision (brief) | ~30 séries par machine |

### 15.2 bis Partie 3 : systemd, mises à jour, Docker

| Choix | Raison | Coût |
|---|---|---|
| systemd par **D-Bus** (`godbus/dbus`), lecture seule, une connexion permanente | aucun `systemctl` lancé ; méthodes de lecture autorisées sans droits | +0,45 Mo de binaire ; décodage par réflexion, acceptable à ce rythme |
| État des unités : **un** appel `ListUnits` toutes les 60 s | une seule requête pour toutes les unités | négligeable |
| Redémarrages (`NRestarts`) et « activé mais arrêté » : un appel par service, **toutes les 5 min** ; dernière valeur renvoyée toutes les 60 s | les règles (1 h, 10 min) n'ont pas besoin de plus | ~2 appels D-Bus par service toutes les 5 min |
| Mêmes exclusions qu'`obs-systemd.sh` ; liste perso dans `MIRADOR_SYSTEMD_IGNORER` | évite les fausses alertes déjà réglées chez l'utilisateur | — |
| Pas d'étiquette `type` sur `node_systemd_unit_state` (contrairement à node_exporter) | elle demande un appel D-Bus par service à chaque passage ; aucune règle ni dashboard ne l'utilise | — |
| Mises à jour : `/usr/bin/apt-get --simulate --quiet dist-upgrade`, **toutes les 6 h**, en tâche de fond, sans root, environnement minimal (`LANG=C`), sortie limitée à 2 Mio, délai 2 min | la simulation lit les listes déjà téléchargées par `apt-daily` ; aucun fork répété | un processus toutes les 6 h |
| Nouvelles métriques : `apt_upgrades_pending`, `apt_upgrades_security_pending` (origine contenant `-security`), `node_reboot_required` (`/run/reboot-required`) | absentes de la stack actuelle, demandées par le brief | 3 séries |
| Docker par **proxy de socket** (`MIRADOR_DOCKER`, désactivé par défaut) : `http://127.0.0.1:PORT` ou `unix:///chemin` uniquement | jamais le socket Docker direct (réponse 3) ; jamais un proxy distant (non chiffré) | 1 + 2 × N requêtes locales toutes les 60 s (N = conteneurs) |
| Décodage JSON limité aux champs utiles, réponse bornée à 4 Mio | les variables d'environnement des conteneurs ne sont jamais lues (vérifié par un test) | allocations à chaque passage (60 s), acceptables |
| Mesures rares renvoyées à chaque passage | sinon VictoriaMetrics juge la série disparue après 5 min et les règles ne la voient plus | quelques points de plus |

**Certificats** : prévus en partie 3 au départ, déplacés en **partie 4** (assistant
root). `/etc/letsencrypt/live/` et `/etc/pve/` ne sont lisibles que par root :
l'agent sans droits ne peut pas les lire.

### 15.2 ter Partie 4 : assistant root

| Choix | Raison | Coût |
|---|---|---|
| Même binaire, option `-collecte-root`, **copie posée par le script d'installation** dans un dossier root (jamais touchée par la mise à jour automatique) | un agent compromis ne peut pas modifier ce que root exécute (réponse 3) | ~8 Mo de disque |
| Lancé par un **timer systemd toutes les 15 min** (oneshot), puis s'arrête | aucun processus root permanent | quelques ms de CPU toutes les 15 min |
| Écrit `/run/mirador-agent/root.json` (tmpfs), de façon atomique, droits `0640` | pas d'écriture disque ; l'agent ne lit jamais un fichier à moitié écrit | < 1 Ko |
| Programmes figés : `/usr/sbin/iptables -S INPUT`, `/usr/sbin/nft list ruleset`, `/usr/sbin/smartctl --scan-open --json` puis `--json --all --nocheck=standby [-d type] /dev/…` | aucune commande ni argument ne vient de la configuration ; nom de disque et type vérifiés par expression régulière | — |
| Programme refusé s'il n'appartient pas à root ou s'il est modifiable par un autre | défense en profondeur avant de lancer quoi que ce soit en root | un `stat` |
| SMART relu toutes les **30 min** (mesure précédente reprise entre-temps) ; disques en veille **non réveillés** | moins d'usure (idée de Nyx) ; les règles SMART ont des durées de 5 à 20 min | — |
| Certificats lus en Go (`crypto/x509`), motifs `MIRADOR_CERTIFICATS` (défaut : ceux d'`obs-certs.sh`), dédoublonnage par inode | `/etc/letsencrypt/live` et `/etc/pve` réservés à root ; plus d'`openssl` | — |
| Attributs SMART gardés : `Reallocated_Sector_Ct`, `Current_Pending_Sector`, `Offline_Uncorrectable`, `Reported_Uncorrect`, `UDMA_CRC_Error_Count` (brut) | ceux des règles et dashboards + signes de dégradation | quelques séries par disque |
| Activation par la configuration locale (`MIRADOR_PAREFEU`, `MIRADOR_SMART`), décidée par le script d'installation (détection, comme `obs-agent-install.sh`) | même logique qu'aujourd'hui (pare-feu : VPS ou INPUT restrictif ; SMART : machines physiques) | — |
| Côté agent : fichier relu seulement s'il a changé ; valeurs renvoyées toutes les 60 s ; fichier de plus de 2 h ignoré, son âge envoyé (`mirador_agent_root_age_seconds`) | assistant arrêté = détectable par une alerte (étape 4) au lieu de valeurs figées | — |

### 15.2 quater Partie 5a : logs du journal

| Choix | Raison | Coût mesuré ou attendu |
|---|---|---|
| **Un seul** `/usr/bin/journalctl --follow -o json` permanent, options figées, relancé avec délai croissant s'il meurt | lecture native = libsystemd (cgo) ; pas de fork répété | un processus permanent (RAM à mesurer sur srv-test) |
| `-p 0..6` : le niveau debug est jeté **par journalctl** | filtrage à la source (comme Alloy, mais plus tôt) | moins de lignes à lire |
| `--output-fields` limité à 5 champs | moins de JSON à lire | — |
| Reprise par curseur (`--after-cursor`, au plus 12 h en arrière) ; sans curseur, départ à la fin (`-n 0`) | pas de relecture complète ; pas de déluge au premier démarrage | — |
| Curseur écrit sur disque **au plus toutes les 5 min** et à l'arrêt (écriture atomique, `/var/lib/mirador-agent/journal.cursor`) | « aucune écriture disque en régime normal, ou très rare » | ~70 octets / 5 min |
| Bruit jeté (mêmes motifs qu'Alloy), limite **200 lignes/s, rafale 500** (seau à jetons), lignes jetées comptées | protège le serveur si une machine s'emballe | — |
| **Masquage à la source** : modèles `mots_de_passe`, `tokens`, `cles_api`, `autorisation`, `cartes` (clé de Luhn) actifs ; `emails` disponible ; jusqu'à 10 expressions perso (`MIRADOR_MASQUAGE_PERSO_1..10`, 256 caractères max) | brief ; moteur `regexp` linéaire ; chaque modèle n'est essayé que si un mot-indice est présent | **13 µs par ligne** (benchmark), soit ≈ 0,02 % d'un cœur à 16 lignes/s |
| Ligne coupée à 16 Kio, ligne lue limitée à 256 Kio | protection mémoire | — |
| Compteurs calculés par l'agent (§ 4.6 bis) : `mirador_logs_lines_total{level}`, `mirador_logs_dropped_total{reason}`, `mirador_logs_ssh_auth_failures_total`, `mirador_logs_kernel_disk_errors_total`, `mirador_logs_oom_kills_total{process}` (50 processus max, puis « autre ») | alertes de logs sans relire les logs côté serveur | quelques séries |
| Envoi : format `jsonline` de VictoriaLogs, JSON écrit à la main, **gzip** (niveau rapide), lot toutes les 5 s ou 512 Kio, file bornée à **4 Mio** | mêmes étiquettes qu'Alloy (`host`, `job`, `site`, `env`, `unit`, `level`, `ident`) ; `_stream_fields` ajouté automatiquement à l'URL | gzip choisi car accepté partout ; zstd possible ensuite (à vérifier sur la version de VictoriaLogs) |

### 15.2 quinquies Partie 5b : logs Docker

| Choix | Raison | Coût |
|---|---|---|
| Lecture par le **proxy de socket** (`/containers/{id}/logs?follow=1&timestamps=1`), GET seulement (vérifié par un test) | même règle que les métriques Docker (réponse 3) | une connexion HTTP locale ouverte par conteneur en marche |
| Liste des conteneurs relue toutes les 30 s ; un flux par conteneur ; tampon de 16 Kio | suit les créations et arrêts de conteneurs | ~16 Kio + une goroutine par conteneur |
| Au premier passage, départ à « maintenant » ; après une coupure, reprise à la dernière ligne reçue, **sans doublon** (testé) | pas de déluge au démarrage ; rien de perdu après un redémarrage de conteneur ou une coupure du proxy | rien sur disque (position en mémoire) |
| Formats multiplexé (sans TTY) et brut (avec TTY) gérés ; trame > 1 Mio jetée | protection mémoire | — |
| Mêmes étiquettes qu'Alloy : `job="docker"`, `container`, `stream` | dashboard Logs inchangé | — |
| Même filtrage, même **limite de débit partagée** avec le journal, même masquage | comme le `stage.limit` d'Alloy (par machine) | — |
| Compteur `mirador_logs_container_crash_lines_total{container}` (`panic:`, `FATAL`, `Segmentation fault`), 200 conteneurs max | règle ConteneurEnBoucle (§ 4.6 bis) sans relire les logs | quelques séries |

À vérifier sur srv-test : vrai `journalctl` (pas de journal ici), RAM de
journalctl, et acceptation du format par VictoriaLogs v1.52.0 (le lancement
local de VictoriaLogs a été refusé par le contrôle de sécurité de
l'environnement de développement).

### 15.2 sexies Partie 6 : prévu pour le serveur, désactivé

> **Branché à l'étape 3, partie 5** (§ 16.7), seulement quand `MIRADOR_SERVEUR`
> est renseigné ; le mode de l'étape 2 (VictoriaMetrics) reste inchangé.

Code écrit et testé, **non branché** sur la boucle de l'agent tant que
`mirador-server` n'existe pas (étape 3).

| Élément | Ce qui est prêt | Garde-fous testés |
|---|---|---|
| Protocole v1 (`internal/protocole`) | chemins `/api/agent/v1/…` (enroll, metrics, logs, config, update, binary, heartbeat), en-têtes `X-Mirador-Protocole` et `X-Mirador-Version`, messages typés | partagé par l'agent et le futur serveur |
| Configuration distante (`internal/agent/distant`) | document JSON de réglages **facultatifs** (intervalles, tampons, exclusions, collecteurs, filtres et masquage des logs, niveau de log, rythme de récupération 5 min à 1 h) ; requête conditionnelle **ETag** (304 = rien transféré) | tout champ inconnu fait refuser le document entier (commande, script, adresse du serveur, chemin de fichier : **absents du schéma**) ; mêmes bornes que le fichier local ; en cas d'erreur, la configuration actuelle est gardée ; fuzzing |
| Enrôlement | jeton à usage unique → « en attente » → nouvelle demande toutes les 30 s → token définitif écrit en `0600` (atomique) | refus explicite géré ; identifiant aléatoire par installation |
| Mise à jour signée (`internal/agent/majagent`) | vérification du manifeste **avant** téléchargement, puis du binaire ; installation dans `/var/lib/mirador-agent/bin/`, `-self-test` du nouveau binaire, lien `current` remplacé de façon atomique, marqueur d'essai, `Confirmer()`, `RetourArriere()` (option `-retour-arriere`) ; seules la version actuelle et la précédente sont gardées | signature Ed25519 sur un message qui inclut version, architecture, SHA-256, taille et autorisation de retour arrière (tout champ modifié casse la signature) ; 2 clés publiques possibles (rotation) ; **aucune clé intégrée = aucune mise à jour** ; version plus ancienne refusée sauf instruction signée ; binaire qui rate son self-test supprimé, lien inchangé |
| Clés publiques | `cles/publiques.txt`, intégrées par `make agent` (`-ldflags -X`) | vide aujourd'hui (la clé sera créée avec `mirador-release` à l'étape 3) ; jamais de clé privée dans le dépôt | **Remplacé le 11/11 : clé posée à l'installation (§ 18.25).**

Orchestration prévue (partie 7) : après la bascule, l'ancien agent s'arrête,
systemd lance le nouveau ; s'il ne démarre pas, `OnFailure=` lance l'unité
`mirador-agent-retour` (ancien binaire, `-retour-arriere`) ; s'il démarre mais
ne joint pas le serveur dans les minutes qui suivent, il se remet lui-même à la
version précédente. Le serveur voit l'échec dans le battement suivant.

### 15.2 septies Partie 7 : installation et unités systemd

- Script `deploy/agent/mirador-agent-install.sh` : une commande, rejouable,
  options `--detection`, `--afficher-config`, `--desinstaller` ; détection
  comme `obs-agent-install.sh` (Docker, INPUT restrictif, machine physique) ;
  reprend `/etc/obs-systemd-ignore` dans `MIRADOR_SYSTEMD_IGNORER`.
- Fichiers : `/etc/mirador-agent/` (root:mirador-agent, 0750 ; config et token
  0640), `/var/lib/mirador-agent/bin/` (à l'agent : mise à jour sans root),
  `/usr/local/lib/mirador-agent/mirador-agent` (copie root de l'assistant).
- `mirador-agent.service` : utilisateur `mirador-agent` + groupe
  `systemd-journal`, aucune capacité, `Nice=19`, `CPUWeight=10`,
  `IOSchedulingClass=idle`, `MemoryMax=150M`, `GOMEMLIMIT=25MiB`, système en
  lecture seule sauf `/var/lib/mirador-agent`, `/home` en lecture seule (pour
  que `statfs` voie les vrais montages), appels système en liste blanche
  (`@system-service`, refus = erreur et non arrêt brutal), familles réseau
  `AF_INET/AF_INET6/AF_UNIX`.
- `mirador-agent-root.service` + `.timer` (15 min) : root, groupe
  `mirador-agent` pour le fichier de résultat, capacités limitées
  (`CAP_NET_ADMIN`, `CAP_NET_RAW`, `CAP_SYS_RAWIO`, `CAP_SYS_ADMIN`,
  `CAP_DAC_READ_SEARCH`), **aucun trafic IP** (`IPAddressDeny=any`), système en
  lecture seule sauf `/run/mirador-agent` (et le verrou d'iptables).
- `mirador-agent-retour.service` : lancé par `OnFailure=` après 5 échecs de
  démarrage en 5 min, seulement s'il y a une mise à jour en essai.
- Proxy Docker : conteneur `mirador-docker-proxy`
  (`tecnativa/docker-socket-proxy:v0.5.0`, version figée), socket en lecture
  seule, `CONTAINERS=1`, `POST=0`, `INFO=0`, système de fichiers en lecture
  seule, 32 Mio max, écoute sur `127.0.0.1:12375` seulement. Un proxy existant
  peut être réutilisé (`--docker-proxy`).
- Binaires : dans la Release de chaque version (agent signé et son
  manifeste) ; la CI ne garde plus d'artefact depuis le 16/11 (le
  stockage Actions, 0,5 Go par mois partagé avec les paquets privés,
  était plein). L'image du serveur sur ghcr.io est publique (ne compte
  pas). Le workflow `nettoyage` (chaque lundi, à la demande) supprime tout
  artefact qui resterait, avec le jeton de l'exécution (droit « actions »
  seulement, commande `gh`, aucune action tierce).
- Guide pas à pas : `docs/installation-agent.md`. Mesures sur la machine :
  `deploy/agent/mesurer.sh` (lecture seule, compare à Alloy).

### 15.3 Différences connues avec node_exporter / Alloy

- `node_cpu_seconds_total` : pas de ligne « cpu » totale (node_exporter ne l'envoie
  pas non plus) ; temps en secondes (÷ 100), comme node_exporter.
- `node_systemd_unit_state` : **une seule série par unité** (`state="failed"`,
  0 ou 1) au lieu de deux chez Alloy (`active` + `failed`) ; seules les unités
  réellement chargées (`LoadState=loaded`, comme node_exporter). Les règles et
  dashboards n'utilisent que `state="failed"`. Mesuré le 09/11 sur srv-test :
  438 séries `unit_state` avant correction contre 382 chez Alloy ; attendu
  après : ~190. Total machine attendu ~600 séries contre 1 053 pour Alloy.
- `node_systemd_timer_last_trigger_seconds` (Alloy) : non envoyé, inutilisé.
- `node_hwmon_temp_celsius` : même règle de nommage de l'étiquette `chip`, réécrite.
  À comparer sur srv-test (peut-être sans capteur, c'est une VM).
- Seules les 42 métriques utilisées (+ ajouts § 4.1) sont envoyées : les
  dashboards communautaires comme « Node Exporter Full » seront incomplets pour
  `srv-test-mirador`. Voulu.

### 15.3 bis Règle de calcul du budget mémoire **[Décidé, mesure du 09/11]**

- Budget de l'agent (< 30 Mio) = mémoire résidente de `mirador-agent`
  **+ mémoire propre (`RssAnon`) de son `journalctl`**.
- La partie `RssFile` de `journalctl` (fichiers du journal projetés en mémoire,
  ~52 Mo sur srv-test) n'est pas comptée : c'est du cache de fichiers,
  partagé avec journald et récupérable par le noyau, pas de la mémoire prise
  par l'agent. Mesure : `RssAnon` = 292 Ko, `RssFile` = 52 Mo.
- `mesurer.sh` affiche les deux (colonne « propre moy »).

### 15.4 Modèle de menaces (parties 1 et 2)

| Menace | Parade |
|---|---|
| Fichier de configuration modifié par un tiers | lu seulement au démarrage ; clé inconnue, valeur hors bornes ou expression invalide = refus de démarrer ; aucun champ ne peut contenir de commande |
| Token volé en lisant la configuration | token dans un fichier à part, `0600`/`0640`, refusé s'il est lisible par tous |
| Interception ou faux serveur | HTTPS obligatoire (TLS 1.2 minimum, certificat vérifié) ; `http://` accepté seulement vers `127.0.0.1` |
| Identifiants dans l'URL qui fuiraient dans les journaux | refusés par la validation |
| Contenu de `/proc` inattendu (noyau différent, fichier tronqué) | analyseurs tolérants, testés par fuzzing ; fichier > 4 Mio refusé |
| Montage réseau bloqué | délai de 5 s, montage sauté ensuite |
| Serveur injoignable longtemps | file bornée en mémoire, plus anciens lots jetés et comptés |
| Agent qui consomme trop | `MemoryMax=150M` (systemd, partie 7), `GOMEMLIMIT`, priorité basse |
| Proxy Docker détourné pour écrire | l'agent n'envoie que des GET (vérifié par un test) ; le proxy doit être en `POST=0` |
| Secrets dans les variables d'environnement des conteneurs | jamais décodés ni envoyés (test dédié) ; un agent compromis pourrait les lire via `inspect` : limite connue du proxy, notée § 6.5 |
| Proxy Docker distant | refusé par la validation : 127.0.0.1 ou socket Unix seulement |
| `apt-get` remplacé par un programme piégé | chemin absolu figé `/usr/bin/apt-get` (appartient à root) ; aucun argument ne vient de la configuration |
| D-Bus | lecture seule ; aucune méthode qui démarre, arrête ou modifie une unité n'est appelée |
| Assistant root détourné par l'agent | l'agent ne peut ni modifier son binaire (dossier root), ni sa configuration (`/etc/mirador-agent`, root), ni ses arguments (fixés dans l'unité systemd) ; il ne fait que lire le résultat |
| Fichier `root.json` falsifié | écrit par root dans `/run/mirador-agent` ; côté agent : taille bornée (1 Mio), champs inconnus refusés |
| Programme externe remplacé (`iptables`, `smartctl`) | chemin absolu figé ; refusé s'il n'appartient pas à root ou s'il est modifiable par d'autres |
| Injection par un nom de disque | nom (`/dev/…`) et type (`sat`, `megaraid,0`…) vérifiés ; aucun shell : arguments passés tels quels |
| Lecture de clés privées par l'assistant | un fichier sans certificat est seulement compté comme erreur ; son contenu n'est ni gardé ni envoyé |
| Assistant qui consomme ou bloque | délai de 1 min par programme, sortie bornée à 4 Mio ; durcissement systemd en partie 7 (pas de réseau sortant, système en lecture seule sauf `/run/mirador-agent`) |
| Token visible par d'autres utilisateurs de la machine | `/etc/mirador-agent/token` en 0640 root:mirador-agent ; affiché une seule fois (ligne nginx) |
| Binaire de test falsifié entre la CI et la machine | empreinte `SHA256SUMS` à vérifier (guide § 1) ; transfert par `scp` |
| Proxy Docker exposé | écoute sur 127.0.0.1 seulement, lecture seule, écritures refusées (`POST=0`) |
| Agent qui modifie ses propres unités ou sa configuration | fichiers à root ; l'agent n'écrit que dans `/var/lib/mirador-agent` |
| Données sensibles dans les logs | masquage dans l'agent **avant** envoi ; testé (mots de passe, tokens, en-têtes, cartes) |
| Expression de masquage abusive | moteur linéaire, 10 expressions max de 256 caractères, ligne coupée à 16 Kio |
| Curseur de journal falsifié | format vérifié (`^[A-Za-z0-9=;_-]+$`) ; passé comme argument, jamais à un shell |
| Log qui casse le JSON (guillemets, octets invalides, codes couleur) | échappement maison testé par fuzzing ; octets invalides remplacés |
| journalctl bloqué | arrêt par SIGTERM, attente de 2 s maximum |

---

## 16. Étape 3 — serveur et stockage (en cours)

### 16.1 Décisions de stockage, mesurées (partie 1, 09/11)

Bancs d'essai reproductibles : module `bancs/stockage` (`go test -run TestBancTSDB`,
`go test -run TestBancLogs`). Profil simulé d'après les mesures réelles :
~600 séries par agent, logs à 60 % Docker, ~190 octets par ligne.

**Métriques : Prometheus TSDB retenue** (première option du brief).

| Mesure (50 agents, 30 000 séries, 1 000 points/s, 12 h simulées) | Résultat |
|---|---|
| CPU d'ingestion | 0,39 µs par point → **0,04 % d'un cœur** en temps réel |
| RAM avec `GOMEMLIMIT=120MiB` | 128 Mo résidents, dont **96 Mo propres** (le reste : fichiers projetés) ; tas Go ~50 Mo stable |
| RAM sans plafond | 186 Mo (Go garde la mémoire libérée) → le serveur fixera `GOMEMLIMIT` |
| Disque, blocs compactés | **2,1 octets par point** → ~176 Mo/jour pour 50 agents, ~90 Mo/jour pour le parc réel |
| WAL (journal d'écriture) | ~240 Mo gardés, ~0,5 Go écrits par jour (≈ 5,5 Ko/s, objectif < 1 Mo/s) |
| WAL compressé zstd | −30 % seulement, CPU × 5 → **non retenu** |
| Rétention changée à chaud | ✅ `ApplyConfig` : 4 blocs sur 5 supprimés **entiers** à la relecture suivante (≤ 1 min) |
| Purge par taille | ✅ option `MaxBytes` de la bibliothèque, à chaud aussi |

Réponse au brief (« mesure sa RAM avec 50 machines et dis-moi si elle respecte
mes objectifs ») : **oui**, ~100 Mo pour 30 000 séries avec un plafond Go, ce
qui laisse ~150 Mo pour le reste du serveur dans l'objectif de 256 Mio.

**Logs : moteur (a) segments zstd + index retenu**, (b) SQLite FTS5 écarté.

| Mesure (1 jour du parc réel : 1,4 M lignes, 135 Mo de texte) | (a) segments zstd | (b) SQLite FTS5 |
|---|---|---|
| CPU d'écriture | **0,72 µs/ligne** | 12,2 µs/ligne |
| Octets écrits | **34 Mo/jour** | 1 511 Mo/jour |
| Taille stockée | **34 Mo/jour** (~1 Go / 30 j) | 370 Mo/jour (~11 Go / 30 j, au-delà du plafond) |
| Erreurs d'une machine sur 1 h | 18 ms | 4 ms |
| Texte sur 24 h, toutes machines | 0,4 s | 0,08 s |
| Texte sur 24 h, une machine | 0,4 s | **23,7 s** (mauvais plan de requête) |
| Purge de 12 h | suppression de fichiers entiers | 10,7 s, 500 Mo écrits |

- Choix (a) : 17 fois moins de CPU, 44 fois moins d'écritures, 11 fois moins de
  disque, purge par fichiers entiers (exigence du brief). FTS5 n'est meilleur que
  sur certaines recherches de texte, et peut être très lent selon les filtres.
- Limite connue de (a) : une recherche de texte lit tous les blocs de la période
  (0,4 s par jour ; ~12 s sur 30 jours). Parades prévues (partie 4) : décompression
  en parallèle bornée, petit index de mots par bloc (filtre de Bloom) pour sauter
  les blocs sans le mot cherché, limites de durée et de taille des recherches
  (brief), résultats paginés.
- Stockage : un fichier par heure (et par flux si besoin), blocs de 256 Kio
  compressés, index en mémoire reconstruit au démarrage ; rétention et plafond
  de taille appliqués en supprimant des fichiers entiers.

**Configuration et données d'administration** : SQLite pur Go
(`modernc.org/sqlite`, WAL), comme prévu § 7.2.

### 16.2 Réponses du 09/11

| Question | Réponse | Conséquences |
|---|---|---|
| WAL des métriques | **gardé** | aucune perte au redémarrage brutal ; ~0,5 Go écrits/jour (≈ 5,5 Ko/s) |
| Installation du serveur | **conteneur Docker** (mise à jour plus simple) | image `FROM scratch` : binaire statique + certificats racine, utilisateur non root (uid 65532), système de fichiers en lecture seule, `cap_drop: ALL`, `no-new-privileges`, limite mémoire ; données dans un volume ; réseau Docker dédié avec IP fixe, **aucun port publié** (nginx joint le conteneur par son IP, comme la stack actuelle) ; image construite par la CI, version figée par étiquette |
| `mirador-release` | **Windows** | binaire `mirador-release.exe` (windows/amd64, statique) ; clé privée Ed25519 dans un fichier **chiffré par une phrase de passe** (Argon2id + AES-256-GCM), jamais en clair sur le disque, jamais sur le serveur ni dans le dépôt |

Remplace, pour le serveur, le durcissement systemd prévu § 11.1 par son
équivalent Docker (même intention : non-root, lecture seule, aucune capacité).

### 16.3 Partie 2 : squelette du serveur

| Choix | Raison | Coût mesuré |
|---|---|---|
| Fichier de démarrage `CLE=valeur` limité à : écoutes, TLS, dossier des données, fichier de clé, **adresse de nginx** (`MIRADOR_PROXY`) | brief ; `MIRADOR_PROXY` est la seule exception : sans elle, l'IP réelle des clients (limites, liste d'IP, journal des connexions) serait inconnue avant même que l'interface existe | — |
| Clé des secrets : 32 octets en base64 dans un fichier (secret Docker), refusée si lisible par tous | jamais dans le fichier de démarrage ni dans l'image | — |
| SQLite : WAL, clés étrangères, `busy_timeout`, une connexion, fichier `0600` | § 7.2 | base vide 48 Ko |
| Schéma initial : machines, tokens (**empreinte SHA-256 seulement**), jetons d'enrôlement (empreinte, expiration, usage unique), réglages (JSON), audit | brief | — |
| Migrations SQL numérotées, embarquées, une transaction chacune ; **copie cohérente (`VACUUM INTO`) avant toute migration** d'une base existante ; base plus récente que le binaire refusée | brief (« sauvegarde automatique avant migration, retour arrière possible ») | — |
| Deux serveurs HTTP séparés (agents : 9440, interface : 9441), délais sur toutes les phases, en-têtes ≤ 16 Kio, corps ≤ 32 Mio (agents) / 1 Mio (interface) | § 5, protection contre les clients lents | — |
| IP du client : celle de la connexion, sauf si elle vient de nginx (dernière adresse de `X-Forwarded-For`) | un client ne peut pas usurper une IP | — |
| En-têtes de sécurité de l'interface : CSP stricte sans `unsafe-inline`, `frame-ancestors 'none'`, `X-Frame-Options`, `nosniff`, `Referrer-Policy`, `Permissions-Policy`, COOP/CORP, `no-store` | § 11.1 | — |
| `/sante` sur les deux écoutes : « ok » ou « base indisponible », aucune version ni chemin | healthcheck Docker et nginx sans fuite d'information | — |
| Image `FROM scratch` + certificats racine, uid 65532, option `-sante` pour le healthcheck (pas de shell dans l'image) ; Compose : lecture seule, `cap_drop: ALL`, `no-new-privileges`, `mem_limit: 300m`, `GOMEMLIMIT=200MiB`, réseau `obs`, IP fixe 172.30.0.20, aucun port publié | § 16.2 | binaire 12 Mo |
| Journaux du serveur en JSON (slog) | lisibles par machine (CrowdSec à l'étape 5) | — |

Mesure au repos (machine de développement) : **12,7 Mo** résidents dont 2,2 Mo
propres.

Modèle de menaces (partie 2) :

| Menace | Parade |
|---|---|
| Fichier de démarrage modifié | clé inconnue = refus de démarrer ; monté en lecture seule dans le conteneur |
| Vol de la base | tokens et jetons stockés sous forme d'empreinte ; secrets chiffrés (étape 4/5) ; fichier `0600` ; copies `0600` |
| Migration ratée | transaction par migration ; copie avant migration ; refus d'une base plus récente |
| Client lent ou requête énorme | délais sur toutes les phases, tailles bornées |
| Usurpation d'IP | `X-Forwarded-For` lu seulement depuis nginx |
| Conteneur compromis | non root, aucune capacité, système en lecture seule, pas de shell, aucun port publié |
| Image modifiée | construite par la CI depuis le dépôt, version figée dans Compose |

### 16.4 Partie 3 : réception des données (ingestion)

Points d'entrée : `POST /api/agent/v1/metrics` (remote write 1.0, protobuf +
snappy) et `POST /api/agent/v1/logs` (jsonline, gzip ou non). Formats
inchangés : l'agent de l'étape 2 fonctionne sans modification de format.

| Choix | Raison | Coût mesuré |
|---|---|---|
| Token `Bearer` → **SHA-256** → recherche en base (token non révoqué, machine `approuvee`) ; cache borné (4 096), 60 s si accepté, 10 s si refusé | brief ; token aléatoire 256 bits : un hachage lent est inutile | 1 requête SQLite par minute et par machine |
| **Nom de la machine imposé par le serveur** : `instance` (métriques) et `host` (logs) remplacés par le nom lié au token ; un collecteur (étape 6) garde l'`instance` de l'équipement et reçoit `collecteur=<nom>` | un token volé ne peut pas écrire au nom d'une autre machine | — |
| Décodeur protobuf écrit à la main, borné : 4 Mio décompressés (taille lue **avant** décompression), 20 000 séries, 200 000 points, 64 étiquettes, nom ≤ 256 o, valeur ≤ 2 048 o ; noms vérifiés, `__…` réservés refusés, doublons refusés, UTF-8 vérifié ; champs inconnus sautés | même raison que l'encodeur de l'agent ; pas de grosse bibliothèque | **~1 µs par série** (lot de 800 séries : 0,76 ms) → ~0,04 % d'un cœur pour 25 agents |
| Structure invalide → 400 (lot abandonné) ; série non conforme → ignorée et comptée, le reste du lot est gardé | une étiquette fausse ne fait pas perdre tout le lot | — |
| jsonline : 4 Mio décompressés (**bombe gzip** arrêtée), 20 000 lignes, ligne ≤ 128 Kio (au-delà : sautée), message ≤ 64 Kio, 32 champs texte (objets ignorés), heure illisible → heure de réception | protection mémoire | **~4,4 µs par ligne** → ~6 s de CPU par jour pour le parc |
| 4 requêtes décodées en même temps au plus, sinon 503 | pire cas mesuré ~16 Mo par requête → **64 Mo au plus** | — |
| Débit (seaux à jetons, 10 000 clés au plus) : 50 req/s par IP (plusieurs machines derrière un NAT), 5 req/s par machine (rafale 50), **échecs d'authentification : 10 puis 1 toutes les 5 s par IP** | brief ; force brute inutile et coûteuse | — |
| Quotas de séries actives (vues depuis < 1 h) : **5 000 par machine**, **2 000 par nom de métrique** (cardinalité), **40 000 au total** ; une **nouvelle** série au-delà est refusée, les séries connues passent toujours | une étiquette qui explose ne sature ni le stockage ni les métriques normales ; 40 000 tient dans le budget mémoire (§ 16.1) | **2,7 Mo** pour 40 000 séries suivies |
| Quota de logs : 15 000 lignes/min par machine (l'agent en envoie au plus 12 000) ; au-delà **429** + `Retry-After` : l'agent garde le lot et réessaie | brief (« refusées ») ; rien n'est perdu pour un pic court ; la file de l'agent est bornée | — |
| Horloge : l'agent envoie son heure d'envoi (`X-Mirador-Heure`) ; écart ≤ 30 s ignoré, ≤ 10 min **corrigé**, au-delà **refusé (400)** ; l'écart est gardé par machine (alerte à l'étape 4) | mesure juste même pour un lot resté en file d'attente ; sans l'en-tête, seuls les points > 10 min dans le futur sont écartés | — |
| `vu_le` gardé en mémoire, écrit en base **une fois par minute** (une transaction) et à l'arrêt | pas d'écriture disque par requête | — |
| Agent : 413 traité comme 400 (lot abandonné) | un lot trop gros ne passera jamais | — |
| Partie 3 seule : données vérifiées puis **jetées** (stockage branché en partie 4) | étape par étape | — |

Mesure au repos avec l'ingestion : **11,5 Mo** résidents, dont 2,1 Mo propres.
Test de bout en bout : le code d'envoi de l'agent parle au vrai serveur
(métriques, logs, en-tête d'heure). Fuzzing des deux décodeurs dans la CI.

Modèle de menaces (partie 3) :

| Menace | Parade |
|---|---|
| Token volé sur une machine | il n'écrit qu'au nom de cette machine (nom imposé) ; inutilisable hors des adresses de la machine si la liste est remplie (§ 16.5) ; quotas ; révocation (effet ≤ 60 s, immédiat via `Oublier`) |
| Base volée | seulement des empreintes SHA-256 de tokens aléatoires |
| Force brute de tokens | 256 bits ; 10 échecs puis 1 essai / 5 s par IP ; liste d'IP de nginx |
| Machine non approuvée ou refusée | 401 tant que le statut n'est pas `approuvee` |
| Bombe snappy / gzip | taille annoncée vérifiée avant décompression ; lecture bornée |
| Protobuf ou JSON piégé | décodeurs bornés, fuzzés ; types protobuf « groupe » refusés |
| Explosion de cardinalité | quotas par machine, par nom, au total |
| Déluge de logs | quota par machine (429) ; limite de l'agent en amont |
| Horloge fausse (volontaire ou non) | corrigée jusqu'à 10 min, refusée au-delà ; écart conservé pour alerte |
| Inondation de requêtes | débit par IP et par machine ; 4 décodages simultanés ; nginx en amont (tampon des corps) |
| Avalanche d'adresses ou de faux tokens | tables bornées (seaux 10 000, cache 4 096 vidé si plein) |
| Fuite d'information | messages d'erreur courts et fixes, texte brut, `nosniff` ; jamais le token dans le journal |

### 16.5 Accès d'ingestion : nginx par IP seulement **[Décidé, réponse du 09/11]**

- nginx ne garde, pour mirador, que : TLS, **liste d'IP**, limite de débit,
  taille des corps. **Plus de `map` de tokens** dans sa configuration.
- Les tokens sont **gérés uniquement par mirador-server** : créés à
  l'enrôlement (partie 5), stockés en empreinte, révoqués ou recréés dans
  l'interface (étape 5) ; en attendant l'interface, par des commandes du
  serveur (partie 5).
- **Token lié à des adresses** (réponse « oui » du 09/11) : chaque machine a une
  liste d'adresses ou de préfixes autorisés (`ips_autorisees`, migration 0002),
  réglable dans l'interface. Vide = toute adresse admise par nginx.
  - Hors liste → **403** : l'agent garde ses lots et réessaie ; rien n'est perdu
    le temps de corriger la liste. Compté comme un échec d'authentification
    pour l'adresse, et écrit dans le journal du serveur.
  - Préfixes acceptés (ex. `2001:db8:1:2::/64` pour une IPv6 qui change, ou le
    bloc d'un fournisseur) ; IPv4 écrite en IPv6 reconnue.
  - Liste illisible → tout est refusé (jamais tout accepté) ; l'interface
    vérifiera la saisie avec la même fonction (`LireIPs`).
  - La **dernière adresse vue** (`derniere_ip`) est gardée, écrite avec `vu_le`
    une fois par minute : l'interface la proposera pour remplir ou corriger la
    liste. À l'approbation d'un enrôlement, l'adresse de la demande sera
    proposée (case cochée par défaut).
  - Coût : une comparaison par préfixe et par requête, rien en plus en base.
- Période en parallèle : la `map` actuelle reste pour VictoriaMetrics (Alloy,
  Proxmox, OPNsense) jusqu'à la bascule (étape 7) ; elle ne s'applique pas aux
  adresses de mirador.
- Étape 6 : Proxmox et OPNsense envoient « `Token xxx` » au lieu de
  « `Bearer xxx` » ; mirador acceptera les deux à ce moment-là.
- Exemple nginx (à adapter par l'utilisateur, rien n'est appliqué par Claude) :

```nginx
location /api/agent/ {
    include /etc/nginx/ip-ingestion.conf;   # allow ...; deny all;
    limit_req zone=ingestion burst=50 nodelay;
    client_max_body_size 8m;
    proxy_set_header X-Forwarded-For $remote_addr;
    proxy_pass http://172.30.0.20:9440;
}
```

  `MIRADOR_PROXY` (fichier de démarrage) doit contenir l'adresse de nginx,
  sinon toutes les requêtes sembleraient venir de nginx (limites par IP et
  liaison token ↔ IP inopérantes).

### 16.6 Partie 4 : stockage branché

**Métriques** (`internal/server/stockage/metriques`) : TSDB de Prometheus (§ 16.1).

| Choix | Raison | Coût mesuré |
|---|---|---|
| Deux bases : `brut/` (tous les points, **7 j**) et `agrege/` (**un point par 5 min, 30 j**) | brief | agrégats : ~10 fois moins de points que le brut à 30 s |
| Agrégat : **moyenne** pour une jauge, **dernière valeur** pour un compteur (`_total`, `_count`, `_sum`, `_bucket`) | `rate` / `increase` restent justes sur les agrégats | — |
| Points en retard acceptés jusqu'à **24 h** (fenêtre « hors ordre » de la TSDB ; 4 h au départ, corrigé en partie 5) | file d'attente d'un agent après une coupure du serveur (8 Mio ≈ **19 h** de lots) ; au-delà : refusés et comptés, le lot n'échoue pas | mesuré : rattraper 20 h pour 9 000 séries (21,6 M points) prend 8 s, tas stable ~25 Mo |
| Les agrégats attendent ces 24 h avant de calculer une fenêtre ; progression gardée sur disque | agrégats toujours complets, reprise sans trou ni doublon ; sans gêne : les 7 derniers jours sont lus dans le brut | — |
| Lecture : brut si la période tient dans sa rétention, agrégats sinon ; limites de séries et de points | protection mémoire | — |
| Rétention et plafond de taille **à chaud** (`ApplyConfig`, fenêtre de retard redonnée) ; suppression de **blocs entiers** | brief ; testé | — |
| Dépendances de la TSDB : 77 modules compilés (licences Apache-2.0 / MIT / BSD relues, THIRD-PARTY.md), dont les SDK des nuages tirés par la configuration de Prometheus, **jamais appelés** | seule bibliothèque mesurée qui tient le budget ; la réécrire coûterait des mois et un risque | binaire **12 → 24,6 Mo** ; RAM ~+1 Mo ; govulncheck en CI |

**Logs** (`internal/server/stockage/journaux`) : option (a) du § 16.1.

| Choix | Raison | Coût mesuré (1 journée du parc : 1,4 M lignes) |
|---|---|---|
| Un fichier par heure de réception ; blocs zstd de ~256 Kio de texte, écrits au plus tard après **30 s** | purge par fichiers entiers ; arrêt brutal : au plus 30 s de logs perdus (arrêt normal : rien) | écriture **2,3 µs/ligne** (~3 s de CPU par jour) ; disque **37 Mo/jour** (~1,1 Go / 30 j) |
| En-tête de bloc : période, valeurs de `host`, `job`, `level`, `unit`, `container`, `site`, `env` (64 max), **filtre de Bloom** des mots (8 bits/mot) ; CRC32C de l'en-tête et des données | sauter les blocs inutiles sans les décompresser ; bloc abîmé détecté et écarté, fin de fichier tronquée coupée au redémarrage | Bloom : ~10 % du disque |
| En mémoire : 24 octets par bloc + un résumé par fichier ; le reste est relu sur disque à la demande | 30 j ≈ 30 000 blocs ≈ 1 Mo | index relu en **15 ms** au démarrage (1 jour) |
| Recherche : champs exacts, **mots entiers** (Bloom), **morceau de texte** (sans Bloom) ; plus récentes d'abord, arrêt dès que la limite est atteinte ; 2 décompressions en parallèle | brief (recherche bornée) | mot rare sur 24 h : **8 ms** (979 blocs sautés sur 980) ; erreurs d'une machine sur 1 h : 21 ms ; morceau de texte sur 24 h : 0,66 s ; 500 dernières lignes : 8 ms |
| Limites : période ≤ 31 j, ≤ 10 000 lignes, ≤ 20 000 blocs lus, ≤ 10 s ; au-delà résultat partiel marqué « tronqué » | protection CPU / mémoire | morceau de texte sur 30 j ≈ 20 s → tronqué à 10 s : préférer les mots entiers |
| Rétention (30 j) et plafond de taille à chaud ; jamais le fichier en cours | brief | — |

Serveur complet (ingestion + stockage), **50 agents × 600 séries / 30 s + logs**,
3 min (machine de développement, générateur de charge dans le même processus) :
RSS stable **~119 Mo** (96 Mo propres), **0,43 %** d'un cœur. Mesure longue et
I/O par jour : partie 7.

Fuzzing : le lecteur d'en-tête de bloc a trouvé un **vrai défaut** (longueur
d'une chaîne comparée au tampon avant d'être lue → plantage sur fichier abîmé),
corrigé ; le cas est gardé comme test.

Modèle de menaces (partie 4) :

| Menace | Parade |
|---|---|
| Disque plein | rétention et plafonds de taille par base, appliqués à chaud ; suppression de fichiers entiers |
| Fichier de logs abîmé ou modifié | CRC32C ; tailles bornées avant toute allocation ; bloc écarté ; lecteur fuzzé en CI |
| Arrêt brutal | métriques : WAL (rien perdu) ; logs : ≤ 30 s ; fin tronquée coupée au redémarrage |
| Recherche coûteuse (volontaire ou non) | période, lignes, blocs et durée bornés ; 2 cœurs au plus |
| Données très anciennes ou futures | > 24 h de retard refusé ; futur : écarté à l'ingestion (§ 16.4) |
| Dépendance vulnérable | govulncheck en CI (code réellement appelé) ; mise à jour de `golang.org/x/net` faite |

### 16.7 Partie 5 : enrôlement, configuration, battements, mises à jour

Rappel : la machine **demande** toujours ; le serveur ne fait que répondre,
avec des valeurs typées. Jamais de commande, de script ni de chemin.

| Élément | Choix | Raison / coût |
|---|---|---|
| Enrôlement | jeton à usage unique (24 h par défaut, 7 j max), empreinte seule en base ; 1re demande = machine « en attente » ; seule l'installation qui a utilisé le jeton (identifiant aléatoire de 128 bits) peut suivre la demande ; nom unique ; à l'approbation, token de 256 bits (`mirador_…`) remis ; une nouvelle remise (réponse perdue) **révoque** le précédent | brief ; un jeton volé après usage ne donne rien ; 10 jetons faux puis 1 essai / 10 s par IP |
| Approbation | commande `approuver` (interface à l'étape 5) ; par défaut, l'adresse de la demande devient la **seule adresse autorisée** du token (§ 16.5) ; `-sans-ip` sinon | réponse du 09/11 |
| Configuration distante | portées `defaut` → `groupe:NOM` → `machine:NOM`, fusion réglage par réglage ; document vérifié **à l'enregistrement** avec le code de l'agent (champs connus, mêmes bornes) puis à chaque envoi ; ETag (304 : rien transféré) | § 6.7 ; un document faux ne part jamais |
| Application chez l'agent | récupération toutes les 5 min (± 20 %) ; document gardé sur disque, agent **redémarré proprement** pour l'appliquer (quelques secondes) ; au démarrage, la dernière configuration gardée est relue sans attendre le réseau | plus simple et plus sûr que reconfigurer chaque collecteur à chaud ; changements rares |
| Battement | toutes les minutes (± 20 %) : version, architecture, démarrage, essai ou échec de mise à jour ; écrit en base une fois par minute (une transaction) | « agent muet » et suivi des mises à jour |
| Publication d'une version | manifeste signé sur le PC Windows (`mirador-release`) ; le serveur **revérifie la signature** avec les mêmes clés publiques que l'agent (aucune clé privée sur le serveur) ; binaire et manifeste passés par l'entrée standard de `docker exec` | une erreur se voit tout de suite, pas sur 25 machines |
| Distribution | version assignée globalement ou par machine ; une machine ne peut télécharger **que** la version qui lui est assignée, pour son architecture | — |
| Mise à jour chez l'agent | seulement avec `MIRADOR_MAJ_AUTO=oui` dans le fichier **local** (jamais réglable à distance) ; manifeste vérifié **avant** téléchargement ; binaire vérifié ; `-self-test` ; bascule atomique ; redémarrage ; **retour arrière** si pas de battement réussi en 10 min ; une version en échec n'est **pas retentée** (signalée au serveur) | § 6.8 |
| Clés publiques | `cles/publiques.txt` intégré par `go:embed` dans l'agent **et** le serveur (même résultat avec `make` et l'image Docker) | une seule source | **Remplacé le 11/11 : clé posée à l'installation (§ 18.25).**
| `mirador-release` (Windows) | Ed25519 ; clé privée chiffrée : **Argon2id** (3 passes, 256 Mio, 4 fils) + **AES-256-GCM**, clé publique et paramètres liés au chiffré ; fichier jamais écrasé ; phrase de passe ≥ 12 caractères, toujours tapée au clavier ; refuse de signer un fichier qui n'est pas un exécutable Linux de l'architecture annoncée | réponse du 09/11 ; binaire de 3,9 Mo |
| Commandes du serveur | `machines`, `jeton`, `approuver`, `refuser`, `revoquer`, `token`, `ips`, `etiquettes`, `config`, `voir-config`, `publier`, `cible` (`docs/administration-serveur.md`) ; base ouverte directement (SQLite accepte un second processus), jamais migrée par une commande ; audit sans secret | en attendant l'interface ; une révocation prend effet en 60 s au plus |

Coût : binaire de l'agent 7,3 → 8,1 Mo (client du serveur, vérification des
mises à jour) ; quelques requêtes par minute. Mémoire de l'agent dans ce mode :
à mesurer sur srv-test quand il passera sur mirador-server (partie 7).

Défauts trouvés et corrigés pendant cette partie (par les tests de bout en bout) :

- la configuration distante gardée sur disque ne se relisait pas (durées
  réécrites en nombre) : l'agent serait reparti sur sa seule configuration
  locale après un redémarrage ;
- le serveur refusait de publier une version d'une autre architecture que la
  sienne (vérification de l'agent réutilisée telle quelle) ;
- la fenêtre de retard des métriques (4 h, partie 4) était plus courte que la
  file d'attente de l'agent (~19 h) : portée à 24 h (§ 16.6).

Modèle de menaces (partie 5) :

| Menace | Parade |
|---|---|
| Jeton d'enrôlement volé | usage unique, durée limitée ; après la 1re demande, seule l'installation d'origine peut continuer ; la machine reste « en attente » tant que l'administrateur n'a pas approuvé un nom qu'il reconnaît |
| Force brute de jetons | 256 bits ; 10 échecs puis 1 essai / 10 s par IP ; liste d'IP de nginx |
| Serveur compromis | ne peut pousser **aucune commande** ; configuration limitée aux champs typés de l'agent ; ne peut pas activer les mises à jour (réglage local) ; ne peut pas signer un binaire (clé privée sur le PC Windows seulement) ; peut seulement retenir une version ou couper l'accès |
| Binaire modifié sur le serveur ou en route | signature Ed25519 et SHA-256 vérifiés par l'agent ; version plus ancienne refusée sauf autorisation signée |
| Mauvaise version poussée | essai sur une seule machine d'abord (`cible -machine`) ; self-test ; retour arrière automatique ; version en échec non retentée |
| Vol du fichier de clé de signature | inutilisable sans la phrase de passe ; Argon2id rend chaque essai coûteux (256 Mio, ~1 s) |
| Perte de la clé de signature | copie hors du PC ; une 2e clé publique peut être ajoutée (rotation) |
| Document de configuration piégé | champs inconnus refusés (commande, script, URL, chemin absents du schéma) ; vérifié avant enregistrement, avant envoi, et chez l'agent |
| Fuite de secrets | tokens et jetons affichés une seule fois, jamais journalisés ni dans l'audit ; empreintes seulement en base ; fichiers 0600 |

### 16.8 Comparatif DuckDB (demande du 10/11) — **[Décidé : on garde la TSDB et le moteur de logs]**

Question : remplacer la TSDB de Prometheus (et éventuellement le moteur de
logs) par DuckDB, pour n'avoir que SQLite + DuckDB. Banc reproductible dans
`bancs/stockage` (`TestBancDuckMetriques`, `TestBancDuckLogs`), mêmes données
et mêmes requêtes que les bancs de la partie 1. DuckDB 2.0.0, pilote
`github.com/duckdb/duckdb-go/v2`, plafond mémoire 100 Mo (et 50 Mo), 2 fils.

**Métriques** (50 agents × 600 séries, 30 s, 12 h simulées) :

| Mesure | TSDB Prometheus | DuckDB, écriture par requête | DuckDB, écriture regroupée par cycle + partitions triées |
|---|---|---|---|
| CPU | 0,04 % d'un cœur | 0,19 % | **0,05 %** |
| RAM propre (en régime) | ~100 Mo (`GOMEMLIMIT=120MiB`) | ~120 Mo | **~55 à 75 Mo** |
| Disque | **2,1 o/point → 176 Mo/jour** | 7,1 o/point → 583 Mo/jour | 5,5 o/point → **450 Mo/jour** (×2,6) |
| Écrit sur disque | **~1,3 Go/jour** | ~2,9 Go/jour | ~3,5 à 4 Go/jour (×3) |
| Q1 : 5 séries d'une machine, 1 h | **0,06 ms** | 7,7 ms | 3,7 ms |
| Q2 : moyenne 5 min, 250 séries, 12 h | **28 ms** | 71 ms | 45 ms |
| Q3 : dernière valeur, 10 000 séries | 11 ms | 16 ms | 12 ms |
| Q4 : agrégats 5 min, toutes séries, 2 h | 850 ms (calcul en Go) | 70 ms | **41 ms** (SQL) |
| Purge | blocs entiers (à chaud) | fichier de partition entier (DELETE ne rend pas la place) | idem |

- « Écriture regroupée par cycle » : le serveur garderait les points en
  mémoire jusqu'à 30 s avant de les écrire. Un arrêt brutal perdrait ces
  secondes, sauf à ne répondre à l'agent qu'après l'écriture.
- Sur 7 jours de brut : ~1,2 Go (TSDB) contre ~3,2 Go (DuckDB), pour un
  plafond total de 8 Go.

**Logs** (1 journée du parc : 1,4 M lignes, 135 Mo de texte) :

| Mesure | Moteur actuel (segments zstd) | DuckDB | SQLite FTS5 (rappel) |
|---|---|---|---|
| CPU d'écriture | 0,6 µs/ligne (prototype) ; 2,3 µs avec index et Bloom (moteur réel, § 16.6) | 3,4 µs/ligne | 11,5 µs/ligne |
| Disque | **34–37 Mo/jour** | 58 Mo/jour | 370 Mo/jour |
| Écrit sur disque | **34–37 Mo/jour** | 274 Mo/jour | 1 511 Mo/jour |
| **RAM ajoutée** | **quelques Mo** (index de 24 o/bloc) | **+140 à 170 Mo** (même plafonné à 50 Mo) | — |
| Erreurs d'une machine, 1 h | 21 ms (moteur réel) | **3 ms** | 4 ms |
| Mot rare, 24 h | **8 ms** (Bloom) | 45–68 ms | — |
| Morceau de texte, 24 h | 660 ms (moteur réel) | **45–68 ms** | 76 ms à 24 s |
| Purge | fichiers entiers | DELETE ne rend pas la place → fichier par jour | 10 s, 500 Mo écrits |

**Coûts communs à DuckDB** :

- moteur **C++** dans le serveur : compilation cgo, binaire **71 Mo**
  (DuckDB seul, statique) au lieu de 24,6 Mo pour tout le serveur actuel ;
  avertissements glibc en statique ; image Docker plus grosse ; govulncheck
  n'analyse pas le C++ ;
- en échange, les 77 modules Go tirés par Prometheus disparaissent ;
- requêtes en **SQL** : agrégations et tableaux plus simples à écrire.

**Lecture des résultats** :

- **Logs avec DuckDB : non.** +140 à 170 Mo de RAM (le plafond n'est pas
  tenu), ajoutés aux métriques, dépassent le budget de 256 Mio. Écritures ×7
  et disque ×1,6. Seul gain : la recherche de texte libre.
- **Métriques avec DuckDB : possible, mais plus cher en disque.** Moins de RAM
  (bon point), CPU équivalent. Mais disque ×2,6 et écritures ×3, cgo, binaire
  ×3 ; et pour tenir le CPU, il faut regrouper les écritures (perte possible
  de 30 s à l'arrêt brutal, ou réponse retardée aux agents).

**Décision du 10/11** : choix 1, on garde la TSDB de Prometheus pour les
métriques et le moteur de segments zstd pour les logs.

**Encodage XOR2 de la TSDB** (mesuré le 10/11, même banc, 2 essais par
variante) : 2,09 → **1,90 octet par point** (172 → 157 Mo/jour pour 30 000
séries, **−9 %**) ; CPU (0,39 µs/point), temps des requêtes et RAM identiques
(écarts de RAM dans les deux sens d'un essai à l'autre). **Activé** dans le
serveur ; les blocs déjà écrits en XOR restent lisibles (testé).

Autres moteurs examinés (sans banc) : VictoriaMetrics plus économe en disque
mais pas prévu pour être intégré à un autre programme (en service séparé :
248 Mo de RAM mesurés sur le VPS) ; InfluxDB, TimescaleDB, QuestDB,
ClickHouse : serveurs complets, hors budget mémoire ; moteur maison : gain
faible pour un coût et un risque élevés.

### 16.9 Partie 6 : sauvegarde et restauration

| Élément | Choix | Raison / coût mesuré |
|---|---|---|
| Contenu | toujours : base SQLite (`VACUUM INTO`, cohérent à chaud), binaires de l'agent publiés, fichier de démarrage (pour mémoire) ; en option : métriques (instantané de la TSDB, tête comprise) et logs (segments jusqu'à une taille figée) | brief (« configuration toujours, métriques et logs en option ») |
| Format | tar → zstd → chiffrement ; manifeste final avec l'empreinte SHA-256 de chaque fichier | une seule passe, en flux |
| Chiffrement | AES-256-GCM par segments de 64 Kio ; clé de chaque archive dérivée (HKDF-SHA256) de la clé des secrets et d'un sel aléatoire ; nonce = numéro de segment + marque « dernier » | modification, réordonnancement, segment ôté, fin coupée : détectés (testés) ; la clé n'est jamais dans l'archive |
| Destinations | dossier local (volume, toujours) ; **SFTP** (empreinte de l'hôte obligatoire, nom provisoire puis renommage, rétention distante) ; **S3** (un envoi signé SigV4 avec la bibliothèque AWS déjà présente ; ≤ 5 Gio) | brief ; S3 sans suppression : clé en **écriture seule** + règle de cycle de vie → un serveur compromis ne peut pas détruire les sauvegardes |
| Secrets (clé SSH, secret S3) | chiffrés en base (paquet `secrets` : AES-256-GCM, clé dérivée de la clé des secrets, usage lié au chiffré) ; masqués à l'affichage et dans l'audit ; « *** » = garder | brief (secrets chiffrés) ; réutilisé pour les notifications (étape 4) |
| Planification | chaque jour à l'heure réglée (UTC), défaut 02:43, configuration seule, 14 archives ; demandes par commande (et interface, étape 5) déposées en base et faites par le serveur en marche (seul à pouvoir copier la TSDB) | aucun nouveau port ; historique en base (migration 0004) pour l'alerte « sauvegarde en échec ou trop ancienne » (étape 4, règle : 26 h) |
| Restauration | seulement dans un dossier **vide** ; tout est déchiffré et vérifié (empreintes, version de la base) avant d'être mis en place ; archive d'un serveur plus récent refusée ; `verifier-sauvegarde` contrôle sans restaurer | jamais d'écrasement de données |
| Coût | configuration seule : 5 Ko en 5 ms ; débit ~280 Mo/s en sauvegarde, ~500 Mo/s en restauration (données déjà compressées, cas le plus défavorable) → ~8 s pour un serveur plein (~2 Go) | sous charge (30 000 séries) : aucune hausse de RAM mesurée |
| Test en CI | `TestSauvegardeRestauration` : données réelles, sauvegarde à chaud par la commande, restauration sur une instance vide, vérification (token, configuration, métriques, logs) ; fuzzing du lecteur d'archive | brief |

Défaut trouvé par la mesure et corrigé : deux sauvegardes dans la même
seconde avaient le même nom (la seconde échouait) → numéro ajouté.

Modèle de menaces (partie 6) :

| Menace | Parade |
|---|---|
| Vol d'une archive | chiffrée ; inutilisable sans la clé des secrets (gardée à part) |
| Archive modifiée ou piégée | GCM par segment + empreintes ; chemins vérifiés (rien hors du dossier, seulement les dossiers attendus) ; tailles bornées ; lecteur fuzzé en CI |
| Faux serveur SFTP | empreinte de l'hôte obligatoire |
| Serveur mirador compromis qui détruit les sauvegardes | S3 : clé en écriture seule, rétention par le compartiment ; SFTP : compte dédié (le serveur SFTP peut garder ses propres instantanés) |
| Vol de la base | secrets de destination chiffrés avec la clé des secrets (fichier séparé) |
| Restauration par erreur sur des données vivantes | refusée si le dossier n'est pas vide |
| Perte de la clé des secrets | documentée : copie hors du VPS obligatoire ; sans elle, les archives sont illisibles (c'est voulu) |

### 16.10 Partie 7 : test de charge, image Docker, installation

**Test de charge** (`TestChargeServeur`, `CHARGE=1`) : vrai binaire du serveur
dans son propre processus, `GOMEMLIMIT=200MiB` comme dans le conteneur ;
**50 agents** simulés en HTTP comme de vrais agents (600 séries chacun toutes
les 30 s = 30 000 séries, ~10 lignes de logs / 30 s chacun ≈ 1,4 M lignes par
jour, battement chaque minute, configuration toutes les 5 min) ; 2 h 30 de
données passées envoyées d'abord (file d'attente vidée en respectant les
limites de débit), pour que la **première compaction de la TSDB** tombe dans
la mesure. Mesures prises dans `/proc/<pid>` du serveur seul.

Mesure : **37 min** en temps réel (le conteneur de développement a redémarré
ensuite ; la compaction, moment le plus chargé, avait eu lieu à la 26e
minute). Données brutes : `docs/mesures/charge-50-agents-2026-11-10.csv`.

| Mesure | Résultat | Objectif (§ 7.1) |
|---|---|---|
| Requêtes | 9 650, **0 erreur** | — |
| CPU moyen | **0,55 %** d'un cœur (pic 1,1 % pendant la compaction) | < 5 % |
| RAM propre | 113 à 127 Mo (moyenne 122 Mo) ; **RSS max 157 Mo** | < 256 Mio |
| Écritures | 0,7 Mo/min en régime + 41 Mo par compaction (toutes les 2 h) → **~1,6 Go/jour ≈ 19 Ko/s** | < 1 Mo/s |
| Comparaison | stack actuelle : ~770 Mio de RAM (VictoriaMetrics 248, VictoriaLogs 187, Grafana 291) | — |

À confirmer sur le VPS par la mesure de 24 h (`docs/installation-serveur.md`
§ 5), avec les vraies machines.

**Image Docker** : construite et essayée par la CI à chaque envoi (démarrage
en lecture seule, sans capacité, `no-new-privileges` ; état « healthy » ;
sauvegarde dans le conteneur) ; publiée sur `ghcr.io/prohand/mirador-server`
**seulement** pour un tag de version `v*` posé volontairement. L'image
contient la licence GPL et la liste des composants tiers (`/licences`) ; le
contexte de construction ne contient que le code nécessaire (`.dockerignore`).
SQLite garde ses tables temporaires en mémoire : l'image n'a pas de `/tmp`.

**Consultation sans interface** : `voir-metriques` et `voir-logs` interrogent
le serveur en marche par un **socket Unix** du dossier des données (0600,
aucun port réseau, lecture seule).

**Installation sur le VPS** : `docs/installation-serveur.md`, à faire par
l'utilisateur. Point relevé : le vhost d'ingestion actuel refuse **au niveau
du serveur** toute requête sans token de la `map` ; le contrôle est déplacé
dans les deux anciennes `location` (VictoriaMetrics, VictoriaLogs), et la
`location /api/agent/` de mirador n'est filtrée que par IP (§ 16.5). nginx
doit transmettre `X-Forwarded-For` (aujourd'hui seulement `X-Real-IP`).

Modèle de menaces (partie 7) :

| Menace | Parade |
|---|---|
| Image publiée par erreur ou modifiée | publication seulement sur tag de version ; construite par la CI depuis le dépôt ; version figée dans Compose |
| Contexte de construction qui embarque des fichiers privés | `.dockerignore` en liste blanche (code et licence seulement) |
| Socket d'administration détourné | fichier 0600 dans le volume, propriétaire = utilisateur du serveur ; lecture seule ; aucun port |
| Agents bloqués ou acceptés par erreur après le changement nginx | `curl` de vérification (401 attendu de mirador) dans le guide ; anciennes routes toujours protégées par la `map` |

---

## 17. Étape 4 — alertes et notifications (en cours)

Plan de l'étape (une partie = un ou plusieurs commits, mesures à chaque fois) :

| Partie | Contenu |
|---|---|
| 1 | Moteur d'évaluation, règles en base rechargées à chaud, historique, import des 70 règles + rapport, règles mirador (OOM, sauvegarde, horloge…), métriques internes, commandes, banc |
| 2 | Silences, inhibitions, dépendances entre équipements, fenêtres de maintenance, accusés de réception, regroupement et rappels, mode « ombre » |
| 3 | Notifications : interface commune, Telegram, ntfy, e-mail ; surveillance des canaux, bascule principal ↔ secours avec anti-rebond, file d'attente bornée sur disque, historique des envois, test d'envoi, heartbeat HetrixTools ; bout en bout avec panne simulée |
| 4 | Olvid : test du daemon par l'utilisateur (commandes fournies), puis module gRPC si le test est concluant |
| 5 | Comparaison avec vmalert (mode ombre), test de charge avec alertes, bilan RAM / CPU / I/O |

### 17.1 Langage et moteur : PromQL restreint **[Décidé, réponse du 10/11]**

Le § 9.2 prévoyait un sous-ensemble de PromQL écrit pour mirador. Mesure faite,
je propose d'utiliser **le moteur PromQL de la bibliothèque Prometheus**, déjà
compilé avec la TSDB :

| Critère | Sous-ensemble maison (§ 9.2) | Moteur PromQL de la bibliothèque (retenu) |
|---|---|---|
| Règles actuelles | à traduire une par une, rapport « approché » probable sur les jointures (`on(instance)`, `and`, `or`, `{__name__=~…}`) | **68 / 70 identiques** (texte inchangé), 2 sans équivalent (Watchdog, SyslogMuet : voir § 17.2) |
| Comparaison avec vmalert (mode ombre) | écarts possibles dus au langage | même langage, même résultat attendu |
| Code à maintenir | analyseur + évaluateur (plusieurs milliers de lignes) | ~250 lignes de restriction et d'adaptation ; le moteur est celui de Prometheus, éprouvé |
| Dépendances | aucune | +2 petits modules BSD-3 (`facette/natsort`, `edsrzf/mmap-go`) |
| Binaire du serveur | — | **25,8 → 27,1 Mo** (+1,3 Mo) |
| CPU (banc § 17.4) | non mesuré | **0,53 à 0,76 % d'un cœur** pour 75 règles sur 28 300 séries |

Restrictions (langage restreint du brief, « sans exécution de code ») :

- PromQL lit et calcule, il ne peut rien exécuter ; en plus, **liste blanche de
  51 fonctions** (`langage.go`) : rien d'expérimental, rien qui n'a pas été relu ;
- pas de modificateur `@`, pas de décalage négatif ; fenêtre ≤ 7 j (rétention brute) ;
- expression ≤ 4 Kio ; **1 000 000 de points** chargés au plus par évaluation
  (~16 Mo), **10 s** au plus ; une règle à la fois (pas de pic parallèle) ;
- modèles de texte (résumé, description) : `text/template` avec seulement des
  fonctions de mise en forme (`humanize`, `humanizePercentage`,
  `humanizeDuration`…), **aucune requête** depuis un modèle (Prometheus en
  permet), texte produit ≤ 2 Kio.

Ajout mirador : **`lag()`** (fonction MetricsQL de VictoriaMetrics, utilisée
par AgentMuet et les 3 « sources muettes ») : secondes depuis le dernier point
de chaque série dans la fenêtre, même sens que MetricsQL.

Réponse à « évaluation incrémentale, pas de rescan complet » du brief : chaque
évaluation relit **seulement la fenêtre de la règle** (pas l'historique), dans
la base brute ; les fenêtres de 1 à 10 min sont dans la tête de la TSDB (en
mémoire). Les fenêtres longues (7 j, 1 j, 12 h) coûtent le plus (§ 17.4) mais
restent sous 0,2 % d'un cœur au total. Si le parc grossit : règles longues
évaluées toutes les 5 min au lieu de 1 (réglage par règle, déjà possible).

### 17.2 Règles : stockage, format, import

| Élément | Choix | Raison / coût |
|---|---|---|
| Stockage | table `regles` (JSON vérifié à l'écriture **et** à la lecture), audit de chaque changement (avant / après) | brief (« règles dans la base, rechargées sans redémarrage ») |
| Rechargement | compteur `generations.regles` augmenté dans la transaction de chaque modification ; le serveur le relit toutes les 10 s (une requête d'une ligne) et ne relit la table que s'il a changé | effet ≤ 10 s, pas de relecture inutile |
| Format d'une règle | champs de vmalert en français : `nom`, `groupe`, `intervalle`, `expr`, `pour`, `garder` (= `keep_firing_for`, anti-clignotement), `gravite`, `etiquettes`, `resume`, `description`, `active`, `origine`, `note` | lisible et modifiable à la main ; même sens que vmalert |
| Export / import | YAML mirador versionné (`version: 1`), relu strictement (champ inconnu = refus) ; import vmalert reconnu tout seul ; rapport règle par règle (exact / approché / impossible) ; option `-essai` | brief (export YAML, suivi Git, rapport d'import) |
| Règles mirador | posées **une fois** (une règle supprimée ne revient pas) : `OomKiller` (activée, § 4.6 bis), `SauvegardeTropAncienne` (26 h), `SauvegardeEnEchec`, `HorlogeDecalee` (> 60 s pendant 10 min), `MiseAJourAgentEchouee`, `ServeurDisquePresquePlein` (< 10 %), `EvaluationAlertesLente` | brief (supervision de la supervision) |

Rapport d'import des fichiers actuels (`TestImportRegLesActuelles`) :

| Classement | Nombre | Règles |
|---|---|---|
| exact | 68 | toutes les autres (dont 37 règles de sources sans agent, notées « données attendues à l'étape 6 ») |
| impossible | 2 | **Watchdog** : remplacée par le heartbeat HetrixTools du serveur (partie 3) ; **SyslogMuet** : lit `syslog_lignes_24h`, calculée aujourd'hui par vmalert-logs → importée **désactivée**, remplacée à l'étape 6 par « dernière ligne reçue » tenue à l'ingestion syslog |

### 17.3 États, historique, métriques internes

- États : `en_attente` (condition vraie, durée « pour » pas atteinte) →
  `active` → `resolue`. Étiquettes de l'alerte = celles de la série sans
  `__name__` + celles de la règle + `alertname` + `severity` (comme vmalert :
  silences et routages restent compatibles).
- **Écriture en base seulement quand l'état change** (début, passage en
  alerte, fin), en une transaction par tour d'évaluation ; une alerte en
  attente qui disparaît est effacée (pas de bruit dans l'historique).
  Historique : début, passage en alerte, fin, valeur au début, valeur
  maximale, dernière valeur, résumé ; purge des alertes résolues après 180 j.
- **Redémarrage du serveur** : alertes en cours relues (pas de nouvelle
  notification, « depuis » conservé, durée « pour » continuée) ; **délai de
  3 min** après le démarrage pendant lequel rien ne passe en alerte (les agents
  vident leur file d'attente ; sinon AgentMuet partirait pour tout le parc).
- Erreur d'évaluation (règle) : état gardé tel quel, erreur affichée
  (`etat-regles`), écrite une fois dans le journal. Une panne du **stockage**
  ou un délai dépassé marque le tour « en échec » : c'est ce qui arrêtera le
  heartbeat (partie 3) ; une règle mal écrite, elle, ne le coupe pas.
- **Métriques internes** (`interne.go`), écrites chaque minute dans la base,
  `instance="mirador-server"`, `job="mirador"` : dernière sauvegarde réussie
  (à défaut : démarrage du serveur), dernière sauvegarde en échec, décalage
  d'horloge et mise à jour en échec par machine, disque libre / total, taille
  de la base et des métriques, RSS, points reçus / refusés, temps d'évaluation
  par minute, alertes actives par gravité, règles en erreur. Coût : ~50 séries.
- Commandes : `regles`, `regles-importer`, `regles-exporter`, `regle-activer`,
  `regle-desactiver`, `regle-supprimer`, `alertes`, `etat-regles`,
  `alertes-en-cours` (`docs/administration-serveur.md`).

### 17.4 Mesures de la partie 1 (machine de développement, 4 cœurs)

Banc `TestBancAlertes` (`BANC_ALERTES=1`) : 68 règles importées + 7 règles
mirador ; **50 machines × 566 séries = 28 300 séries** (profil de l'étape 2 :
200 unités systemd, 10 conteneurs, 5 systèmes de fichiers…) ; 2 h d'historique
pour tout, **7 j** pour `ssl_certs_found_total` et `node_uname_info`, **24 h**
pour les systèmes de fichiers et SMART (fenêtres longues réellement remplies) :
9,7 millions de points.

| Mesure | Résultat |
|---|---|
| Un tour complet (75 règles) | 230 à 300 ms |
| Coût en régime (chaque règle à son intervalle, 30 s ou 1 min) | **0,53 à 0,76 % d'un cœur** (objectif serveur complet : < 5 %) |
| Règles les plus chères | CollecteCertificatsMuette (7 j) 65 ms, ServiceRedemarreEnBoucle (1 h × 5 000 séries) 36 ms, DisqueSaturationPrevue (12 h) 31 ms, SecteursRealloues (24 h) 19 ms |
| Mémoire allouée par tour | 40 Mo (libérée aussitôt) ; pic de tas +74 Mo au-dessus du départ, déchets non ramassés compris (avec `GOMEMLIMIT` le ramasse-miettes passe plus tôt) |
| Écritures en base | une transaction par tour **seulement si un état change** ; rien en régime calme |
| Binaire | +1,3 Mo |

À mesurer en partie 5 : RAM du serveur complet sous charge **avec** les
alertes (test de charge de l'étape 3 + règles).

### 17.5 Modèle de menaces (partie 1)

| Menace | Parade |
|---|---|
| Règle qui épuise la mémoire ou le CPU (volontaire ou non) | 1 000 000 de points et 10 s par évaluation ; une règle à la fois ; fenêtre ≤ 7 j ; expression ≤ 4 Kio ; durée de chaque règle affichée + alerte EvaluationAlertesLente |
| Règle qui exécute du code ou lit hors des métriques | PromQL ne peut que lire la base des métriques ; liste blanche de fonctions ; modèles de texte sans requête ni accès externe |
| Fichier de règles piégé (YAML géant, alias en boucle) | 2 Mio au plus ; lecture stricte (champ inconnu refusé) ; tout est vérifié avant enregistrement, tout ou rien |
| Définition abîmée en base | relue et vérifiée au chargement ; erreur signalée, jamais ignorée en silence |
| Avalanche de fausses alertes au redémarrage | état relu depuis la base ; délai de 3 min |
| Panne du stockage pendant l'évaluation | état gardé (ni début, ni fin) ; tour marqué en échec (heartbeat coupé, partie 3) |
| Modification d'une règle sans trace | audit avant / après de chaque changement |

### 17.6 Partie 2 : sourdine, regroupement, rappels, mode ombre

Paquet `internal/server/notif`. Chemin d'une alerte : moteur (début / fin) →
**aiguillage** (groupe, sourdine, attente, rappels) → envoi (partie 3) ou
simple note en **mode ombre**.

| Élément | Choix | Raison / coût |
|---|---|---|
| Regroupement | par `alertname` + `instance` (réglable) ; une alerte est rangée **dès son arrivée** ; premier message après l'**attente** (critical 10 s, warning / info 30 s), ensuite au plus un message par **intervalle** (5 min) ; message de fin pour une alerte annoncée ; une alerte jamais annoncée (finie pendant l'attente) ne fait aucun bruit | repris d'Alertmanager (SPEC § 4.7), même comportement qu'aujourd'hui |
| Rappels | critical toutes les 2 h, warning / info toutes les 12 h, tant que l'alerte dure et n'est **pas acquittée** ; 0 = jamais | brief (« rappels tant qu'une alerte n'est pas acquittée ») |
| Accusé de réception | `acquitter N -commentaire "…"` : qui, quand, commentaire gardés avec l'alerte (historique) ; arrête les rappels ; la fin est quand même annoncée | brief ; depuis Telegram / Olvid : partie 3 / 4 |
| Silences | sélecteur d'étiquettes PromQL (`{instance="pve1"}`, `=~` permis : expressions RE2, temps linéaire), début, durée (1 min à 90 j), auteur, commentaire ; annulables ; gardés pour l'historique | brief ; même syntaxe que les règles |
| Maintenances | cible `machine:NOM`, `groupe:NOM` (groupe réglé sur la machine) ou sélecteur ; ponctuelle ou **récurrente** (jours, heure, durée ≤ 7 j, fuseau Europe/Paris, passage à l'heure d'été compris) ; reprise automatique à la fin | brief |
| Inhibitions | source, cible, étiquettes égales ; posées par défaut : `AgentMuet` tue les warning / info de la même machine ; `SynologyVolumeDegrade` tue `SynologyVolumeEnReparation` | repris d'alertmanager.yml |
| Dépendances | enfant (`machine:`, `groupe:` ou sélecteur) → parent (machine) ; parent « en panne » = une alerte active d'une règle de la liste `regles_panne` (AgentMuet, ProxmoxMuet, OpnsenseMuet, UnifiMuet, SynologyInjoignable) ; les alertes des enfants sont tues, le message du parent dit « + N alertes d'équipements dépendants en sourdine » ; posée par défaut : `{via="collecteur"}` → `miradorcollector` (3e inhibition d'Alertmanager ; nom donné le 10/11) ; un seul niveau (pas de chaîne) | brief (« regroupées ou supprimées ») |
| Après la sourdine | une alerte toujours active à la fin d'un silence ou d'une maintenance est alors annoncée | même comportement qu'Alertmanager |
| **Mode ombre** | **par défaut** : tout est calculé, et chaque message qui serait parti est noté dans l'historique des envois (`envois`, statut `ombre`, texte complet) ; `mode actif` pour envoyer | brief ; vmalert et Alertmanager notifient pendant la période en parallèle |
| Message | modèle Telegram actuel (pastille, nom en gras, résumé, description en italique, machine, « depuis ») + valeur, durée, lien direct vers l'alerte (`lien` = adresse de l'interface), mention du canal de secours ; HTML **échappé** (le résumé contient des étiquettes, donc des données venues des machines) ; message trop long coupé entre deux alertes | SPEC § 4.7, brief |
| Relecture | réglages, sourdine et acquittements relus quand leur compteur change (≤ 5 s), et chaque minute (groupes des machines) ; redémarrage : les alertes actives sont tenues pour déjà annoncées (pas de doublon, rappels repris) | à chaud, sans redémarrage |
| Historique | `envois` : quand, canal, statut, type, gravité, nombre, durée, erreur, texte (≤ 4 Kio) ; purge après 180 j | brief (« historique des envois ») |
| Coût | un tour toutes les 5 s : quelques comparaisons par alerte active ; 3 lectures d'une ligne (compteurs) ; **aucune écriture** hors envoi | négligeable (non mesurable dans le banc) |

Commandes : `silence`, `silences`, `silence-annuler`, `maintenance`,
`maintenances`, `maintenance-supprimer`, `inhibition`, `inhibitions`,
`inhibition-supprimer`, `dependance`, `dependances`, `dependance-supprimer`,
`acquitter`, `envois`, `notif`, `notif-reglages`, `mode`.

Modèle de menaces (partie 2) :

| Menace | Parade |
|---|---|
| Silence trop large qui cache tout (erreur ou malveillance) | durée ≤ 90 j ; audit (qui, quoi) ; liste des silences et raison de sourdine de chaque alerte (`alertes-en-cours`) ; **sélecteur qui viserait tout refusé** (`{}`, `{instance=~".*"}` : au moins un filtre précis, testé) |
| Expression régulière abusive dans un sélecteur | RE2 (temps linéaire), sélecteur ≤ 1 Kio |
| Contenu des alertes (étiquettes venues des machines) dans un message HTML | échappement systématique, testé avec `<script>` |
| Déluge de messages | regroupement, intervalle de 5 min par groupe, dépendances, inhibitions |
| Rappels qui ne s'arrêtent jamais | acquittement ; rappel 0 = jamais |
| Mode ombre oublié | affiché par `alertes-en-cours` et `notif` ; l'interface l'affichera en permanence |

### 17.7 Partie 3 : canaux, bascule, file d'attente, heartbeat

| Élément | Choix | Raison / coût |
|---|---|---|
| Interface commune | `Canal` : `Envoyer(message)` et `Verifier()` (sans message) ; un canal = une structure + sa configuration ; Olvid s'ajoutera de la même façon (partie 4) | brief (« on peut en ajouter d'autres sans toucher au reste ») |
| Telegram | API Bot, `sendMessage` en HTML (modèle actuel), son seulement pour critical (comme aujourd'hui) ; vérification `getMe` **toutes les 5 min** (aucun message) ; adresse de l'API figée dans le code | brief ; détecte le blocage par le pare-feu vécu avant d'en avoir besoin |
| ntfy | `POST` sur `https://serveur/sujet`, titre, priorité (urgent / default / low), jeton facultatif ; vérification `/v1/health` chaque minute | brief ; désactivé par défaut |
| E-mail | SMTP **STARTTLS ou TLS obligatoire** (jamais en clair), authentification, texte brut UTF-8 ; vérification = connexion + chiffrement + authentification, sans message | brief ; désactivé par défaut ; **pas testé contre un vrai serveur** (seulement la configuration) : à essayer chez l'utilisateur s'il l'active |
| Principal / secours | deux réglages (`principal`, `secours`), **inversables** en changeant deux valeurs (testé) ; au départ Telegram principal, pas de secours | brief, SPEC § 10 |
| Routage par gravité | liste `tous` : gravités envoyées sur **tous** les canaux actifs en même temps (ex. `["critical"]`) ; les autres sur le principal seul (secours s'il est en panne) | brief (« critique sur plusieurs canaux, info sur un seul ») |
| Anti-rebond | panne après **3** échecs de suite (vérifications **ou** envois : un envoi raté compte, le daemon peut tourner sans réseau), retour après **3** vérifications réussies de suite ; réglables 1 à 20 ; un canal en panne est revérifié chaque minute | brief |
| Messages de bascule | une seule fois par changement d'état : « canal X (principal) en panne : erreur ; les alertes partent par : Y » sur les autres canaux ; « canal X revenu à la normale » sur le canal revenu et les autres ; le secours en panne est annoncé sur le principal ; chaque alerte envoyée par le secours le dit en tête | brief |
| Tout en panne | message gardé dans `file_notifications` (SQLite, **500 au plus**, les plus anciens partent d'abord), renvoyé dans l'ordre dès qu'un canal marche, avec « En attente depuis le JJ/MM HH:MM » | brief (« file bornée, sur disque léger, heure d'origine ») ; une ligne par message, seulement pendant une panne |
| Historique | chaque essai noté (`envois`) : canal, statut (`ok`, `echec`, `en_file`, `ombre`), secours oui / non, durée, erreur, texte | brief |
| Test d'envoi | `notif-tester CANAL` : demande déposée en base, faite par le **serveur en marche** (seul à détenir les secrets déchiffrés), résultat affiché | brief (« bouton envoyer un test », interface à l'étape 5) |
| Heartbeat | URL (secret) appelée chaque minute **seulement si les règles ont été évaluées sans panne du stockage depuis moins de 3 intervalles** ; sinon suspendu (HetrixTools prévient) ; indépendant des canaux et du mode ombre ; **désactivé par défaut** : utiliser un **nouveau** moniteur HetrixTools pour mirador (celui d'Alertmanager reste pour l'ancienne stack) | brief (« si le serveur ne peut plus évaluer ses règles, le heartbeat s'arrête ») |
| Secrets | jeton Telegram, jeton ntfy, mot de passe SMTP, URL du heartbeat : **chiffrés** en base (paquet `secrets`, usage lié au champ), jamais affichés (`***`), jamais dans l'audit ni le journal ; « *** » à l'enregistrement = garder | brief |
| Réseau | délais sur toutes les phases (10 s connexion, 15 s réponse, 30 s par envoi) ; TLS 1.2 au moins ; **redirections non suivies** ; **adresses internes refusées** pour ntfy, SMTP et heartbeat (loopback, privées, lien local, CGNAT), vérifiées sur l'adresse réellement jointe après DNS ; `reseau_prive` pour un ntfy ou un SMTP du réseau local ; erreurs **sans l'URL** (elle contient le jeton Telegram) | brief (« webhooks protégés contre le SSRF ») |
| Mode actif | refusé tant que le canal principal n'est pas configuré | évite de croire que les alertes partent |
| Coût | 1 requête HTTPS toutes les 5 min (Telegram) + 1 par minute et par autre canal actif + 1 par minute (heartbeat) ; RAM : quelques clients HTTP (~1 Mo) ; disque : une ligne par envoi | négligeable devant le budget (§ 7.1) |

Accusé de réception depuis Telegram : **fait** (réponse « oui » du 10/11), § 17.10.

Commandes : `notif-reglages` (secrets en clair chiffrés à l'enregistrement),
`notif` (secrets masqués), `notif-tester`, `notif-etat`, `mode`.

Modèle de menaces (partie 3) :

| Menace | Parade |
|---|---|
| Vol de la base | secrets chiffrés (clé des secrets à part) ; usage lié au champ (un secret ne peut pas être déplacé) |
| Fuite du jeton Telegram par un message d'erreur ou le journal | erreurs réseau réécrites sans l'URL (testé) ; jamais journalisé ni dans l'audit (testé) |
| SSRF par l'adresse ntfy, SMTP ou heartbeat | https obligatoire, adresses internes refusées après résolution DNS, redirections non suivies |
| Canal bloqué sans que personne ne le voie (vécu) | vérification sans message de chaque canal ; panne annoncée sur l'autre canal ; heartbeat externe |
| Faux « retour à la normale » qui clignote | 3 vérifications réussies de suite ; un message par changement d'état seulement |
| Tous les canaux en panne longtemps | file bornée (500) : la mémoire et le disque ne grossissent pas ; heartbeat externe toujours appelé |
| Contenu piégé dans un message (étiquettes venues des machines) | HTML échappé (Telegram) ; e-mail en texte brut, en-têtes sans retour à la ligne, sujet encodé |
| SMTP en clair ou TLS rétrogradé | STARTTLS obligatoire ou TLS direct, TLS 1.2 au moins, nom du serveur vérifié |
| Serveur arrêté ou bloqué | heartbeat suspendu → HetrixTools prévient de l'extérieur |

### 17.8 Partie 4 (Olvid) et partie 5 (comparaison avec vmalert)

**Olvid — ce que dit la documentation officielle** (lue le 10/11,
<https://doc.bot.olvid.io>, version 2.0.1) :

- daemon `olvid/bot-daemon:2.0.1`, gRPC sur 50051, données dans
  `/daemon/data` ; outil en ligne de commande `olvid/bot-python-runner:2.0.1`
  (`olvid-cli`) ; clé admin `OLVID_ADMIN_CLIENT_KEY_CLI` ; `identity new`
  crée le bot et affiche **une fois** la clé client ;
- envoi : `MessageSend(discussion_id, body)` ; liste : `DiscussionList` ;
- santé : `Ping` (sans authentification) ne dit **que** « le daemon répond » ;
  **aucun appel ne donne l'état de connexion aux serveurs Olvid** ;
- en revanche, notifications **`MessageUploaded`** (message sortant arrivé sur
  le serveur Olvid), `MessageDelivered`, `MessageRead` (non garanties pour les
  messages éphémères) ;
- licence du daemon : **non indiquée** dans la documentation (à vérifier sur
  l'image ou auprès d'Olvid) ; nom exact de la métadonnée gRPC de la clé
  client : à relever dans Olvid-Bot-Protobuf au moment d'écrire le module.

**Conception proposée** (complète § 10) : un envoi Olvid ne compte comme
réussi qu'à la réception de `MessageUploaded` dans les **2 min** ; sinon c'est
un échec (bascule). Vérification sans message : `Ping` chaque minute + état du
dernier envoi. Option (désactivée par défaut) : message de contrôle quotidien
vers une discussion technique, pour détecter une coupure même sans alerte.

**Test à faire par l'utilisateur** : `docs/test-olvid.md` (installation sans
port publié, identité, contact, groupe, mesure RAM/CPU/disque avec
`-Xmx256m` puis `-Xmx128m`, coupure réseau simulée par `iptables` dans
`DOCKER-USER`). Le module n'est écrit qu'après ce test (§ 10, décidé).

**Comparaison avec vmalert** (partie 5, mode ombre) :

| Élément | Choix | Raison / coût |
|---|---|---|
| Source | `GET /api/v1/alerts` de vmalert (lecture seule), réseau Docker interne ; 10 s et 4 Mio au plus ; redirections non suivies ; désactivé par défaut (`vmalert-reglages`) | brief (« un écran compare ses alertes à celles de vmalert ») |
| Comparaison | par **règle et machine**, alertes `firing` seulement ; seulement les machines **approuvées dans mirador** (vmalert surveille tout le parc, mirador une partie pendant la période en parallèle) ; nom ramené à celui de vmalert (suffixe `-mirador` retiré, ou table de correspondances) ; Watchdog ignorée | comparaison juste dès la première machine |
| Résultat | `commune`, `manquante` (vmalert seul), `en_trop` (mirador seul) | brief (« présentes des deux côtés, manquantes, en trop ») |
| Historique | toutes les 5 min, chaque écart ouvre un **épisode** (début, fin) dans `ecarts_vmalert` ; un écart qui dure est une seule ligne | rapport sur la durée de la période en parallèle |
| Rapport | `comparer-vmalert` : maintenant + 7 derniers jours ; `-csv` : 30 jours pour un tableur ; l'écran viendra à l'étape 5 | brief (« rapport exportable ») |
| Coût | une requête HTTP toutes les 5 min, une transaction seulement quand un écart change | négligeable |

Modèle de menaces (parties 4 et 5) :

| Menace | Parade |
|---|---|
| Daemon Olvid exposé | aucun port publié ; réseau Docker interne ; clé admin dans un fichier `.env` en 0600, jamais dans le compose ni le dépôt |
| Vol de l'identité du bot | volume `data/` sur le VPS seulement, à sauvegarder chiffré comme le reste |
| Daemon qui tourne sans réseau Olvid (alertes perdues sans le savoir) | envoi réussi seulement avec `MessageUploaded` ; test de coupure fait avant d'écrire le module |
| Réponse de vmalert énorme ou piégée | 4 Mio, 10 s, JSON décodé dans des champs fixes ; seulement des noms et des étiquettes comparés, jamais exécutés |

### 17.9 Mesure sous charge avec les alertes (10/11)

Test de charge de l'étape 3 (`TestChargeServeur`, 50 agents × 600 séries,
logs, battements, 2 h 30 de rattrapage puis 12 min de mesure), **avec les 68
règles importées + 7 règles mirador**. Données : `docs/mesures/charge-50-agents-alertes-*.csv`.

| Mesure | Étape 3 (sans alertes) | Étape 4 (avec alertes) | Avec `GOGC=50` | Objectif |
|---|---|---|---|---|
| Requêtes / erreurs | 9 650 / 0 | 3 150 / 0 | 3 150 / 0 | — |
| CPU moyen | 0,55 % | **1,00 %** | 1,00 % | < 5 % ✅ |
| RAM propre (moyenne) | 122 Mo | **185 Mo** | 175 Mo | < 256 Mio ✅ (marge ~70 Mo) |
| RSS max | 157 Mo | **218 Mo** | 205 Mo | < 256 Mio ✅ (marge ~38 Mo) |
| Écritures | ~19 Ko/s | ~23 Ko/s | ~18 Ko/s | < 1 Mo/s ✅ |

Lecture : objectifs tenus, mais le moteur d'alertes ajoute **~60 Mo** de RAM
(10 seulement dus au ramasse-miettes, d'après l'essai `GOGC=50`). Le parc
réel (25 machines) est deux fois plus petit que le test. Pistes, par ordre de
coût : (1) `GOGC=50` dans le conteneur (−10 Mo, CPU inchangé) ; (2) profil
mémoire (`pprof`) pour trouver ce qui est gardé entre deux évaluations ;
(3) règles à fenêtre longue évaluées toutes les 5 min ; (4) limite de points
par évaluation abaissée.

**Profil mémoire (10/11, `TestProfilAlertes`)** : même charge dans un seul
processus, profil du tas après 5 min **avec** les 75 règles, puis 5 min
**sans** aucune règle.

| | Avec les règles | Sans règle |
|---|---|---|
| Tas utilisé | 180,1 Mo | 179,6 Mo |
| RAM propre (générateur compris) | 161,8 Mo | 162,4 Mo |
| Différence des profils | — | **−3,5 Mo** au total |

- **Le moteur d'alertes ne garde presque rien** entre deux évaluations.
- Premier poste du tas : **42,6 Mo dans les morceaux « hors ordre » de la
  TSDB** (`OOOChunk.Insert`, 55 Mo avec ce qui l'entoure). Cause très
  probable : le serveur écrit maintenant ses métriques internes **dès le
  démarrage**, à l'heure courante ; les 2 h 30 de rattrapage des agents
  arrivent ensuite « dans le passé » et passent par la voie hors ordre de la
  TSDB (jusqu'à 32 points non compressés par série en mémoire, × 30 000
  séries). À l'étape 3, le rattrapage commençait sur une base vide, donc en
  ordre.
- En production, ce cas arrive vraiment : après une coupure, la file
  d'attente des agents arrive alors que d'autres données récentes sont déjà
  écrites. La mémoire est rendue quand la TSDB compacte (toutes les 1 à 2 h).
- **Essai `OutOfOrderCapMax` 32 → 8** (mesuré le 10/11, même charge, deux
  passages) : tas 180 → 172 Mo (−8 Mo), **RAM propre inchangée** (162 Mo),
  écritures disque **+29 %** (181 → 233 Mo sur l'essai). Les points ne sont
  plus gardés en mémoire, mais la TSDB garde une référence par morceau écrit
  sur disque (30 Mo) : le gain disparaît. **[Décidé : on garde 32]** (valeur
  de la bibliothèque) ; le réglage `metriques.HorsOrdreMax` reste dans le code
  pour d'autres essais.

### 17.10 Réponses du 10/11 et suites

| Question | Réponse | Conséquence |
|---|---|---|
| PromQL à la place du langage maison | **oui** | § 17.1 décidé |
| Accusé de réception depuis Telegram | **oui** | fait (ci-dessous) |
| Nom du collecteur LAN | **`miradorcollector`** | dépendance par défaut `{via="collecteur"}` → `miradorcollector` |
| `GOGC=50` | **oui** | ajouté au `docker-compose.yml` du serveur |
| Olvid | test fait en partie (ci-dessous) | coupure à refaire (`docs/test-olvid.md` § 5) |
| **Demande** : gérer l'invitation Olvid dans l'interface web | **[Décidé]** | étape 5 : page Olvid (lien d'invitation, QR code, invitations en attente, saisie des codes SAS, contacts, choix de la discussion ou du groupe) ; le module Olvid exposera ces actions (appels `Invitation*` et `Contact*` de l'API, clé client seulement) |

**Accusé de réception depuis Telegram**

| Élément | Choix | Raison / coût |
|---|---|---|
| Bouton | `"acquitter": true` : un bouton « ✔ Acquitter » par alerte active sous le message (8 au plus, un par ligne) ; pas de bouton sur un message de fin | réponse du 10/11 |
| Lecture | `getUpdates` toutes les **15 s** (sans attente), en sortie, **seulement les appuis sur boutons** (`allowed_updates: callback_query`) : le bot ne lit jamais les messages du groupe | aucun port ouvert ; 4 petites requêtes par minute |
| Contrôles | appui venu d'un autre groupe : ignoré ; liste `acquitteurs` (identifiants Telegram) : si remplie, les autres sont refusés et journalisés ; 20 acquittements par minute au plus ; identifiant numérique du groupe obligatoire | un bouton transféré ou un inconnu ne peut rien faire |
| Effet | même acquittement que la commande : arrête les rappels, gardé dans l'historique (`telegram:ID (@nom)`, « depuis Telegram ») et l'audit ; réponse affichée à la personne, bouton retiré | brief (« depuis la notification si le canal le permet ») |
| Seule action possible | acquitter une alerte en cours, par son numéro ; rien d'autre | aucun ordre ne passe par Telegram |

Point à savoir : `getUpdates` ne marche pas si un *webhook* est posé sur le
bot, ou si un autre programme lit les mises à jour du même bot. Alertmanager
ne fait qu'envoyer : pas de conflit.

**Olvid — résultats du 10/11** (VPS, `olvid/bot-daemon:2.0.1`)

| Mesure | `-Xmx256m` | `-Xmx128m` |
|---|---|---|
| RAM du conteneur (15 min au repos) | **~210 Mio** | **~170 Mio** |
| CPU | 0,11 à 0,26 % (pics à 1,6 %) | 0,10 à 0,34 % (pic 1,2 %) |

- Daemon : démarre en ~2 s, base interne SQLite, graine de sauvegarde créée
  dans `backups/` (à garder, comme `data/`).
- Identité, contact, envoi en tête-à-tête : **ok**. Groupes : création ok,
  ajout d'un contact en « membre en attente » ok ; envoi au groupe : à
  vérifier après acceptation.
- Coupure : le test n'a pas eu lieu (mauvaise identité dans le CLI). Mais
  les logs montrent que **le daemon voit lui-même la perte du serveur
  Olvid** en ~20 s (`Websocket … sent ping but didn't receive pong within
  20000ms`) et réessaie avec une attente croissante. Cet état n'est pas
  dans l'API documentée : mirador gardera la règle « envoi confirmé par
  `MessageUploaded` dans les 2 min, sinon échec ».
- Coût : ~170 Mio avec `-Xmx128m`, **compté à part** du budget du serveur
  (§ 10). VPS : 3,7 Gio, stack actuelle ~770 Mio + mirador ~200 Mio +
  Olvid ~170 Mio : la place existe. Choix proposé : `-Xmx128m` si le message
  de test est bien passé.

Modèle de menaces (accusé de réception Telegram) :

| Menace | Parade |
|---|---|
| Quelqu'un acquitte à ma place (membre du groupe, message transféré) | chat vérifié ; liste `acquitteurs` ; qui est noté dans l'historique et l'audit |
| Avalanche d'appuis | 20 par minute ; une seule action possible (acquitter) |
| Le bot lit des messages privés du groupe | seuls les appuis sur boutons sont demandés à Telegram |
| Faux appui avec un numéro d'alerte au hasard | l'alerte doit être en cours ; refus sinon (« déjà finie ou inconnue ») ; ne fait qu'arrêter des rappels |

**Olvid — second essai du 10/11** (installation neuve, une seule identité,
`-Xmx128m`)

| Contrôle | Résultat |
|---|---|
| Envoi en tête-à-tête avec `-Xmx128m` | ✅ reçu |
| Envoi dans un groupe (contact ajouté, invitation acceptée sur le téléphone) | ✅ reçu |
| Disque | `data/` 5,3 Mo, `backups/` 8 Ko |
| `message send` pendant la coupure | **répond « envoyé » tout de suite** (id O3) : le daemon accepte le message, il ne dit pas qu'il est parti |
| Détection de la coupure par le daemon | 33 s (websocket sans réponse), puis réessais avec attente croissante (0,9 s → 56 s) ; message gardé et réessayé |
| Retour du réseau | websocket reconnecté **1 s** après, « retrying all scheduled network jobs » |
| Message envoyé pendant la coupure | **reçu à 11:46**, juste après le retour : rien n'est perdu |

Conséquences pour le module (à écrire) :

- **Olvid retenu** comme canal (test concluant), `-Xmx128m` (~170 Mio,
  compté à part du serveur).
- Un `MessageSend` réussi **ne prouve rien** : l'envoi ne compte comme réussi
  qu'avec la notification `MessageUploaded` ; sans elle au bout de **2 min**,
  c'est un échec et la bascule s'applique (§ 17.8, confirmé par l'essai).
- Le message gardé par le daemon arrivera **en retard** au retour du réseau
  (doublon possible avec le secours) : question posée à l'utilisateur.
- **Licence** : la description protobuf officielle (`olvid-io/Olvid-Bot-Protobuf`)
  est sous **AGPL-3.0**. Le client Go généré en dériverait. L'AGPL-3.0 est
  compatible avec la GPL-3.0 (article 13 de la GPL-3.0), mais la partie
  Olvid du serveur serait alors sous AGPL : obligation de fournir le code
  source à qui utilise le serveur par le réseau. Question posée à
  l'utilisateur avant d'écrire le module (`CLAUDE.md` : dépendances
  compatibles GPL-3.0 seulement).

### 17.11 Partie 4 : module Olvid (réponses du 10/11)

Réponses : licence AGPL du protobuf **acceptée** ; si l'alerte est partie
par le secours, le message Olvid doit être **annulé** (pas de doublon en
retard).

| Élément | Choix | Raison / coût |
|---|---|---|
| Client | généré depuis `olvid-io/Olvid-Bot-Protobuf` **v2.0.1** (même version que le daemon) avec buf + protoc-gen-go + protoc-gen-go-grpc ; dans `internal/server/notif/olvid/pb/`, **sous AGPL-3.0** (en-tête, licence, `LISEZMOI.md`) ; régénération : `make olvid-pb` | § 10 (client Go généré) ; réponse 1 |
| Dépendances | `google.golang.org/grpc` et `protobuf`, déjà compilés avec la TSDB, maintenant utilisés directement | aucun module nouveau |
| Coût | binaire **27,1 → 29,7 Mo** (+2,6 Mo de code généré) ; RAM : une connexion gRPC (~quelques centaines de Ko) | — |
| Connexion | réseau Docker `obs` (adresse fixe du daemon), sans TLS, aucun port publié ; en-tête `daemon-client-key` (relevé dans le client Python officiel) ; **délai de 15 s sur chaque appel** et **keepalive HTTP/2** (30 s), repris de gladys-olvid | un daemon disparu ne bloque rien |
| Envoi | abonnement `MessageUploaded` (messages sortants de la discussion) **avant** l'envoi, `MessageSend` (texte brut, sans aperçu de lien), attente de la confirmation (**30 s** par défaut, 5 s à 5 min) | essai du 10/11 : `MessageSend` répond « envoyé » même coupé |
| Sans confirmation | **`MessageDelete`** (dans le daemon seulement) puis échec → bascule sur le secours ; si l'annulation échoue, l'erreur le dit (« doublon possible ») | réponse 2 ; à confirmer par l'essai `docs/olvid.md` § 4 |
| Vérification | chaque minute : `Ping` + `AuthenticationTest` (clé acceptée), sans message ; ne peut pas voir la perte des serveurs Olvid (aucun appel) : c'est la confirmation des envois qui la détecte | brief (« un échec d'envoi compte comme une panne ») |
| Secret | clé client chiffrée en base (usage `notif.olvid.cle`), masquée, hors audit | brief |
| Tests | faux daemon gRPC (mêmes services générés) : envoi confirmé ; coupure (accepté, jamais confirmé) → échec en 1 s et message annulé ; mauvaise clé refusée sans être montrée ; daemon arrêté | brief (simulation de panne) |
| Gestion de l'invitation | dans l'interface web (étape 5, § 17.10) | demande de l'utilisateur |

Installation à côté de mirador : `docs/olvid.md` (compose sur le réseau
`obs`, réglages du canal, essai de l'annulation).

Modèle de menaces (module Olvid) :

| Menace | Parade |
|---|---|
| Daemon joignable depuis l'extérieur | aucun port publié ; seul le réseau Docker interne |
| Vol de la clé client | chiffrée en base ; jamais affichée, ni dans l'audit ni dans les erreurs (testé) ; elle ne donne accès qu'à l'identité du bot |
| Daemon qui accepte mais ne livre pas (réseau Olvid coupé) | confirmation obligatoire ; sinon annulation et secours |
| Doublon en retard | annulation dans le daemon ; à confirmer par l'essai |
| Code généré modifié à la main | régénéré par script depuis l'étiquette officielle ; en-tête « DO NOT EDIT » |
| Code généré signalé par gosec (`unsafe` de protobuf) | exclu de gosec (`-exclude-generated`) ; notre code reste analysé |


---

## 18. Étape 5 — interface web (10/11)

| Partie | Contenu |
|---|---|
| 1 | Socle sécurité : comptes, 2FA, sessions, CSRF, verrouillage, CrowdSec, rôles, premier admin, liste d'adresses à confirmer |
| 2 | Vue d'ensemble, machines, enrôlement, mises à jour des agents, tableaux de bord (moteur + 6 tableaux convertis), courbes |
| 3 | Alertes, sourdine (silences, maintenances, inhibitions, dépendances), envois, comparaison vmalert, règles (formulaire, modèles, test 24 h, YAML) |
| 4 | Logs (recherche bornée), flux entrants bloqués |
| 5 | Réglages : notifications, conservation des données, sauvegardes, agents, interface, audit, export / import YAML |
| 6 | Mesures, documentation (`docs/interface-web.md`), CrowdSec (`deploy/crowdsec/`) |

Code : `internal/server/console/` (Go + `html/template`), fichiers
statiques et gabarits embarqués. Tests : `internal/server/console/*_test.go`
et `internal/server/console_e2e_test.go` (vrai serveur, données, connexion
2FA, chaque page, chaque formulaire important).

### 18.1 Choix techniques et coût

| Élément | Choix | Raison / coût mesuré |
|---|---|---|
| Rendu | `html/template` côté serveur (échappement automatique), ~200 lignes de JavaScript sans framework (`mirador.js`, `graphes.js`) | § 8 ; rien à compiler côté navigateur, maintenable seul |
| Graphiques | **uPlot 1.6.32** (MIT, 51 Ko, ~22 Ko compressé), embarqué ; données en JSON alignées (un tableau de temps + une valeur par série), **compressées gzip** | § 8 ; canvas : léger sur téléphone ; aucun `eval` ni `innerHTML` de style (compatible CSP stricte, vérifié : 0 erreur dans la console du navigateur) |
| Statiques | embarqués (`embed`), compressés **une fois au démarrage**, adresse avec empreinte (`?v=`) → gardés un an par le navigateur | 1 requête par fichier et par version |
| Rafraîchissement | fragments rechargés toutes les 60 s, **en pause quand l'onglet est caché** ; courbes idem | brief (« pas de rafraîchissement agressif ») |
| Requêtes des pages | **PromQL restreint** (même liste blanche que les règles) via `alertes.Lecteur` : 2 requêtes à la fois, 500 000 points, 10 s, 2 000 séries, 1 500 pas ; base brute ou agrégée selon la période ; **cache de 20 s** (400 entrées) | remplace les « requêtes nommées en Go » du § 4.8 : les 134 panneaux Grafana gardent leur requête telle quelle |
| Tableaux de bord | **données JSON versionnées** (`console/tableaux/*.json`), converties une fois depuis `reference/grafana/` par `grafana.go` (`CONVERTIR_GRAFANA=1 go test …`) : 6 tableaux, 134 panneaux ; LogsQL et carte non repris (listés dans chaque tableau) | ajouter un tableau = un fichier ; chaque expression est vérifiée par un test |
| Sessions | **en mémoire** (empreinte SHA-256 de l'identifiant, 1 000 au plus) ; compte relu en base toutes les 10 s au plus | aucune écriture disque par requête ; un redémarrage ferme les sessions (acceptable, plus sûr) |
| Mots de passe | **Argon2id** 19 Mio / 2 passes / 1 fil (minimum OWASP), **2 calculs à la fois au plus** | ~35 ms par connexion ; 38 Mio au pire même sous une avalanche de tentatives |
| 2FA | **TOTP** (RFC 6238, écrit ici, ~60 lignes), anti-rejeu (dernier pas gardé, mise à jour conditionnelle) ; QR code dessiné par le serveur (`rsc.io/qr`, BSD-3) | WebAuthn non fait (bibliothèque lourde + JS) : le brief accepte « TOTP ou WebAuthn » |
| Recherche de logs | 2 recherches à la fois au plus, limites du moteur (31 j, 10 000 lignes, 10 s) ; lignes répétées repliées ; palmarès calculé sur les lignes trouvées seulement | protection CPU |
| Envoi d'une version d'agent | formulaire ≤ 30 Mio gardé **en mémoire** (pas de fichier temporaire : l'image n'a pas de `/tmp`) ; limite de corps à 31 Mio pour cette seule adresse, 1 Mio ailleurs | pic de ~30 Mo pendant une publication (rare) |
| Binaire | 29,7 → **30,8 Mo** (+1,0 Mo : gabarits, uPlot, tableaux, QR) | — |

### 18.2 Sécurité de l'interface

- Aucun compte par défaut : jeton à usage unique dans le journal ; annulé
  dès qu'un compte existe.
- Mot de passe (12 à 128 caractères) puis **code 2FA obligatoire** ; 2FA créée
  à la première connexion ; nouvelle 2FA appliquée seulement après un code
  juste de la nouvelle (on ne s'enferme pas dehors).
- Cookie `__Host-mirador` : `Secure`, `HttpOnly`, `SameSite=Strict`, sans
  date (fermé avec le navigateur) ; **nouvel identifiant** après le mot de
  passe et après la 2FA ; inactivité 30 min, durée max 12 h (réglables).
- **CSRF** : jeton par session (champ caché ou en-tête `X-CSRF`) + contrôle
  d'origine (`Sec-Fetch-Site`, à défaut `Origin` = `Host`).
- Verrouillage : 5 échecs / 15 min par adresse (réglable), 15 par compte ;
  même message et même durée de calcul pour un compte inconnu ; chaque échec
  dans le journal sous une forme fixe (**CrowdSec** : `deploy/crowdsec/`).
- Rôles admin / lecture ; **code 2FA redemandé** pour les actions sensibles
  (liste : `docs/interface-web.md` § 3) ; dernier admin impossible à
  supprimer, rétrograder ou désactiver.
- **Liste d'adresses** et **changement de son propre rôle** : appliqués puis
  annulés automatiquement sans confirmation sous 2 min ; une liste qui exclut
  l'adresse actuelle est refusée.
- Secrets (Telegram, Olvid, ntfy, SMTP, heartbeat, SFTP, S3) : champs en
  écriture seule (vide = garder), chiffrés, jamais réaffichés ni dans
  l'audit (testé).
- En-têtes de l'étape 3 (CSP sans `unsafe-inline`, aucun style en ligne dans
  les gabarits, `frame-ancestors 'none'`, `nosniff`…) ; HSTS par nginx.
- Logs et étiquettes = contenu non fiable : toujours du texte échappé (testé
  avec `<script>`) ; export CSV protégé contre l'injection de formules.
- Requêtes SQL paramétrées ; filtres des tableaux limités à des caractères
  sûrs et transformés en **égalité exacte** (jamais d'expression régulière
  fournie par l'utilisateur dans une requête de tableau).
- Secours sans interface : `utilisateur-secours`, `console-ips-vider`.

### 18.3 Modèle de menaces (étape 5)

| Menace | Parade |
|---|---|
| Vol du mot de passe | 2FA obligatoire ; verrouillage ; CrowdSec ; liste d'adresses |
| Force brute en masse | 2 calculs Argon2id à la fois ; tables de verrouillage bornées (10 000) ; nginx + CrowdSec en amont |
| Vol du cookie | `HttpOnly`, `Secure`, `__Host-` ; sessions courtes ; fermeture à distance (page Sécurité) ; changement de mot de passe = toutes les sessions fermées |
| Fixation de session | nouvel identifiant à chaque étape de connexion |
| CSRF | `SameSite=Strict` + jeton + origine |
| XSS par des logs ou des étiquettes | échappement de `html/template`, CSP sans script en ligne |
| Rejeu d'un code 2FA | dernier pas accepté gardé ; mise à jour conditionnelle (une seule requête gagne) |
| S'enfermer dehors | liste d'adresses / rôle à confirmer sous 2 min ; dernier admin protégé ; commandes de secours |
| Admin dont la session est volée | code 2FA pour chaque action sensible |
| Page lourde qui sature le serveur | limites du lecteur PromQL et des recherches ; cache ; 1 Mio par requête |
| Fichier importé piégé (YAML) | 4 Mio, champs inconnus refusés, chaque partie passe par sa fonction habituelle (mêmes contrôles) |
| Énumération des adresses | toute adresse inconnue renvoie vers la connexion (pas de 404 sans session) |

### 18.4 Mesures (`MESURE_INTERFACE=1 go test -run TestMesureInterface ./internal/server`)

50 machines (~35 séries chacune, 6 h de données à 30 s), 20 passages sur
11 pages, machine de développement (4 cœurs).

| Page | Médiane | Max | Taille |
|---|---|---|---|
| Vue d'ensemble | 3 ms | 4 ms | 21 Ko |
| Tableau « Serveurs Linux » (6 h ou 7 j) | 3 ms | 7 ms | 32 Ko |
| Machine, alertes, règles, logs | 1 à 2 ms | 2 ms | 2 à 10 Ko |
| Courbe CPU, 50 séries (JSON) | 22 ms | 34 ms | 267 Ko avant gzip (~5 fois moins compressé) |
| Connexion (Argon2id) | 35 ms | — | — |

- CPU : ~7 ms par requête en moyenne (les courbes coûtent le plus). Un
  tableau ouvert, rafraîchi chaque minute : **~0,3 % d'un cœur**.
- Mémoire : tas Go +20 Mo environ pendant l'essai en rafale, rendu ensuite ;
  au repos l'interface ne garde que les sessions et le cache (≤ 400 résultats).
- Pas encore mesuré sous la charge complète (50 agents + règles +
  interface) : à faire avec le test de charge de l'étape 3 (§ 18.7).

### 18.5 Écarts avec le brief et restes à faire

| Point | État |
|---|---|
| Page Olvid (invitations, QR, codes SAS, contacts) — décidé le 10/11 | **fait** (réponse du 10/11 : « maintenant »), § 18.9 |
| Vues personnalisables (blocs réordonnables, ordinateur / téléphone) | **pas fait** ; le format JSON des tableaux le permettra (tableaux en base) |
| GéoIP des flux bloqués | **fait, automatique** (réponse du 10/11), § 18.8 : pays par adresse et palmarès des pays ; la **carte** du monde elle-même reste à faire (fond SVG embarqué) |
| Destinataire par règle | **non voulu** (réponse du 10/11) : un seul canal à la fois, § 18.10 |
| WebAuthn | non : TOTP seul (§ 18.1) |
| Collecteurs | étape 6 |
| Tableau « Logs » de Grafana (13 panneaux LogsQL) | remplacé par la page Logs (recherche, palmarès) ; courbes de volume par machine non faites |
| Rétention par groupe, machine ou type de log | voir § 18.6 |

### 18.6 Conservation des données

- Durées (brut, agrégé, logs) et plafonds de taille réglables, appliqués **à
  chaud** aux moteurs (suppression de blocs / fichiers entiers) ; défauts
  7 j / 30 j / 30 j et **5 + 1 + 4 Gio = 10 Gio** (réponse du 10/11).
- Page : place occupée, séries actives, plus ancienne donnée, volume de logs
  par jour, disque libre ; **estimation** avant validation (d'après le volume
  mesuré).
- Réduction : case de confirmation + code 2FA, puis **appliquée après 5 min,
  annulable** ; audit de chaque étape ; règle `ServeurDisqueSeRemplitVite`
  (`predict_linear` sur 6 h, plein dans moins de 3 jours).
- **Limite signalée (brief)** : la TSDB et le stockage des logs n'ont qu'une
  rétention par base. Une durée **par groupe, par machine ou par type de log**
  demanderait une base par catégorie (plus de RAM, plus de fichiers ouverts).
  Proposition si besoin : deux bases de logs (« docker » à 7 j, le reste à
  30 j), Docker faisant 61 % des lignes.

### 18.7 À faire avant la validation

- ~~Test de charge complet avec un navigateur qui regarde un tableau (RAM < 256 Mio ?).~~
  **Fait le 11/11** (`MESURE_COMPLETE=1 DUREE=6m go test ./internal/server -run TestMesureComplete`) :
  50 agents (~640 séries chacun / 30 s, 6 h d'historique) + logs du parc, et
  en même temps un navigateur (vue d'ensemble rafraîchie, tableau Linux et
  ses 18 courbes, périodes 6 h et 7 j, éditeur de panneau avec la liste des
  métriques, vérification d'une requête à chaque tour) ; GOMEMLIMIT 200 Mio,
  GOGC 50 comme en production. **Pic 211 Mio** (VmHWM du processus, qui
  contient aussi le générateur de charge et le navigateur), RSS courant
  164 à 197 Mio, tas Go max 139 Mio ; CPU 2,3 % d'un cœur en moyenne sur
  6 min (805 requêtes de l'interface). Objectif < 256 Mio : **tenu**, marge
  ~45 Mio.
- Essai sur le VPS derrière nginx (`docs/interface-web.md`).

### 18.8 GéoIP automatique **[Décidé, réponse du 10/11 : « automatique »]**

| Élément | Choix | Raison / coût mesuré |
|---|---|---|
| Base | DB-IP « country lite » (CC BY 4.0, mention « IP Geolocation by DB-IP » sur les pages) | gratuite, sans compte ; mise à jour mensuelle par DB-IP |
| Téléchargement | **par le serveur, automatique** : chaque mois, adresse **figée dans le code** (`download.db-ip.com`), premier essai 5 min après le démarrage puis toutes les 6 h tant que la base du mois manque ; mois précédent si celle du mois n'est pas encore publiée ; 64 Mio au plus ; **vérifiée avant de remplacer** l'ancienne ; désactivable (Réglages › Interface) | brief ; aucun réglage d'adresse (pas de SSRF possible) ; 4,6 Mo par mois sur le réseau |
| Disque | fichier compressé gardé tel quel (4,6 Mo) dans le dossier des données | — |
| Mémoire | **chargée seulement à l'usage** (page « Flux bloqués »), libérée après 10 min sans usage ; on garde le début de chaque plage (IPv4 : 4 octets, IPv6 : 64 premiers bits) | mesuré sur la vraie base (11/2026) : 363 000 plages IPv4, 369 000 IPv6, **6,3 Mo**, chargée en **0,3 s** ; 0 octet le reste du temps |
| Réseau du conteneur | le serveur doit pouvoir joindre `download.db-ip.com` en HTTPS (sortie) | comme Telegram aujourd'hui |

### 18.9 Page Olvid **[Décidé, réponse du 10/11 : « maintenant »]**

- Réglages › Olvid : **lien d'invitation du bot et son QR code** (à scanner
  dans l'application), invitation d'une personne par **son** lien (seuls les
  liens `https://invitation.olvid.io/#…` sont acceptés), invitations en
  cours avec leur état, **acceptation**, **code à 4 chiffres** à donner et à
  saisir, refus / suppression, contacts, **choix de la discussion** qui
  reçoit les alertes (code 2FA).
- Appels gRPC avec la **clé client** du bot seulement (jamais la clé admin),
  15 s par appel ; logique reprise de gladys-olvid, réécrite en Go.
- Acceptation automatique des invitations : **toujours désactivée**.
- Testé avec un faux daemon gRPC (états, codes, lien étranger refusé,
  mauvaise clé refusée sans être montrée). Essai réel à faire sur le VPS.
- Menace ajoutée : un lien piégé collé dans le formulaire → seul le domaine
  d'invitation d'Olvid est accepté, et l'action est notée dans l'audit.

### 18.10 Un seul canal à la fois **[Décidé, réponse du 10/11]**

- Toutes les alertes, quelle que soit leur gravité, partent sur **le canal
  principal** choisi dans l'interface. Si le principal est en panne
  (3 échecs de suite), elles partent sur **le secours**, avec le message
  « canal principal en panne » ; retour sur le principal après 3
  vérifications réussies (déjà en place depuis l'étape 4, testé de bout en
  bout). Inversion principal / secours en un clic.
- Remplace le routage « critique sur tous les canaux » prévu au brief :
  l'option est retirée de l'interface et de l'import (liste `tous` toujours
  vide) ; le code de l'étape 4 la garde, inutilisée.
- La gravité ne règle plus que les délais (attente, écart, rappels) et le son.
- Pas de destinataire par règle.

### 18.11 Design de l'interface **[Décidé, réponses du 10/11]**

- Pistes dessinées et comparées sur une page de maquettes (12 pistes) ;
  choix : **B2** (clair, aéré, cartes blanches, bleu `#2F5BEA`, titres en
  Space Grotesk, texte en Inter), avec les **cartes de chiffres de la piste
  B** (pastille d'icône colorée, gros chiffre, ligne de détail colorée).
- **Menu à gauche**, repliable (icônes seules ; le choix est retenu dans le
  navigateur), ou **en haut** : réglage « Position du menu » (Réglages ›
  Interface), avec « menu replié par défaut ». Sur téléphone : toujours en
  haut, déroulant.
- **Thèmes clair et sombre** (automatique selon l'appareil, ou forcé).
- Vue d'ensemble : 4 chiffres clés (machines, alertes, notifications,
  stockage), alertes en cours, **tableau des machines** (état, petite
  courbe CPU sur 6 h, CPU, mémoire, disque, dernière donnée), sauvegarde,
  canaux. Une seule requête PromQL pour toutes les petites courbes,
  gardée 60 s.
- Polices **embarquées** (123 Ko, licence OFL), icônes SVG dessinées dans la
  page : aucune ressource externe, CSP inchangée ; fichiers statiques avec
  ETag (304 si déjà reçus).
- Coût : binaire +0,2 Mo ; aucune requête en plus côté serveur hors la
  courbe des machines (une requête par minute au plus).

Défaut trouvé pendant ce travail et corrigé : deux écritures (« Réglages ›
Interface » et création d'un compte dont le nom est pris) relisaient la
base pendant leur propre transaction ; la base n'ayant qu'une connexion,
**toute l'interface se figeait**. Corrigé, test `TestPasDeBlocageBase`
ajouté, et toutes les transactions de l'interface relues.

### 18.12 Retours du 10/11 sur l'affichage **[Décidé]**

- **Majuscules** : première lettre en majuscule pour les libellés, cases,
  listes, pastilles et messages (fonction `majusculeDebut`, accents compris).
  Les noms de produits gardent leur graphie (ntfy, vmalert, starttls).
- **Tailles** : virgule décimale (« 20,5 Go ») ; formulaire « Conservation »
  en tableau aligné (durée, plafond, limites).
- **Adresse d'ingestion** (Réglages › Interface › Adresse du serveur) :
  `https://hôte[:port]`, sans chemin. La page Machines montre la commande
  d'installation complète ; après « Créer un jeton », la commande contient le
  jeton. Le jeton n'est affiché que dans cette réponse (ni session, ni base
  en clair) ; recharger ne recrée rien (code 2FA à usage unique).
- **Olvid** : l'onglet et la page n'apparaissent que si le canal Olvid est
  **activé** (case « Actif »). Le numéro de discussion n'est exigé que si
  Olvid est le canal principal ou de secours : on active Olvid (adresse +
  clé), puis on choisit la discussion dans la page Olvid. Sans discussion,
  l'envoi échoue avec un message clair.
- **Heartbeat** : plus seulement HetrixTools. Choix du service
  (HetrixTools, Healthchecks.io, Uptime Kuma, Better Stack, UptimeRobot,
  Cronitor, autre) : tous reçoivent un simple GET, le choix sert à
  l'affichage et aux exemples d'adresse. Bouton « Tester le heartbeat ».
  Service sur le réseau local : même case « adresse privée » que ntfy / SMTP
  (protection SSRF sinon).
- Coût : une lecture SQLite de plus par page (réglages des notifications,
  quelques centaines d'octets) ; rien d'autre.

### 18.13 Installer Olvid depuis l'interface **[Décidé, réponse du 10/11 : « oui »]**

- Demande : déployer le conteneur Olvid depuis l'interface, comme les
  intégrations de Gladys. **Écarté** : piloter Docker depuis mirador (socket
  Docker = droits root sur le VPS ; un proxy filtré ne suffit pas, créer un
  conteneur suffit à devenir root). SPEC § 10 inchangé.
- **Retenu** : le daemon est un service du compose de mirador, lancé
  seulement avec le profil `olvid` (`docker compose --profile olvid up -d`),
  nommé `mirador-olvid` (joint par son nom sur le réseau `obs`, aucun port
  publié). Puis un **assistant** (Réglages › Olvid › installer, admin + 2FA) :
  1. « Vérifier que le daemon répond » (Ping, sans clé) ;
  2. avec la clé **admin** du daemon, donnée une fois : identité du bot
     reprise (même nom) ou créée, puis **clé client** dédiée
     (`mirador-server`) ; canal Olvid enregistré et activé ;
  3. renvoi vers la page Olvid (invitations, codes, discussion).
- Menaces : la clé admin ne passe que dans cette requête (HTTPS, admin, 2FA),
  n'est ni gardée, ni journalisée, ni écrite dans l'audit ; seule la clé
  client est gardée, chiffrée. Adresse du daemon libre (admin seulement) :
  réseau interne attendu.
- Coût : aucun pour mirador hors de l'assistant ; daemon ~170 Mio quand le
  profil est lancé (SPEC § 17.10).

### 18.14 Tableaux modifiables **[Décidé, réponses du 10/11]**

Réponses : glisser-déposer (« un peu de javascript »), **admins
seulement**, **avant** la validation de l'étape 5 ; les 6 tableaux fournis
modifiables **et** supprimables.

- Stockage : table `tableaux` (migration 0008) : une ligne par tableau en
  service. Les 6 tableaux fournis sont **proposés** (catalogue : aperçu avec
  ses données, « Installer »), pas installés d'office (demande du 10/11).
  Installé tel quel = ligne au contenu vide (il suit les versions de
  mirador) ; modifié = son JSON ; « Rétablir l'original » revide le contenu ;
  supprimer efface la ligne (un tableau fourni retourne au catalogue). La
  fiche d'une machine garde ses graphiques (tableau Linux du catalogue).
- En mémoire : l'ensemble en service est recalculé après chaque changement
  (pointeur atomique, jamais modifié sur place). Quelques dizaines de Kio.
- Éditeur sans JSON : rangées (ajout, titre, repliée, suppression), panneaux
  (formulaire complet, aperçu calculé sans enregistrer, dupliquer,
  supprimer), largeur en douzièmes (auto, ¼, ⅓, ½, ⅔, ¾, pleine), ordre par
  glisser-déposer (HTML5, ~70 lignes de JS, envoi par formulaire ordinaire),
  propriétés et filtres. Import Grafana (convertisseur existant) ou JSON
  mirador, export JSON.
- Menaces : admins seulement, CSRF comme partout ; suppression = code 2FA ;
  chaque tableau est revérifié en entier (PromQL restreint, 80 panneaux,
  12 requêtes, 512 Kio) ; empreinte du contenu contre l'écrasement par un
  onglet ancien ; ordre reçu vérifié (chaque rangée et chaque panneau une
  seule fois) ; audit de chaque changement (4 Kio du JSON avant / après).
  Les valeurs de filtres restent limitées (pas de PromQL injecté).
- Coût : aucun hors de l'édition ; une requête SQL par changement ;
  l'aperçu d'un panneau = les mêmes requêtes que son affichage.
- Pas fait : édition au doigt sur téléphone (le glisser-déposer HTML5 n'y
  est pas fiable) ; les tableaux ne sont pas dans l'export YAML de la
  configuration (ils ont leur export JSON ; la sauvegarde de la base les
  contient).

### 18.15 Site et type choisis dans l'interface **[Décidé, réponse du 10/11]**

- Demande : enlever `--site` et `--env` du script, et choisir le site et le
  type à l'ajout de la machine ou après.
- Le jeton d'enrôlement porte le site, le type (étiquette `env`) et le groupe
  (migration 0009) ; la machine créée à sa demande les reçoit ; ils se
  corrigent à l'approbation ou dans la fiche de la machine.
- **Le serveur les impose** aux métriques et aux logs d'un agent, comme il
  impose déjà le nom (`instance` / `host`) : un changement dans l'interface
  vaut tout de suite, sans toucher à la machine. Fiche vide = étiquettes de
  l'agent gardées (machines déjà installées). Jamais pour un collecteur (ses
  séries décrivent d'autres appareils).
- Agent : `MIRADOR_SITE` / `MIRADOR_ENV` facultatifs avec mirador-server
  (`MIRADOR_SERVEUR`), toujours obligatoires sans lui ; script :
  `--site` / `--env` facultatifs avec `--jeton` / `--mirador`, repris de
  l'installation précédente s'ils ne sont pas redonnés.
- **Listes** (demande du 10/11) : sites et types se créent dans Réglages ›
  Sites et types (réglage `machines.listes`) ; les formulaires (jeton,
  approbation, fiche) proposent des listes déroulantes et le serveur refuse
  une valeur hors liste (la valeur actuelle d'une machine reste permise).
  Premier passage : listes tirées des machines existantes (+ types vm, vps,
  physique, nas, conteneur). Une valeur utilisée ne peut pas être supprimée.
  La ligne de commande `jeton -site -env` reste libre (secours).
- Coût : deux colonnes de plus lues avec le token (déjà en cache).
- Comme pour un renommage, changer le site ou le type crée de nouvelles
  séries ; l'historique garde les anciennes valeurs.

### 18.16 Conservation du journal d'audit et de l'historique **[Décidé, demande du 10/11]**

- Avant : audit jamais purgé, alertes résolues et envois gardés 180 j (fixé
  dans le code).
- Maintenant, dans « Conservation des données » : **journal d'audit**
  (défaut 365 j, de 90 j à 10 ans) et **alertes résolues et envois** (défaut
  180 j, de 30 à 730 j). Purge une fois par jour (ligne par ligne, SQLite) ;
  une purge de l'audit écrit elle-même une ligne d'audit.
- Menace : un admin malveillant qui voudrait effacer ses traces. Garde-fous :
  90 j minimum, et une baisse est une **réduction** comme les autres
  (confirmation, code 2FA, appliquée après 5 min, annulable, auditée).
- Page : nombre de lignes d'audit, taille approchée, plus ancienne ligne ;
  nombre d'alertes résolues et d'envois gardés.

### 18.17 Retours sur la v0.2.3 **[Décidé, 11/11]**

- Page Tableaux : « Logs » et « Flux entrants bloqués » ne sont plus
  installés d'office ; proposés comme raccourcis (Aperçu, Installer,
  Retirer ; réglage `tableaux.pages`). Les pages restent dans le menu Logs.
- Icône et couleur **du tableau** (à côté du titre, dans la liste et sur la
  page), choisies dans ses propriétés ; une par défaut pour les 6 tableaux
  fournis. 48 icônes (26 de plus), dessinées ici (aucune ressource externe).
- Affichage : un tableau suivi d'un formulaire dans une carte ne le chevauche
  plus ; champs de durée de « Conservation » assez larges ; lien
  « Supprimer » en rouge.

### 18.18 Aide aux requêtes PromQL des tableaux **[Décidé, réponse du 11/11 : 1 + 2 + 4 + 5]**

- **Requêtes toutes faites** (≈ 40, `modeles_requetes.go`) : système, disques,
  **températures** (capteurs hwmon, zones thermiques, disques SMART, NAS
  Synology, UniFi), réseau, services, Docker, sécurité (pare-feu, SSH,
  erreurs disque, certificats), agents. « Insérer » remplit la première
  ligne vide (ou tout le panneau : titre, type, unité, couleurs, icône) ;
  les filtres du tableau sont ajoutés tout seuls. Chaque modèle est vérifié
  par un test (avec et sans filtres).
- **Assistant en 4 choix** : métrique réellement reçue (liste du stockage,
  cache 5 min, description en français quand elle est connue), calcul
  (valeur, débit, augmentation, moyenne 5 min), combinaison + regroupement
  (étiquettes de la métrique), filtre en plus. La requête est construite
  **côté serveur** à partir de valeurs contrôlées (noms et valeurs simples :
  rien ne peut sortir de la chaîne PromQL), puis reste modifiable.
- **Vérification en direct** (700 ms après la frappe) : erreur en français,
  nombre de séries, une valeur d'exemple, **phrase qui explique la requête**,
  **étiquettes cliquables** pour la légende, **unité proposée** (Appliquer).
- Menaces : admins seulement, CSRF (en-tête X-CSRF), même lecteur restreint
  que les tableaux (2 requêtes à la fois, 500 000 points, 10 s) ; réponses
  insérées comme texte (jamais HTML). Coût : une requête par vérification
  (cache 20 s) ; liste des métriques une fois par 5 min.
- Pas fait (choix du 11/11) : autocomplétion pendant la frappe.

### 18.19 Installation de l'agent par téléchargement HTTPS **[Décidé, demande du 11/11]**

- Plus de copie manuelle du script ni du binaire : la machine les télécharge
  auprès de mirador-server (**vhost d'ingestion**, adresse réglée dans
  Réglages › Interface ; chemins `/api/agent/v1/install` et
  `/api/agent/v1/install/binary?arch=amd64|arm64`). Le **script est public**
  (il est dans le dépôt ; demande du 11/11 : pas d'en-tête à taper) ; le
  **binaire exige le jeton d'enrôlement**, que le script envoie dans l'en-tête
  `Authorization` (jamais dans l'adresse, donc ni dans les journaux nginx ni
  dans l'historique).
- Commande affichée par l'interface (Machines › Ajouter une machine), jeton
  compris : `curl https://INGESTION/api/agent/v1/install -o mirador-agent-install.sh` puis
  `bash mirador-agent-install.sh --serveur … --jeton …`. Le script détecte
  l'architecture, télécharge le binaire (curl ou wget, HTTPS seulement) et
  vérifie son empreinte SHA-256 (en-tête `X-Mirador-SHA256`).
- Binaire servi : la **version globale publiée** (signée, vérifiée à la
  publication) si elle existe pour l'architecture, sinon celui **embarqué
  dans l'image** du serveur (construit par la CI avec le serveur, même
  version ; amd64 et arm64, +~16 Mo d'image, rien en mémoire).
- Menaces : jeton exigé (valable, non expiré, token pas encore remis) ;
  essais faux comptés par adresse (même limite que l'enrôlement) ; nginx ne
  laisse passer que les adresses autorisées ; architecture vérifiée (pas de
  chemin fourni par le client). La confiance repose sur HTTPS vers son
  propre serveur, comme pour l'enrôlement ; les mises à jour suivantes
  restent signées (ed25519).
- `--binaire FICHIER` reste possible (machine sans accès au serveur).

### 18.20 Carte des flux bloqués et volume des logs **[Décidé, demande du 11/11]**

- **Carte du monde** (page Flux entrants bloqués) : fond Natural Earth
  1:110m (domaine public, 174 pays, noms français), converti une fois en
  contours SVG (`scripts/generer-carte.py` → `carte_monde.go`, 118 Ko de
  source, ~35 Ko compressé dans la page). Dessinée **côté serveur** : chaque
  pays a une classe de couleur (6 niveaux, échelle logarithmique) et une
  infobulle (nom, nombre de flux). Aucun script ni ressource externe ; CSP
  inchangée. Visible quand la GéoIP est chargée. Le palmarès des pays
  affiche aussi le nom en français.
- **Volume des logs par machine** : le serveur compte les lignes reçues par
  hôte (`host`) à l'ingestion (2 000 hôtes au plus, puis « autres »), et
  écrit chaque minute `mirador_logs_recues_total{instance=…}` (métrique
  interne, job="mirador"). Courbes « lignes par minute » sur la page Logs
  (toutes les machines) et dans la fiche de chaque machine ; deux requêtes
  toutes faites (volume, machines les plus bavardes). Compte aussi ce que le
  collecteur enverra (étape 6). Coût : un compteur par hôte, une série par
  hôte.

### 18.21 Métriques seules, logs seuls, ou les deux **[Décidé, demande du 11/11]**

- Nouveau réglage de l'agent `metriques` (défaut oui) : `MIRADOR_METRIQUES`
  dans le fichier local, `metriques` dans la configuration distante (défaut,
  groupe ou machine : interface › Configuration des agents, « Métriques de la
  machine »). Les logs avaient déjà `journal` et `logs_docker`.
- Métriques désactivées (« logs seulement ») : aucun collecteur ne tourne ;
  l'agent n'envoie que ses propres métriques (~20 séries : version, mémoire,
  file d'envoi, compteurs de logs) : la machine reste vue (pas « muette ») et
  son agent reste supervisé.
- Installation : choix « Envoyer » sur la page Machines (métriques et logs /
  métriques seulement / logs seulement) → `--sans-logs` ou `--sans-metriques`
  ajouté à la commande. Les deux à la fois : refusé (rien à envoyer).
- Au passage : à l'arrêt, l'agent attend la fin de ses lecteurs de logs
  avant le dernier lot (curseur du journal toujours écrit ; corrige un test
  qui échouait au hasard).

### 18.22 Revue de sécurité du 11/11 (ensemble + connexion) **[Fait]**

Deux revues en lecture seule : l'ensemble du projet, et l'authentification
de l'interface en profondeur. Aucun constat critique ni élevé. Corrigé :

| Gravité | Constat | Correction |
|---|---|---|
| Moyenne | Verrous de connexion contournables : clé = adresse IPv6 complète, table vidée en entier quand pleine (verrou du compte effacé par 10 000 noms bidon) | adresses IPv6 regroupées par /64 ; tables adresses / comptes séparées ; un verrou actif n'est jamais effacé (§ verrou.go) |
| Moyenne | Changement de mot de passe : Argon2id avant le verrou, réponse différente selon que le mot de passe est juste (oracle avec un cookie volé) | verrou vérifié d'abord, même message, échecs comptés par adresse et compte, 5 échecs ferment la session |
| Moyenne | Code 2FA des actions sensibles limité par adresse seulement (cookie volé + rotation IPv6) | compté aussi par compte et par session (5 → session fermée) |
| Moyenne | Script d'installation : écritures root dans `/var/lib/mirador-agent` (dossier de l'agent) → un agent compromis pouvait faire écrire root ailleurs par un lien | écritures faites sous l'identité de l'agent (`runuser`), contenu par l'entrée standard |
| Faible | Verrou de compte = déni de service possible sur l'admin | adresses de confiance (connexion réussie < 30 j) non bloquées par le verrou du compte ; `utilisateur-secours` lève les verrous du serveur en marche |
| Faible | Création de la 2FA sans relire le compte | compte relu (désactivé, génération, 2FA déjà là), 5 codes faux au plus |
| Faible | File Argon2id sans délai | attente bornée à 10 s, puis « serveur occupé » (pas compté comme échec) |
| Faible | Une réussite remettait à zéro le compteur de l'adresse | plus maintenant (seul celui du compte) |
| Faible | Assistant root qui lisait la configuration distante (fichier de l'agent) | ne la lit plus |
| Faible | Jeton d'enrôlement visible dans `ps` (`curl -H …`, `--jeton …`) | en-tête passé par un fichier de configuration curl / wget (root 0700) ; jeton par `MIRADOR_JETON` dans la commande affichée |
| Faible | Limiteurs de l'ingestion saturables (une simple vérification créait une entrée) ; IPv6 par adresse | vérification sans création ; IPv6 par /64 (ingestion, enrôlement, installation) |
| Faible | Une machine pouvait occuper les 4 places de décodage avec des corps lents | 3 envois (POST) en cours au plus par machine (métriques, logs, battement) ; corps à recevoir en 30 s |
| Faible | `POST /confirmer` ouvert au rôle lecture | auteur du changement ou admin seulement |
| Faible | Page Olvid (lien d'invitation, contacts) en lecture seule | admins seulement |
| Info | Configuration distante des agents sans code 2FA | code 2FA exigé |
| Info | Suppression d'un compte : sessions pas fermées (recherche faite après) | fermées (compte lu avant) |
| Info | Cache de construction partagé branches / version | version publiée construite sans cache |

Accepté ou reporté (expliqué) :
- Binaire de l'agent à l'installation non signé (seulement l'empreinte
  envoyée par le serveur, en HTTPS) : la confiance repose sur son propre
  serveur, comme l'enrôlement ; les mises à jour restent signées (Ed25519).
  Possible plus tard : servir le manifeste signé quand une version est publiée.
- Olvid en gRPC sans TLS : le daemon est sur le réseau Docker interne
  (`mirador-olvid`, aucun port publié) ; ne pas le placer sur une autre machine.
- Réseau Docker `obs` partagé avec la stack actuelle : un autre conteneur de
  ce réseau joint mirador sans nginx (comme aujourd'hui pour VictoriaMetrics).
- Actions GitHub et images de base figées par version, pas par empreinte :
  à faire si le dépôt s'ouvre à d'autres personnes.
- `MIRADOR_PROXY` vide derrière nginx : tous les clients auraient la même
  adresse (verrous partagés) ; la doc d'installation le règle.
- Pas de nouveau calcul de l'empreinte Argon2id si ses paramètres changent.

### 18.23 Groupe, nom de la machine et historique après renommage **[Décidé, réponses du 11/11]**

Questions : « À quoi correspond groupe ? Comment le nom de la machine est
repris ? » Réponses : 1. Oui, 2. Oui, 3. Oui, 4. Après (essai sur le VPS
après ces changements) ; « Il faudrait reprendre l'historique entre
l'ancien nom et le nouveau ».

- **Groupe de configuration** : nom expliqué partout. Liste gérée comme les
  sites et types (Réglages › Sites, types et groupes, nombre de machines,
  suppression refusée si utilisé), listes déroulantes au jeton, à
  l'approbation et dans la fiche. La page « Configuration des agents »
  montre chaque groupe et ses machines.
- **Étiquette `groupe`** imposée par l'ingestion (comme `site` et `env`,
  agents seulement) aux métriques et aux logs ; champ indexé dans les blocs
  de logs (filtre de la page Logs). Coût : une étiquette de plus par
  série (quelques octets, même nombre de séries).
- **Nom** : facultatif à la création du jeton (sinon le nom d'hôte proposé
  par la machine) ; vérifié libre (machines, anciens noms, autres jetons
  valables). Corrigeable à l'approbation (champ « Nom »). Toujours
  modifiable ensuite (fiche › Renommer).
- **Historique après renommage** : la base des métriques et les fichiers de
  logs ne se réécrivent pas. L'ancien nom est gardé (table `noms_anciens`,
  migration 0010) et rattaché au nouveau **à la lecture**, dans l'interface :
  - métriques : enveloppe du lecteur PromQL ; un filtre sur `instance`
    porte sur le nom affiché (traduit en liste exacte des noms stockés),
    les séries de l'ancien nom sont montrées sous le nouveau et fusionnées
    avec lui (une courbe continue, pas de double compte dans `sum`) ;
  - logs : filtre machine élargi aux anciens noms (index des blocs
    compris), nom affiché remplacé.
  - Coût : nul tant qu'aucune machine n'a été renommée ; sinon une lecture
    des valeurs de `instance` par sélection et un tri des séries trouvées
    (quelques ms pour 50 machines).
  - Les règles d'alerte lisent les données telles quelles (elles regardent
    les dernières minutes, toujours sous le nouveau nom).
  - Un ancien nom reste réservé (pas de jeton ni de renommage d'une autre
    machine vers lui) ; y revenir l'enlève de la liste. Une machine
    renommée avant approbation n'a pas d'ancien nom (aucune donnée).
- Menaces : un admin peut rattacher l'historique d'une machine à une autre
  seulement en renommant la première (journal d'audit) ; les noms restent
  au format strict (lettres, chiffres, `. _ -`), passés comme valeurs
  exactes (expression régulière échappée).

**Site et type proposés par la machine (11/11, enrôlement de srv-test)** :
l'agent envoie à l'enrôlement le site et le type de sa configuration
(`--site` / `--env`, installation précédente, ou à défaut `OBS_SITE` /
`OBS_ENV` d'Alloy). Le serveur les garde seulement si le jeton n'en donne
pas ; ils sont pré-remplis à l'approbation (corrigeables) et ajoutés aux
listes si l'admin les garde. Jamais le groupe (il décide de la
configuration de l'agent : choix de l'admin seulement). Champs facultatifs
du protocole (anciens agents : rien ne change).

**GéoIP (11/11)** : bouton « Télécharger maintenant » ; case cochée →
essai dans la minute ; après un échec, nouvel essai 30 min plus tard (au
lieu de 6 h) ; case **décochée → base effacée** du disque et de la mémoire.
La carte des flux bloqués est toujours affichée quand la case est cochée,
avec la raison si elle est vide (base pas encore là, aucune adresse
publique).

### 18.24 CI : économie de minutes GitHub Actions **[Fait, 11/11]**

Constat : 90 % du forfait mensuel (2 000 min) utilisé au 11/11. Un push
coûtait ~33 min facturées : 8 tâches, chacune arrondie à la minute
supérieure, test de sauvegarde / restauration lancé deux fois, benchmarks
à chaque push. Changements (durée d'une CI inchangée, ~7 min, celle des
tests) :

| Avant | Après |
|---|---|
| tout push lance la CI | push de documentation seule (`*.md`, `docs/`, `reference/`) : rien (les tags passent toujours) |
| contrôles et compilation : 2 tâches | 1 tâche |
| fuzzing : 4 tâches × (14 cibles / 4) × 20 s | 1 tâche, 14 cibles × 15 s |
| sauvegarde / restauration lancée 2 fois | 1 fois (dans `go test -race ./...`) |
| benchmarks à chaque push | à la demande (Actions › ci › Run workflow) |

Côté méthode : commits regroupés, push moins souvent (un push par lot de
changements, pas par petite correction).

### 18.25 Clé publique de signature posée à l'installation **[Décidé, réponse du 11/11]**

Question de l'utilisateur : « Pourquoi intégrer la clé publique au code ? Le
code est public. » Une clé publique n'est pas un secret (elle vérifie, elle
ne signe pas) ; mais intégrée au code, chaque utilisateur du projet devrait
compiler ses binaires avec sa clé. Choix : **clé posée à l'installation**.

| Élément | Choix | Raison / coût |
|---|---|---|
| Où l'agent lit la clé | `/etc/mirador-agent/cles-publiques.txt` (`MIRADOR_FICHIER_CLES`), **root:root 0644**, relu à chaque mise à jour proposée | l'agent (utilisateur sans droits) lit, ne peut pas modifier ; refus si le fichier est un lien, n'appartient pas à root ou est modifiable par le groupe / les autres |
| D'où vient la clé | interface : Mises à jour des agents › Clé publique (admin, code 2FA, audit) ; servie en clair sur `GET /api/agent/v1/cles` ; le script la demande à la **première** installation, ou la prend de `--cle-publique` | même confiance qu'à l'installation (le binaire vient déjà du serveur) |
| Changement | jamais à distance : seulement en relançant le script avec `--cle-publique` | un serveur piraté ne peut pas remplacer la clé des machines déjà installées, donc ne peut pas leur faire accepter un faux agent |
| Rotation | deux clés au plus (l'actuelle et la suivante) | changer de clé sans coupure |
| Serveur | vérifie chaque version publiée avec les clés réglées ; refus tant qu'aucune clé n'est réglée | erreur vue tout de suite, pas sur 25 machines |
| Empreinte | 16 premiers caractères hexadécimaux du SHA-256 de la clé, affichée par `mirador-release`, l'interface et le script | comparer d'un coup d'œil |
| `cles/publiques.txt` | gardé, facultatif (pour qui compile lui-même) ; s'ajoute au fichier | — |
| Relancer le script sur une machine enrôlée (ajout du 11/11) | sans jeton ni `--binaire` : le binaire est téléchargé avec le **token de la machine** (mêmes règles que l'ingestion : valable, non révoqué, adresse autorisée) ; `--binaire` téléchargé à la main (non exécutable) : copie privée rendue exécutable | vu à l'enrôlement de la machine de test : deux erreurs évitables |
| Coût | une lecture d'un fichier de 200 octets par version proposée | négligeable |

Menaces :

| Menace | Parade |
|---|---|
| Serveur piraté qui veut pousser un faux agent | la clé des machines installées ne vient plus du serveur ; signature exigée |
| Agent compromis qui remplace la clé | fichier root non modifiable par l'agent ; droits vérifiés à chaque lecture |
| Serveur piraté au moment d'une **nouvelle** installation | même risque qu'avant (le binaire vient aussi du serveur) ; empreinte affichée par le script, à comparer avec `mirador-release` |

### 18.26 Versions de l'agent signées par la CI de GitHub **[Décidé, réponse du 11/11]**

Demande : « se passer de l'étape du PC et tout faire via l'interface web ».
La clé privée ne peut pas aller sur le serveur (un serveur piraté signerait
un faux agent pour tout le parc). Choix (option 3) : **signature par la CI
de GitHub à chaque tag**, import depuis l'interface. Change la décision du
brief « mirador-release signe sur mon poste » (gardée en secours).

| Élément | Choix | Raison / coût |
|---|---|---|
| Où est la clé privée | secrets de l'environnement GitHub `signature` (`MIRADOR_CLE_SIGNATURE` = fichier chiffré de mirador-release, `MIRADOR_PHRASE`), environnement limité aux tags `v*` | une branche modifiée (y compris par une session Claude) ne peut pas lire les secrets ; les forks et PR externes non plus |
| Signature | tâche `versions-agent` de la CI, après contrôles et tests : `make agent VERSION=X.Y.Z`, `mirador-release signer` (phrase lue dans `MIRADOR_PHRASE`), fichiers ajoutés à la Release du tag | même outil et même format que la signature sur le PC |
| Import | interface (admin, code 2FA) : version `vX.Y.Z` → le serveur télécharge `releases/download/vX.Y.Z/mirador-agent-X.Y.Z-linux-{amd64,arm64}[.manifeste.json]`, vérifie la **signature avant** de télécharger le binaire, puis `Publier` (empreinte, taille) ; en arrière-plan (~20 Mo) | HTTPS seulement ; redirections suivies seulement vers `github.com` / `*.githubusercontent.com` ; manifeste ≤ 64 Kio, binaire ≤ 64 Mio |
| Dépôt source | réglage `maj.depot_versions` (défaut `prohand/mirador-stack`) | un fork règle le sien ; changer de dépôt sans changer de clé ne fait rien accepter de plus |
| Sans secrets | binaires ajoutés à la Release sans signature (avertissement) | un fork sans clé reste utilisable (installation, mise à jour à la main) |
| Confiance | presque la même qu'avant : les binaires signés sur le PC venaient déjà de la CI de GitHub ; le serveur n'a toujours pas la clé | compte GitHub à protéger (2FA, seul à écrire) |

Menaces :

| Menace | Parade |
|---|---|
| Serveur piraté | pas de clé privée sur le serveur ; les agents n'acceptent que des versions signées |
| Workflow modifié sur une branche pour lire la clé | secrets d'environnement limités aux tags `v*` ; `main` protégée (PR obligatoire) |
| Compte GitHub piraté | peut signer : protéger le compte (2FA, clés de sécurité) ; c'était déjà le cas (le code venait de GitHub) |
| GitHub ou réseau qui sert un autre fichier | signature vérifiée avec la clé réglée ; empreinte et taille vérifiées |

### 18.27 Deuxième revue du 11/11 : connexion et architecture **[Correction faite ; propositions traitées : § 18.28, § 18.29, réseau Docker à l'étape 7 (§ 40)]**

Revue en lecture du code de connexion (mot de passe, 2FA, sessions,
verrous), du déploiement et de l'organisation du code.

Corrigé (gravité moyenne) :

| Constat | Correction |
|---|---|
| Verrou contournable par une rafale : le verrou était lu avant le calcul, l'échec compté après. Des requêtes envoyées en même temps passaient toutes le contrôle. Mesuré : 40 mots de passe essayés au lieu de 5 depuis une adresse (`TestRafaleMotsDePasse`). Même principe pour les codes 2FA (mesuré : 6 au lieu de 5 ; en théorie plus avec beaucoup de sessions partielles) | un essai est **réservé avant** la vérification (`verrous.debuter`) : échecs récents + essais en cours ≥ seuil → refus (429, non compté, écrit dans le journal). Appliqué au mot de passe, au code 2FA, aux actions sensibles et au changement de mot de passe. Coût : une table en mémoire qui ne contient que les requêtes en cours |

Propositions de sécurité (état au 16/11) :

| Gravité | Constat | Proposition |
|---|---|---|
| Moyenne (**fait**, § 18.28) | Mot de passe volé : rien ne prévient l'admin. Les codes 2FA faux après un bon mot de passe ne font qu'une ligne de journal | notification (canal existant) sur « code 2FA faux après un bon mot de passe », « compte verrouillé » et « connexion depuis une nouvelle adresse » |
| Faible (**fait**, § 18.28) | Le nom saisi est écrit tel quel dans le journal : un mot de passe tapé par erreur dans le champ « Nom » s'y retrouve (et part vers CrowdSec) | écrire le nom seulement s'il a la forme d'un nom valide, sinon « (invalide) » |
| Faible (**fait**, § 18.29) | Durée du verrou fixe (15 min) : un attaquant patient recommence sans fin | durée doublée à chaque nouveau verrou du même compte (plafond 24 h) |
| Faible (**fait**, § 18.29) | Sessions partielles sans limite par compte (avec le bon mot de passe) | 3 sessions partielles au plus par compte, la plus ancienne est fermée |
| Faible (**accepté**) | Redémarrage = verrous oubliés | acceptable (CrowdSec garde ses bannissements) ; à noter |
| Info (**fait autrement**, § 18.29) | Pas de liste de mots de passe trop courants | petite liste embarquée (~10 Ko) ou rien : 12 caractères + 2FA suffisent |
| Info (**gardé**) | `Sec-Fetch-Site: none` accepté pour une écriture | garder (jeton CSRF + `SameSite=Strict` couvrent) |

Vérifié, rien à redire : cookie `__Host-`, nouvel identifiant à chaque
étape, empreinte SHA-256 de l'identifiant, Argon2id + leurre, anti-rejeu
TOTP, CSRF, CSP sans script en ligne, corps limité à 1 Mio, adresse du
client prise seulement derrière le proxy déclaré, nom d'utilisateur sensible
à la casse (pas de contournement du verrou par « Admin »).

Propositions d'architecture (état au 16/11) :

| Sujet | Constat | Proposition | Coût |
|---|---|---|---|
| Paquet `console` (**fait**, § 18.29) | 10 500 lignes, 30 % du code : pages, comptes, sessions, verrous, tableaux mélangés | sortir l'authentification dans `internal/server/auth` (comptes, mot de passe, TOTP, sessions, verrous) : plus facile à relire et à tester seul | aucun à l'exécution |
| Réseau Docker `obs` (**reporté à l'étape 7**) | partagé avec l'ancienne stack : un autre conteneur joint mirador sans nginx | réseau dédié `mirador` (nginx de l'hôte + Olvid seulement) à la bascule (étape 7) | aucun |
| Écoutes (**fait**, § 18.29) | `0.0.0.0` dans le conteneur, protection = pas de port publié | garder, mais refuser au démarrage une écoute hors boucle locale sans `MIRADOR_PROXY` ni TLS | aucun |
| Dépendances (**fait en partie**, § 18.29) | AWS SDK, SFTP, Prometheus, gRPC dans un seul binaire : grosse surface | `govulncheck` déjà en CI ; ajouter Dependabot (ou Renovate) et figer les actions par empreinte | aucun |
| Démarrage (**fait**, § 18.29) | `Demarrer` ferme à la main à chaque erreur (`errors.Join(…Close())` répété 8 fois) | une liste de « fermetures » dépilée en cas d'erreur | aucun |
| Réglages (**écarté après mesure**, § 18.29) | relus en base toutes les 5 s par la console (5 requêtes SQLite par tour) | un numéro de version des réglages, relu seul ; le reste seulement s'il change | moins d'I/O |
| Sessions (**gardé**) | en mémoire : un redémarrage (mise à jour) déconnecte tout le monde | garder (plus sûr, zéro écriture disque) | — |

### 18.28 Messages de sécurité et nom dans le journal **[Fait, validé le 11/11]**

Deux propositions du § 18.27 retenues (« oui » du 11/11).

Messages envoyés à l'administrateur par les canaux de notification :

| Événement | Texte | Pourquoi |
|---|---|---|
| Code 2FA faux après un mot de passe juste | compte, adresse | le mot de passe est sans doute connu de quelqu'un d'autre |
| Compte verrouillé | compte, dernière adresse | attaque en cours sur ce compte |
| Connexion réussie depuis une nouvelle adresse | compte, adresse | session ouverte ailleurs que d'habitude |

- Chemin des alertes critiques : canal principal, secours si panne, file
  d'attente si tout est en panne. **Mode ombre** : seulement noté dans
  l'historique des envois (type `securite`).
- Envoi à part : la page de connexion n'attend pas le canal.
- **Un message par heure et par sujet** au plus (table bornée à 1 000) : un
  attaquant ne peut pas noyer l'administrateur.
- Compte verrouillé : message seulement si le compte existe.
- Adresses connues gardées en mémoire (30 jours) : la première connexion
  après un redémarrage du serveur prévient aussi.
- Jamais de mot de passe ni de code dans le texte (testé).
- Coût : rien au repos ; un envoi par événement.

Nom dans le journal : un texte saisi dans le champ « Nom » qui n'a pas la
forme d'un nom de compte est écrit `(invalide)`. Un mot de passe tapé au
mauvais endroit ne va plus dans le journal ni vers CrowdSec. La forme de la
ligne ne change pas (`deploy/crowdsec/` inchangé).

| Menace | Parade |
|---|---|
| Inconnu qui déclenche des messages en rafale | 1 message / heure / sujet ; rien pour un compte inconnu |
| Durée de réponse qui trahit l'envoi | envoi hors de la requête |
| Secret dans un message ou dans le journal | texte fixe + nom du compte + adresse seulement ; nom filtré |

### 18.29 Reste des propositions du § 18.27 **[Fait, « fait le reste » du 12/11]**

Sécurité de la connexion :

| Sujet | Décision | Coût |
|---|---|---|
| Verrou progressif | chaque nouveau verrou de la même adresse ou du même compte dure 2 fois plus (15 min, 30 min, 1 h… **24 h au plus**) ; série oubliée après 24 h sans verrou, ou après une connexion réussie (compte). La durée réelle est dans le journal et dans le message à l'admin | un entier par compteur |
| Sessions partielles | **3** sessions « avant le code » au plus par compte ; la plus ancienne est fermée | aucun |
| Mots de passe trop simples | refusés : un caractère ou un motif court répété, une suite (`123456789012`, `abcdef…`), le nom du compte ou « mirador » avec moins de 8 autres caractères utiles. **Pas de liste embarquée** (rien à maintenir, 0 octet) ; 12 caractères + 2FA font le reste | aucun |
| Verrous oubliés au redémarrage | accepté : rien sur disque ; CrowdSec garde ses bannissements | — |
| `Sec-Fetch-Site: none` | gardé : jeton CSRF + `SameSite=Strict` couvrent | — |

Adresse de confiance (connexion réussie < 30 j) : toujours exemptée du
verrou du **compte**, même long. Sinon : `utilisateur-secours`.

Architecture :

| Sujet | Décision | Coût |
|---|---|---|
| Paquet `internal/server/auth` | comptes, mots de passe, TOTP, sessions, verrous (1 250 lignes) sortis de `console` (10 500 → 9 760 lignes). `auth` ne connaît pas `console` ; ses tests unitaires l'ont suivi. Aucun changement de comportement. `Auditer` reste dans `console` ; `auth` écrit dans la même table (`auditer`, à changer ensemble) | aucun à l'exécution |
| Écoute ouverte | refus au démarrage si une écoute sort de la machine sans `MIRADOR_PROXY` ni TLS (interface en clair, verrous communs à tous). `127.0.0.1`, `::1`, `localhost` : acceptés sans rien | aucun |
| Démarrage | une liste de fermetures dépilée en cas d'échec (10 fermetures recopiées → 1) | aucun |
| Dependabot | modules Go, actions GitHub, images Docker ; **une fois par mois, regroupé** (5 pull requests au plus). Coût : ~7 min de CI par pull request (§ 18.24) ; supprimer `.github/dependabot.yml` pour l'arrêter | minutes de CI |
| Actions figées par empreinte | **pas fait** : il faut les empreintes exactes de chaque action, que je ne peux pas vérifier d'ici. À faire à la main (`uses: actions/checkout@<sha> # v4`) | — |
| Réglages relus toutes les 5 s | **écarté après mesure** : un passage = 52 µs, 18 Ko alloués, aucune écriture disque, soit 0,001 % d'un cœur. Un numéro de version ajouterait du code pour un gain non mesurable | — |
| Réseau Docker dédié | **reporté à l'étape 7** : tant que la comparaison avec vmalert tourne, mirador doit joindre l'ancienne stack sur `obs` | — |

Menaces (ajouts) :

| Menace | Parade |
|---|---|
| Attaquant patient qui revient après chaque verrou | verrou doublé jusqu'à 24 h ; message à l'admin (§ 18.28) |
| Mot de passe connu : centaines de sessions partielles pour essayer des codes | 3 par compte ; essais réservés (§ 18.27) |
| Serveur exposé par erreur sans nginx | refus de démarrer |
| Dépendance avec une faille connue | `govulncheck` en CI + Dependabot |

---

## 19. Idées reprises de Beszel : surveillance par machine et vue d'ensemble **[Validé le 16/11 — demande du 12/11]**

- Source : `github.com/henrygd/beszel` (Go, licence MIT), documentation lue
  le 12/11. **Aucun code repris** : seulement des idées, réécrites ici.
- Branche : `claude/laughing-sagan-7w7xrm`. Maquette validée le 12/11
  (deux écrans : vue d'ensemble, page machine).

### 19.1 Ce que je retiens, ce que j'écarte

| Idée Beszel | Dans mirador | Pourquoi |
|---|---|---|
| Barres colorées, tri, filtre, choix des colonnes dans la liste des machines | vue d'ensemble (§ 19.5) | on voit d'un coup d'œil la machine qui va mal |
| Bandeau d'identité sous le nom de la machine | page machine | plus court que le tableau « Identité » (gardé, replié) |
| Vitesse des ventilateurs (`/sys/class/hwmon`) | collecteur « matériel », éteint par défaut | panne matérielle non vue aujourd'hui |
| Santé des pools ZFS | idem, lu dans `/proc/spl/kstat/zfs/*/state` | pas de commande `zpool`, pas de root |
| État du RAID logiciel (mdraid) par sysfs | idem, `/sys/block/md*/md/degraded` | idem |
| Sondes ajoutées dans l'interface, par machine | devient « ce que la machine envoie se choisit dans l'interface », avec détection | demande de l'utilisateur |

| Écarté | Raison |
|---|---|
| Mode SSH (le hub se connecte à l'agent, port 45876) | l'agent n'ouvre jamais de port |
| Mise à jour de l'agent depuis GitHub | mirador : versions signées Ed25519, clé hors serveur |
| PocketBase, Shoutrrr | déjà écartés (§ 12.2) |
| Socket Docker monté, capacités `SYS_RAWIO` / `SYS_ADMIN` pour SMART | proxy en lecture seule et assistant root gardés |
| OAuth / OIDC, partage entre utilisateurs | surface d'attaque en plus, un seul admin |
| GPU, batterie, Wi-Fi, Podman, Windows | absents du parc |
| Sondes de port (TCP, HTTP, ping, DNS) | **refusé par l'utilisateur le 12/11** |
| CPU et mémoire par service systemd | pas demandé ; beaucoup de séries, à mesurer d'abord |
| Scrub ZFS, usage par dataset | demande la commande `zpool` (root) ; seule la santé est lue |

### 19.2 Principe **[Décidé, réponses du 12/11]**

- Ce qui était collecté avant **reste actif** sans rien valider.
- Tout ce qui est surveillé **peut être retiré**, machine par machine.
- Les **nouveautés** (ventilateurs, ZFS, RAID logiciel, et les services si on
  le demande) ne sont pas surveillées sans validation : l'agent dit seulement
  « ça existe », l'interface propose.
- Services systemd proposés : seulement ceux démarrés au boot et qui
  devraient tourner (mêmes exclusions qu'avant, § 4.2), sans les services de
  base (`systemd-*`, `getty@`, `user@`, `dbus*`).

### 19.3 Agent

| Réglage (local `MIRADOR_…` et distant) | Défaut | Effet |
|---|---|---|
| `temperatures` | oui | capteurs de `/sys` |
| `docker_metriques` | oui | métriques des conteneurs (les logs ont leur propre réglage) |
| `envoyer_pare_feu`, `envoyer_smart`, `envoyer_certificats` | oui | résultats de l'assistant root envoyés ou non |
| `ventilateurs`, `zfs`, `mdraid` | **non** | nouveaux collecteurs |
| `systemd_validation` + `systemd_surveiller` | non | seuls les services de la liste sont suivis |

- Collecteur « matériel » : lecture directe de `/sys` et `/proc`, **aucune
  commande, aucun droit en plus**. Rythme lent (60 s) ; liste des éléments
  relue toutes les 10 min.
- Métriques (noms de node_exporter quand ils existent) :
  `node_hwmon_fan_rpm{chip,sensor}`, `node_zfs_zpool_state{zpool,state}`
  (toujours `state="online"` à 0 ou 1, plus l'état courant s'il diffère),
  `node_md_degraded{device}` (disques manquants), `node_md_disks_required{device}`.
- Détection : un genre éteint n'envoie qu'**une série par élément trouvé**,
  `mirador_agent_detecte{genre,nom}` (genres : `ventilateurs`, `zfs`,
  `mdraid`, `service`). Pas de nouveau point d'entrée, pas de table en plus.
- **Limite connue** : `envoyer_smart = non` (et pare-feu, certificats) arrête
  l'**envoi**, pas l'assistant root : il ne lit jamais la configuration
  distante (§ 6.4), il continue donc de lancer `smartctl` toutes les 30 min.
  Pour l'arrêter vraiment : `MIRADOR_SMART=non` dans le fichier local.
- À vérifier sur une vraie machine (rien de tel ici) : le fichier
  `/proc/spl/kstat/zfs/<pool>/state` sur les Proxmox (OpenZFS ≥ 0.8), les
  capteurs `fan*_input`, une grappe `md`.

### 19.4 Serveur et compatibilité

- Les choix sont des **clés de la configuration distante** (valeurs typées,
  mêmes contrôles et même audit que « Configuration des agents »), à la
  portée de la machine ou du groupe. Les services sont toujours réglés par
  machine.
- **Ancien agent** : il refuserait tout le document à cause d'une clé
  inconnue. L'agent annonce donc ce qu'il sait faire (en-tête
  `X-Mirador-Capacites: surveillance`, ignoré par un ancien serveur) ; sans
  cet en-tête, le serveur **retire** les clés nouvelles du document (ETag
  recalculé). La capacité est gardée sur la fiche (migration 0011) et
  l'interface bloque les réglages nouveaux pour un agent trop ancien.
- État affiché : **aucun stockage en plus**. Il se déduit d'une requête sur
  les dernières métriques de la machine (en cache 60 s) et de sa
  configuration effective.
- La page « Configuration des agents » garde ces clés quand on enregistre
  une portée (« retirer la portée » les enlève aussi).
- Trois règles fournies, actives par défaut, sans effet là où rien n'est
  validé : `PoolZfsDegrade` (critique, 2 min), `RaidLogicielDegrade`
  (critique, 2 min), `VentilateurArrete` (avertissement, 10 min, seulement si
  le ventilateur a tourné dans les 24 h). Un service validé qui s'arrête est
  couvert par `ServiceAutomatiqueArrete` (règle 11).

### 19.5 Interface

- **Page machine**, bloc « À surveiller sur cette machine » : « Détecté, à
  valider » (Surveiller / Ignorer / Plus tard), « Surveillé » (Garder /
  Retirer, avec un point rouge si quelque chose ne va pas), « Ignoré ou
  absent » (Rétablir). Boutons « Tout surveiller » et « Tout ignorer ». Choix
  pour la machine ou pour tout son groupe.
- **Un seul formulaire, un seul code 2FA** pour tous les choix : changer la
  configuration d'un agent est une action sensible (revue du 11/11). Écart
  avec la maquette, qui montrait un bouton par ligne.
- Services : repliés ; option « Nouveaux services : me les proposer avant
  de les surveiller » (au départ, ceux suivis aujourd'hui restent suivis).
- **Vue d'ensemble** : barres CPU / mémoire / disque (vert < 70 %, orange
  70 à 85 %, rouge au-dessus, chiffre toujours affiché), tri par colonne
  (3 clics : décroissant, croissant, ordre par gravité), filtre, colonnes
  masquables, cloche avec le nombre d'alertes, colonne « À valider ». Tri et
  colonnes retenus dans le navigateur ; le tableau n'est pas rechargé
  pendant une saisie.
- **Ajouté ensuite** (réponse du 12/11, « maintenant ») : recherche rapide,
  sous-titres, filtre des courbes, empreinte de la machine (§ 19.8).
- **Pas fait** : bouton 1 / 2 colonnes. Écart : la colonne « Réseau » de la
  maquette n'y est pas (une requête de plus par minute pour un chiffre peu
  utile).

### 19.8 Compléments du 12/11

- **Recherche rapide** : Ctrl+K (ou bouton « Aller à… » du menu) ouvre une
  liste des pages, des machines et des tableaux ; flèches et Entrée. La
  liste est lue une fois à l'ouverture (`/donnees/aller`), le filtrage se
  fait dans le navigateur : aucune requête pendant la frappe.
- **Sous-titres** : une ligne sous le titre des 18 graphiques fournis qui
  n'en avaient pas (« Serveurs Linux » et « Proxmox »). Un tableau déjà
  installé ou modifié garde son texte : « Rétablir » pour prendre le nouveau.
- **Filtre des courbes** : à partir de 6 courbes (conteneurs, machines…),
  un champ au-dessus du graphique ne garde que celles dont le nom contient
  le texte tapé. Rien n'est redemandé au serveur.
- **Empreinte de la machine** (idée de Beszel) :
  - l'agent envoie `X-Mirador-Empreinte` : SHA-256 de `/etc/machine-id`
    (32 chiffres hexadécimaux ; l'identifiant lui-même ne sort pas), et
    seulement vers mirador-server ;
  - le serveur garde la dernière empreinte reçue (écrite avec « vu le »,
    une fois par minute : pas d'écriture en plus) ;
  - **option par machine**, éteinte par défaut : « Lier le token à cette
    machine » (code 2FA). Lié, le token est refusé (403) s'il vient d'une
    autre machine ou sans empreinte ;
  - **limite** : cela protège d'un token qui fuit seul (sauvegarde, journal,
    copie d'un fichier). Qui lit le token sur la machine peut aussi lire
    `/etc/machine-id` ;
  - **à savoir** : délier avant de réinstaller ou de cloner la machine, et
    avant de revenir à un agent plus ancien, sinon elle est refusée. Un
    retour arrière automatique après une mise à jour ratée vers une version
    sans empreinte serait bloqué de la même façon : lier seulement quand
    toutes les versions en service envoient l'empreinte ;
  - migration 0013.

### 19.6 Coût mesuré (machine de développement, 4 cœurs)

| Mesure | Résultat | Objectif |
|---|---|---|
| Agent, collecteur « matériel », détection seule | 0,13 µs par passage, 0 allocation | < 1 % d'un cœur ✅ |
| Agent, tout activé (2 ventilateurs, 1 pool, 1 grappe) | 21 µs par passage (toutes les 60 s), 20 allocations (ouverture des fichiers) | idem ✅ |
| Agent, RAM | pas de tampon en plus ; non mesurée sur une vraie machine | < 30 Mio, à confirmer |
| Séries en plus par machine | 1 par élément détecté ; 1 par ventilateur, 1 à 2 par pool, 2 par grappe une fois validés | — |
| Serveur, classement d'une machine | 4,2 µs | — |
| Serveur, page machine / fragment de la vue d'ensemble (5 machines, démonstration) | 3 à 5 ms | — |
| Serveur, écritures disque | aucune hors enregistrement d'un choix | ✅ |

Non mesuré : RAM du serveur sous charge (50 agents). Rien n'est gardé en
mémoire en plus du cache de requêtes existant (deux entrées de plus par
page machine ouverte, une pour la vue d'ensemble) ; à confirmer au prochain
test de charge.

### 19.7 Modèle de menaces

| Menace | Parade |
|---|---|
| Session volée qui éteint la surveillance d'une machine | code 2FA à chaque enregistrement ; ligne d'audit ; un code ne sert qu'une fois |
| Serveur compromis qui pousse une configuration malveillante | inchangé : que des booléens et des noms de services validés (`….service`, 200 au plus) ; jamais de commande ni de chemin |
| Machine compromise qui ment sur ce qu'elle détecte | elle ne peut que proposer ; rien n'est activé sans l'admin ; noms limités à des caractères sûrs, 64 éléments et 100 services au plus |
| Nom de service ou de pool piégé (XSS) | échappé par `html/template` ; formats vérifiés à l'entrée |
| En-tête de capacité falsifié | il ne donne aucun droit : il change seulement les clés que l'agent reçoit pour lui-même |
| Ancien agent cassé par une clé inconnue | clés nouvelles retirées pour lui (§ 19.4), testé |

---

## 20. Codes de secours de la 2FA et passkeys **[Validé le 16/11 — demande du 12/11]**

Même branche que le § 19 (`claude/laughing-sagan-7w7xrm`).

### 20.1 Codes de secours

- **10 codes** par compte, forme `XXXX-XXXX-XXXX` (12 signes sans 0, O, 1,
  I : 60 bits d'aléa). Créés dans « Mon compte » avec le code 2FA, **affichés
  une seule fois**. En créer de nouveaux annule les anciens.
- En base : seulement l'empreinte SHA-256 (un code a assez d'aléa pour se
  passer d'Argon2id : pas de calcul lourd à la connexion).
- Un code **remplace le code 2FA une seule fois** :
  - à la connexion (lien « Téléphone perdu ? ») ;
  - pour préparer une nouvelle 2FA dans « Mon compte » (sinon un téléphone
    perdu enfermerait dehors).
- Il ne sert **pas** aux autres actions sensibles : elles gardent le code de
  l'application.
- Un code faux compte comme un code 2FA faux (verrous, CrowdSec, 5 essais).
- Chaque usage : ligne d'audit, ligne de journal, et message à
  l'administrateur avec le nombre de codes restants.
- La remise à zéro de la 2FA par un administrateur, ou la commande
  `utilisateur-secours`, efface aussi les codes de secours et les passkeys
  (compte repris en main).

### 20.2 Passkeys (WebAuthn)

- **Deux usages** (réponse du 12/11 : « je veux les 2 ») :
  1. après le mot de passe, la passkey **remplace le code 2FA** ;
  2. **passkey seule**, sans mot de passe : bouton « Se connecter avec une
     passkey » sur la page de connexion. La clé dit de quel compte il
     s'agit, et l'appareil **doit vérifier l'utilisateur** (code PIN,
     empreinte, visage) : cela fait deux facteurs. Une clé qui ne fait que
     « toucher » est refusée ici (elle reste valable après le mot de passe).
- La passkey seule demande une clé « découvrable » (téléphone, ordinateur,
  gestionnaire de mots de passe, clé USB récente). Réglage « Sécurité » pour
  l'interdire ; autorisée par défaut.
- Défi de la passkey seule : demandé au clic, gardé en mémoire (2 000 au
  plus, 5 min, usage unique) puisqu'il n'y a pas encore de session.
- La 2FA par application reste obligatoire et reste demandée pour les
  actions sensibles.
- Création dans « Mon compte » : nom + code 2FA, puis le navigateur crée la
  clé. 10 passkeys au plus par compte. Suppression avec le code 2FA.
- **Aucune dépendance ajoutée** : vérification écrite ici (~450 lignes avec
  le lecteur CBOR), avec la bibliothèque standard de Go. Une bibliothèque
  WebAuthn aurait amené une dizaine de modules pour des fonctions dont on ne
  se sert pas (attestation des fabricants, TPM).
- Algorithmes acceptés : ES256 (P-256), Ed25519, RS256 (Windows Hello).
- Ce qui est vérifié : défi aléatoire à usage unique (5 min, dans la
  session), type d'opération, **origine** (anti-hameçonnage), domaine,
  présence de l'utilisateur, signature, compteur de signatures s'il est
  utilisé. Attestation du fabricant non demandée (« none »).
- Le serveur ne garde que la clé publique, le domaine et un compteur.
- **Domaine** : celui sous lequel l'interface est ouverte (en-tête `Host`,
  transmis par nginx). Une passkey ne marche que sous le domaine où elle a
  été créée. Sous une adresse IP, les passkeys sont refusées (les
  navigateurs l'imposent) et la page le dit. HTTPS obligatoire, sauf
  `localhost`.
- Script : `passkey.js` (3,9 Ko, sans dépendance, CSP inchangée).

### 20.3 Coût

| Mesure | Résultat |
|---|---|
| Connexion par passkey (vérification ES256) | 0,09 ms, 1,7 Ko alloués (machine de développement) |
| Code de secours | une empreinte SHA-256 + une écriture |
| RAM | un défi de 43 caractères par session en cours ; rien d'autre |
| Disque | 2 tables (migration 0012) ; une écriture par usage |
| Binaire | pas de dépendance en plus |

### 20.4 Essais

- Tests unitaires : les 3 algorithmes, défi faux ou rejoué, autre origine,
  autre domaine, autre clé, signature tronquée, compteur en recul, entrées
  aléatoires (fuzzing du lecteur CBOR et des vérifications : 400 000
  essais sans plantage).
- Bout en bout : création, connexion, suppression avec un faux
  authentificateur ; codes de secours (une fois, pas deux, pas pour un
  autre compte).
- **Vrai navigateur** (Chromium, authentificateur virtuel, sur
  `localhost`) : création d'une passkey, connexion par passkey, connexion
  par code de secours.
- **Pas essayé** : un vrai téléphone, une vraie clé USB, Firefox, Safari,
  Windows Hello (RS256 n'est testé qu'avec le faux authentificateur), et
  derrière nginx.

### 20.5 Modèle de menaces

| Menace | Parade |
|---|---|
| Fausse page de connexion (hameçonnage) | la passkey est liée au domaine par le navigateur ; le serveur revérifie l'origine et le domaine |
| Réponse rejouée | défi aléatoire, effacé dès le premier essai, valable 5 min |
| Passkey ajoutée par une session volée | code 2FA demandé pour préparer la création ; audit |
| Codes de secours volés en base | empreintes seulement ; 60 bits d'aléa |
| Essais de codes de secours en rafale | mêmes verrous que le code 2FA (adresse, compte, 5 essais par session) |
| Même code utilisé deux fois en même temps | mise à jour conditionnelle (`utilise_le = 0`) |
| Donnée piégée envoyée par le navigateur | lecteur CBOR borné (profondeur 6, 64 éléments, tailles limitées), fuzzing |
| En-tête `Host` falsifié | il ne change que le domaine attendu pour sa propre requête ; une clé créée ailleurs reste refusée |
| Passkey seule : appareil volé et déverrouillé | vérification de l'utilisateur exigée à chaque connexion ; supprimer la passkey (ou remise à zéro par un admin) |
| Passkey seule : remplissage de la mémoire par des demandes de défi | 2 000 défis au plus, 5 min ; refusé pour une adresse verrouillée |
| Téléphone volé avec la 2FA | l'administrateur remet la 2FA à zéro : codes et passkeys effacés |
| Mot de passe connu de l'attaquant | la passkey ou le code reste nécessaire ; message à l'administrateur à chaque essai faux |


---

### 20.9 Passkey sur téléphone (correction du 14/11)

Retour : « la passkey ne marche pas sur mobile ». Cause la plus probable,
corrigée : en connexion **sans mot de passe**, le script demandait le défi
au serveur **puis** la passkey ; Safari (iPhone, iPad) refuse une demande
WebAuthn faite après une attente réseau (le clic n'est plus « récent »).
Le défi est maintenant préparé dès l'affichage de la page de connexion
(renouvelé toutes les 4 min, valable 5 min, à usage unique) : au clic, la
passkey est demandée tout de suite. Le message d'erreur affiche aussi le
nom technique de l'erreur (`NotAllowedError`…) pour diagnostiquer un autre
cas. Côté serveur, rien à changer : compteur à 0 des passkeys
synchronisées (iCloud, Google) accepté, formats d'attestation ignorés
(« none » demandé), origine vérifiée. Vérifié dans Chromium avec un
authentificateur virtuel (création puis connexion sans mot de passe).

## 21. Élément par élément, signal « à valider », réglages rangés **[Validé le 16/11 — demandes du 12/11]**

Branche : `claude/affectionate-albattani-izl0lb`. Demandes :

- gérer ce qui est surveillé **dans** « Système » et « Certificats » ;
- réponse « tout gérer depuis l'interface » (y compris les chemins de
  certificats, au prix d'un écart avec la revue du 11/11, voir § 21.3) ;
- une info visible quand une machine a quelque chose de nouveau à valider ;
- un menu Réglages cohérent ;
- tri et filtre sur « État » et « À valider » dans la vue d'ensemble.

### 21.1 Agent

| Réglage (distant, et local `MIRADOR_…`) | Effet |
|---|---|
| `ignorer_disques` / `MIRADOR_IGNORER_DISQUES` | disques retirés (noms exacts, 100 au plus) |
| `ignorer_interfaces` / `MIRADOR_IGNORER_INTERFACES` | interfaces retirées (100 au plus) |
| `ignorer_montages` / `MIRADOR_IGNORER_MONTAGES` | points de montage retirés (chemins exacts, 100 au plus) |
| `certificats_ignorer` | certificats lus mais pas envoyés (chemins exacts, 200 au plus) |
| `certificats_ajout` | chemins en plus de `MIRADOR_CERTIFICATS`, lus par l'assistant root (20 au plus, règles du § 21.3) |
| `MIRADOR_CERTIFICATS_DISTANTS` (local seulement, défaut oui) | non = l'assistant root ignore `certificats_ajout` |

- Les listes s'ajoutent aux expressions `MIRADOR_EXCLURE_*` (même filtre,
  une table de noms en plus : pas de coût mesurable).
- `ssl_certs_found_total` compte les certificats **envoyés** (les retirés
  n'y sont plus).
- Nouvelle capacité annoncée : `elements` (en-tête `X-Mirador-Capacites:
  surveillance,elements`). Le serveur retire ces clés pour un agent qui ne
  l'annonce pas (0.2.12 compris) : il refuserait sinon tout le document.

### 21.2 Interface

- Page machine, bloc « À surveiller » :
  - « Système : disques, interfaces, montages » (replié) : un tableau,
    Garder / Retirer / Rétablir par ligne ;
  - « Certificats » (replié, visible même si aucun certificat n'est encore
    trouvé) : fichier, nom, émetteur, jours restants (rouge < 7, orange
    < 21), Garder / Retirer ; chemins ajoutés (Retirer) ; champ « Ajouter
    des chemins » ;
  - même formulaire et **même code 2FA** que le reste du bloc ; toujours à
    la portée de la machine ;
  - rien de stocké en plus : le détail vient des dernières métriques (4
    noms de métriques ajoutés à la requête du bloc, une série par élément)
    et de la configuration.
- Signal « à valider » : compteur **orange** sur « Machines » dans le menu
  (nombre de machines concernées, sur toutes les pages), colonne dans la
  liste des machines, pastille à côté du nom de la machine. Le calcul est
  gardé 60 s (oublié dès qu'un choix est enregistré).
- Vue d'ensemble : tri sur « État » (par gravité), « À valider » et « Vu » ;
  liste « Afficher » (toutes, à regarder, critiques, muettes,
  avertissement, en ligne, à valider), retenue dans le navigateur.
- Réglages : **une seule liste** en Go sert aux onglets et aux cartes
  (mêmes noms, même ordre). Onglets : Notifications, Olvid (si activé),
  Conservation des données, Sauvegardes, Sites, types et groupes,
  Interface, Export / import (devenu une page). Les réglages rangés
  ailleurs sont rappelés sous « Ailleurs dans mirador » avec leur place :
  Configuration et Mises à jour des agents (Machines, avec les onglets de
  Machines), Silences et maintenances (Alertes), Utilisateurs, Sécurité,
  Journal d'audit.

### 21.3 Chemins de certificats venus du serveur : écart assumé **[Décidé, validé par l'utilisateur le 12/11]**

La revue du 11/11 avait décidé : « root ne lit jamais la configuration
distante » (le fichier gardé appartient à l'agent, sans root). Choix de
l'utilisateur le 12/11 : tout gérer depuis l'interface. L'assistant root lit
donc **une seule clé** de ce fichier, avec ces règles :

| Contrôle | Où |
|---|---|
| Fichier ouvert sans suivre de lien (`O_NOFOLLOW`), sans bloquer, fichier ordinaire, 256 Kio au plus ; message d'erreur sans contenu du fichier | assistant root |
| Chemin absolu, sans `..`, `//`, `/./`, ni caractère de contrôle | agent à la réception, serveur à l'enregistrement, assistant root avant lecture |
| Dossiers autorisés : `/etc/`, `/opt/`, `/srv/`, `/var/lib/`, `/usr/local/`, `/usr/share/ca-certificates/` (ajout du 12/11, § 22.4) (pas `/proc`, `/sys`, `/dev`, `/run`, `/tmp`, `/home`, `/root`) | idem |
| Nom en `.pem`, `.crt`, `.cer` ou `.cert` (écrit en toutes lettres) | idem |
| Aucun nom contenant « key » ou « priv » (clé privée), ni dans le motif, ni dans le fichier trouvé, ni dans sa cible réelle | idem + assistant root |
| Cible réelle (liens suivis) dans un dossier autorisé | assistant root |
| 3 jokers au plus, 20 chemins, 100 fichiers examinés par chemin | idem |
| Fichier ouvert sans bloquer (tube, périphérique), fichier ordinaire, 1 Mio au plus | assistant root (aussi pour les chemins locaux) |
| Sortie : chemin, nom (CN), émetteur, dates : rien d'autre du fichier | inchangé |
| Veto local : `MIRADOR_CERTIFICATS_DISTANTS=non` | fichier local, que le serveur ne peut pas changer |

Ce qui reste possible pour un attaquant qui tient le serveur ou le compte
de l'agent : faire lire à root un fichier `*.pem` / `*.crt` d'un dossier
autorisé, et savoir s'il contient un certificat (et lequel). Un certificat
n'est pas un secret. Il ne peut ni lire une clé privée nommée comme telle,
ni `/etc/shadow`, ni bloquer l'assistant.

### 21.4 Coût mesuré (machine de développement, 4 cœurs)

| Mesure | Résultat | Objectif |
|---|---|---|
| Agent, disques + réseau, sans / avec 4 noms retirés | 8,8 / 7,7 µs par passage, 6 allocations (identique) | < 1 % d'un cœur ✅ |
| Assistant root, 20 certificats, chemin local / distant | 0,22 / 0,31 ms toutes les 15 min | ✅ |
| Serveur, classement du bloc d'une machine | 5,4 µs (4,2 avant) | — |
| Serveur, compteur du menu | 1 requête + 1 lecture SQLite par machine ayant une détection, au plus une fois par minute | — |
| Séries en plus | aucune côté agent ; la requête du bloc lit des séries déjà envoyées | ✅ |
| Écritures disque | seulement à l'enregistrement d'un choix | ✅ |

### 21.5 Modèle de menaces

| Menace | Parade |
|---|---|
| Serveur compromis qui fait lire un fichier sensible à root | règles du § 21.3 ; veto local |
| Compte de l'agent compromis qui modifie le fichier gardé | même chose : root revérifie chaque chemin ; lien symbolique refusé |
| Session volée qui retire un disque ou un certificat (alerte masquée) | code 2FA à chaque enregistrement ; ligne d'audit |
| Nom piégé (XSS) dans un disque, un montage, un CN | échappé par `html/template` ; noms vérifiés à l'entrée |
| Ancien agent cassé par une clé inconnue | clés retirées pour lui selon ses capacités (testé avec 0.2.12) |
| Formulaire géant | 600 éléments au plus par envoi, 20 chemins ajoutés |

## 22. Retours du 12/11 : temps réel, paquets, certificats, filtres **[Validé le 16/11]**

Retours après l'essai de la partie 5 quater (§ 21) et réponses du 12/11
(« configuration en temps réel : oui », « logs 1 à 2 s », « métriques 30 s
réglables par machine », « paquets : vue d'ensemble, en-tête, alerte »).

### 22.1 Configuration en temps réel

| Élément | Choix | Raison / coût |
|---|---|---|
| Principe | l'agent garde sa demande de configuration ouverte (`X-Mirador-Attendre: 40`) ; le serveur répond dès que la configuration de la machine change, sur « Actualiser », ou au bout de 45 s au plus | l'agent n'ouvre toujours aucun port ; changements appliqués en 1 à 2 s au lieu de 5 min |
| Serveur | signal en mémoire réveillé par `DefinirConfig` (et donc `ModifierConfig`) ; la place de décodage de l'ingestion est rendue pendant l'attente ; 45 s < délai d'écriture (60 s) du serveur et de nginx | une goroutine et une connexion TLS par machine (quelques dizaines de Ko) ; aucune requête en base pendant l'attente |
| Compatibilité | le serveur l'annonce (`X-Mirador-Attente: 1`) ; sans cette réponse (ancien serveur), l'agent garde le rythme de 5 min | pas de boucle rapide contre un ancien serveur |
| Agent | une demande au plus toutes les 2 s | protège le serveur si une réponse revient tout de suite |
| Commande en ligne (`mirador-server config`, autre processus) | pas de signal : appliquée à la fin de l'attente en cours (45 s au plus) | — |

Un changement de configuration fait toujours redémarrer l'agent par systemd
(choix de l'utilisateur du 12/11 : pas de relance « sur place ») ; plusieurs
choix enregistrés d'affilée peuvent donc déclencher une règle de type
« service qui redémarre en boucle ».

### 22.2 « Actualiser » et assistant root

| Élément | Choix |
|---|---|
| Bouton « Actualiser » (fiche machine, bloc « À surveiller », admin) | rien n'est modifié (pas de code 2FA) ; numéro de demande en mémoire, réveille l'agent en attente |
| Agent | à la demande, touche `/var/lib/mirador-agent/actualiser-racine` (une fois par minute au plus) |
| Script d'installation | unité `mirador-agent-root.path` (`PathModified`, `TriggerLimitIntervalSec=60`, `TriggerLimitBurst=3`) qui lance `mirador-agent-root.service` ; à poser en relançant le script une fois |
| Menace | un agent compromis peut seulement déclencher des passages de l'assistant (même programme, mêmes règles) ; systemd coupe au-delà de 3 par minute, le minuteur de 15 min reste |
| Version de l'assistant | l'agent envoie `mirador_agent_root_info{version}` ; la fiche signale un assistant plus ancien que l'agent, avec la commande de réinstallation (l'assistant n'est jamais mis à jour automatiquement) |
| Chemins de certificats ajoutés | compte rendu par chemin (`mirador_agent_certificats_chemin_fichiers` / `_lus`) : lu, aucun fichier, aucun certificat lisible, pas encore lu, assistant trop ancien |

### 22.3 Paquets, logs, métriques, filtres

- Paquets (`apt_upgrades_pending`, `apt_upgrades_security_pending`,
  `node_reboot_required`) : colonne « Mises à jour » de la vue d'ensemble
  (tri, filtre « Mises à jour en attente »), en-tête de la fiche machine,
  règle mirador `MisesAJourSecuriteEnAttente` (sécurité en attente depuis
  3 jours, avertissement).
- Logs : lots toutes les 2 s au lieu de 5 (un lot vide n'envoie rien).
- Métriques : 30 s par défaut ; intervalle affiché dans la fiche avec un
  lien vers son réglage par machine (10 s à 5 min).
- « Alertes en cours » : tri par colonne et filtres gravité + texte.
- Service ignoré : retiré tout de suite de « à valider ».
- Réglages : barre d'onglets retirée de la page d'accueil des réglages.
- Code 2FA des actions sensibles : nom d'utilisateur caché pour le
  remplissage par Vaultwarden.

### 22.4 Chemins de certificats : raison précise, certificats d'autorités **[Fait, demande du 12/11]**

Constat : `/etc/ssl/certs/GlobalSign_ECC_Root_CA_-_R4.pem` ajouté dans
l'interface donnait « 1 fichier trouvé, aucun certificat lisible (clé
privée, lien hors dossier autorisé, autre format) ». C'est un lien vers
`/usr/share/ca-certificates/mozilla/…`, hors des dossiers autorisés.

- Raison précise par fichier écarté : nom de clé privée, lien vers un
  dossier non autorisé (ou lien cassé), pas un certificat lisible, déjà lu
  par un autre chemin. Assistant root : compteurs dans `root.json` ; agent :
  `mirador_agent_certificats_chemin_ecartes{chemin, raison}` ; interface :
  « aucun lu : lien vers un dossier non autorisé (1) ».
- `/usr/share/ca-certificates/` ajouté aux dossiers autorisés (une seule
  liste pour le serveur, l'agent et l'assistant root) : certificats publics
  des autorités, cibles des liens de `/etc/ssl/certs`. Le reste de
  `/usr/share` reste refusé. Risque : nul (fichiers publics, lecture seule,
  mêmes contrôles de nom et de format).

### 22.5 Assistant root : vraie version requise, mise à jour signée par lui-même **[Fait, demande du 12/11 « a et b, avec une sécurité »]**

Constat : « Assistant root en 0.2.15, agent en 0.2.17 » affiché alors que
l'assistant n'avait pas changé (0.2.16 et 0.2.17 ne touchaient que le
serveur) ; et l'assistant ne se mettait à jour qu'en relançant le script.

**a. Fausse alerte** : l'assistant écrit un numéro de comportement
(`FormatAssistant`, 2 aujourd'hui) dans `root.json`, qui ne change que
lorsque l'assistant change. L'agent envoie `mirador_agent_root_a_jour`
(1 si le numéro suffit) ; l'interface n'alerte que sur 0 (agents plus
anciens : comparaison des versions, comme avant).

**b. Mise à jour par lui-même** : à chacun de ses passages (15 min, ou
« Actualiser »), l'assistant root peut se remplacer par la version de
l'agent en service.

| Sécurité | Comment |
|---|---|
| Accord local | seulement avec `MIRADOR_MAJ_AUTO=oui` ; `MIRADOR_MAJ_RACINE=non` (ou `--sans-maj-racine` à l'installation) l'interdit ; fichier `agent.conf` appartenant à root, que ni l'agent ni le serveur ne peuvent changer |
| Version déjà éprouvée | seulement la version en service de l'agent, une fois **confirmée** (plus de `essai.json`) : vérifiée, testée, et a tourné |
| Jamais de retour arrière | version strictement plus récente que l'assistant, même signée « retour autorisé » |
| Signature revérifiée par root | manifeste gardé par l'agent à côté du binaire (`mirador-agent-X.manifeste.json`), signature Ed25519 vérifiée avec la clé du fichier root `/etc/mirador-agent/cles-publiques.txt` (droits revérifiés) ; architecture vérifiée |
| Fichiers de l'agent non fiables | nom du lien `current` vérifié, fichiers ouverts sans suivre de lien et sans bloquer, taille bornée ; empreinte et taille vérifiées sur les octets lus, puis ces octets-là écrits |
| Remplacement sûr | seulement dans `/usr/local/lib/mirador-agent/` (seul dossier en écriture ajouté à l'unité root) ; fichier `.nouveau` puis renommage atomique ; ancienne version gardée (`.precedent`) |
| Traces | message dans le journal de `mirador-agent-root` ; refus écrit dans `root.json` (erreurs de l'assistant) |

Risque accepté (expliqué à l'utilisateur) : avec la clé de signature (secret
GitHub), un attaquant pourrait faire installer son programme **en root**
(avant : seulement sous l'identité de l'agent). La protection reste la
clé : environnement GitHub limité aux tags, 2FA du compte. Le refus local
`MIRADOR_MAJ_RACINE=non` reste possible machine par machine.

Mise en route : relancer le script une fois (assistant au format 2 et
dossier en écriture dans l'unité) ; ensuite, l'assistant suit l'agent seul.
La version actuelle de l'agent (installée avant ce changement) n'a pas de
manifeste gardé : l'assistant attend la version suivante.

Correction du 13/11 : le nettoyage qui suit la confirmation d'une version
effaçait aussi son manifeste (0.2.18 et 0.2.19 touchées : l'assistant
attendait sans rien dire). Corrigé en 0.2.20 ; un manifeste absent est
maintenant écrit dans le journal de l'assistant. Comme c'est la nouvelle
version qui confirme (et donc nettoie), la 0.2.20 garde son propre
manifeste : l'assistant la suit dès qu'elle est confirmée.


### 22.6 Versions de l'agent gardées sur le serveur **[Validé le 16/11 — mesure du 13/11]**

Constat (mesure réelle de 18 h 40, `PROGRES.md`) : dossier `versions` à
166 Mo (12 versions × amd64 + arm64), recopié dans chaque sauvegarde
(8 Ko → 19 Mo → 51 Mo en 3 jours ; ~1 Go avec 14 jours gardés).

| Règle | Pourquoi |
|---|---|
| Gardées : cible globale, cibles propres, version en service et à l'essai sur chaque machine (sauf refusées) | jamais retirer ce qu'une machine utilise ou va télécharger |
| + les **2** plus récentes (numéro, pas date d'import ; réglable de 1 à 10 dans Mises à jour, demande du 13/11) | retour arrière rapide sans réimport |
| + celles importées depuis **moins de 24 h** | le temps d'en faire la cible (retour arrière vers une ancienne) |
| Seulement les architectures du parc (toutes si aucune machine) | arm64 inutile ici : la moitié de la place |
| Purge 2 min après le démarrage, puis toutes les 24 h ; fichiers orphelins (`.tmp`, sans ligne en base) effacés | coût : quelques requêtes SQL par jour |
| Import : architectures du parc seulement, case « toutes les architectures » | ne pas télécharger ce qui serait purgé |
| Bouton « Supprimer » (2FA), refusé pour une version en service ; audit `version_supprimee` | tout est réimportable depuis GitHub (signé) |

Une machine d'une architecture absente s'installe avec le binaire intégré à
l'image du serveur (`binaireInstallation`), puis on importe sa version.
Effet attendu ici : versions ~30 Mo (≈ 4 versions amd64), archive de
sauvegarde ~15 Mo au lieu de 51 Mo.

Menaces : un attaquant connecté en admin peut supprimer des versions →
2FA, audit, versions en service protégées, réimport possible ; la purge
lit seulement la base (aucun chemin venu de l'extérieur : noms fabriqués
par `nomBinaire`, fichiers orphelins filtrés par une expression stricte,
fichiers ordinaires seulement).
---

## 23. Étape 6 — sources sans agent et `mirador-collector` **[Décidé le 13/11 ; code après validation de l'étape 5]**

Rien n'est codé. Ce brouillon reprend ce qui est déjà décidé (§ 4.4, § 5.2,
§ 5.3, § 5.3.1, § 11.2) et propose le découpage, les choix techniques et le
modèle de menaces. Les points marqués **[Question]** attendent une réponse.

### 23.1 Ce qui existe déjà et sera réutilisé

| Brique | Reprise pour le collecteur |
|---|---|
| enrôlement, token haché, approbation | même chemin, type « collecteur » |
| configuration distante typée (long-poll, ETag) | même chemin, schéma propre au collecteur |
| envoi métriques (remote write) et logs (JSON + zstd) | même code (`internal/agent/envoi`, `remotewrite`) |
| mise à jour signée, essai, retour arrière | même code (`majagent`), binaire `mirador-collector-*` |
| secrets chiffrés au repos, jamais ré-affichés | même paquet (`internal/server/secrets`) |
| règles importées (37 « données attendues à l'étape 6 ») | rien à refaire si les noms de métriques sont les mêmes qu'aujourd'hui |

### 23.2 Découpage proposé (chaque partie marche seule)

| Partie | Contenu | Pourquoi dans cet ordre |
|---|---|---|
| 1 | Serveur : entrée InfluxDB v2 (`POST /api/v2/write`, `GET /health`) pour Proxmox et Telegraf (OPNsense) | aucun binaire nouveau ; supprime déjà un usage de VictoriaMetrics |
| 2 | `mirador-collector` : squelette, enrôlement, configuration, mise à jour, page « Collecteurs » | socle des parties 3 à 5 |
| 3 | Syslog TLS (écoute LAN) | remplace la partie la plus exposée d'Alloy |
| 4 | SNMP v3 (Synology) et sondes de certificats distants | sorties seulement |
| 5 | UniFi (métriques et événements) | dépend de la question 3 |
| 6 | Mesures, comparaison avec Alloy pendant quelques jours, puis retrait d'Alloy **sur validation** | critère de fin d'étape (§ 13) |

### 23.3 Choix techniques proposés (coût RAM / CPU / disque)

- **Entrée InfluxDB** : lecteur du *line protocol* écrit à la main (format
  simple), bornes strictes (taille du corps, nombre de champs, longueur des
  noms), fuzzing. Noms produits `mesure_champ` + tags en étiquettes, comme
  VictoriaMetrics aujourd'hui. Coût : quelques centaines de lignes, aucune
  dépendance. Token `Authorization: Token …` accepté en plus de `Bearer`.
- **Syslog** : TLS 1.2 minimum, trame RFC 5425 (longueur en tête) et, par
  compatibilité, une ligne par message ; formats RFC 5424 et RFC 3164 ; lecteur
  écrit à la main, fuzzing. Limites : 8 Kio par message, 32 connexions,
  connexion fermée après 5 min sans rien, 500 lignes/s par émetteur (rafale
  1000), `filterlog` jeté par défaut. Tampon borné en mémoire (pas de disque) :
  au-delà, les lignes sont comptées comme perdues. Coût visé : < 5 Mio pour
  ~0,6 ligne/s en moyenne (52 500 lignes/jour aujourd'hui).
- **SNMP v3** : bibliothèque `gosnmp` (BSD-2-Clause, compatible GPL-3.0). Écrire
  soi-même la sécurité SNMP v3 (USM) serait risqué. Seulement GET/WALK, en
  lecture, sur une liste d'OID **figée dans le code** par préréglage
  (Synology, interfaces, système), avec les mêmes noms de métriques que
  `snmp_exporter` aujourd'hui. Pas de fichier MIB à charger.
- **Sondes de certificats** : connexion TLS, lecture du certificat sans le
  valider, mêmes noms qu'aujourd'hui (`ssl_cert_not_after_timestamp_seconds`,
  `probe_success`). Aucune dépendance.
- **UniFi** : voir **[Question 3]**. Lecture seule de l'API du contrôleur.
- **Proxmox, SMART** : déjà couvert par `mirador-agent` + assistant root si
  l'agent est installé sur les Proxmox (plus besoin de `smartctl_exporter`).
- **Objectifs du collecteur** (proposés) : < 40 Mio de RAM, < 1 % d'un cœur
  en moyenne, aucune écriture disque en régime normal (sauf le cache de
  configuration). Référence à mesurer : Alloy + unpoller sur le collecteur
  actuel (LXC, 512 Mo conseillés).

### 23.4 Modèle de menaces de l'étape 6

| Menace | Conséquence | Parade |
|---|---|---|
| Émetteur syslog inconnu sur le LAN | fausses lignes, saturation | liste d'IP émettrices avec rôle, TLS obligatoire, limites par émetteur et globales, lignes refusées comptées par motif |
| Message syslog piégé | plantage, XSS dans l'interface, CPU | lecteur borné et testé par fuzzing ; échappement à l'affichage (déjà en place) ; pas d'expression régulière fournie de l'extérieur |
| Écoute détournée vers Internet | port ouvert au monde | adresse d'écoute choisie parmi les adresses **privées** déclarées par le collecteur ; jamais `0.0.0.0` ni une adresse publique ; refus au démarrage sinon |
| Corps InfluxDB énorme ou piégé | mémoire, CPU du serveur | taille du corps bornée, lecture en flux, fuzzing, token par équipement, quotas d'ingestion existants |
| Secrets SNMP / UniFi volés | lecture des équipements | comptes en **lecture seule** ; chiffrés sur le serveur ; fichier cache `0600` sur le collecteur ; jamais dans les logs ni ré-affichés |
| Serveur compromis | collecteur détourné | config typée sans commande ; tests limités aux cibles déjà configurées (§ 5.3.1) ; mises à jour signées hors serveur |
| Contrôleur UniFi au certificat auto-signé | interception | vérification du certificat activée par défaut ; sinon empreinte du certificat épinglée dans la configuration |
| Collecteur compromis | faux syslog, accès aux équipements | utilisateur dédié, systemd strict, `CAP_NET_BIND_SERVICE` seulement si port < 1024 (6514 n'en a pas besoin), aucun accès Docker ni root |

### 23.5 Questions à l'utilisateur (5 au plus)

1. **[Question 1]** Proxmox et OPNsense envoient-ils **directement** au serveur
   (comme aujourd'hui vers VictoriaMetrics), ou via le collecteur ? Proposé :
   directement (rien à coder côté collecteur).
2. **[Question 2]** Certificat du syslog : (a) obtenu par le collecteur
   (ACME, défi DNS) ou (b) certbot garde la main, le collecteur relit le
   fichier quand il change ? Proposé : (b) (moins de code, rien de nouveau à
   protéger).
3. **[Question 3]** UniFi : (a) garder unpoller au début et le lire en local,
   ou (b) client Go natif tout de suite (unpoller retiré, RAM en moins, plus de
   code) ? Proposé : (a) puis (b) une fois le reste validé.
4. **[Question 4]** Ordre des parties (§ 23.2) : d'accord pour commencer par
   l'entrée InfluxDB côté serveur ?
5. **[Question 5]** Mesure de référence : coller la consommation d'Alloy et
   d'unpoller sur le collecteur actuel (commandes dans `PROGRES.md`).

### 23.6 Réponses du 13/11 et conséquences

| # | Réponse | Conséquence proposée |
|---|---|---|
| 1 | Proxmox : agent seul, sans le flux InfluxDB, si possible ; OPNsense : pas le choix | **Proxmox par l'agent** (§ 23.7) ; entrée InfluxDB gardée pour **OPNsense seul** (Telegraf) |
| 2 | (a), avec le renouvellement géré dans l'interface | certificat du syslog obtenu par ACME, renouvelé automatiquement, état et bouton « Renouveler » dans l'interface (§ 23.8) |
| 3 | (b) | client UniFi natif dans le collecteur, unpoller retiré |
| 4 | voir 1 | nouvel ordre (§ 23.9) |
| 5 | mesure du collecteur actuel | Alloy : 155 Mo résidents (179 Mo pour systemd), 0,86 % d'un cœur en moyenne (256 s de CPU en 8 h 13) ; unpoller : 26 Mo ; **référence : ~180 Mo** pour < 40 Mio visés |

### 23.7 Proxmox par l'agent, sans metric server **[Décidé, réponse du 13/11]**

Ce que le metric server donne aujourd'hui, et d'où l'agent peut le lire, sans
`pvesh`, sans API, sans token dans Proxmox (écarté au § 12 : `pvesh` lancé par
l'agent) :

| Donnée | Source lue par l'agent | Coût |
|---|---|---|
| nœud : CPU, load, mémoire, swap, réseau, disques | déjà collecté (comme toute machine) | 0 |
| ARC ZFS, état des pools | `/proc/spl/kstat/zfs/` (déjà lu pour l'état des pools) | quelques fichiers |
| invités : CPU, mémoire, I/O disque, PSI | cgroups v2 : `/sys/fs/cgroup/qemu.slice/<id>.scope/`, `/sys/fs/cgroup/lxc/<id>/` (lisibles sans root) | ~5 petits fichiers par invité, toutes les 30 s |
| invités : réseau | interfaces `tap<id>i<n>` (VM) et `veth<id>i<n>` (LXC), déjà collectées | 0 |
| invités : liste, nom, en marche ou arrêté | `/etc/pve/.vmlist` et ligne `name:`/`hostname:` des fichiers de `/etc/pve/` : réservés à root → **assistant root** (lecture seule, chemins figés) | 1 lecture / 15 min |
| stockages : occupation | systèmes de fichiers déjà collectés ; pools ZFS par kstat | 0 |

Limites (dites à l'avance) :
- mémoire d'une VM = mémoire prise par son processus QEMU côté hôte, pas
  la mémoire vue de l'intérieur (le metric server ne la donne qu'avec le
  ballon ou l'agent invité) ;
- place libre dans le disque d'une VM : seulement si l'agent mirador tourne
  dans la VM (cas normal ici).

Conséquences : le tableau « Proxmox » (31 panneaux) et les règles qui lisent
le metric server sont réécrits avec les nouveaux noms
(`mirador_pve_invite_*{id, nom, type}`) ; `ProxmoxMuet` devient l'`AgentMuet`
du nœud. Aucun port, aucun token ajouté sur Proxmox. Le metric server reste
utilisable pendant la comparaison.

#### 23.7.1 Partie 1a faite (13/11) : collecte **[Fait, à tester sur un nœud]**

- Agent (`collect/proxmox.go`), lancé seulement si `qemu.slice` ou `lxc`
  existe dans les cgroups : `mirador_pve_invite_en_marche`,
  `_cpu_secondes_total`, `_memoire_octets` (sans le cache inactif, comme
  `docker stats`), `_disque_lus_octets_total`, `_disque_ecrits_octets_total`,
  `_pression_secondes_total{ressource,niveau}`, `_reseau_recus_octets_total`
  et `_reseau_envoyes_octets_total` (vus de l'invité), étiquettes `id`,
  `type` (`qemu`, `lxc`).
- Disques des **VM** : Proxmox n'active pas le contrôleur `io` pour
  `qemu.slice` (constaté sur Proxmox 9.2 : pas d'`io.stat`). L'agent lit
  alors les compteurs des volumes de la VM, vus comme disques du nœud
  (`/sys/block/<dev>/stat`, lisibles sans root) : ZFS par les liens
  `/dev/zvol/<pool>/…/vm-<id>-disk-<n>` → `zdN`, LVM par
  `/sys/block/dm-N/dm/name` (`<vg>-vm--<id>--disk--<n>`) ; partitions et
  modèles ignorés ; table refaite toutes les 10 collectes. LXC : `io.stat`
  du cgroup quand il existe.
- Disques **en fichier** (qcow2 sur un stockage « dossier », cas réel du
  13/11) : ni `io.stat` ni volume. **[Décidé, réponse du 13/11 : choix 1,
  rythme réglable dans l'interface]** L'assistant root lit
  `/proc/<pid>/io` (`read_bytes`, `write_bytes`) des processus du cgroup de
  chaque VM (root seulement). Réglage « Disques des VM Proxmox »
  (`proxmox_disques`, 1 à 15 min, défaut 5 min, capacité d'agent
  `proxmox`) : l'agent écrit « disques » dans le fichier surveillé par
  `mirador-agent-root.path` ; l'assistant fait alors un passage **léger**
  (disques des VM seulement, le reste repris du dernier passage complet
  de moins de 20 min ; contenu non fiable qui ne peut que réduire le
  travail, demande vieille de plus d'une minute ignorée). Valeurs
  envoyées sous les mêmes noms (`mirador_pve_invite_disque_*`), mises à
  jour au rythme réglé : courbes en moyenne sur 2 × ce rythme.
  Priorité : `io.stat` > volumes ZFS/LVM > assistant root.
  Constaté le 13/11 : lecture refusée (« permission denied »). Le noyau ne
  montre `/proc/<pid>/io` d'un processus root qu'à un appelant du même
  **groupe** (ou avec `CAP_SYS_PTRACE`) ; l'assistant tournait dans le
  groupe `mirador-agent`. Corrigé sans droit en plus : unité de
  l'assistant en `Group=root`, et `/run/mirador-agent` créé par
  `/etc/tmpfiles.d/mirador-agent.conf` en `2750 root:mirador-agent`
  (bit setgid : `root.json` garde le groupe de l'agent). Prend effet
  en relançant le script une fois.
  **Pas suffisant** (vérifié le 13/11 avec `setpriv --bounding-set=-sys_ptrace`) :
  QEMU est protégé par le noyau (pas « inspectable »), même root a besoin
  de `CAP_SYS_PTRACE`. **[Décidé, réponse du 13/11]** `CAP_SYS_PTRACE`
  ajouté au jeu de droits de l'unité de l'assistant root (0.2.25). Risque :
  l'assistant pourrait inspecter tout processus ; il a déjà
  `CAP_SYS_ADMIN` (plus large) et son code ne lit que `/proc/<pid>/io` des
  processus du cgroup de chaque VM (pid numérique lu dans le cgroup).
  Alternative écartée : pas de disque par VM pour les disques qcow2.
  Coût : un passage léger ≈ lancement du programme (~30 ms de CPU) ; à
  1 min, ~0,05 % d'un cœur.
- Assistant root (`racine/proxmox.go`) : `/etc/pve/.vmlist` + configuration
  courante de chaque invité de CE nœud (nom, mémoire, cœurs × sockets ; les
  instantanés et toutes les autres clés sont ignorés), fichiers lus sans
  suivre de lien et bornés. L'agent envoie `mirador_pve_invite_info{id,type,
  nom,noeud}`, `_memoire_max_octets`, `_coeurs`. Invité arrêté :
  `info` présent, `en_marche` absent.
- Assistant au format 3 ; exigé seulement sur un nœud Proxmox
  (`FormatRequis`) : pas d'alerte « trop ancien » ailleurs.
- Coût mesuré (30 invités, banc d'essai) : 0,83 ms par cycle de 30 s
  (~0,003 % d'un cœur), ~68 Ko de mémoire temporaire par cycle (ouverture
  des fichiers), chemins gardés en cache ; assistant : quelques fichiers
  de /etc/pve toutes les 15 min. Aucune écriture disque.
- Menaces : noms d'invités venus de /etc/pve (écrits par un admin Proxmox)
  → format strict (sinon non gardé) et échappement à l'affichage ; lien à
  la place d'un fichier de configuration → refusé ; .vmlist énorme ou
  abîmé → borné, erreur dans `root.json` ; aucune clé sensible lue
  (mot de passe cloud-init, `args`).
- Total disque de toutes les VM : `mirador_pve_vms_disque_lus_octets_total`,
  `_ecrits_` (`qemu.slice/io.stat`, toutes les 30 s).

#### 23.7.2 Partie 1b faite (13/11) : tableau et règles **[Validé le 16/11]**

- Tableau « Proxmox VE » réécrit (même nom `proxmox` : un tableau installé
  sans modification suit tout seul ; modifié : « Rétablir ») : vue
  d'ensemble (nœuds, VM et LXC en marche, invités arrêtés, uptime), nœuds
  (`node_*` de l'agent), disque de toutes les VM, inventaire des invités
  (état, CPU en % des cœurs alloués, mémoire, mémoire configurée, cœurs),
  CPU / mémoire / disque (moyenne 15 min) / réseau / pression par invité,
  stockage (systèmes de fichiers, pools ZFS), matériel (températures,
  SMART de l'assistant root), services systemd. Nom de l'invité joint par
  `* on(instance, id) group_left(nom) mirador_pve_invite_info`.
  Testé sur des données simulées (`tableau_proxmox_test.go`).
- 3 règles fournies (groupe `proxmox`, posées au démarrage suivant) :
  `ProxmoxInviteCpuSature` (> 95 % des cœurs, 15 min),
  `ProxmoxConteneurMemoireSaturee` (LXC seulement, > 95 % de la mémoire
  configurée, 15 min : pour une VM, la mémoire vue de l'hôte ne dit rien
  du manque de mémoire dans l'invité), `ProxmoxListeInvitesIndisponible`
  (invités en marche sans liste de l'assistant root, 30 min).
- Règle importée `ProxmoxMuet` (lit le metric server) : à désactiver dans
  « Règles » quand le metric server est coupé ; la panne d'un nœud est
  alors couverte par `AgentMuet`.

### 23.8 Certificat du syslog par ACME, géré dans l'interface **[Décidé, réponse du 13/11]**

- Protocole ACME avec `golang.org/x/crypto/acme` (déjà dans les
  dépendances : rien de nouveau à embarquer), défi DNS-01 chez deSEC (un appel
  HTTPS simple, écrit à la main).
- **La clé privée TLS ne quitte jamais le collecteur** : il la crée et envoie
  seulement une demande de certificat (CSR) au serveur.
- **Le serveur fait la démarche ACME** (le VPS sort déjà sur Internet ; le
  jeton deSEC, secret chiffré, reste sur le serveur et n'est jamais envoyé au
  collecteur) puis renvoie le certificat dans la configuration du collecteur.
  Collecteur compromis : ni jeton deSEC ni compte ACME à voler.
- Jeton deSEC limité à l'enregistrement `_acme-challenge` du nom du collecteur
  (règle de jeton deSEC), saisi une fois dans l'interface.
- Interface : nom du certificat, échéance, dernier renouvellement et son
  résultat, bouton « Renouveler maintenant » (2FA), renouvellement
  automatique 30 jours avant l'échéance, alerte si échec ou si moins de 14 jours.
- Le collecteur recharge le certificat sans couper les connexions en cours.

### 23.9 Ordre **[Décidé, 13/11]** (remplace § 23.2)

| Partie | Contenu |
|---|---|
| 1 | Proxmox dans l'agent (cgroups, assistant root pour la liste des invités), tableau et règles réécrits |
| 2 | Serveur : entrée InfluxDB pour OPNsense (Telegraf) — **fait le 14/11** (§ 23.10) |
| 3 | `mirador-collector` : squelette, enrôlement, configuration, mise à jour, page « Collecteurs » — **fait le 14/11** (§ 23.13) |
| 4 | Syslog TLS + certificat ACME géré dans l'interface — **fait le 14/11** (§ 23.14) |
| 5 | SNMP v3 (Synology) et sondes de certificats — **fait le 14/11** (§ 23.15) |
| 6 | Client UniFi natif (unpoller retiré) — **fait le 14/11** (§ 23.16) |
| 7 | Mesures, comparaison avec Alloy, retrait d'Alloy sur validation |

### 23.10 Partie 2 faite (14/11) : entrée InfluxDB v2 **[Fait, à tester avec OPNsense]**

- Adresse : `POST /api/agent/v1/influx/api/v2/write` (Telegraf ajoute
  `/api/v2/write` à l'URL donnée), `GET …/influx/health` et `…/ping` sans
  token (réponse fixe). Sous `/api/agent/` : même bloc nginx que les agents.
- Accueil identique aux agents : token par machine (`Token …` d'InfluxDB
  accepté en plus de `Bearer …`), adresses autorisées, limites par adresse
  et par machine, quotas de séries, nom imposé (`instance`) ; étiquette
  `host` de Telegraf gardée (règles et tableau actuels inchangés).
- Lecteur du *line protocol* écrit à la main (`ingestion/influx.go`) :
  échappements, textes ignorés, booléens 1/0, entiers `i` et `u`,
  précision `ns`/`us`/`ms`/`s`, gzip ; noms `mesure_champ` nettoyés comme
  VictoriaMetrics. Ligne invalide : ignorée et comptée ; aucune ligne
  valable : 400 ; au-delà de 4 Mio décompressés, 20 000 séries ou
  200 000 points : 413.
- Création depuis l'interface (demande du 14/11) : Machines › « Équipement
  sans agent » : machine approuvée tout de suite, adresses autorisées,
  site / type / groupe, token affiché une seule fois avec les réglages de
  Telegraf ; refusé pour un nom existant (le token d'une machine en
  service n'est jamais remplacé par ce formulaire) ; code 2FA, audit
  `token_manuel`.
- Coût mesuré : pires cas de 4 Mio (champs, tags, lignes, guillemets,
  antislash) traités en 4 à 120 ms ; un envoi normal d'OPNsense (~300
  séries toutes les 30 s) : négligeable. Aucune dépendance ajoutée.
- Menaces : corps piégé (bornes ci-dessus, fuzzing `FuzzDecoderInflux` en
  CI), étiquettes réservées (`__…`) refusées, valeurs non UTF-8 refusées,
  NaN / infini refusés ; `/health` sans token ne dit rien (texte fixe) et
  passe par la limite par adresse ; token d'OPNsense volé : mêmes parades
  que pour un agent (adresses autorisées, révocation).

### 23.11 Page Logs : listes déroulantes (demande du 14/11) **[Validé le 16/11]**

- Chaque filtre (Machine, Source, Niveau, Unité, Conteneur, Site, Type,
  Groupe) devient une liste « (tous) + valeurs » ; la valeur choisie y est
  toujours.
- Machines, sites, types et groupes : base (machines, Réglages › Sites,
  types et groupes). Niveaux : les 8 niveaux syslog + ceux vus.
- Source, niveau, unité, conteneur : valeurs **vues dans les lignes
  reçues** par le moteur de logs, gardées en mémoire (500 par champ au
  plus, les plus anciennes oubliées). Coût : 4 recherches dans une table
  par ligne reçue (~16 lignes/s), quelques Ko de mémoire. La liste
  repart vide au redémarrage du serveur et se remplit au fil des lignes
  (pas de relecture du disque).

### 23.12 Message « nouvelle adresse » répété (correction du 14/11) **[Fait]**

Les adresses déjà connues d'un compte n'étaient gardées qu'en mémoire :
après chaque redémarrage du serveur (5 mises à jour le 13/11), la
connexion suivante redonnait le message. Elles sont maintenant en base
(table `adresses_connues`, migration 0014 : compte, adresse ou /64 IPv6,
dernière connexion). Une adresse non revue depuis 30 jours redevient
« nouvelle » et est effacée. Si la base ne répond pas, la mémoire sert
(comme avant). Le verrouillage des comptes n'est pas changé (mémoire).
Coût : une lecture et une écriture SQLite par connexion réussie.

### 23.13 Partie 3 faite (14/11) : `mirador-collector`, le squelette **[Fait, à tester sur le LAN]**

Choix faits sans question (dans la ligne des § 23.3 à 23.9) :

- **Binaire séparé** `mirador-collector` (l'agent n'ouvre jamais de port),
  qui **réutilise** le code de l'agent : fichier de configuration (mêmes
  clés `MIRADOR_*`, valeurs par défaut propres : `DefautCollecteur`),
  enrôlement, configuration distante (long-poll, ETag), file et envoi
  remote write, mise à jour signée avec essai et retour arrière. Il ne
  relève pas la machine où il tourne (un mirador-agent s'en charge si
  besoin) : pas de systemd, de journal, de Docker ni d'assistant root.
- **Produit signé** : le manifeste porte `produit` (`mirador-collector`) et
  le nom du produit est la première ligne du message signé. Un binaire de
  l'agent ne peut pas être installé comme collecteur, ni l'inverse. Les
  manifestes de l'agent restent **sans** ce champ (les agents déjà
  installés savent les lire). `mirador-release signer -produit
  mirador-collector …` ; la CI signe et publie les deux produits à chaque
  tag ; l'import GitHub prend le collecteur s'il est publié.
- **Serveur** : binaires rangés par produit (migration 0019 : table
  `versions_binaires`, reprise de `versions_agent`) ; un collecteur reçoit
  le manifeste et le binaire de `mirador-collector` pour la version cible
  (globale ou propre), un agent ceux de `mirador-agent`. Installation :
  `…/install/collecteur` (script public) et `…/install/binary?produit=
  mirador-collector` (avec le jeton) ; binaires du collecteur embarqués
  dans l'image du serveur.
- **Type choisi au jeton** : « Ajouter une machine » › Machine : *Agent* ou
  *Collecteur du LAN* ; la machine créée à l'enrôlement prend le type du
  jeton, jamais celui qu'elle annoncerait.
- **Équipements rattachés** (table `equipements_collecteur`, un équipement
  → un collecteur) : page du collecteur (« Rattacher », « Détacher »),
  page de l'équipement (« Relevé par »), « Équipement sans agent » ›
  *Relevé par* un collecteur (pas de token). La liste est posée par le
  serveur dans la clé `collecteur.equipements` de la configuration du
  collecteur (une liste saisie à la main est remplacée), envoyée seulement
  au binaire qui annonce la capacité `collecteur`.
- **Contrôle à l'arrivée** : un collecteur ne peut envoyer que pour
  lui-même et ses équipements rattachés (approuvés). Série d'une autre
  `instance` : refusée et comptée (`SeriesHorsParc`, avertissement dans le
  journal). Site, type et groupe : ceux de la **fiche de l'équipement**.
  Étiquette `collecteur` = nom du collecteur (sourdine « collecteur muet »
  déjà en place). Logs : hôte rattaché gardé, hôte inconnu rangé sous le
  nom du collecteur (champ `hote_annonce`). Les équipements relevés sont
  notés « vus » à chaque donnée reçue (sans adresse).
- **Mesures propres** : `mirador_collecteur_info{version}`,
  `mirador_collecteur_equipements`, et les métriques internes de l'agent
  (mêmes noms), `job="mirador-collector"`.
- **Installation** : `mirador-collector-install.sh` (utilisateur dédié
  sans droits, `/etc/mirador-collector`, `/var/lib/mirador-collector`,
  unité systemd durcie sans aucun droit particulier, `MemoryMax=120M`,
  `GOMEMLIMIT=35MiB`, retour arrière automatique).

**Coût mesuré** (binaire réel, serveur injoignable, 40 s) : **12,5 Mo
résidents** (4,5 Mo de mémoire propre), moins de 10 ms de CPU (~0 %),
binaire 7,7 Mo. Objectif < 40 Mio : ✅ (référence actuelle : Alloy +
unpoller ≈ 180 Mo). Serveur : une lecture SQL par collecteur toutes les
30 s (liste en cache), une table de plus.

**Menaces** (complète § 23.4) :
- Collecteur compromis : ne peut parler que pour ses équipements
  (contrôle serveur, liste en base, rattachement réservé aux admins avec
  code 2FA et audit) ; ses logs d'hôtes inconnus restent sous son nom.
- Serveur compromis : peut changer la liste des équipements (n'élargit que
  ce que le serveur accepte lui-même), jamais faire exécuter quoi que ce
  soit ; mises à jour signées hors serveur, produit vérifié.
- Mauvais binaire poussé : produit dans le message signé → refusé.
- Agent qui s'annoncerait collecteur : le type vient du jeton créé par
  l'admin ; la capacité seule n'ouvre que la clé `collecteur` de sa
  configuration, sans droit d'envoi en plus.

### 23.14 Partie 4 faite (14/11) : syslog TLS et certificat ACME **[Fait, à tester sur le LAN]**

**Réception (collecteur)**
- Réglage par collecteur (page du collecteur › *Syslog TLS*, clé
  `collecteur.syslog`) : adresse d'écoute (**IP privée** du LAN : port ≥ 1024,
  6514 conseillé ; jamais `0.0.0.0`, la boucle locale ni une adresse
  publique : refusé à l'enregistrement et revérifié au démarrage), nom du
  certificat, **émetteurs** « adresse ou réseau du LAN → équipement
  rattaché », `filterlog` jeté par défaut.
- Un émetteur inconnu est coupé **avant** la poignée de main TLS. 32
  connexions au plus, 4 par adresse, 10 s pour la poignée de main, 5 min
  sans rien = fermée. TLS 1.2 minimum.
- Lecteur écrit à la main (trames RFC 5425 « longueur espace message », ou
  une ligne par message ; formats RFC 5424 et RFC 3164), 8 Kio par message,
  UTF-8 rendu valide, testé par fuzzing (CI). Le fuzzing a trouvé un défaut
  avant la livraison (nom du programme qui dépassait 48 octets après
  remplacement des caractères invalides) : corrigé.
- L'**hôte vient de l'adresse** de l'émetteur, jamais du message. Heure :
  celle du message RFC 5424 si elle est plausible (−24 h, +5 min), sinon
  l'heure de réception. Débit par équipement 500 lignes/s (rafale 1 000),
  réglable ; au-delà, compté et jeté. Masquage des mots de passe, tokens…
  comme les logs de l'agent.
- Envoi : lots (job `syslog`, champs `host`, `ident`, `level`) toutes les
  2 s ou dès 512 Kio, file de 4 Mio en mémoire (pas de disque), même envoi
  que les logs de l'agent ; le serveur range chaque ligne sous l'équipement
  (contrôle de la partie 3).
- Mesures : `mirador_collecteur_syslog_ecoute`, `_connexions`,
  `_lignes_total{equipement}`, `_refus_total{motif}` (émetteur inconnu, trop
  de connexions, TLS, trame, format, débit, filterlog, file pleine),
  `_certificat_fin_timestamp_seconds`.

**Certificat (serveur)** — réponse 2 du 13/11 (§ 23.8)
- Le collecteur crée sa clé ECDSA P-256 (`/var/lib/mirador-collector/syslog.key`,
  0600, ne sort jamais) et joint une **demande (CSR)** à son battement.
- Le serveur la vérifie (signature, clé, un seul nom) et fait la démarche
  **ACME** (`golang.org/x/crypto/acme`, déjà dans les dépendances) avec le
  défi **DNS-01 chez deSEC** (un appel PATCH écrit à la main, enregistrement
  `_acme-challenge…` posé puis retiré). Jeton deSEC et clé du compte ACME
  **chiffrés** sur le serveur, jamais envoyés au collecteur ni réaffichés,
  absents de l'audit.
- Certificat vérifié (même clé, bon nom) puis posé dans la configuration du
  collecteur (`collecteur.syslog.certificat`, jamais saisi à la main) ; le
  collecteur redémarre et ouvre l'écoute. Renouvellement automatique
  30 jours avant l'échéance ; nouvelle clé = nouveau certificat ; échec :
  message sur la page, nouvel essai toutes les 6 h ; **« Renouveler
  maintenant »** (code 2FA).
- Alertes fournies : `CertificatSyslogBientotExpire` (< 14 jours) et
  `CertificatSyslogRenouvellementEchoue` (groupe `collecteurs`).
- Testé sans réseau : faux serveur ACME (RFC 8555, le strict nécessaire) et
  faux deSEC ; démarche complète, renouvellement, échec et attente.

**Coût mesuré** (banc d'essai) : **7 µs par ligne** (lecture, masquage, mise
en lot), soit ~0,0005 % d'un cœur au rythme actuel (0,6 ligne/s,
52 500/jour) et 0,35 % à 500 lignes/s. Mémoire : file d'attente de 1 000
lignes (quelques centaines de Ko en vrai, 8 Mio au pire), lots de 512 Kio,
file d'envoi de 4 Mio. Aucune écriture disque (sauf la clé, une fois).
Serveur : un passage par minute sur la table des certificats.

**Menaces** (complète § 23.4)
- Écoute détournée vers Internet : adresse privée obligatoire (deux
  contrôles) ; le collecteur ne peut pas écouter ailleurs.
- Émetteur inconnu ou usurpant un nom : coupé avant le TLS ; le nom vient
  de l'adresse ; débit et connexions bornés.
- Message piégé : lecteur borné et fuzzé ; échappement à l'affichage.
- Vol du jeton deSEC : il reste sur le serveur, chiffré, limité par la règle
  de jeton deSEC au seul enregistrement du défi.
- Collecteur compromis : n'a ni jeton deSEC ni compte ACME ; sa clé ne vaut
  que pour son propre nom.
- Serveur compromis : pourrait obtenir un certificat pour le nom réglé
  (comme aujourd'hui avec certbot) ; ne peut pas lire la clé du collecteur.

**À savoir** : le syslog distant des équipements UniFi n'existe souvent qu'en
UDP sans chiffrement ; ses événements arriveront par le client UniFi natif
(partie 6). OPNsense et Synology savent envoyer en TLS.

### 23.15 Partie 5 faite (14/11) : SNMP v3 et sondes de certificats **[Fait, à vérifier sur le NAS]**

- **SNMP v3** avec `gosnmp` (BSD-2-Clause, § 23.3 ; seule dépendance
  ajoutée) : **authPriv obligatoire** (SHA/SHA256/SHA512 + AES/AES256),
  GET et GETBULK seulement, liste d'OID **figée dans le code** :
  - *standard* : `sysUpTime`, mémoire et CPU UCD-SNMP (`memTotalReal`,
    `memAvailReal`, `memBuffer`, `memCached`, `ssCpuIdle`), interfaces
    IF-MIB (`ifHCInOctets`, `ifHCOutOctets`, erreurs et rejets, étiquettes
    `ifName` et `ifIndex`) ;
  - *synology* : en plus, SYNOLOGY-SYSTEM/DISK/RAID/UPS/SERVICES/STORAGEIO
    (`systemStatus`, `temperature`, `powerStatus`, ventilateurs,
    `modelName`, `version`, `upgradeAvailable`, `cpuUtilization`,
    `memUtilization`, onduleur, disques par `diskID`, volumes par
    `raidName`, services par `serviceName`, E/S par `storageIODevice`).
  Noms et étiquettes de **snmp_exporter** (textes en métrique « info »,
  valeur 1), `job="snmp"`, `instance` = équipement rattaché, plus `up` et
  `snmp_scrape_duration_seconds` : les 13 règles Synology importées et le
  tableau « Synology » marchent sans changement. 2 000 lignes par table au
  plus ; textes rendus lisibles (UTF-8, 128 caractères).
- **Sondes de certificats** : poignée de main TLS (certificat lu sans être
  validé : seule l'échéance compte), toutes les 5 min ;
  `ssl_cert_not_after_timestamp_seconds`, `probe_success`,
  `probe_ssl_last_chain_info{subject, issuer}`, `probe_duration_seconds`,
  `job="certs"`, `path` = adresse : tableau « Certificats TLS » et règle
  `CertificatIllisible` inchangés.
- **Secrets** : mots de passe SNMP **chiffrés en base** (`secrets`, usage
  `collecteur.snmp`), jamais réaffichés (vides à la modification =
  gardés), déchiffrés seulement à l'envoi de la configuration au
  collecteur (HTTPS, même ETag) ; secret illisible = rien n'est envoyé. Sur
  le collecteur : fichier de cache `0600`, utilisateur dédié. Un secret
  encore chiffré reçu par le collecteur est refusé.
- Adresses du LAN seulement (IP privée ou nom d'hôte simple) ; cibles et
  sondes d'un équipement détaché retirées de la configuration.
- Interface : page du collecteur › *SNMP v3 et sondes de certificats*.

**À vérifier sur le NAS** (les OID `cpuUtilization` et `memUtilization`
dépendent de la version de DSM) : commande dans `PROGRES.md`.

**Coût mesuré** : 16 µs de traitement par relevé Synology (hors réseau), une
dizaine d'échanges UDP par minute et par équipement ; sondes : une
connexion TLS toutes les 5 min par service. Binaire du collecteur : 8,8 Mo
(+1 Mo avec gosnmp). RAM : quelques dizaines de Ko par relevé.

**Menaces** : comptes SNMP en lecture seule conseillés ; jamais d'écriture
SNMP (pas de SET dans le code) ; secrets chiffrés au repos et absents de
l'audit (seule la valeur chiffrée y passe) ; serveur compromis : peut lire
les mots de passe SNMP (comme toute configuration qu'il envoie) mais pas
faire écrire le collecteur sur un équipement ; sonde vers un service du LAN
seulement.

### 23.16 Partie 6 faite (14/11) : client UniFi natif **[Fait, à brancher sur le contrôleur]**

Réponse 3 du 13/11 : client Go natif, unpoller retiré (−26 Mo sur le
collecteur).

- Lecture seule de l'API « stat » du contrôleur UniFi Network : sites,
  appareils (`stat/device`), clients (`stat/sta`), santé (`stat/health`).
  Aucune dépendance ajoutée (HTTP et JSON de la bibliothèque standard).
- Accès : **compte local en lecture seule** (UniFi OS : `/api/auth/login`
  puis chemins `/proxy/network/…` ; contrôleur logiciel : `/api/login`,
  essayé si le premier n'existe pas) ou **clé API** (UniFi OS, en-tête
  `X-API-KEY`). Secrets chiffrés en base, déchiffrés à l'envoi au
  collecteur (comme le SNMP).
- Certificat **vérifié** par défaut ; certificat fait maison : **empreinte
  SHA-256 épinglée** (vérifiée à chaque connexion, session reprise
  comprise : `VerifyConnection`, signalé par gosec et corrigé avant la
  livraison). Pas de proxy, pas de redirection suivie, réponses de 16 Mio
  au plus ; adresse du LAN seulement.
- Métriques aux **noms et étiquettes d'unpoller** (`job="unifi"`, `instance`
  = équipement du contrôleur) : `unpoller_device_info`, `_uptime_seconds`,
  `_cpu_utilization_ratio`, `_memory_utilization_ratio`,
  `_temperature_celsius{temp_area}`, `_stations{station_type}`,
  `_receive/transmit_bytes_total`, `_upgradeable`, ports
  (`_port_*{port_num, port_name}` : octets, erreurs, rejets, vitesse, PoE),
  radios (`_radio_channel_utilization_total_ratio{radio}`), clients
  (`unpoller_client_*{name, mac, wired, essid, ap_name, radio}`),
  `unpoller_site_disconnected{subsystem}`, plus `up`. Règles UniFi
  importées (dont `UnifiMuet`) et tableau « UniFi » inchangés. 300
  appareils, 1 000 clients, 64 ports par appareil au plus.
- Interface : page du collecteur › *Contrôleur UniFi*.

**Coût mesuré** : 16 µs de traitement par site (3 appareils, 2 clients ;
quelques centaines de µs pour un site de 50 clients), 5 requêtes HTTPS par
minute. Remplace unpoller (26 Mo).

**Menaces** : compte en lecture seule (le client ne fait que des GET après
la connexion) ; contrôleur usurpé sur le LAN : certificat vérifié ou
épinglé ; secrets chiffrés au repos ; réponse piégée : taille bornée,
décodage typé, nombres illisibles ignorés, textes nettoyés.

### 23.17 Fournisseurs DNS du défi ACME (demande du 14/11) **[Validé le 16/11]**

Demande : « rajouter des fournisseurs proposés par ACME », sans délégation
par CNAME (réponse « 2. Non »).

- Choix dans *Réglages du certificat* : **deSEC** (défaut, comme avant),
  **Cloudflare**, **OVHcloud**, **Gandi LiveDNS**, **Infomaniak**, **CSC**
  (Domain Manager, ajouté le 14/11 à la demande). Ce sont
  des fournisseurs courants des clients ACME (lego, acme.sh) avec une API
  publique stable et des jetons limitables.
- Code : interface `FournisseurDNS` (poser / retirer le TXT
  `_acme-challenge…`), un client HTTPS écrit à la main par fournisseur
  (`internal/server/certsyslog/dns.go`). **Aucune dépendance ajoutée** :
  une bibliothèque multi-fournisseurs (lego) ajouterait des dizaines de
  modules pour 5 appels.
- Signature OVH : SHA-1 imposé par leur API (`$1$` + SHA-1 de
  secret+clé consommateur+méthode+URL+corps+heure). Ce n'est pas un choix
  de chiffrement de mirador (`#nosec` justifié).
- Identifiants : champs non secrets (point d'accès et clé d'application
  OVH) en clair dans les réglages ; secrets chiffrés (JSON, usage
  `certsyslog.dns`), jamais réaffichés ni envoyés au collecteur. Secret
  laissé vide = gardé si le fournisseur ne change pas ; changer de
  fournisseur demande ses identifiants. L'ancien jeton deSEC est toujours lu
  (rien à refaire).
- Droits conseillés : Cloudflare « Zone › DNS › Modifier » sur la seule
  zone ; OVH : GET/POST/DELETE sur `/domain/zone/<zone>/*` ; Gandi : jeton
  d'accès personnel limité au domaine ; Infomaniak : jeton `domain:read`,
  `dns:write` ; CSC : clé d'API + jeton Bearer, droits GET `/zones` et POST
  `/zones/edits` sur la zone (API activée par le chargé de compte CSC).
- CSC : chaque changement est une « modification de zone » publiée par CSC
  (jusqu'à ~15 min). Le serveur suit son état (`/zones/edits/status/<id>`,
  toutes les 10 s) puis attend 3 min avant la validation ; démarche bornée
  à 30 min. Une modification « en attente d'approbation » est signalée
  (approbation manuelle à retirer pour le compte API). L'adresse d'état est
  reconstruite sur la base connue : le jeton ne part jamais vers une
  adresse donnée par une réponse. Premier essai conseillé sur l'annuaire *staging* de
  Let's Encrypt.

**Coût** : quelques appels HTTPS par certificat (tous les 60 jours) ;
mémoire et CPU négligeables, binaire du serveur à peine plus gros.

**Menaces** : vol des identifiants DNS → ils restent sur le serveur,
chiffrés ; conseiller des jetons limités à la zone (le serveur ne peut pas
vérifier la portée chez le fournisseur). Réponse piégée d'une API : lue au
plus 1 Mo, décodage typé. Pas de redirection vers une autre adresse
configurable : seules les adresses officielles sont codées.

---

## 24. Modèles de surveillance **[Décidé le 14/11 — parties A, B et C faites, validées le 16/11]**

Demande du 14/11 : « dire : cette machine est une machine Linux, donc tu fais
ces checks » ; « si je veux les certificats, on les lui attribue depuis
l'interface » ; détection automatique qui propose les modèles. Réponses :
modèles **cumulables**, attribuables à une **machine** ou à un **groupe**,
**seuils réglables par machine**, à faire **avant** la suite de l'étape 6 ;
une règle de modèle **remplace** la règle importée équivalente pour les
machines qui portent le modèle.

### 24.1 Ce qu'est un modèle

Un fichier JSON (fournis : `internal/server/modeles/fournis/*.json`, comme les
tableaux) :

| Champ | Rôle |
|---|---|
| `nom`, `titre`, `description`, `icone` | affichage |
| `detection` | expression PromQL qui renvoie une série par `instance` où le modèle s'applique (ex. `count by (instance) (node_docker_up)`) : sert aux propositions |
| `config` | clés de la configuration distante des agents (valeurs typées, vérifiées par le code de l'agent ; jamais de commande) |
| `parametres` | seuils nommés (nombre, unité, défaut, bornes), réglables par groupe puis par machine |
| `regles` | règles d'alerte ; `$parametre` à droite d'une comparaison ; `remplace` : nom de la règle importée équivalente |

### 24.2 Attribution et assemblage

- Table `modeles_attribues` (portée `machine:ID` ou `groupe:NOM`, modèle,
  état `actif` ou `ignore`, paramètres). Modèles d'une machine = ceux de son
  groupe + les siens, moins ceux qu'elle ignore. Paramètres : machine > groupe
  > défaut du modèle.
- Configuration des agents : défaut → **modèles** (par ordre de nom) → groupe
  → machine. Un réglage fait à la main l'emporte toujours sur un modèle.
- Séries internes écrites chaque minute par le serveur, pour chaque machine
  approuvée : `mirador_modele_parametre{instance, modele, parametre}` (valeur
  effective) et `mirador_machine_regle_remplacee{instance, regle}`.
- Règles d'un modèle : posées une fois au démarrage (origine `modele:NOM`,
  groupe `modele_NOM`), modifiables ou désactivables dans « Règles » comme
  les autres. `$p` devient `on(instance) group_left()
  mirador_modele_parametre{modele="…", parametre="p"}` : la règle ne voit que
  les machines qui portent le modèle, chacune avec son seuil.
- Remplacement : une règle importée (`vmalert:…`) nommée dans un `remplace`
  est évaluée avec `unless on(instance) mirador_machine_regle_remplacee{regle=…}`
  : elle se tait pour les machines qui portent le modèle, continue pour les
  autres. Effet sur la comparaison avec vmalert : écart attendu (même
  problème, nom de règle différent).

### 24.3 Détection et propositions

L'expression `detection` de chaque modèle est évaluée (1 fois par minute au
plus, en cache) ; un modèle détecté, ni attribué ni ignoré, est **proposé**
sur la page de la machine (« Ajouter » / « Ignorer ») et compté dans la
liste des machines. Rien n'est attribué sans validation.

### 24.4 Parties

| Partie | Contenu |
|---|---|
| A | serveur : modèles fournis (Linux, Physique, Docker, Proxmox, Certificats, ZFS, RAID logiciel), attribution machine et groupe, paramètres par machine, assemblage, séries internes, règles et remplacement, détection, interface |
| B | agent : catalogue de checks (HTTP(S), port TCP, processus, fichier récent, taille de dossier, DNS, ping) et modèles « Serveur web », « Sauvegarde » |
| C | modèles personnels créés ou modifiés dans l'interface |

### 24.5 Coût et menaces

- Coût : une lecture SQL par configuration assemblée ; séries internes
  ≈ machines × paramètres (une vingtaine par machine) ; détection : 7
  requêtes instantanées par minute au plus.
- Menaces : un modèle ne contient que des valeurs typées (vérifiées comme
  toute configuration d'agent) et des règles PromQL (même lecteur borné que
  les autres règles) ; attribution réservée aux admins (CSRF, audit) ;
  paramètres bornés (min, max) ; `$p` seulement après un opérateur de
  comparaison (vérifié au chargement) ; un modèle ne peut pas rendre une
  machine muette (rien n'y est désactivé par défaut).

### 24.6 Partie A faite (14/11)

- 7 modèles fournis : Linux (systemd, mises à jour ; seuils CPU 90 %,
  charge 2 par cœur, mémoire 10 %, disque 15 %, inodes 10 % : ceux des
  règles importées), Machine physique (températures 80 °C, disques 55 °C,
  ventilateurs, SMART), Docker (mémoire des conteneurs 90 %), Proxmox VE
  (invités : CPU 95 %, mémoire des LXC 95 %), Certificats (21 et 7 jours),
  ZFS, RAID logiciel. 12 règles de modèle ; chacune remplace l'équivalente
  (importée de vmalert, ou fournie pour Proxmox).
- Page de la machine, bloc « Modèles de surveillance » : proposés
  (détectés) avec « Ajouter » / « Ignorer », appliqués (d'où ils viennent,
  seuils de la machine, « Retirer »), « Ajouter un modèle », ignorés avec
  « reprendre ». Pastille « N modèle(s) proposé(s) » et colonne « À
  valider » de la liste des machines. Page « Modèles » (onglet de
  Machines) : catalogue, règles, machines qui portent ou où le modèle est
  détecté, attribution aux groupes et seuils du groupe.
- Code 2FA pour ajouter, retirer, régler les seuils (change la
  configuration de l'agent et les alertes) ; « Ignorer » sans code.
- Mesures : configuration assemblée = 2 lectures SQL de plus ; séries
  internes = ~1 + nombre de paramètres par modèle et par machine (Linux :
  6 séries + 5 « remplacée ») ; détection = 7 requêtes instantanées,
  passées par le cache des requêtes de l'interface (1 min).

### 24.7 Services des modèles (demande du 14/11) **[Validé le 16/11]**

- Chaque modèle a une liste de services systemd attendus (départ : Docker
  → `containerd`, `docker` ; Proxmox → `pve-cluster`, `pvedaemon`,
  `pveproxy`, `pvestatd`), modifiable dans Machines › Modèles (code 2FA,
  audit `modele_services`, réglage `modeles.services.NOM`). Noms vérifiés
  (`.service` ou `.timer`), 100 au plus.
- Sur les machines du modèle : ajoutés à `systemd_surveiller` (surveillés
  même en validation service par service) ; série interne
  `mirador_modele_service{instance, modele, name}`.
- Règle fournie `ServiceDuModeleAbsent` : service attendu que l'agent ne
  voit pas (pas installé, désinstallé, nom faux) pendant 15 min. Un
  service en échec ou arrêté reste signalé par les règles des services
  (pas de doublon).

### 24.8 Interface simplifiée (demande du 14/11 : « plus simple ») **[Validé le 16/11]**

1. **Mode administration** : après un code 2FA juste, les actions
   sensibles passent sans nouveau code pendant 10 min (comme le « sudo » de
   GitHub) ; bandeau « Mode administration jusqu'à HH:MM » avec « Quitter » ;
   champs du code masqués. Toujours un code frais pour la sécurité des
   comptes (mot de passe, 2FA, codes de secours, passkeys, utilisateurs,
   page Sécurité). Le mode tient dans la session (mémoire) : fermé à la
   déconnexion et au redémarrage du serveur. Menace : poste laissé ouvert →
   10 min au plus, sessions courtes inchangées.
2. **Page de la machine** : une case par modèle (« ★ détecté », « du
   groupe », « auto », « ignoré »), un seul « Enregistrer » ; un modèle
   détecté laissé décoché devient « ignoré » (plus proposé) ; décocher un
   modèle du groupe le retire pour la machine seule. Seuils sur une page à
   part (`/machines/NOM/modeles/MODELE`, lien « Seuils… »).
3. **Application automatique** (réglage `modeles.auto`, non par défaut) :
   une fois par minute, les modèles détectés (ni appliqués ni ignorés) sont
   appliqués (auteur `auto`, audit), signalés par les canaux de
   notification (un message par machine et par modèle, au plus une fois par
   heure) et marqués « auto » ; décochés, ils ne reviennent pas.
4. **Page « Modèles »** : tableau modèles × groupes avec un seul
   « Enregistrer », nombre de machines et de détections sans attribution ;
   détail replié par modèle (règles, services, seuils par groupe).

### 24.9 Retours du 14/11 : 100 modèles, services détectés, durée du mode administration **[Validé le 16/11]**

- **Beaucoup de modèles** : le tableau modèles × groupes et la liste des
  cases d'une machine ont le filtre et le tri communs à tous les tableaux
  (champ « Filtrer » dès 6 lignes, tri par colonne). Les détails restent
  repliés. Avec les modèles personnels (partie C), une « famille » (Système,
  Matériel, Applications, Réseau) pourra ranger la liste si besoin.
- **Services** : l'agent voit les services de chaque machine (tous, ou les
  services validés en mode « un par un »). La liste d'un modèle reste
  choisie par l'admin, mais la page « Modèles » **suggère** les services vus
  sur **toutes** les machines du modèle et **pas** sur toutes les autres
  (propres au modèle), hors services de base (systemd-*, ssh, cron, dbus…)
  et déjà listés, 20 au plus : cases cochées, « Ajouter à la liste ».
- **Durée du mode administration** réglable dans Réglages › Sécurité
  (`mode_admin`) : 0 = désactivé (code à chaque action sensible), 1 min à
  1 h, défaut 10 min. Changer ce réglage demande un code frais (sécurité).

### 24.10 Partie B : checks faits par l'agent **[Validé le 16/11 — « Fais la b », 14/11]**

Un modèle peut avoir des **listes** (adresses, ports, fichiers…) : chaque
ligne devient un **check** fait par l'agent de la machine, étiqueté avec le
nom du modèle. Les règles du modèle filtrent sur `modele="…"`.

| Type | Ce que fait l'agent | Mesures (étiquettes `type, nom, modele, cible`) |
|---|---|---|
| `http` | GET d'une adresse http(s), sans proxy, 3 redirections au plus, 64 Kio lus au plus ; code 2xx/3xx ; texte attendu (`contient=`) ; certificat vérifié sauf `non-verifie` | `mirador_check_ok`, `_duree_secondes`, `_http_code`, `_certificat_fin_timestamp_seconds` |
| `tcp` | connexion ouverte puis refermée | `_ok`, `_duree_secondes` |
| `processus` | processus de ce nom (`/proc/PID/comm`), mémoire résidente totale | `_processus_nombre`, `_processus_memoire_octets` |
| `fichier` | le fichier, ou le plus récent d'un motif `*` (sauvegardes datées) : date et taille (rien n'est lu dedans) | `_fichier_age_secondes`, `_fichier_taille_octets` |
| `dossier` | taille et nombre de fichiers (sans suivre les liens), 200 000 entrées au plus, toutes les 15 min | `_dossier_taille_octets`, `_dossier_fichiers`, `_dossier_incomplet` |
| `dns` | résolution avec les serveurs DNS de la machine | `_dns_adresses` |
| `ping` | un écho ICMP par socket « datagramme » (sans root : `net.ipv4.ping_group_range`) | `_ok`, `_duree_secondes` |

- **Modèles fournis** (5 de plus, 12 en tout) : *Serveur web* (adresses ;
  temps max 2000 ms, certificat 14 jours ; détecté si nginx, apache2,
  httpd, caddy, traefik ou lighttpd tourne), *Sauvegarde* (fichiers,
  dossiers ; âge max 26 h, taille min 1 Mo ; détecté si borgmatic, restic,
  rsnapshot, duplicati, urbackup, Proxmox Backup Server ou bacula tourne),
  *Ports TCP*, *Processus* (nombre min 1), *Joignabilité* (ping, noms DNS ;
  latence max 200 ms). 13 règles.
- **Listes** : saisies par groupe (page « Modèles ») et par machine (page
  « Réglages… » du modèle), une valeur par ligne, `#` = commentaire ; la
  liste de la machine remplace celle du groupe (même vidée) ; identique à
  celle du groupe : rien n'est enregistré (reste héritée). Pastille « à
  remplir » sur la machine quand toutes les listes sont vides. Migration
  0016 : colonne `listes` dans `modeles_attribues`.
- **Envoi** : clé `checks` de la configuration distante (liste typée,
  vérifiée par le code de l'agent : `DisallowUnknownFields`, format de
  chaque cible). Envoyée seulement aux agents qui annoncent la capacité
  `checks` ; sinon la page le signale (« agent trop ancien »).
- **Rythme** : tâche de fond de l'agent, toutes les 60 s (dossiers 15 min),
  8 checks à la fois au plus, délai 10 s par check (30 s au plus) : un site
  lent ne retarde pas la collecte.

**Coût** (banc d'essai `BenchmarkPassage`, 40 checks : 10 http, 10 tcp, 10
processus sur le vrai /proc, 10 fichiers) : **6,5 ms par passage** toutes les
60 s, soit ~0,01 % d'un cœur ; 1,8 Mo de mémoire temporaire par passage
(rendue par le ramasse-miettes) ; aucune écriture disque. Le check
`processus` relit /proc à chaque fois (≈ 1 ms pour 500 processus) : à
regrouper si beaucoup de checks de ce type. Séries : 2 à 5 par check, 50
checks au plus par machine. Côté serveur : un calcul de plus à chaque
configuration assemblée. Objectifs agent (< 30 Mio, < 1 % d'un cœur)
inchangés ✅.

**Menaces** :
- Jamais de commande : 7 types fixés dans le code de l'agent, cibles
  vérifiées (adresse http(s) sans identifiant, hôte:port, chemin absolu sans
  `..`, nom de processus de 15 caractères).
- Un serveur compromis pourrait faire **sonder le réseau local** par les
  agents (ports, adresses) : 50 checks au plus, une requête par minute et
  par check, résultat réduit à « ok / durée / code » (le contenu des pages
  n'est jamais renvoyé ; un fichier n'est jamais lu, seules sa date et sa
  taille). Refus local possible : ligne `MIRADOR_CHECKS=non` dans
  `/etc/mirador-agent/agent.conf` (fichier de root, le serveur ne peut pas
  le changer) ; l'agent ignore alors la clé `checks`.
- Fichiers et dossiers : l'agent ne voit que ce que son utilisateur
  (`mirador-agent`, sans droits) peut lire.
- Les cibles apparaissent dans les mesures (étiquette `cible`) : l'interface
  rappelle de n'y mettre aucun secret ; une adresse avec identifiant est
  refusée.
- `non-verifie` : choisi par l'admin pour un certificat maison ; la date de
  fin reste mesurée et alertée.

### 24.11 Partie C : modèles personnels **[Validé le 16/11 — « Fais la partie c », 14/11]**

Choix faits sans question (valeurs par défaut, à corriger si besoin) :

- **Créer** un modèle vide (« Nouveau modèle »), **dupliquer** un modèle
  livré (règles renommées avec le suffixe « Perso », filtre
  `modele="…"` des règles réécrit pour la copie), ou **importer** le JSON
  exporté d'un autre serveur. Tout modèle (livré ou personnel) s'**exporte**
  en JSON.
- Un modèle livré ne se modifie pas : il est mis à jour avec mirador. Pour
  l'adapter : le dupliquer (ne pas porter les deux sur les mêmes machines :
  alertes en double).
- **Formulaire** sans JavaScript : titre, description, détection (PromQL),
  services attendus, tableaux des listes, des seuils et des règles (lignes
  vides pour ajouter, nom vidé pour retirer) ; configuration des agents en
  JSON dans « Avancé ». Après une erreur, la page est réaffichée avec la
  saisie.
- **Rangement** : table `modeles_perso` (migration 0017), JSON du modèle,
  relu au démarrage et après chaque changement. Même format et mêmes
  contrôles que les modèles livrés. Un modèle abîmé, ou dont le nom est pris
  plus tard par un modèle livré, est laissé de côté (journal du serveur) ;
  les autres servent.
- **Règles** : posées à l'enregistrement (origine `modele:NOM`) ; une règle
  désactivée dans « Règles » le reste ; une règle retirée du modèle est
  retirée de la base. Le remplacement des règles importées (`remplace`) est
  pris en compte sans redémarrage (le moteur d'alertes consulte la liste à
  chaque évaluation).
- **Suppression** refusée tant que le modèle est attribué (machines ou
  groupes cités) ; sinon modèle, règles, lignes « ignoré » et liste de
  services retirés.

**Coût** : un modèle fait 1 à 5 Ko (64 Kio au plus), 200 modèles personnels
au plus : < 1 Mo en mémoire dans un cas courant (13 Mo au pire). Détection :
une requête instantanée par minute et par modèle qui en a une (cache du
lecteur). Moteur d'alertes : une lecture de table en mémoire de plus par
règle et par évaluation (négligeable). Aucun coût côté agent (mêmes clés de
configuration et mêmes checks qu'en partie B).

**Menaces** :
- Réservé aux admins (code 2FA ou mode administration, CSRF, audit
  `modele_perso` et `modele_perso_supprime` avec le JSON avant/après).
- Contenu typé seulement : configuration vérifiée par le code de l'agent
  (jamais de commande), règles et détection par le lecteur PromQL borné,
  JSON lu avec refus des champs inconnus, tailles bornées (64 Kio, 30
  seuils, 10 listes, 50 règles, 200 modèles).
- Un modèle personnel ne peut pas **prendre le nom** d'un modèle livré, ni
  celui d'une règle existante (sinon il pourrait remplacer
  `AgentMuet` par une règle qui ne sonne jamais) ; une règle qui filtre
  les tests d'un autre modèle est refusée (elle ne sonnerait jamais).
- Un import ne remplace jamais un modèle livré.
- Les pages échappent tout le texte saisi (gabarits Go) ; le nom du fichier
  exporté vient du nom du modèle (minuscules, chiffres, « - »).

## 25. Design « A bis » et thème par utilisateur **[Validé le 16/11 — choix du 14/11 : « À bis les deux »]**

Propositions montrées en maquette (A épuré, B compact, C doux, D
technique, A bis = A avec le menu noir) ; choix : **A bis**, avec le mode
jour/nuit **par utilisateur**.

### 25.1 Design

- Gris neutres (texte #18181B, fond #F7F7F8 ; nuit : fond #0C0D10, cartes
  #15161A), **un seul bleu** (#3E63DD, nuit #7C9BFF) ; vert, orange et rouge
  réservés aux états.
- **Menu noir** à gauche (#111318), jour comme nuit : page active sur fond
  gris foncé en blanc, compteurs en orange ou rouge doux, recherche
  « Aller à… » dans un champ sombre.
- Une seule police (Inter, déjà embarquée) ; titres plus petits (22 px,
  graisse 650) ; chiffres alignés (`tabular-nums`).
- Cartes sans ombre, trait fin, coins de 12 px ; en-têtes de tableau
  discrets ; onglets en segments ; boutons et champs de 36 px.
- Mode ombre : la grande bande jaune devient une petite pastille en haut à
  droite de chaque page (lien vers les réglages des notifications).
- Rien n'a changé dans les pages elles-mêmes (mêmes gabarits, mêmes
  formulaires) : seule la feuille de style et le cadre (`base.html`).

### 25.2 Thème par utilisateur

- Sélecteur **Jour / Nuit / Auto** en bas du menu, sous le compte (Auto =
  réglage de l'ordinateur ou du téléphone). Menu replié : une seule icône
  qui passe au choix suivant à chaque clic.
- Le thème change tout de suite (attribut `data-theme`, courbes
  redessinées), puis le choix est enregistré dans le compte
  (`POST /compte/theme`, migration 0018 : colonne `theme` des
  utilisateurs) : le même choix sur tous ses appareils. Sans JavaScript, le
  formulaire enregistre et revient à la page.
- Le thème du serveur (Réglages › Interface) reste la valeur des comptes qui
  n'ont rien choisi.

**Coût** : une colonne en base (lue avec le compte, déjà en cache 10 s),
une requête à chaque changement de thème ; feuille de style de même taille
(~40 Ko) ; aucune police de plus (Space Grotesk n'est plus utilisée).

**Menaces** : préférence d'affichage seulement (pas d'audit) ; jeton CSRF
obligatoire (formulaire ou en-tête `X-CSRF`) ; valeur parmi `clair`,
`sombre`, `auto` ; retour après le formulaire limité à un chemin du site
(`//…`, `https://…`, `\` refusés → « / »).

---

## 26. Aides allégées (demande du 14/11, choix « A b d ») **[Validé le 16/11]**

Demande : « épurer dans les réglages et ailleurs ce qui est affiché en tant
qu'aide ». Maquettes A+B+D et C montrées ; choix : **A + B + D**.

- **A — une ligne** : chaque aide tient en une phrase. Le détail part dans
  `docs/aide.md` ; un lien « En savoir plus › » y mène (dépôt public, ouvert
  dans un nouvel onglet).
- **B — bouton par utilisateur** : « Avec aides / Sans aides » en bas du
  menu, au-dessus de Jour / Nuit / Auto. Retenu dans le compte (colonne
  `utilisateurs.sans_aides`, migration 0021), appliqué tout de suite par
  `mirador.js`, sans JavaScript par le formulaire.
- **D — relecture** : 143 `<small>` relus. Environ 60 sont des données
  (dates, états, rôles, valeurs) : inchangés. 81 aides (`class="aide"`)
  raccourcies ; 3 retirées (cellules « — », renvoi à la SPEC). Durées par
  défaut écrites simplement (« 30 min », « 7 j », « 1 an » : fonction
  `dureeC`).
- **Avertissements toujours affichés** (pas de classe `aide`) : votre
  adresse et la confirmation sous 2 min, clé publique (machines déjà
  installées), délier avant de réinstaller, base GéoIP effacée, attribution
  DB-IP (licence CC BY), bascule InfluxDB, « rien de secret » dans les
  listes, import (« *** » et ajouts), alertes en double d'une copie de
  modèle.

**Coût** : une colonne en base, une requête par clic, quelques lignes de
CSS et de JavaScript ; pages un peu plus légères.

**Menaces** : préférence d'affichage (pas d'audit) ; jeton CSRF obligatoire ;
valeur `avec` ou `sans` seulement ; retour limité à un chemin du site (même
contrôle que le thème). Liens de doc : adresse fixe dans le code.

---

## 27. Retours du 14/11 au soir **[Validé le 16/11]**

- **Champ « Mode administration » rempli par le gestionnaire de mots de
  passe** : le libellé contenait « code 2FA », pris pour un champ de code à
  usage unique. Libellé : « Durée du mode administration », sans « code »
  ni « 2FA » autour ; `autocomplete="off"` et marques « à ignorer »
  (Bitwarden, LastPass, 1Password).
- **Mode administration après une reconnexion** : il était lié à la
  session, donc fermé par la déconnexion. Il s'ouvre maintenant à la
  connexion, puisque le second facteur (code, passkey ou code de secours)
  vient d'être donné, et le bandeau s'affiche. Même durée réglée ; 0 = pas
  de mode administration. Menace : aucune nouvelle, la connexion vaut un
  code juste ; « Quitter » le ferme toujours.
- **Modèles des machines déjà en place** : un modèle détecté est coché
  d'office sur la page de la machine (« ★ détecté · à enregistrer ») et
  appliqué à l'enregistrement ; décoché, il n'est plus proposé. La page
  Modèles liste les modèles détectés pas encore cochés et propose de les
  cocher d'un coup (code 2FA ou mode administration, audit).
  L'application automatique (désactivée par défaut) ne change pas.
- **Le modèle gère la surveillance et les métriques** (demande : « il
  faudrait que cela soit enlevé dans tous les cas ») : décocher un modèle
  (machine, groupe, ou suppression d'un modèle personnel) arrête ses
  alertes **et** coupe pour la machine les relevés qu'il allume (clés
  `docker_metriques`, `systemd`, `mises_a_jour`, `temperatures`… mises à
  `false` dans la configuration de la machine), même s'ils étaient réglés
  avant les modèles. Une clé allumée par un autre modèle encore porté reste
  allumée. Recocher le modèle lève la coupure : il rallume ses relevés. Les
  tests de l'agent (listes) partent avec le modèle ; l'historique reste.
  Chaque changement de configuration passe par l'audit habituel.
- **Règles complètes dans les modèles** (« il manque des règles ») : chaque
  modèle livré porte maintenant les règles de son domaine ; chacune
  remplace, sur les machines du modèle, la règle importée ou fournie
  équivalente (pas d'alerte en double ; les machines sans le modèle gardent
  l'ancienne règle). 19 règles ajoutées :
  - Docker (6) : démon injoignable, conteneur arrêté, redémarrages en
    boucle (seuil réglable), pas en bonne santé, tué par manque de mémoire,
    mémoire ;
  - Linux (13) : + services en échec, service automatique arrêté, service
    en boucle, swap intensif, disque bientôt plein, I/O saturée, erreurs
    réseau, redémarrage ;
  - Machine physique (4) : + SMART en échec, secteurs réalloués ;
  - Certificats (4) : + collecte muette, certificats illisibles ;
  - ZFS (1) : pool dégradé ; RAID logiciel (1) : grappe dégradée.
- **Services attendus** : services systemd qui doivent exister et tourner
  sur toutes les machines du modèle (ex. `docker.service`). Suggestions
  corrigées : seulement les services **actifs** (plus les « oneshot » du
  démarrage), présents sur toutes les machines du modèle et sur **aucune**
  autre, hors services de distribution (liste étendue) ; 10 au plus ;
  **plus cochés d'office** ; affichés en liste.
- **Toutes les règles liées aux modèles** (choix du 15/11 : « 1. Oui,
  2. auto ») :
  1. Règle de modèle **sans seuil** : limitée aux machines qui portent le
     modèle (`… and on (instance) mirador_machine_modele{modele="…"}`) ;
     avant, elle s'appliquait partout (alerte en double avec la règle
     importée sur les machines sans le modèle).
  2. Linux : + pare-feu non restrictif (et relevé du pare-feu allumé), trafic
     anormal (seuil réglable, défaut 800 Mbit/s).
  3. **Modèles d'équipement**, repris des règles importées (mêmes
     expressions, mêmes textes, étiquette `via=collecteur` gardée) :
     **NAS Synology** (15 règles, détecté par `systemStatus{job="snmp"}`),
     **OPNsense** (12, `pf_entries`), **UniFi** (7, `unpoller_device_info` ;
     regroupements gardant `instance`). Champ `auto` : appliqués **dès la
     détection**, même si l'application automatique générale est coupée ;
     décochés ensuite, ils ne reviennent pas.
  4. Page Règles : « Règles importées remplacées par un modèle ». Une règle
     **ne sert plus** si chaque machine ou source où son modèle est détecté
     le porte ; sinon « sert encore » avec la liste. Bouton « Retirer les N
     règles qui ne servent plus » (code 2FA, audit ; export YAML pour revenir
     en arrière). Seules les règles importées de vmalert sont concernées.
  Restent générales : Watchdog, AgentMuet, les « … muet » (OPNsense,
  Proxmox, UniFi, Syslog) et CertificatIllisible (sondes du collecteur).
  Coût : quelques requêtes de détection en plus par minute (une par modèle).
- **Une seule source pour les règles d'un modèle : le modèle** (choix du
  15/11) :
  - au démarrage, `modeles.Synchroniser` remet à jour les règles des
    modèles livrés (une amélioration d'une version arrive seule), garde le
    choix « active / désactivée », et retire les règles d'un modèle livré
    qui n'existent plus (ou d'un modèle disparu) ; remplace la pose unique
    d'avant ;
  - page Règles : une règle de modèle est marquée « modèle X » (lien vers
    le modèle) ; elle se **désactive** ou se **réactive** ici, mais ne se
    modifie ni ne se supprime (renvoi au modèle ; copie personnelle pour un
    modèle livré) ;
  - côté serveur : `alertes.Enregistrer` refuse d'écraser une règle de
    modèle depuis une autre source (import YAML, formulaire) ; les
    modèles personnels gardent leurs règles à jour à leur enregistrement.
  Menace : aucune nouvelle (mêmes contrôles et audit). Coût : une
  comparaison des règles au démarrage.
- **Passkey Bitwarden sur iPhone (`NotAllowedError`)** : la même passkey
  marche sur le PC, et Bitwarden iPhone marche sur webauthn.io (qui demande
  `userVerification: "preferred"`). La connexion sans mot de passe demandait
  `"required"`, que Bitwarden iPhone semble refuser. Elle demande maintenant
  `"preferred"` ; la sécurité ne change pas : le serveur refuse toujours une
  réponse sans le drapeau « utilisateur vérifié » (test
  `auth/passkey_test.go`). À confirmer sur l'iPhone.
- Coût : nul (mêmes requêtes que l'affichage existant).

---

## 28. Pages Alertes et Audit lisibles (maquettes validées le 15/11) **[Validé le 16/11]**

**Alertes** (recommandations 1, 2, 3, 4, 6) :
- 4 tuiles : critiques, avertissements, infos, « nouvelles depuis ta
  dernière visite » (heure de la visite précédente gardée dans le
  navigateur, rien sur le serveur) ;
- alertes en cours **groupées par machine** (ou par règle, ou en liste) ;
- sur chaque ligne : **Acquitter**, **Silence** (1 h, 4 h, 24 h ; même
  sélecteur que le silence en un clic), **Graphique** (fiche de l'alerte) ;
- **mini-courbe des 6 dernières heures** avec le seuil en pointillé : la
  partie gauche de la comparaison de la règle (en passant « and/unless »
  des règles de modèle) est relue sur 6 h au pas de 5 min, la série qui
  correspond aux étiquettes de l'alerte est tracée ; la partie droite donne
  le seuil (nombre, ou seuil de la machine pour une règle de modèle). Pas de
  courbe pour une règle qui n'est pas une simple comparaison (« a or b »).
  30 courbes au plus par affichage ;
- fiche d'une alerte : courbe sur 24 h ;
- bloc rechargé toutes les 30 s (fragment), nombre d'alertes dans le titre
  de l'onglet.

**Audit** (recommandations 1, 2, 3, 4, 6, 7, plus 5 et 8) :
- actions en **français** (dictionnaire des codes ; code inconnu affiché
  lisiblement), rangées en **catégories** de couleur (Sécurité, Comptes,
  Machines, Modèles, Règles, Alertes, Notifications, Mises à jour,
  Réglages) ;
- **seulement ce qui a changé** (documents JSON aplatis et comparés clé par
  clé, 8 lignes au plus) ; le JSON complet reste dans « Détail » ;
- lignes groupées par jour, séparation bien marquée (bandeau de couleur
  avec « Aujourd'hui · dimanche 15/11 », « Hier · … » ou la date, retour du
  15/11) ; liens vers la règle, le modèle, la machine,
  les utilisateurs ;
- filtres : catégorie, qui, période (24 h, 7 j, 30 j, tout), recherche,
  actions automatiques (mirador, auto, agents ; masquées par défaut, leur
  nombre est affiché), **sensibles seulement** (pastille « sensible » : 2FA,
  passkeys, comptes, adresses autorisées, clés publiques, dépôt, export…) ;
- **export CSV** de la vue filtrée (10 000 lignes au plus ; cellules
  commençant par = + - @ préfixées pour qu'un tableur n'exécute rien).

**Historique des alertes** (recommandations 7 et 8, demandées le 15/11) :
- **frise** sur 24 h (graduations toutes les 3 h) ou 7 jours (une par
  jour) : une ligne par machine (les 25 qui ont eu le plus d'alertes), une
  barre par alerte, du passage en alerte à la fin (ou maintenant), couleur
  de la gravité, infobulle (règle, heure, durée), clic → fiche de l'alerte ;
  nom de la machine → historique filtré ;
- **les plus bruyantes** : 10 règles qui ont sonné le plus souvent sur la
  période (fois, machines, durée totale), lien « Régler les seuils du
  modèle » ou « Régler la règle » ;
- lecture : une requête SQL par affichage (alertes qui chevauchent la
  période, 5 000 au plus), SVG en attributs (pas de style en ligne : CSP).

**Conditions « en attente »** (retour du 15/11 : « les alertes semblent mal
calculées ») : une règle avec un délai (ex. MisesAJourSecuriteEnAttente,
72 h) a une condition vraie avant d'être une alerte. Elle n'était pas
comptée (juste) mais apparaissait dans « Alertes en cours » (trompeur).
Elle est maintenant dans un bloc à part « En attente » (vue d'ensemble,
page Alertes, page de la machine), avec « deviendra une alerte dans … si
elle dure » ; la pastille « N alerte(s) » de la machine ne la compte plus.

**Coût** : page Alertes : une requête de plage par alerte en cours (30 au
plus) toutes les 30 s quand la page est ouverte ; audit : mêmes requêtes
qu'avant, filtres en SQL.

**Menaces** : aucune nouvelle donnée affichée (l'audit montrait déjà
avant/après) ; actions d'alerte réservées aux administrateurs, jeton
CSRF ; retour après action limité à un chemin du site ; SVG construit de
nombres et de couleurs fixes ; export CSV protégé contre les formules.

---

## 29. Page d'une machine et graphiques (maquette validée le 15/11) **[Validé le 16/11]**

**Page d'une machine** — ordre : en-tête, alertes en cours et en attente,
**En ce moment**, graphiques, **Surveillance et modèles** (repliable, ouvert
d'office s'il y a quelque chose à valider ou un modèle détecté), détails,
modifier.
- **En ce moment** : CPU, mémoire, disque le plus plein (point de montage et
  place libre), swap, charge par cœur, durée depuis le démarrage. Couleur
  selon le seuil du modèle Linux de la machine (ses valeurs par défaut s'il
  n'est pas porté) : vert, orange à 80 % du seuil, rouge au seuil.
  Quelques requêtes instantanées par affichage.
- Sommaire (sections, surveillance, logs) et **boutons de période**.
- Graphiques sur **2 colonnes** (1 sur téléphone) ; panneaux sans données
  regroupés en une ligne « Pas de données sur la période : … » ; sections
  repliées retenues dans le navigateur.
- **Seuil du modèle sur la courbe** (pointillé rouge) : champ
  `seuil_modele` d'un panneau (modèle, paramètre, `inverse` pour « % libre »,
  `facteur` d'unité) ; la valeur vient des seuils effectifs de la machine
  (`seuil` dans le JSON de la courbe, seulement pour une seule machine).
  Serveurs Linux : CPU, charge, mémoire, trafic. Un seuil plus de 2 fois
  au-dessus des valeurs n'est pas tracé (il écraserait la courbe).

**Graphiques, partout** :
- légende en français (« Heure : 15 nov. 10:18 »), pastilles cliquables
  pour masquer une courbe ; axe des temps en français ;
- aire légère sous une courbe seule, trait plus épais ;
- axe depuis zéro ; 0 à 100 % fixe pour les pourcentages ; libellés de
  l'axe jamais coupés ;
- **survol synchronisé** entre les courbes de la page ;
- **zoom** en glissant (le rechargement automatique attend alors) ;
  double-clic pour revenir.

**Accents** des titres et descriptions des tableaux fournis (« Mémoire
utilisée », « Réseau », « Unités en échec »…).

**Coût** : navigateur seulement pour les graphiques ; page machine : 7 à 8
requêtes instantanées de plus.

**Menaces** : aucune nouvelle donnée ; nom de machine contrôlé avant d'entrer
dans une requête ; pas de style en ligne (CSP).

---

## 30. Acquittement : prises en charge, durée, par qui, escalade (choix du 15/11) **[Validé le 16/11]**

- **Défaut corrigé** : la page lisait l'état « acquittée » dans la mémoire du
  moteur d'alertes, qui ne le reçoit pas : une alerte acquittée semblait ne
  pas changer. Il est maintenant lu en base (une requête par affichage).
- **Durée** (migration 0022, colonne `acquittee_jusqua`) : bouton
  « Acquitter ▾ » : **4 h** (défaut, aussi pour le bouton Telegram),
  **24 h**, **jusqu'à la fin** (ancien fonctionnement, gardé pour la ligne de
  commande). À l'échéance, l'aiguillage lève la prise en charge (audit
  `acquittement_expire`) et envoie **un rappel tout de suite** si l'alerte
  dure.
- **Prises en charge** : bloc à part, grisé (vue d'ensemble, page Alertes,
  page machine), « par X à HH:MM, jusqu'au … » ; hors des compteurs et des
  tuiles ; bouton **Reprendre** (de nouveau à traiter, audit
  `acquittement_repris`). La couleur d'une machine dans le parc compte
  toujours ses alertes prises en charge.
- **Escalade** (option, **coupée par défaut**) : réglage par gravité dans
  Notifications › Routage ; une alerte toujours pas prise en charge après ce
  délai part **aussi sur le canal de secours**, une fois, marquée « ⏫
  Escalade ».

**Coût** : une requête de plus par affichage ; une requête par tour de
l'aiguillage pour les échéances.

**Menaces** : actions réservées aux administrateurs (jeton CSRF) ;
audit de chaque prise en charge, reprise et échéance ; durée bornée aux
choix proposés (24 h au plus depuis l'interface).


## 31. Métriques et graphiques : 11 améliorations (demande du 15/11) **[Validé le 16/11]**

Livrées en trois fois. Maquettes de 1, 7 et 9 validées le 15/11.

### 31.1 Livraison 1 **[Validé le 16/11]**

- **Alertes et événements sur les courbes d'une machine** (page machine
  seulement, paramètre `annoter=1`) :
  - alertes de la période en **bandes** (orange : avertissement, rouge :
    critique, bleu : info), avec un liseré en haut ;
  - événements en **traits gris pointillés** : redémarrage (valeurs de
    `node_boot_time_seconds` dans la période), nouvelle version de l'agent
    (première apparition d'une version de `mirador_agent_info`),
    changements faits dans l'interface pour la machine (audit, hors actions
    automatiques) ; 20 au plus ;
  - noms **au survol** seulement (ligne sous la courbe), pour ne pas
    charger le dessin.
- **Âge des données** : pastille dans le titre de chaque courbe de la page
  machine, verte « il y a 25 s », orange « dernière donnée il y a 12 min »
  au-delà de 5 minutes (heure de `timestamp(mirador_agent_info)`). Sur les
  tableaux : pastille orange seulement, quand la courbe s'arrête.
- **Disques : « plein dans X j »** sur les barres « Occupation des systèmes
  de fichiers » (option de panneau `prevision_disque`) et, pour le disque le
  plus pressé, dans le parc de la vue d'ensemble. Calcul :
  place libre ÷ pente de la place libre sur **7 jours** (`deriv`), affiché
  seulement **sous 30 jours** et si la place diminue ; rouge sous 7 jours.

**Coût** :

| Ajout | Coût |
|---|---|
| Annotations | 3 petites requêtes PromQL + 2 lectures SQLite par machine et période, gardées 30 s (toutes les courbes d'une page les partagent) |
| Prévision des disques | 2 requêtes, dont une sur 7 jours : au plus **44 ms, 28 Mio passagers** pour 50 systèmes de fichiers (mesuré sur points bruts ; la base agrégée en a 5 fois moins) ; gardée **10 min** (1 min si rien à prévoir) |
| Navigateur | dessin sur le même canevas uPlot, pas de bibliothèque en plus |

**Piège évité** : une lecture qui remonte à plus de 7 jours est servie par
la base **agrégée** (un point par 5 min, en retard, vide au début). La
pente est donc calculée sur **7 jours moins 1 heure** (`[167h]`, reste
dans la base brute), et la place libre actuelle lue à part puis divisée
dans le serveur (juste aussi si la conservation brute est réglée plus
courte). Test de non-régression avec deux bases.

**Menaces** : lecture seule ; nom de machine vérifié (mêmes caractères
permis que les filtres) avant d'entrer dans une requête ; requêtes SQL
paramétrées ; noms d'événements écrits en texte (`textContent`), jamais en
HTML. Les phrases de l'audit montrées au survol sont celles de la page
Audit, réservée aux mêmes comptes.

### 31.2 Livraison 2 **[Validé le 16/11]**

Petite barre d'outils en haut à droite de chaque courbe :

- **« −7 j »** (courbe seule) : la même période **il y a 7 jours** en
  pointillé gris (paramètre `comparer=1` : une seconde requête sur la
  période décalée, mêmes instants). Pas pour les aires empilées ni au-delà
  de 7 jours de période.
- **« ⤢ » Agrandir** : la carte passe en plein écran, Échap pour revenir.
- **PNG** : image de la courbe avec titre et période (canevas copié dans
  le navigateur, rien côté serveur).
- **CSV** : valeurs de la période pour un tableur français (« ; » entre
  colonnes, virgule décimale, UTF-8 avec BOM), fait dans le navigateur.

Et :

- **Aires empilées** (option de panneau `empile`) : deux courbes ajoutées à
  « Ressources » : **CPU par mode** (user, system, iowait, steal… en % de
  la machine) et **Répartition de la mémoire** (utilisée, cache, libre ; le
  haut de la pile = mémoire totale). La légende montre la valeur propre de
  chaque part. Cache = min(Cached + Buffers, MemAvailable) : pas de
  nouvelle métrique dans l'agent.
- **Zoom partagé et dans l'adresse** : glisser sur une courbe zoome
  **toutes** les courbes de la page et écrit `de=…&a=…` dans l'adresse
  (`history.replaceState`) ; le lien copié rouvre la même vue ; double-clic
  pour revenir. La période était déjà dans l'adresse (`periode=`).

**Coût** : « −7 j » double la requête de la courbe concernée, seulement
quand on clique ; les deux courbes empilées ajoutent 2 requêtes par page
machine (environ 1 ms chacune sur une machine) ; plein écran, PNG, CSV et
zoom : navigateur seulement.

**Menaces** : rien de nouveau côté serveur hors `comparer` (même
contrôle des filtres) ; noms des fichiers exportés nettoyés (lettres,
chiffres, tirets) ; cellules CSV entre guillemets si besoin ; les valeurs
`de` et `a` de l'adresse sont lues comme des nombres entiers seulement.

### 31.3 Livraison 3 **[Validé le 16/11]**

- **Les plus gourmands** (page machine, sous « En ce moment ») : les
  5 conteneurs Docker qui prennent le plus de **processeur** (part de toute
  la machine, sur 5 min) et de **mémoire** (octets et part de la mémoire de
  la machine). Carte absente sans conteneurs. Les **processus** ne sont pas
  relevés par l'agent : les ajouter demanderait de lire `/proc/*/stat` à
  chaque passage (plus de CPU sur chaque machine) ; pas fait sans accord.
- **Le parc sur 24 h** (vue d'ensemble, sous le parc) : carte de chaleur,
  une ligne par machine, une case par heure, **processeur** et **mémoire**
  côte à côte ; 6 teintes (< 20 %, 20, 40, 60, 75, ≥ 90 %), valeur au survol,
  plus forte heure en bout de ligne ; 100 machines au plus.
- **Écran mural** (`/mur`, lien « Écran mural ↗ » au-dessus du parc) :
  sans menu, thème sombre, grands caractères ; compteurs (critiques,
  avertissements, muettes, machines), 8 alertes en cours au plus (ou « Tout
  va bien »), une tuile par machine (état, CPU, mémoire, disque, « plein
  dans X j ») ; rechargé toutes les 30 s (`<meta http-equiv="refresh">`,
  pas de script).

**Coût** :

| Ajout | Coût |
|---|---|
| Plus gourmands | 3 petites requêtes par page machine (cache commun de 20 s) |
| Carte de chaleur | 2 requêtes sur 24 h, pas d'une heure : **46 ms, 26 Mio passagers** pour 50 machines (4 cœurs, 8 modes) ; gardée **5 min** |
| Écran mural | les mêmes données que la vue d'ensemble, une fois toutes les 30 s par écran |

**Menaces** : lecture seule, réservé aux comptes connectés (même droit que
la vue d'ensemble) ; un écran mural garde une session ouverte : utiliser
un compte en **lecture seule** pour lui (la session expire comme les
autres) ; pas de formulaire ni d'action sur la page ; nom de machine
vérifié avant d'entrer dans une requête.

## 32. Certificats : réglage général et avancement (demande du 15/11) **[Validé le 16/11]**

- **Réglages › Certificats** (nouvelle page) : le compte DNS et
  Let's Encrypt (fournisseur, zone, jeton, annuaire), **communs à tous les
  collecteurs**, quittent la page du collecteur. La page liste aussi tous
  les collecteurs : nom du certificat, « où en est-on », fin, bouton
  « Renouveler », et l'avancement pas à pas.
- **Page du collecteur** : seulement ce qui est à lui (écoute, nom du
  certificat, émetteurs), l'avancement, « Renouveler maintenant », un lien
  vers Réglages › Certificats et un message si le compte DNS n'est pas réglé.
- **Avancement** (6 pas) : compte DNS réglé → syslog réglé sur le
  collecteur → demande du collecteur reçue → Let's Encrypt (étape en
  cours parmi 6 : compte, commande, TXT posé, attente de publication
  « jusqu'à HH:MM », vérification, signature) → certificat obtenu →
  installé sur le collecteur, écoute ouverte (confirmé par ses mesures
  `mirador_collecteur_syslog_ecoute` et la fin du certificat chargé). En
  échec : l'étape fautive, le message et l'heure du nouvel essai.
  La page se recharge seule toutes les 15 s tant qu'une étape avance (pas
  pendant la saisie d'un champ).
- Migration 0023 : colonnes `etape`, `etape_le`, `etape_jusqua` ; la
  démarche ACME note chaque étape (contexte de suivi), effacées une fois le
  certificat obtenu, gardées en cas d'échec.
- La commande d'installation affichée reprend le nom choisi (`--nom`).
- **Publication DNS vérifiée** (demande du 16/11) : après la pose du TXT,
  le serveur interroge directement les serveurs DNS qui font autorité pour
  la zone (6 au plus), toutes les 10 s, et demande la vérification à
  Let's Encrypt dès que l'enregistrement est visible partout (« visible sur
  1/2 serveurs DNS » dans l'avancement). Le délai réglé dans Réglages ›
  Certificats devient un **maximum** (défaut 3 min, CSC 20 min ; 10 s à
  30 min). Serveurs DNS injoignables depuis le serveur (port 53 sortant
  fermé) : attente fixe comme avant (1 min, CSC 3 min), dite dans
  l'avancement.

**Coût** : une écriture SQLite par étape (6 par certificat, tous les
60 jours) ; 2 petites requêtes de mesures par collecteur à l'affichage des
pages. Rechargement seulement pendant une démarche.

**Menaces** : mêmes droits qu'avant (lecture pour voir, admin + code 2FA
pour régler et renouveler) ; secrets DNS toujours chiffrés et jamais
réaffichés ; « Renouveler » vérifie que la cible est bien un collecteur ;
messages d'erreur ACME tronqués à 500 caractères et affichés en texte.

## 33. Page Logs refaite (maquette validée le 16/11) **[Validé le 16/11]**

- **Filtres rangés** : machine, niveau (dont « err et plus grave » et
  « warning et plus grave »), période, un champ « Rechercher » (mots
  entiers ; `"texte exact"` entre guillemets) ; le reste dans « Plus de
  filtres ». Filtres actifs en **pastilles** retirables (bleu : inclus,
  rouge : exclu). Anciennes adresses (`mots=`, `contient=`) toujours lues.
- **Exclusion** (« − exclure ») faite par le moteur de logs (nouveau
  champ `Exclus` de la requête) : la limite de lignes reste juste ; un
  bloc dont toutes les valeurs sont exclues n'est pas lu.
- **Raccourcis** : les 4 fournis et les **recherches enregistrées** de
  chaque utilisateur (★, 30 au plus, migration 0024 ; paramètres connus
  seulement, revérifiés ; préférence personnelle sans code 2FA).
- **Histogramme** des lignes trouvées (60 tranches, erreurs en rouge,
  avertissements en orange, SVG sans style en ligne) ; glisser = zoom
  (`de`, `a` dans l'adresse).
- **Lignes compactes** : heure seule, séparateur par jour, erreurs et
  avertissements teintés, mots cherchés surlignés, répétitions repliées.
  **Détail au clic** : tous les champs avec « ＋ filtrer / － exclure »,
  « Voir autour », copier la ligne, copier en JSON, copier le lien.
- **Messages regroupés** : IP, nombres, identifiants, hexadécimal
  remplacés (`IP`, `N`, `UUID`, `HEX`) ; tri plus fréquents / rares /
  erreurs ; « Ces lignes » filtre sur le message type.
- **Contexte** (« Voir autour ») : 20 lignes avant et après sur la même
  machine (toutes sources, ±1 h), « 20 de plus ».
- **Mode direct** : `/logs/direct` toutes les 5 s, lignes ajoutées en haut
  (construites avec `textContent`), pause au survol ou ligne ouverte,
  arrêt seul après 30 min.
- **Export** CSV (« ; », BOM, cellules commençant par = + - @ protégées)
  ou texte, 10 000 lignes au plus. « Charger plus ancien » pour la suite.
- La courbe « volume par machine » reste sur la page de chaque machine.

**Coût** : une recherche lit 2 000 lignes au plus (au lieu de 300 à 2 000)
pour l'histogramme et les regroupements ; calcul mesuré **44 ms et 4 Mio
passagers** pour 2 000 lignes. Mode direct : une petite recherche (15 min
au plus) toutes les 5 s par onglet actif, 30 min au plus. Toujours
2 recherches en même temps au plus.

**Menaces** : contenu des logs toujours échappé (gabarits, JSON,
`textContent`) ; valeurs des filtres et exclusions vérifiées (mêmes
caractères permis qu'avant, 10 exclusions au plus) ; recherches
enregistrées réduites aux paramètres connus et liées à leur utilisateur
(suppression vérifiée) ; formulaires avec jeton CSRF ; export protégé
contre les formules de tableur.

## 34. Audit du 16/11 : sécurité, graphique, téléphone, application installable **[Validé le 16/11]**

Demande du 16/11 : audit du projet (sécurité, design, graphique), failles de
l'interface et de la connexion, puis améliorations pour le mode application
sur téléphone. Réponse : « fait tout ». Aucune faille critique ni haute
trouvée ; ce qui suit corrige les faiblesses relevées.

### 34.1 Connexion

- **Verrou de compte** : les comptes qui existent ont leur propre table
  (`auth/verrou.go`). Avant, 10 000 échecs sur des noms inventés (environ
  2 000 adresses, soit un bloc IPv6 /48) chassaient le compteur d'un vrai
  compte pas encore verrouillé (mesuré : 14 échecs oubliés). Les noms
  inconnus sont comptés à part, mêmes seuils, mêmes messages : la réponse ne
  dit toujours pas si un nom existe.
- **Passkey seule** : 10 défis au plus par adresse (ou /64), le plus ancien
  de l'adresse est remplacé. Avant, 2 000 demandes d'une seule adresse
  bloquaient ce mode de connexion 5 min pour tout le monde.

### 34.2 Console

- **Code 2FA ou mode administration** maintenant demandé pour : adresse
  d'ingestion (reprise dans la commande d'installation lancée en root),
  règles (modifier, désactiver, importer ; l'essai et « activer » restent
  libres), maintenances, inhibitions, dépendances, silences de plus de 24 h,
  adresse de vmalert, essai de connexion Olvid, export avec les secrets
  chiffrés (devenu un POST).
- **Import de la partie « securite »** : nouveau code 2FA, toujours, comme la
  page Sécurité (avant : le mode administration suffisait).
- Le code est demandé dans une **boîte** au moment d'envoyer (boutons
  `data-code`), sans perdre la saisie ; sans JavaScript, le serveur répond
  « code 2FA demandé » et le mode administration s'ouvre depuis « Mon
  compte » (`POST /mode-admin/ouvrir`). Un code absent n'est pas compté
  comme un essai manqué.
- **Secret gardé et adresse changée** : refusé pour ntfy (même serveur
  exigé, le sujet peut changer), e-mail, Olvid, UniFi (adresse ou empreinte)
  et SNMP. Le secret doit être ressaisi : il ne part plus vers une nouvelle
  adresse par « Tester ».
- **Rôle avant le corps** : un compte lecture est refusé avant la lecture de
  la requête (jusqu'à 31 Mio sur la publication d'une version).
- **Adresse de vmalert** : ni `?` ni `#`.
- **Secrets chiffrés** remplacés par `***` dans l'audit des configurations
  d'agents et dans l'export « sans secret » ; à l'import, `***` reprend la
  valeur actuelle (équipement retrouvé par son champ `machine`).
- **Rôle lecture** : le sujet ntfy n'est plus montré.
- **Export CSV d'une courbe** : cellules commençant par `= + - @` protégées.
- **Import « interface »** : ligne d'audit, mode debug jamais importé.

### 34.3 Application installable (PWA)

Manifeste (`statique/mirador.webmanifest`), icônes PNG 192 / 512 /
masquable / iOS 180 (`scripts/generer-icones.py`), couleur de barre, zones
sûres de l'écran (encoche). Raccourcis : Alertes, Machines, Logs.

**Pas de service worker** : il n'est plus nécessaire pour installer
l'application, et sans lui aucune page connectée ne peut rester en cache
sur le téléphone. Page « hors ligne », notifications push et pastille sur
l'icône : option non faite, à décider (une table, un canal, des clés VAPID).

### 34.4 Téléphone

- Barre de navigation en bas (Accueil, Alertes, Machines, Logs, « Plus »
  qui ouvre tout le menu) ; en-tête sur une ligne ; bandeau du mode
  administration sur une ligne.
- Écran tactile (`pointer: coarse`) : champs à 16 px (sinon iOS zoome à la
  saisie), boutons de 44 px.
- Tableaux marqués `data-cartes` affichés en cartes sous 640 px (titre de
  colonne copié dans `data-label` par `mirador.js`) ; `data-cartes="serrees"`
  pour les longues listes (règles).
- Page d'une machine : à la première visite sur téléphone, seule la
  première section est ouverte ; les exports PNG / CSV restent sur grand
  écran.
- **Courbes demandées à l'affichage** (`IntersectionObserver`, 400 px
  d'avance), puis rafraîchies seulement tant qu'elles sont à l'écran.
- Courbes : dernière valeur dans la légende ; aide affichée une seule fois
  par page, adaptée au doigt. Correction de l'audit : sur écran tactile, le
  zoom « glisser » de uPlot ne gênait pas le défilement (il ne réagit qu'à
  la souris) ; seule l'aide était fausse.
- Graphique : nom « mirador » visible sur la page de connexion (thème
  clair), textes de 12 px au moins (hors carte de chaleur), « 1 machine » /
  « 3 machines » (`accord`, `accorderTexte`), colonne « Vu il y a ».

### 34.5 Connexion depuis un lien

- **Page demandée gardée** (`suite`) : après la connexion on arrive sur la
  page du lien (alerte d'une notification), plus sur l'accueil.
- **Lien ouvert depuis un autre site** (Telegram, e-mail) : le cookie
  `SameSite=Strict` n'est pas envoyé au premier passage. Une page relais
  recharge la même adresse « de l'intérieur » ; le cookie part alors et la
  page s'affiche sans reconnexion. Vérifié dans Chromium.
- Passkey proposée en premier sur écran tactile. La proposition automatique
  dans le champ « Nom » (WebAuthn conditionnel) n'est **pas** faite : non
  testable ici sur iPhone, et la connexion par passkey y a déjà été fragile.

### 34.6 Machine muette (retour du 16/11)

« Un agent muet ne devrait rien remonter d'autre. » Cause : le serveur
écrivait `mirador_modele_service` (services attendus par un modèle) même
pour une machine muette ; l'état des services n'arrivant plus, la règle
`ServiceDuModeleAbsent` les croyait tous désinstallés.

- Les services attendus ne sont écrits que pour une machine vue depuis
  moins de 3 min (`interne.go`). Plus court que le « pour » de la règle
  (15 min) : la série disparaît avant que l'alerte ne parte. Les autres
  séries du modèle restent écrites (règles importées toujours remplacées).
- Affichage : les alertes inhibées ou dont le parent est en panne vont dans
  un bloc replié « Masquées par une panne », hors compte. Silences et
  maintenances restent dans la liste.

**Coût** (objectif serveur < 256 Mio, < 5 % d'un cœur : inchangé) :

- mémoire : une table de plus pour les noms inconnus, bornée à 10 000
  compteurs (environ 1 Mo au pire, vide en temps normal) ; rien d'autre ;
- CPU : page d'une machine, **4 courbes demandées au chargement au lieu de
  13** sur un écran 1440 × 900, **0 sur téléphone** avant défilement
  (mesuré dans Chromium, serveur de démonstration) ;
- disque : aucune écriture ajoutée ; binaire + ~60 Ko (icônes 45 Ko,
  manifeste, styles et scripts).

**Menaces** :

- page relais : seulement pour afficher une page (GET, navigation, document)
  sans cookie, vers un chemin de ce site vérifié (`suiteLocale`) ; une
  écriture venue d'un autre site reste refusée (origine + jeton CSRF + cookie
  Strict). Le cookie n'est pas affaibli. Ce que le relais change : un lien
  d'un autre site ouvre la page connectée, comme avec un cookie « Lax » ;
  aucun GET ne modifie d'état ;
- `suite` : chemin local seulement, jamais les pages de connexion ni les
  adresses des scripts, 512 caractères au plus, gardé dans la session ;
- boîte du code 2FA : simple confort, tout est vérifié par le serveur ;
- manifeste et icônes : publics, sans donnée ; pas de service worker, donc
  pas de cache de pages ni de code qui survit à une mise à jour.

**Non fait, noté** : copie d'un secret SNMP chiffré vers la portée d'un
autre collecteur par l'import (admin avec code) ; assistant root : le
fichier ouvert est le chemin trouvé, pas le chemin réel vérifié (course
possible seulement pour qui écrit déjà dans `/etc/…`, et seuls le nom et
les dates d'un certificat sortent) ; actions GitHub non figées par empreinte.

## 35. Notifications push et pastille de l'icône (demande du 16/11) **[Validé le 16/11]**

Suite de § 34.3 (« fais ceci : notifications push, pastille sur l'icône »).

### 35.1 Choix

- **Canal « application »**, comme les autres : il peut être principal ou
  secours (décision du 10/11 : un seul canal à la fois). En plus, réglage
  **Copie** (coché par défaut) : quand un autre canal porte le message, les
  appareils abonnés le reçoivent aussi. Sans copie, la pastille ne serait
  à jour que si l'application était le canal principal.
- La copie **ne compte pas pour la bascule** ni pour la file d'attente : un
  téléphone éteint n'est pas une panne. Sans appareil abonné, rien n'est
  fait ni noté. Elle est notée dans l'historique des envois (« … (copie) »).
- Mode ombre : rien ne part, comme pour les autres canaux.
- **Chaque compte abonne ses appareils** (« Mon compte »), 10 au plus ; code
  2FA ou mode administration. Tous les comptes actifs reçoivent les mêmes
  alertes (pas de choix par règle ou par personne).
- **Pastille** : nombre d'alertes actives non prises en charge, envoyé dans
  chaque notification ; remise à jour à chaque page ouverte (`push.js`).

### 35.2 Technique, sans dépendance

Paquet `internal/server/webpush` (~250 lignes, bibliothèque standard) plutôt
qu'une bibliothèque : chiffrement RFC 8291 / 8188 (ECDH P-256, HKDF,
AES-128-GCM), jeton VAPID RFC 8292 (ES256). Coût d'une dépendance évité :
licence à vérifier, code tiers dans le serveur.

- Clé VAPID du serveur : créée au premier besoin, clé privée chiffrée en
  base avec la clé des secrets (`notif.push.vapid`).
- Table `abonnements_push` (migration 0025) : adresse du service de push,
  clés de l'appareil, origine, nom d'appareil (« Chrome sur Android », tiré
  du navigateur ; le texte complet n'est pas gardé).
- `sw.js` (2,5 Ko), servi à la racine : affiche la notification, règle la
  pastille, ouvre la page au toucher. **Aucune interception de requête,
  aucun cache** (testé : pas de `fetch`, pas de `caches`).
- Abonnement refusé par le service (404 / 410) : effacé ; 30 échecs de
  suite : effacé aussi.

### 35.3 Coût

- CPU : **0,15 ms et 17 Ko passagers par appareil et par notification**
  (chiffrement + jeton, mesuré) ; 4 appareils servis en même temps.
- RAM : rien de permanent. Disque : une ligne par appareil, une écriture
  par envoi réussi (date du dernier envoi).
- Réseau : une requête HTTPS sortante par appareil et par notification.
- Binaire : + ~9 Ko de scripts ; aucune dépendance ajoutée.

Objectif serveur (< 256 Mio, < 5 % d'un cœur) : inchangé.

### 35.4 Menaces

- **Adresse de l'abonnement fournie par le navigateur** (donc par
  l'utilisateur, même en lecture seule) : le serveur n'appelle que les
  services de push connus (`fcm.googleapis.com`, `*.push.apple.com`,
  `*.push.services.mozilla.com`, `*.notify.windows.com`, `jmt17.google.com`),
  en HTTPS, port 443, sans redirection, et jamais une adresse interne
  (contrôle de l'adresse réellement jointe, comme ntfy). Pas de SSRF.
- **Contenu** : chiffré pour l'appareil ; le service de push ne voit que
  l'adresse de l'interface (champ « sub » du jeton), l'heure et la taille.
- **Session volée** : s'abonner demande le code 2FA ou le mode
  administration ; abonnement et retrait sont dans l'audit (sans l'adresse
  de l'abonnement, qui vaut un secret et n'est jamais affichée ni
  journalisée).
- **Base volée** : sans la clé des secrets, pas de clé VAPID, donc pas
  d'envoi possible vers les appareils.
- **Compte désactivé ou supprimé** : plus servi ; abonnements effacés avec
  le compte.
- **Notification piégée** : le lien ouvert au toucher est toujours un
  chemin de ce site (`sw.js` refuse `//hôte` et `\`).
- **Service worker** : même origine, script embarqué, CSP inchangée ; sans
  gestion des requêtes, il ne peut ni garder ni modifier une page.
- **Avalanche d'essais** : une notification d'essai par session toutes les
  10 s.

### 35.5 Vérifications

- Vecteur de test de la RFC 8291 : corps chiffré identique à l'octet près
  (`TestVecteurRFC8291`).
- Hors dépôt : message déchiffré à l'identique par la bibliothèque
  `http_ece` (Node) et jeton vérifié par `jws`.
- Chromium : service worker actif à la racine, notification affichée à la
  réception d'un push, lien piégé ramené à « / », abonnement complet depuis
  « Mon compte » (appareil listé, marqué « cet appareil »).
- **Envoi réel accepté par le service de push de Google** (HTTP 2xx).
- **Non vérifié ici** : l'arrivée de la notification sur un vrai téléphone
  (iPhone, Android) et la pastille sur l'icône ; à faire chez toi.

### 35.6 Limites connues

- iOS : seulement dans l'application installée (iOS 16.4+). La pastille
  n'existe que pour une application installée (Android, iOS, ordinateur).
- Pas de notification « silencieuse » pour baisser la pastille : iOS exige
  qu'un push affiche quelque chose. Elle baisse au message « résolu » et à
  l'ouverture de l'application.
- Navigateur dont le service de push n'est pas dans la liste : refusé, avec
  le nom de l'hôte dans le message (à ajouter à `hotesPush`).


## 36. Page Machines plus claire (choix du 16/11 : B + C, page à part) **[Validé le 16/11]**

Maquettes A, B, C présentées le 16/11 ; choix : **B** (liste par site avec
filtres) + **C** (ajout guidé sur sa propre page). Les deux pastilles des
machines en attente sont acceptées.

### 36.1 Liste (`/machines`)

- En tête : nombre de machines, de sites, d'attentes ; bouton
  **+ Ajouter une machine** (administrateurs).
- **À approuver (N)** en haut, une ligne par machine (la plus ancienne
  d'abord) : nom modifiable, origine du nom, adresse et âge de la demande,
  site / type / groupe, « limiter à l'adresse », **Approuver** et
  **Refuser** côte à côte. 5 lignes au plus, puis « Voir les N autres »
  (`?attente=toutes`). Pas de « tout approuver » : approuver donne un token.
- Recherche (nom, site, type, groupe), filtres **Toutes / Problèmes /
  Agents / Collecteurs et équipements**, regroupement **Par site / Par
  groupe / Liste**. Tout est dans l'adresse
  (`/machines?vue=groupe&filtre=problemes&q=web`) : favori possible, pas de
  JavaScript. Valeurs inconnues ramenées au choix par défaut ; recherche
  bornée à 100 caractères.
- Un bloc repliable par site (ou groupe) : « N machines », « N problèmes »
  ou « tout va bien ». Problème = machine muette (plus de 5 min) ou mise à
  jour en échec. Dans un bloc : problèmes d'abord, puis par nom ; un
  équipement est placé sous son collecteur (« ↳ … relevé par … ») ; un
  collecteur remonte si l'un de ses équipements a un problème.
- Colonne « Adresse » retirée de la liste (elle reste sur la page de la
  machine). Refusées repliées en bas, « refusée » écrit en français.

### 36.2 Pastilles des machines en attente (indices, rien n'est bloqué)

- **nom à changer ?** : nom proposé par la machine (pas choisi au jeton)
  dans une liste fixe de noms d'hôte courants (`debian`, `ubuntu`,
  `localhost`, `raspberrypi`, `pve`…) ou commençant par `vps-`, `ip-`,
  `ubuntu-`, `debian-` (noms d'hébergeurs).
- **adresse inconnue** : l'adresse de la demande n'est dans le réseau
  (/24 IPv4, /64 IPv6) d'aucune machine approuvée (dernière adresse vue ou
  adresse d'enrôlement). Rien pour la première machine, ni pour une
  adresse illisible. Coût : nul (adresses déjà en base).

### 36.3 Ajout (`/machines/ajouter`, administrateurs)

- Étapes visibles : 1. Quoi ? (Machine Linux / Collecteur du LAN /
  Équipement sans agent, `?quoi=`), 2. Réglages (seulement les champs
  utiles ; envoi, mises à jour et durée du jeton repliés dans « Options »),
  3. Commande à coller (affichée **après** la création du jeton, bouton
  **Copier**), 4. Approuver (lien vers « À approuver »). Pour un équipement :
  token et réglages Telegraf affichés une seule fois, bouton Copier.
- Formulaires et contrôles inchangés (`POST /machines/jeton`,
  `POST /machines/equipement`, code 2FA) ; ils affichent maintenant cette
  page, et y reviennent en cas d'erreur.
- Noms **réservés** : `ajouter`, `jeton`, `equipement` (adresses de
  l'interface sous `/machines/`) refusés pour une machine (jeton, enrôlement,
  équipement, renommage). Sans cela la page d'une telle machine serait
  inaccessible.

### 36.4 Bug corrigé au passage

- Depuis § 35, `<body>` portait `data-alertes` (nombre d'alertes pour la
  pastille de l'icône). mirador.js le prenait pour une liste d'alertes : il
  triait et filtrait le premier tableau de la page, puis plantait. Effets :
  plus de cartes sur téléphone, plus de tri des tableaux, plus de bouton
  Copier sur la plupart des pages. Attribut renommé `data-nb-alertes`
  (test de non-régression).

### 36.5 Coût

- Tout est calculé à l'affichage, en mémoire, sur la liste des machines
  déjà lue (une requête SQL en plus : équipements → collecteur). Rien en
  base, aucune collecte. Mesure : voir PROGRES.md.

### 36.6 Menaces

- Recherche et filtres : lus dans l'adresse, ramenés à des valeurs
  admises, échappés par les gabarits ; jamais dans une requête SQL.
- Page d'ajout : réservée aux administrateurs (comme avant), jetons et
  tokens toujours affichés une seule fois, jamais gardés en clair.
- Les pastilles ne remplacent pas le jugement de l'admin : un attaquant
  qui a le jeton et une adresse « connue » n'en a pas. Elles aident
  seulement à voir l'inattendu.

## 37. Carte du parc, liste des versions, écran mural (demandes du 16/11) **[Validé le 16/11]**

### 37.1 « Le parc sur 24 h » avec beaucoup de machines (choix 1 + 2 + 3)

Avant : 100 machines au plus, les premières par ordre alphabétique (les
autres disparaissaient sans le dire), triées par nom, 20 px par ligne.

- **Tri par pic** sur 24 h, les plus chargées en haut (machines sans
  données en bas), dans chaque carte (processeur, mémoire).
- **10 lignes** d'office, puis « Voir les N autres » (mirador.js ; sans
  JavaScript, tout reste affiché).
- **Au-delà de 100** : les 100 plus chargées gardées, et « + N machines non
  affichées (les moins chargées) ».
- **Suit les filtres du tableau du parc** juste au-dessus (texte et
  « Afficher ») ; message si plus rien ne correspond. Une machine absente
  du tableau n'est montrée que sans filtre.
- Coût : aucune requête en plus (les deux requêtes de la carte, gardées
  5 min, renvoyaient déjà toutes les machines) ; un tri de quelques
  centaines de lignes.

### 37.2 Liste des versions à importer (5 dernières)

- « Importer une version signée » propose une **liste des 5 dernières
  versions** du dépôt, avec leur date ; « (déjà importée) » pour celles qui
  sont là ; la plus récente non importée est choisie d'office. Un champ
  « ou autre » garde la saisie d'une version plus ancienne.
- Source : flux Atom des Releases (`https://github.com/DÉPÔT/releases.atom`),
  **même hôte que les téléchargements** (rien à ouvrir de plus dans le
  pare-feu), sans clé d'API. Lu par le serveur, 1 Mo au plus, 4 s au plus ;
  gardé 10 min (1 min après un échec). Seulement pour les administrateurs.
- GitHub injoignable : message « liste indisponible » et le champ à taper,
  comme avant.
- Menaces : le flux n'est qu'une liste de noms ; chaque nom est vérifié
  (`vX.Y.Z`) avant d'être affiché ou utilisé, et la **signature** reste
  vérifiée à l'import. Un flux trafiqué peut au pire proposer un nom de
  version : l'import d'un binaire non signé par la clé est refusé.

### 37.3 Écran mural : rappel des alertes prises en charge

- Avant : les alertes acquittées n'y étaient pas, et l'écran affichait
  « Tout va bien » alors qu'il en restait.
- Maintenant : bloc **Prises en charge (N)** sous les alertes en cours,
  plus discret (bord pointillé, texte plus petit) : machine, alerte, par
  qui, jusqu'à quand, depuis quand ; 6 lignes au plus, puis « … et N en
  tout ». S'il n'y a que des alertes prises en charge : « Rien à traiter »
  au lieu de « Tout va bien ».
- Corrigé au passage : « depuis il y a 10 min » → « depuis 10 min ».

## 38. Réglages, Utilisateurs, Sécurité plus clairs (choix du 16/11 : R, N, U, S) **[Validé le 16/11]**

Maquettes présentées le 16/11 ; toutes retenues, barre d'enregistrement en
bas, dans la même pull request que § 36 et § 37.

### 38.1 Raccourcis retirés de la page Tableaux

- « Logs » et « Flux entrants bloqués » n'étaient que des liens vers des
  pages déjà dans le menu Logs : retirés. Le réglage `tableaux.pages`
  éventuellement en base est ignoré.

### 38.2 Bloc « Général » des notifications rangé à sa place

| Réglage | Avant | Maintenant |
|---|---|---|
| Mode (ombre / actif) | liste dans « Général » | bandeau en haut de Notifications, bouton « Activer l'envoi » / « Passer en ombre » (code 2FA) |
| Adresse de l'interface (lien dans les messages) | « Général » | Réglages › Interface, carte « Adresse de cette interface » |
| Fuseau des messages | « Général » (doublon) | **un seul fuseau**, celui de Réglages › Interface ; l'enregistrer aligne celui des messages (message tant qu'ils diffèrent) |
| Regrouper par (étiquettes) | « Général » | carte « Routage par gravité » |
| Règles « machine en panne » | « Général » | Alertes › Silences et maintenances › Dépendances |

- Stockage inchangé (réglages des notifications) : export / import YAML
  identiques. Le formulaire des notifications **garde** ces valeurs quand
  elles n'y sont plus (test : le formulaire de la page est renvoyé tel quel).

### 38.3 Notifications (N)

- Résumé en tête : principal et son état, secours (orange si aucun), file
  d'attente, heartbeat. Bandeau « Envoi des notifications » (mode).
- Une **carte par canal** : icône, état (« Actif · principal », « En
  panne », « Inactif »), une ligne utile, « Régler ▾ » (actif) ou
  « Configurer ▾ » (inactif) qui déplie ses champs, « Tester ».
- « Principal, secours, bascule » : vérifications et adresses privées
  repliées dans « Avancé ». Derniers tests et comparaison vmalert repliés.
- **Barre d'enregistrement collée en bas** (sur ordinateur) : « N
  changements non enregistrés », « Rétablir les valeurs par défaut » en
  simple lien, « Inverser », code 2FA, « Enregistrer ».

### 38.4 Accueil des Réglages (R)

- Chaque carte : icône et **état du moment** (pastille orange = à
  regarder) : canaux et mode ; place prise sur le plafond (jauge) ;
  dernière sauvegarde (orange si aucune ou plus de 2 jours) ; jours restants
  du certificat le plus proche de sa fin (orange sous 14 j) ; tailles des
  listes ; fuseau et adresses à renseigner ; dernier export (journal d'audit).
- « Ailleurs dans mirador » : une liste de liens (au lieu de 6 cartes),
  avec « 3 comptes » et « N points à regarder » (bilan de sécurité).

### 38.5 Utilisateurs (U)

- « + Nouveau compte » en haut ; formulaire de création sur une ligne.
- Résumé : comptes actifs, admins, sans 2FA, désactivés.
- Pastilles : rôle, 2FA (ou « à créer »), passkeys, « pas de codes de
  secours », état ; « vous » ; dernière connexion en « il y a… ».
- « Modifier ▾ » : un bouton par action (rôle, mot de passe, effacer la
  2FA, désactiver / réactiver, supprimer en rouge) au lieu d'une liste
  « Action » ; code 2FA toujours demandé, comme avant.

### 38.6 Sécurité (S)

- **Bilan** en tête : ✓ ce qui va bien, ! ce qui est à regarder (comptes
  sans 2FA, interface ouverte à toutes les adresses), avec un lien.
- Trois cartes : Sessions, Verrouillage, Accès. Les durées se saisissent
  dans l'**unité affichée** à côté du champ (« 30 » min, « 12 » h) ; une
  durée écrite (« 1h30m ») reste acceptée et s'affiche telle quelle.
- Barre d'enregistrement en bas, « Rétablir » en simple lien.
- Sessions : « cette session », heures en « il y a… », **« Fermer toutes
  les autres »** (journal d'audit : `sessions_fermees`).
- L'avertissement « Votre adresse… à confirmer sous 2 min » reste visible
  en mode « Sans aides ».

### 38.7 Corrigé au passage

- Conservation : colonne « Limites » coupée (formulaire trop étroit).
- Utilisateurs : plus de flèches de tri sur la colonne des actions.

### 38.8 Coût et menaces

- Affichage seulement : lectures en base à l'ouverture des pages (réglages,
  comptes, passkeys, une requête sur le journal d'audit). Rien de collecté,
  rien gardé en mémoire.
- Mêmes contrôles qu'avant : code 2FA (ou mode administration) pour les
  réglages, code 2FA à chaque action sur un compte ; le passage en mode
  actif reste refusé si le canal principal n'est pas configuré.
- « Fermer toutes les autres » : réservé aux admins, comme « Fermer », et
  ne ferme jamais la session qui le demande.

## 39. Site injoignable et page des coupures (demande du 16/11) **[Fait, à valider]**

Contexte : le 16/11 à 10:31, le site de l'utilisateur a perdu internet.
Toutes ses machines se sont tues ; mirador (sur le VPS) envoyait une alerte
`AgentMuet` par machine.

### 39.1 Un seul message pour un site injoignable

- **Machines d'un site** : agents et collecteurs approuvés, avec un site,
  vus dans les dernières 24 h ; les équipements relevés par un collecteur
  ne comptent pas (ils suivent déjà leur collecteur par la dépendance).
  Au moins 2 machines, sinon rien ne change.
- **Site injoignable** : chacune de ses machines a une alerte de panne
  active (réglage « règles de panne » : `AgentMuet`, `ProxmoxMuet`…).
  Calculé par l'aiguillage des notifications toutes les 5 s, à partir des
  alertes actives : pas de règle de plus, pas de métrique de plus.
- **Attente** : la panne d'une machine qui partage son site est annoncée
  2 min après son début (`AttenteSite`), le temps que les autres machines
  coupées en même temps aient aussi leur alerte (`AgentMuet` évaluée chaque
  minute). Coût : une machine seule en panne est annoncée 2 min plus tard.
- **Regroupement** : les pannes des machines d'un site injoignable passent
  dans un groupe « site » : **un message** « 📡 Site X injoignable » (une
  ligne : machines muettes, depuis quand, lien vers la page des coupures),
  puis les rappels habituels (2 h pour une critique).
- **Retour** : les machines reviennent à quelques secondes d'écart ; le
  message de retour attend jusqu'à 3 min (`AttenteRetourSite`) qu'elles
  soient toutes revenues : **un message** « ✅ Site X de nouveau
  joignable », durée de la coupure, machines revenues.
- Les alertes restent une par machine (page Alertes, prise en charge,
  historique) : seul l'envoi est regroupé. Titre court pour le push, ntfy
  et l'e-mail (« Site X injoignable (N machines) »).
- **Bandeau** sur l'accueil et la page Alertes tant que le site est
  injoignable, avec un lien vers la coupure.

| Choix | Raison | Coût |
|---|---|---|
| Calcul dans l'aiguillage, pas une règle | une règle passerait par une métrique écrite chaque minute : 1 à 2 min de retard, donc des messages par machine avant celui du site | 57 µs toutes les 5 s pour 300 machines toutes en panne (banc), ~0,001 % d'un cœur ; une requête SQL de plus par minute |
| Toutes les machines du site, pas « la plupart » | pas de faux « site injoignable » quand une machine sur trois redémarre | une coupure partielle (une machine sur le 4G) donne des messages par machine, comme avant |
| Machines vues dans les 24 h | une machine éteinte depuis longtemps ne bloque pas la détection | — |

Menaces : rien de nouveau exposé (calcul interne, pas de réglage, pas de
saisie). Le seul risque est de retarder une alerte de panne de 2 min : on
le garde court et seulement pour les machines qui partagent un site.

### 39.2 Page « Coupures » (Alertes › Coupures) **[Vérifiée le 18/11]**

Vérifiée sur la vraie coupure (16/11 10:30 → 18/11 10:11, environ 47 h 40) :
la page affichait une fin au 17/11 10:30 (23 h 59), fausse à cause du
défaut d'AgentMuet corrigé au § 45. La règle importée
`ProxmoxMuet` n'existe pas chez lui : rien à désactiver.

Limite connue (vue le 18/11) : `AgentMuet` lit `lag(node_uname_info[1d])`.
Au-delà de 24 h sans aucune donnée, la série sort de la fenêtre, `lag()`
ne renvoie plus rien et l'alerte passe « résolue » à tort (message
« Résolu », fin de coupure fausse). Sans effet le 16/11 (retour à 23 h 59).
Corrigée le 18/11 : § 45.

- **Coupures retrouvées dans l'historique des alertes** (30 jours) : au
  moins 2 machines d'un même site en panne à moins de 10 min d'écart.
  Marche aussi pour une coupure d'avant cette version (celle du 16/11).
  Machines sans site : regroupées sous « (sans site) ».
- **Bilan par machine**, lu dans la base de métriques :
  - relevés reçus pour la coupure (`node_uname_info`), comparés à l'heure
    d'avant : 0 % tant que la machine est muette, ~100 % une fois rattrapée ;
  - taille maximale de la file d'attente de l'agent
    (`mirador_agent_queue_bytes`), en % du tampon par défaut (8 Mo) ;
  - lots jetés (file pleine) : `mirador_agent_queue_dropped_total` et
    `mirador_logs_queue_dropped_total` ;
  - autonomie du tampon : 8 Mo / débit envoyé dans l'heure d'avant
    (`mirador_agent_sent_bytes_total`), et l'heure où il déborderait ;
  - état : « gardées dans le tampon », « tampon sans doute plein »,
    « rattrapage en cours », « rattrapé », « trou : N lots jetés ».
- Mesure (banc, 2 machines, 5 h de relevés à 30 s) : 3 à 9 ms, 18 Ko de
  HTML ; 7 requêtes par machine, seulement à l'ouverture de la page ; rien
  en base, rien en mémoire entre deux pages.
- Limite : le tampon de 8 Mo est la valeur par défaut ; un agent réglé
  autrement (configuration distante) a une autre autonomie.

Menaces : page en lecture (comme Alertes) ; le site et la date viennent de
l'adresse et ne servent qu'à choisir parmi les coupures trouvées ; les noms
de machines sont vérifiés (`reValeurVariable`) avant d'entrer dans une
requête.

### 39.3 Écarts vus dans la comparaison vmalert pendant la coupure

- `ProxmoxMuet` manquante chez mirador : normal, la règle importée lit le
  metric server de Proxmox (`cpustat_cpu`), que mirador ne reçoit pas. La
  panne du nœud est couverte par `AgentMuet`. À désactiver dans Règles.
- `ServiceDuModeleAbsent` en trop (10:54 → 17:28) : défaut déjà corrigé
  le 16/11 (§ 34.6 : services attendus écrits seulement pour une machine
  qui répond) ; la fin à 17:28 correspond sans doute à la mise à jour du
  serveur (à confirmer).
- `AgentMuet` de `dhcpdns02` en trop 6 min (10:30 → 10:36) : mirador a vu la
  machine muette avant vmalert ; explication probable : les deux chaînes de
  données sont différentes (agent mirador d'un côté, Alloy de l'autre). Pas
  un défaut de mirador.

## 40. Étape 7 préparée : guide de bascule finale (demande du 16/11) **[Validé le 18/11]**

`docs/bascule-finale.md` : arrêt de l'ancienne stack en 5 phases, chacune
réversible par une commande, **rien d'effacé avant la phase E** :

| Phase | Quoi | Retour arrière |
|---|---|---|
| 0 | liste de contrôle (agents partout avec leur site, 7 jours de comparaison, heartbeat HetrixTools dans mirador, sauvegarde, mesure 24 h) | — |
| A | arrêt d'Alertmanager (fin des alertes en double) | `docker start` |
| B | machines : arrêt d'Alloy, des scripts `obs-*`, de `smartctl_exporter`, du metric server Proxmox | `systemctl enable --now` |
| C | VPS : arrêt de VictoriaMetrics, VictoriaLogs, vmalert(-logs), Grafana ; nginx nettoyé (copie gardée) | `docker start`, copie de nginx |
| D | réseau Docker dédié `mirador` (reporté au § 18.27) | copie du compose et de `server.conf` |
| E | nettoyage, au moins 30 jours après, sur validation | aucun (définitif) |

| Choix | Raison | Coût |
|---|---|---|
| `docker stop` + `--restart=no`, pas `docker compose down` | retour arrière en une commande ; rien n'est effacé | les volumes de l'ancienne stack gardent leur place disque jusqu'à la phase E |
| Une machine à la fois, `srv-test` d'abord | une erreur ne touche qu'une machine | quelques jours de plus |
| Lignes nginx et noms des services confirmés par une sortie collée | je n'ai pas accès au VPS : pas de commande devinée qui modifie | un aller-retour de plus |

Gain attendu : ~770 Mio de RAM rendus (SPEC § 2.1), à mesurer après la
phase C.

Menaces :
- Perte des notifications pendant la bascule : Alertmanager arrêté
  seulement quand mirador est en mode actif, testé, avec le heartbeat
  externe (HetrixTools) qui prévient si mirador se tait.
- Perte de données : rien d'effacé avant 30 jours ; les agents gardent
  ~19 h de données pendant un changement de réseau.
- Phase D : `MIRADOR_PROXY` faux → adresses des machines mal lues
  (refusées) ; le serveur refuse déjà de démarrer sans proxy ni TLS sur une
  écoute ouverte ; corriger et relancer, rien n'est perdu. Réseau dédié :
  plus aucun autre conteneur ne joint mirador sans passer par nginx.
- Grafana : non gardé (API de lecture compatible Prometheus du brief non
  faite, facultative) ; à demander avant la phase C.

## 41. Derniers logs joints aux messages d'alerte (idée de Beszel, demande du 17/11) **[Validé le 18/11]**

Idée reprise de Beszel (qui joint les logs d'un conteneur « unhealthy ») ;
**aucun code repris**, réécrit pour mirador.

- Pour une alerte **nouvelle** d'un message d'alerte, le message contient
  « 📄 Derniers logs : » et les 5 dernières lignes, dans l'ordre :
  - étiquette `name` en `.service` (`ServiceSystemdEnEchec`,
    `ServiceRedemarreEnBoucle`…) : logs de cette unité sur la machine ;
  - étiquette `name` sans `.service` (règles des conteneurs Docker :
    `ConteneurNonSain`, `ConteneurRedemarreEnBoucle`, OOM d'un conteneur…) :
    logs de ce conteneur ;
  - `OomKiller` : lignes du noyau « Out of memory: Killed process … ».
- Période : depuis le début de la condition moins 10 min (24 h au plus).
- Pas d'extrait pour les rappels, les fins, ni le message d'un site
  injoignable. 3 alertes au plus par message, 160 caractères par ligne,
  caractères de contrôle retirés.
- Réglages › Notifications › Routage : case « Joindre les 5 dernières lignes
  de logs… », cochée par défaut.

| Choix | Raison | Coût |
|---|---|---|
| Recherche dans le stockage du serveur, pas une demande à l'agent | les logs y sont déjà ; l'agent n'ouvre aucun port et ne reçoit pas d'ordre | 1,5 ms et 1,5 Mo temporaires par recherche (banc : 24 h, 200 000 lignes, 10 machines) |
| Limites serrées (200 blocs, 2 s, 1 travailleur ; 3 s par message) | un message ne doit jamais attendre ; une recherche lente est abandonnée, le message part sans extrait | — |
| Choix d'après les étiquettes, pas une liste par règle | marche aussi pour une règle ajoutée plus tard avec la même étiquette | une règle dont `name` n'est ni un service ni un conteneur ne trouve rien : pas d'extrait |

Menaces :
- **Des logs quittent le serveur** (Telegram, e-mail, ntfy, push) : ils
  sont déjà masqués par l'agent (mots de passe, jetons, règles de masquage)
  avant d'arriver ; la case permet de tout couper.
- Texte des logs hostile (balises, retours à la ligne) : échappé en HTML,
  caractères de contrôle remplacés, longueur bornée ; le message entier
  reste coupé proprement à la limite du canal.
- Charge : une recherche par alerte nouvelle concernée, plafonnée (voir
  ci-dessus) ; une rafale d'alertes reste bornée à 3 recherches par message.

## 42. Conteneurs, Services, Paquets, détail au clic (idées de Beszel, demande du 17/11) **[Validé le 18/11]**

Idées reprises de Beszel après maquettes validées le 17/11 (captures) ;
**aucun code repris**.

### 42.1 Page « Conteneurs »

- Menu : « Conteneurs » sous « Machines », avec un compteur orange (non
  sains + arrêtés alors qu'ils devraient redémarrer, mêmes conditions que
  `ConteneurNonSain` et `ConteneurArrete`). Aussi dans « Aller à… » (Ctrl K)
  et en lien depuis « Les plus gourmands » de chaque machine.
- Quatre tuiles (en marche, non sains, arrêtés, redémarrés sur 24 h) ; un
  tableau, problèmes en premier : conteneur (et projet compose), machine,
  état, santé, processeur (part de toute la machine), mémoire (et limite),
  redémarrages sur 24 h, depuis quand, bouton « Logs » (page Logs filtrée).
- Filtres machine et état (dans l'adresse), tri par colonne et « Filtrer
  ce tableau » comme les autres tableaux.
- **Agent** : une série de plus par conteneur en marche,
  `node_docker_container_started_timestamp_seconds` (date lue dans la
  réponse déjà demandée à Docker, rien de plus). Agent ancien : « — ».

### 42.2 Bloc « Services » (page machine)

- Placé après les alertes, avant « En ce moment ». Replié, sauf si un
  service est en échec (ouvert d'office) ; sinon le choix est retenu par
  le navigateur. Flèche › / ⌄, désormais la même sur tous les blocs
  repliables de l'interface.
- Onglets Tous / À regarder (en échec + arrêtés) / Au démarrage /
  Surveillés ; tri, filtre, bouton « Logs » (page Logs filtrée sur l'unité).
- Rien de nouveau collecté : `node_systemd_unit_state`,
  `node_systemd_unit_enabled_active`, `node_systemd_service_restart_total`.
  L'agent n'envoie pas l'état « actif » de chaque unité (écarté le 09/11) :
  un service ni en échec ni lancé au démarrage est « À la demande ». Les
  services du système (systemd-*, getty, dbus…) sont cachés, sauf en échec.

### 42.3 Bloc « Paquets à mettre à jour » (page machine)

- Même place, même pliage ; ouvert d'office si l'alerte
  `MisesAJourSecuriteEnAttente` est en cours. Paquet, version installée,
  nouvelle version, sécurité ou non ; « Redémarrage nécessaire » ; la
  commande à lancer (mirador n'installe jamais rien).
- **Agent** : la simulation `apt-get --simulate dist-upgrade` (déjà lancée
  toutes les 6 h) est lue en entier : une série
  `apt_upgrade_package{package, from, to, security}` par paquet, **200 au
  plus** (le nombre total reste juste), champs bornés (128 / 64 caractères).
  Agent ancien : le nombre seulement, avec une phrase qui le dit.

### 42.4 Détail au clic

- Les tuiles « En ce moment » (CPU, mémoire, disque, swap, charge) et
  4 liens ouvrent un panneau à droite (plein écran sur téléphone) :
  processeur par cœur et répartition du temps (user, system, iowait…),
  mémoire détaillée et swap, disques (place par point de montage, lecture,
  écriture, occupation par disque), réseau par interface (reçu, envoyé,
  erreurs) ; conteneurs les plus gourmands. Onglets pour passer de l'un à
  l'autre ; fermeture par bouton, Échap ou clic à côté.
- `GET /machines/{nom}/detail/{cpu|memoire|disques|reseau}` : fragment pour
  la page, page complète sans JavaScript (ou dans un nouvel onglet).

### 42.5 Coût mesuré (banc : 10 machines, 200 conteneurs, 60 services par machine, 16 cœurs)

| Quoi | Temps | HTML | Mémoire allouée (libérée ensuite) |
|---|---|---|---|
| Page Conteneurs (200) | 86 ms (cache froid) | 126 Ko | 4,3 Mo |
| Page machine avec Services (60) et Paquets | 26 ms | 78 Ko | 3,5 Mo |
| Panneau CPU (16 cœurs) | 5 ms | 3 Ko | 0,4 Mo |
| Compteur du menu (chaque page) | 1 requête, gardée 20 s | — | — |

Séries en plus : 1 par conteneur en marche ; 1 par paquet en attente
(200 au plus par machine, quelques-unes en général). Rien en base, rien
gardé en mémoire entre deux pages.

### 42.6 Menaces

- Noms de paquets, de services et de conteneurs : valeurs envoyées par
  l'agent, affichées échappées (html/template) ; dans les liens, encodées
  (`url.Values`) ; un nom de machine n'entre dans une requête qu'après
  vérification (`reValeurVariable`).
- Panneau : fragment HTML du serveur (gabarit échappé) inséré dans la
  page, aucun script ; adresse refusée (404) si la machine ou la rubrique
  est inconnue. Lecture seule, mêmes droits que la page machine.
- L'agent ne lance rien de nouveau : la commande apt est la même qu'avant
  (chemin et arguments figés, sans root) ; seule sa sortie est lue en plus.
- `localStorage` : seulement l'état plié / déplié (aucune donnée).

## 43. Pages de connexion refaites (demandes du 17/11 : « plus classe », puis style « A ») **[Validé le 18/11]**

- Première version (présentation à gauche, formulaire à droite), puis
  4 maquettes présentées (A Nuit, B Console, C Topographie, D Aperçu) :
  **A choisi le 17/11**.
- Style « Nuit » : carte centrée, semi-transparente (flou d'arrière-plan),
  sur fond bleu nuit avec une grille de points et un halo bleu ; logo,
  « Connectez-vous pour voir votre parc. » (sans titre visible : « Bon
  retour » retiré le 17/11 ; titre caché pour les lecteurs d'écran), champs avec
  icône, bouton bleu clair « Continuer → », séparateur « ou » avant la
  passkey ; en bas : « Connexion chiffrée · 2FA obligatoire · mirador-stack,
  logiciel libre ».
- **Toujours sombre**, même quand le système est en mode clair : les
  couleurs de l'interface sont redéfinies pour ces pages seulement
  (champs, boutons, messages d'erreur, liens suivent).
- Même cadre pour toutes les pages hors connexion : code 2FA (champ large,
  chiffres espacés), code de secours, création de la 2FA (QR code sur fond
  blanc), premier administrateur. Téléphone : la même carte ; sur écran
  tactile, la passkey reste en premier (§ 34).
- Coût : quelques règles CSS et un SVG en ligne ; aucun script, aucune
  image, aucune police nouvelle.
- Menaces : **rien sur le parc n'est montré avant connexion** (ni nom de
  machine, ni chiffre). Noms des champs, formulaires, jeton CSRF, passkey
  et limites de tentatives inchangés ; connexion complète vérifiée dans
  Chromium (mot de passe faux, puis bon mot de passe et code 2FA).

## 44. « Réessayer la mise à jour » / « Mettre à jour maintenant » (demande du 18/11) **[Validé le 18/11]**

Constat : une machine affichait « Mise à jour en échec » (0.3.1) sans moyen
de relancer. L'agent note l'échec (`echec.json`) et ne retente jamais la
même version (choix de l'étape 2 : pas de boucle d'essais).

- Fiche de la machine, juste après les alertes : bloc **Mise à jour** quand
  une mise à jour a échoué (raison affichée) ou qu'une version assignée
  n'est pas encore installée ; bouton **Réessayer la mise à jour** ou
  **Mettre à jour maintenant** (admin, code 2FA ou mode administration,
  audit `maj_forcee`). La pastille « Mise à jour en échec » de la liste des
  machines mène à ce bloc.
- Effets (`flotte.ForcerMaj`) : la version reste celle assignée et signée ;
  - fenêtre horaire ignorée 24 h pour cette machine ;
  - l'agent est réveillé tout de suite (même canal que « Actualiser » ;
    les agents à jour vérifient la mise à jour dans la seconde, les anciens
    à leur rythme, 15 min au plus) ;
  - nouvelle capacité **`reessai`** : le serveur ajoute `reessai` (heure de
    la demande) à la réponse de mise à jour, **seulement** aux agents qui
    l'annoncent (les anciens refusent tout champ inconnu). L'agent oublie
    l'échec s'il est plus ancien que la demande et retente ; s'il échoue
    encore, il faut une nouvelle demande (pas de boucle).
- Agent trop ancien : le bloc donne la commande à lancer une fois
  (`sudo rm …/bin/echec.json`), puis le bouton.
- Corrigé au passage : l'agent n'efface plus jamais son message d'erreur de
  mise à jour ; il le garde maintenant seulement tant que l'erreur dure.

| Choix | Raison | Coût |
|---|---|---|
| Une demande datée, comparée à la date de l'échec | un clic = un essai ; jamais de boucle d'essais qui ferait redémarrer l'agent sans fin | une colonne (`maj_forcee_le`), une lecture en base par demande de mise à jour |
| Champ envoyé seulement aux agents qui annoncent `reessai` | les agents déjà installés refusent une réponse avec un champ inconnu : leurs mises à jour casseraient | — |
| Réveil par l'actualisation existante | aucun port ouvert sur l'agent, aucune nouvelle connexion | aucun |

Menaces :
- Rien de nouveau n'est installable : seule la version assignée, publiée et
  signée, vérifiée par l'agent (signature, empreinte, anti-retour arrière,
  self-test, retour automatique). Le bouton change seulement *quand*.
- Action réservée aux admins, code 2FA (ou mode administration), journal
  d'audit ; l'ancien agent relance aussi son assistant root (comme
  « Actualiser »), sans autre effet.
- La colonne « Santé » de la page Conteneurs affiche « — » (au lieu de « pas
  de test » répété) avec une légende (retour du 18/11).

## 45. AgentMuet ne se ferme plus seule après 24 h (défaut vu le 18/11) **[Validé le 18/11]**

### 45.1 Le défaut

- Règle reprise de vmalert : `min by (instance) (lag(node_uname_info[1d])) > 60`.
- `lag()` ne voit que les points des dernières 24 h. Après 24 h sans
  aucune donnée, la série sort de la fenêtre : plus de résultat, l'alerte
  passe « résolue » alors que la machine est toujours muette.
- Effets : faux message « Résolu », fin de coupure fausse (page Coupures),
  bilan lu trop tôt (relevés, file d'attente, lots jetés lus jusqu'à 1 h
  après une « fin » qui n'en est pas une).
- Coupure du 16/11 : les deux machines sont « revenues » exactement 24 h
  après leur dernière donnée, avec 0 % de relevés reçus et une file
  d'attente vide : c'est la signature de ce défaut.
- Même défaut dans vmalert (même expression), qui n'envoie plus rien depuis
  le mode actif de mirador.

### 45.2 La correction

- Le serveur note l'heure des dernières métriques reçues de chaque machine
  (en mémoire, à chaque envoi accepté) et l'écrit chaque minute dans une
  série interne : `mirador_machine_derniere_metrique_timestamp_seconds{instance}`.
  Avant toute réception depuis le démarrage : `vu_le` (en base).
  Pas pour les équipements relevés par un collecteur (pas de
  `node_uname_info`, AgentMuet ne les concerne pas).
- Nouvelle expression :
  ```
  min by (instance) (lag(node_uname_info[1d])) > 60
  or on (instance)
  max by (instance) (time() - mirador_machine_derniere_metrique_timestamp_seconds) > 82800
  ```
  - 1re partie inchangée : alerte en 1 à 2 min, comme avant.
  - 2e partie : prend le relais à partir de 23 h (1 h de recouvrement),
    mêmes étiquettes `{instance}`, donc la même alerte continue (pas de
    « Résolu » puis nouvelle alerte).
  - Machine qui répond : la 2e partie est fausse (dernière réception de
    moins de 2 min, même avec le retard d'une minute de la série interne).
- Appliquée seule :
  - au démarrage du serveur, aux règles en base dont l'expression est
    exactement l'ancienne (espaces mis à part) ; trace dans l'audit
    (« modifiée par mirador ») et note sur la règle ;
  - à l'import d'un fichier vmalert (classée « approchée » dans le rapport).
  - Une règle déjà modifiée par l'utilisateur n'est pas touchée.

### 45.3 Coût

- Une série par machine, un point par minute (quelques Ko par jour).
- Évaluation : 3,3 à 3,6 ms contre 3,8 à 4,0 ms avant (20 machines, 24 h
  de données, banc `BenchmarkAgentMuet`) : pas de surcoût.

### 45.4 À savoir

- Une machine arrêtée pour de bon reste en alerte tant qu'elle est dans le
  parc (avant, l'alerte se fermait seule au bout de 24 h). La supprimer,
  ou la mettre en sourdine, quand elle est retirée.
- Redémarrage du serveur : rien à faire, `vu_le` sert de point de départ.
- Vérifié sur le serveur le 18/11 : « règles d'alerte corrigées (SPEC § 45)
  nombre=1 » au démarrage, expression en deux parties, série présente pour
  dhcp-dns02 (17 s d'âge), règle évaluée sans erreur en 5 ms, aucune
  fausse alerte.

### 45.5 Menaces

- Rien de nouveau exposé : série écrite par le serveur lui-même, rien
  d'ouvert en plus.
- Une machine ne peut pas agir sur la série d'une autre : l'étiquette
  `instance` est imposée à l'ingestion.
- Une machine compromise pourrait envoyer cette série sous son propre nom
  et retarder sa propre alerte : elle le peut déjà en envoyant
  `node_uname_info`. Pas de risque nouveau.
