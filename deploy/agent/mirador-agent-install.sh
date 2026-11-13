#!/usr/bin/env bash
# SPDX-License-Identifier: GPL-3.0-or-later
#
# mirador-agent-install.sh — installation de mirador-agent
#
# Installe l'agent sur une machine Debian/Ubuntu, en parallèle d'Alloy :
#   - utilisateur système « mirador-agent » (sans droits root) ;
#   - binaire dans /var/lib/mirador-agent/bin (lien « current ») ;
#   - copie root de l'assistant dans /usr/local/lib/mirador-agent ;
#   - configuration et token dans /etc/mirador-agent ;
#   - unités systemd durcies : agent, assistant root (timer 15 min),
#     retour arrière ;
#   - si Docker est présent : proxy de socket en lecture seule (conteneur).
#
# Deux modes :
#   - étape 2 (défaut) : envoi vers VictoriaMetrics / VictoriaLogs derrière nginx,
#     token à ajouter dans la « map » nginx ;
#   - mirador-server (--mirador, ou --jeton) : enrôlement, configuration distante,
#     mises à jour signées (avec --auto-update) ; nginx ne filtre que par IP.
#
# Usage :
#   mirador-agent-install.sh --binaire ./mirador-agent-linux-amd64 \
#       --serveur https://ingest.example.org --site bureau --env vm --nom srv-test-mirador
#   mirador-agent-install.sh --serveur https://mirador.example.org --jeton mirador_enrol_…
#       (binaire téléchargé depuis le serveur ; le script aussi :
#        curl -fsSL https://…/api/agent/v1/install -o mirador-agent-install.sh)
#   MIRADOR_JETON=mirador_enrol_… mirador-agent-install.sh --serveur https://…
#       (jeton par variable : invisible dans « ps » ; --jeton reste accepté)
#
# Options :
#   --binaire FICHIER    binaire mirador-agent à installer (sinon, avec --jeton,
#                        téléchargé depuis mirador-server, empreinte vérifiée)
#   --serveur URL        adresse d'ingestion (https://…)            OBLIGATOIRE
#   --site NOM           étiquette « site »   (obligatoire sans --jeton / --mirador :
#                        sinon choisie dans l'interface, qui l'impose)
#   --env NOM            étiquette « env »    (idem)
#   --nom NOM            nom de la machine (défaut : nom mémorisé, sinon
#                        hostname court ; refusé s'il est générique)
#   --token VALEUR       token imposé (étape 2 : défaut = reprend l'existant, sinon en
#                        génère un ; --mirador : token donné par « mirador-server token NOM »)
#   --mirador            mode mirador-server (--serveur = adresse de mirador-server)
#   --jeton VALEUR       jeton d'enrôlement à usage unique (« mirador-server jeton ») ;
#                        implique --mirador
#   --docker MODE        auto | oui | non   (défaut auto : oui si Docker est présent)
#   --docker-proxy URL   utiliser un proxy de socket existant (http://127.0.0.1:PORT)
#                        au lieu d'en créer un
#   --pare-feu MODE      auto | oui | non   (défaut auto : oui si --env vps ou INPUT restrictif)
#   --smart MODE         auto | oui | non   (défaut auto : oui sur machine physique)
#   --sans-logs          ne pas envoyer les logs (métriques seulement)
#   --sans-metriques     ne pas envoyer les métriques de la machine (logs seulement ;
#                        l'agent envoie encore ses propres métriques) ; mode mirador
#                        seulement. Réglable ensuite dans l'interface (Configuration
#                        des agents : « Métriques de la machine », « Logs du journal »)
#   --auto-update        mises à jour signées automatiques (mode --mirador seulement)
#   --sans-maj-racine    l'assistant root ne se met PAS à jour lui-même (il ne
#                        change alors qu'en relançant ce script) ; par défaut, avec
#                        --auto-update, il se met à jour depuis la version signée
#                        et confirmée de l'agent (SPEC § 22.5). Gardé d'un passage
#                        à l'autre (MIRADOR_MAJ_RACINE dans agent.conf)
#   --cle-publique CLE   clé PUBLIQUE de signature des versions (base64 ; deux au
#                        plus, séparées par une virgule). Sans cette option, en mode
#                        mirador, elle est demandée au serveur à la première
#                        installation, puis jamais changée (sauf avec cette option)
#   --detection          affiche la détection et s'arrête
#   --afficher-config    affiche la configuration et les unités, et s'arrête
#   --desinstaller       arrête et supprime mirador-agent (garde Alloy intact)
#
# Rejouable : un second passage reprend le token et le nom déjà en place.
# Le token n'est jamais affiché ailleurs que dans la ligne nginx finale.

set -euo pipefail
export PATH="/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
umask 022

# ---------------------------------------------------------------- constantes
UTILISATEUR=mirador-agent
ETC=/etc/mirador-agent
VARLIB=/var/lib/mirador-agent
BIN_DIR=$VARLIB/bin
ROOT_DIR=/usr/local/lib/mirador-agent
UNITES=/etc/systemd/system
TMPFILES=/etc/tmpfiles.d/mirador-agent.conf
# Image du proxy de socket Docker, version figée (vérifiée le 08/11/2026).
PROXY_IMAGE="tecnativa/docker-socket-proxy:v0.5.0"
PROXY_NOM=mirador-docker-proxy
PROXY_PORT=12375

BINAIRE=""; SERVEUR=""; SITE=""; ENV_=""; NOM=""; TOKEN=""; CLE_PUBLIQUE=""; MAJ_RACINE=""
DOCKER_MODE=auto; DOCKER_PROXY=""; PAREFEU_MODE=auto; SMART_MODE=auto
LOGS=oui; METRIQUES=oui; MODE=installer; MIRADOR=non; JETON=""; MAJ_AUTO=non

die()  { echo "ERREUR : $*" >&2; exit 1; }
info() { echo "==> $*"; }

while [ $# -gt 0 ]; do
  case "$1" in
    --binaire)         BINAIRE="${2:-}"; shift 2 ;;
    --serveur)         SERVEUR="${2:-}"; shift 2 ;;
    --site)            SITE="${2:-}"; shift 2 ;;
    --env)             ENV_="${2:-}"; shift 2 ;;
    --nom)             NOM="${2:-}"; shift 2 ;;
    --cle-publique)    CLE_PUBLIQUE="${2:-}"; shift 2 ;;
    --sans-maj-racine) MAJ_RACINE=non; shift ;;
    --token)           TOKEN="${2:-}"; shift 2 ;;
    --mirador)         MIRADOR=oui; shift ;;
    --jeton)           JETON="${2:-}"; MIRADOR=oui; shift 2 ;;
    --docker)          DOCKER_MODE="${2:-}"; shift 2 ;;
    --docker-proxy)    DOCKER_PROXY="${2:-}"; shift 2 ;;
    --pare-feu)        PAREFEU_MODE="${2:-}"; shift 2 ;;
    --smart)           SMART_MODE="${2:-}"; shift 2 ;;
    --sans-logs)       LOGS=non; shift ;;
    --sans-metriques)  METRIQUES=non; shift ;;
    --auto-update)     MAJ_AUTO=oui; shift ;;
    --detection)       MODE=detection; shift ;;
    --afficher-config) MODE=afficher; shift ;;
    --desinstaller)    MODE=desinstaller; shift ;;
    -h|--help)         sed -n '2,40p' "$0"; exit 0 ;;
    *)                 die "option inconnue : $1 (voir --help)" ;;
  esac
done
# Jeton par variable d'environnement (MIRADOR_JETON=… bash …) : il n'apparaît
# alors ni dans la liste des processus (ps), ni dans les arguments du script
# (revue de sécurité du 11/11).
if [ -z "$JETON" ] && [ -n "${MIRADOR_JETON:-}" ]; then
  JETON=$MIRADOR_JETON; MIRADOR=oui
fi
unset MIRADOR_JETON

[ "$(id -u)" -eq 0 ] || die "à lancer en root (sudo)"

# ---------------------------------------------------------------- désinstallation
if [ "$MODE" = desinstaller ]; then
  info "Arrêt et suppression de mirador-agent"
  systemctl disable --now mirador-agent.service mirador-agent-root.timer mirador-agent-root.path >/dev/null 2>&1 || true
  rm -f "$UNITES/mirador-agent.service" "$UNITES/mirador-agent-root.service" \
        "$UNITES/mirador-agent-root.timer" "$UNITES/mirador-agent-root.path" "$UNITES/mirador-agent-retour.service"
  systemctl daemon-reload
  if command -v docker >/dev/null 2>&1 && docker inspect "$PROXY_NOM" >/dev/null 2>&1; then
    docker rm -f "$PROXY_NOM" >/dev/null
  fi
  rm -f "$TMPFILES"
  rm -rf /run/mirador-agent
  rm -f "$ETC/agent.conf" "$ETC/token" "$ETC/cles-publiques.txt"
  rmdir "$ETC" 2>/dev/null || true
  rm -f "$ROOT_DIR/mirador-agent"
  rmdir "$ROOT_DIR" 2>/dev/null || true
  rm -f "$BIN_DIR"/mirador-agent-* "$BIN_DIR/current" "$BIN_DIR/precedent" "$BIN_DIR"/*.json
  rmdir "$BIN_DIR" 2>/dev/null || true
  rm -f "$VARLIB/journal.cursor" "$VARLIB/token" "$VARLIB/jeton" "$VARLIB/config-distante.json"
  rmdir "$VARLIB" 2>/dev/null || true
  userdel "$UTILISATEUR" 2>/dev/null || true
  echo "mirador-agent supprimé. Pensez à retirer sa ligne de la « map » nginx (étape 2),"
  echo "ou à révoquer la machine sur mirador-server (« revoquer NOM »)."
  exit 0
fi

# ---------------------------------------------------------------- vérifications
OS_ID=$(. /etc/os-release && echo "${ID:-}")
OS_LIKE=$(. /etc/os-release && echo "${ID_LIKE:-}")
case " $OS_ID $OS_LIKE " in
  *" debian "*|*" ubuntu "*) ;;
  *) die "Debian ou Ubuntu uniquement" ;;
esac
for v in "$DOCKER_MODE" "$PAREFEU_MODE" "$SMART_MODE"; do
  case "$v" in auto|oui|non) ;; *) die "mode invalide : $v (auto, oui ou non)" ;; esac
done

# Site et env : repris de l'installation précédente s'ils ne sont pas donnés
# (un second passage ne fait rien perdre).
if [ -r "$ETC/agent.conf" ]; then
  [ -n "$MAJ_RACINE" ] || MAJ_RACINE=$(sed -n 's/^MIRADOR_MAJ_RACINE=//p' "$ETC/agent.conf" | head -1 | tr -d '"')
  [ -n "$SITE" ] || SITE=$(sed -n 's/^MIRADOR_SITE=//p' "$ETC/agent.conf" | head -1 | tr -d '"')
  [ -n "$ENV_" ] || ENV_=$(sed -n 's/^MIRADOR_ENV=//p' "$ETC/agent.conf" | head -1 | tr -d '"')
fi

# Nom : --nom, sinon nom mémorisé, sinon hostname court (même règle qu'obs-agent-install.sh).
if [ -z "$NOM" ] && [ -r "$ETC/agent.conf" ]; then
  NOM=$(sed -n 's/^MIRADOR_NOM=//p' "$ETC/agent.conf" | head -1)
fi
if [ -z "$NOM" ]; then
  NOM=$(hostname -s 2>/dev/null || cat /etc/hostname)
  case "$(echo "$NOM" | tr '[:upper:]' '[:lower:]')" in
    ""|ubuntu|debian|localhost|localhost.localdomain|ubuntu-server|debian12|debian13|template)
      die "hostname générique « $NOM » : indiquez le nom avec --nom" ;;
  esac
fi
echo "$NOM" | grep -Eq '^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$' || die "nom invalide « $NOM »"

# Essai en parallèle d'Alloy, mode étape 2 seulement : les deux agents
# écrivent dans la MÊME base VictoriaMetrics, mirador doit donc envoyer sous
# un AUTRE nom, sinon leurs séries se mélangeraient (débits faux).
# Vers mirador-server (--mirador, --jeton), les données vont dans une base
# séparée : le même nom qu'Alloy est permis (comparaison plus simple), et
# le nom final est de toute façon choisi par le serveur (jeton, approbation).
NOM_ALLOY=""
if [ -r /etc/default/alloy ]; then
  NOM_ALLOY=$(sed -n 's/^OBS_NAME=//p' /etc/default/alloy | head -1)
fi
if [ "$MIRADOR" != oui ] && [ -n "$NOM_ALLOY" ] && [ "$NOM" = "$NOM_ALLOY" ]; then
  die "« $NOM » est déjà le nom d'Alloy sur cette machine : choisissez un autre --nom"
fi
# Vers mirador-server : site et env PROPOSÉS au serveur (ceux du jeton ont la
# priorité, l'admin les corrige à l'approbation). À défaut de --site / --env
# ou de l'installation précédente : ceux d'Alloy (valeurs bien formées seulement).
if [ "$MIRADOR" = oui ] && [ -r /etc/default/alloy ]; then
  if [ -z "$SITE" ]; then
    SITE=$(sed -n 's/^OBS_SITE=//p' /etc/default/alloy | head -1 | tr -d '"')
    echo "$SITE" | grep -Eq '^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$' || SITE=""
  fi
  if [ -z "$ENV_" ]; then
    ENV_=$(sed -n 's/^OBS_ENV=//p' /etc/default/alloy | head -1 | tr -d '"')
    echo "$ENV_" | grep -Eq '^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$' || ENV_=""
  fi
fi

# ---------------------------------------------------------------- détection
A_DOCKER=non
[ -S /var/run/docker.sock ] && command -v docker >/dev/null 2>&1 && A_DOCKER=oui

INPUT_RESTRICTIF=non
if command -v iptables >/dev/null 2>&1; then
  case "$(iptables -S INPUT 2>/dev/null | awk '$1=="-P"{print $3; exit}')" in
    DROP|REJECT) INPUT_RESTRICTIF=oui ;;
  esac
fi
if [ "$INPUT_RESTRICTIF" = non ] && command -v nft >/dev/null 2>&1; then
  nft list ruleset 2>/dev/null | grep -qiE 'hook input .*policy drop' && INPUT_RESTRICTIF=oui
fi

PHYSIQUE=non
if command -v systemd-detect-virt >/dev/null 2>&1 && ! systemd-detect-virt --quiet; then
  PHYSIQUE=oui
fi

decider() { [ "$1" = auto ] && echo "$2" || echo "$1"; }
USE_DOCKER=$(decider "$DOCKER_MODE" "$A_DOCKER")
[ -n "$DOCKER_PROXY" ] && USE_DOCKER=oui
USE_PAREFEU=$(decider "$PAREFEU_MODE" "$([ "$ENV_" = vps ] && echo oui || echo "$INPUT_RESTRICTIF")")
USE_SMART=$(decider "$SMART_MODE" "$PHYSIQUE")

afficher_detection() {
  echo
  echo "  Machine         : $NOM"
  [ -n "$NOM_ALLOY" ] && echo "  Nom chez Alloy  : $NOM_ALLOY"
  echo "  Étiquettes      : site=${SITE:-?}  env=${ENV_:-?}"
  echo "  Docker          : $A_DOCKER présent -> métriques et logs Docker : $USE_DOCKER"
  echo "  Pare-feu        : INPUT restrictif : $INPUT_RESTRICTIF -> contrôle : $USE_PAREFEU"
  echo "  Machine physique: $PHYSIQUE -> SMART : $USE_SMART"
  echo "  Métriques       : $METRIQUES"
  echo "  Logs            : $LOGS"
  echo
}

if [ "$MODE" = detection ]; then
  afficher_detection
  exit 0
fi

[ -n "$SERVEUR" ] || die "--serveur est obligatoire"
# Avec mirador-server (--jeton / --mirador), site et env se choisissent dans
# l'interface (à la création du jeton ou dans la fiche de la machine).
if [ "$MIRADOR" != oui ]; then
  [ -n "$SITE" ] || die "--site est obligatoire (sauf avec --jeton / --mirador)"
  [ -n "$ENV_" ] || die "--env est obligatoire (sauf avec --jeton / --mirador)"
fi
for v in "$SITE" "$ENV_"; do
  [ -z "$v" ] || echo "$v" | grep -Eq '^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$' || die "site / env « $v » : lettres, chiffres, . _ -"
done
case "$SERVEUR" in https://*) ;; *) die "--serveur doit commencer par https://" ;; esac
SERVEUR=${SERVEUR%/}
[ "$MAJ_AUTO" = non ] || [ "$MIRADOR" = oui ] || die "--auto-update exige le mode --mirador"
[ "$METRIQUES" = oui ] || [ "$MIRADOR" = oui ] || die "--sans-metriques exige le mode --mirador (ou --jeton)"
[ "$METRIQUES" = oui ] || [ "$LOGS" = oui ] || die "--sans-metriques et --sans-logs : il ne resterait rien à envoyer"
if [ "$MIRADOR" = oui ]; then
  [ -z "$JETON" ] || [ -z "$TOKEN" ] || die "--jeton et --token : l'un ou l'autre"
  if [ -z "$JETON" ] && [ -z "$TOKEN" ] && [ ! -s "$VARLIB/token" ]; then
    die "mode mirador : --jeton (enrôlement) ou --token (« mirador-server token NOM ») requis"
  fi
  [ -z "$JETON" ] || echo "$JETON" | grep -Eq '^[A-Za-z0-9_-]{20,256}$' || die "jeton mal formé"
  [ -z "$TOKEN" ] || echo "$TOKEN" | grep -Eq '^[A-Za-z0-9_-]{20,256}$' || die "token mal formé"
fi

if [ -n "$DOCKER_PROXY" ]; then
  URL_DOCKER="$DOCKER_PROXY"
elif [ "$USE_DOCKER" = oui ]; then
  URL_DOCKER="http://127.0.0.1:$PROXY_PORT"
else
  URL_DOCKER=""
fi

# ---------------------------------------------------------------- contenu des fichiers
config_agent() {
  if [ "$MIRADOR" = oui ]; then
    config_agent_mirador
    return
  fi
  cat <<EOF
# /etc/mirador-agent/agent.conf — généré par mirador-agent-install.sh
# Format CLE=valeur. Une clé inconnue empêche le démarrage (sécurité).
# Le token est dans $ETC/token, jamais ici.

MIRADOR_NOM=$NOM
MIRADOR_SITE=$SITE
MIRADOR_ENV=$ENV_
MIRADOR_URL_METRIQUES=$SERVEUR/api/v1/write
MIRADOR_URL_LOGS=$([ "$LOGS" = oui ] && echo "$SERVEUR/insert/jsonline" || true)
MIRADOR_FICHIER_TOKEN=$ETC/token

# Collecteurs
MIRADOR_DOCKER=$URL_DOCKER
MIRADOR_PAREFEU=$USE_PAREFEU
MIRADOR_SMART=$USE_SMART

# Services à ne pas surveiller (« activé mais arrêté »), séparés par des virgules.
# Reprend /etc/obs-systemd-ignore s'il existe.
MIRADOR_SYSTEMD_IGNORER=$(ignorer_systemd)
EOF
}

# Mode mirador-server : adresses de métriques et de logs déduites du serveur,
# token dans le dossier de l'agent (écrit par lui à l'enrôlement).
config_agent_mirador() {
  cat <<EOF
# /etc/mirador-agent/agent.conf — généré par mirador-agent-install.sh (mode mirador-server)
# Format CLE=valeur. Une clé inconnue empêche le démarrage (sécurité).
# Le token est dans $VARLIB/token, jamais ici. Les autres réglages viennent de
# l'interface de mirador (configuration distante).

MIRADOR_NOM=$NOM
$([ -n "$SITE" ] && echo "MIRADOR_SITE=$SITE" || echo "# Site et env : choisis dans l'interface de mirador (fiche de la machine).")
$([ -n "$ENV_" ] && echo "MIRADOR_ENV=$ENV_" || true)
MIRADOR_SERVEUR=$SERVEUR
MIRADOR_FICHIER_TOKEN=$VARLIB/token
MIRADOR_FICHIER_JETON=$VARLIB/jeton
MIRADOR_MAJ_AUTO=$MAJ_AUTO
# Assistant root mis à jour par lui-même (version signée et confirmée de
# l'agent, signature revérifiée par root) ; « non » l'interdit sur cette machine.
MIRADOR_MAJ_RACINE=${MAJ_RACINE:-oui}
MIRADOR_METRIQUES=$METRIQUES
MIRADOR_JOURNAL=$LOGS
MIRADOR_LOGS_DOCKER=$LOGS

# Collecteurs
MIRADOR_DOCKER=$URL_DOCKER
MIRADOR_PAREFEU=$USE_PAREFEU
MIRADOR_SMART=$USE_SMART

# Services à ne pas surveiller (« activé mais arrêté »), séparés par des virgules.
MIRADOR_SYSTEMD_IGNORER=$(ignorer_systemd)
EOF
}

# ignorer_systemd reprend la liste locale d'obs-systemd.sh (une unité par ligne).
ignorer_systemd() {
  [ -r /etc/obs-systemd-ignore ] || return 0
  sed 's/#.*//' /etc/obs-systemd-ignore | tr -s ' \t' '\n' | grep -E '\.service$' | paste -sd, - || true
}

unite_agent() {
  cat <<'EOF'
# mirador-agent — agent de supervision (sans root, aucun port ouvert)
[Unit]
Description=mirador-agent (supervision)
Documentation=https://github.com/prohand/mirador-stack
Wants=network-online.target
After=network-online.target
# Après 5 échecs en 5 minutes (nouvelle version qui ne démarre pas) :
# retour à la version précédente.
StartLimitIntervalSec=300
StartLimitBurst=5
OnFailure=mirador-agent-retour.service

[Service]
User=mirador-agent
Group=mirador-agent
SupplementaryGroups=systemd-journal
ExecStart=/var/lib/mirador-agent/bin/current -config /etc/mirador-agent/agent.conf
Restart=always
RestartSec=10
Environment=GOMEMLIMIT=25MiB

# Priorité basse (SPEC.md § 6.1)
Nice=19
CPUWeight=10
IOSchedulingClass=idle
MemoryMax=150M

# Durcissement
NoNewPrivileges=yes
CapabilityBoundingSet=
AmbientCapabilities=
ProtectSystem=strict
ProtectHome=read-only
ReadWritePaths=/var/lib/mirador-agent
StateDirectory=mirador-agent
StateDirectoryMode=0750
PrivateDevices=yes
ProtectKernelModules=yes
ProtectKernelTunables=yes
ProtectKernelLogs=yes
ProtectControlGroups=yes
ProtectClock=yes
ProtectHostname=yes
RestrictNamespaces=yes
RestrictRealtime=yes
RestrictSUIDSGID=yes
LockPersonality=yes
MemoryDenyWriteExecute=yes
RemoveIPC=yes
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
SystemCallArchitectures=native
# Liste blanche des appels système ; un appel refusé renvoie une erreur au lieu
# de tuer le processus. (@resources n'est pas interdit : Go appelle setrlimit
# au démarrage.)
SystemCallFilter=@system-service
SystemCallFilter=~@privileged
SystemCallErrorNumber=EPERM

[Install]
WantedBy=multi-user.target
EOF
}

unite_root() {
  cat <<'EOF'
# mirador-agent-root — assistant root : pare-feu, SMART, certificats.
# Lancé par mirador-agent-root.timer, écrit /run/mirador-agent/root.json puis
# s'arrête. Binaire root : jamais modifié par l'agent ; il se remplace
# lui-même seulement depuis une version signée (revérifiée par root) et
# confirmée de l'agent (SPEC § 22.5, MIRADOR_MAJ_RACINE).
[Unit]
Description=mirador-agent : mesures réservées à root

[Service]
Type=oneshot
# Groupe root (pas mirador-agent) : Linux ne montre /proc/<pid>/io d'un
# processus root (QEMU) qu'à un appelant du même groupe (disques des VM,
# SPEC § 23.7). root.json reste lisible par l'agent : /run/mirador-agent est
# créé par tmpfiles.d en 2750 root:mirador-agent (les fichiers créés dedans
# prennent le groupe mirador-agent).
User=root
Group=root
ExecStart=/usr/local/lib/mirador-agent/mirador-agent -collecte-root -config /etc/mirador-agent/agent.conf
TimeoutStartSec=5min
Nice=19
IOSchedulingClass=idle
MemoryMax=100M

# Durcissement : pas de réseau sortant, système en lecture seule
NoNewPrivileges=yes
# CAP_SYS_PTRACE : seulement pour lire /proc/<pid>/io des VM Proxmox (QEMU
# est protégé par le noyau ; choix de l'utilisateur du 13/11, SPEC § 23.7).
CapabilityBoundingSet=CAP_NET_ADMIN CAP_NET_RAW CAP_SYS_RAWIO CAP_SYS_ADMIN CAP_DAC_READ_SEARCH CAP_SYS_PTRACE
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
ReadWritePaths=/run/mirador-agent
# Son propre dossier : mise à jour par lui-même (SPEC § 22.5).
ReadWritePaths=/usr/local/lib/mirador-agent
# Verrou d'iptables (mode « legacy ») ; le « - » ignore le chemin s'il n'existe pas.
ReadWritePaths=-/run/xtables.lock
IPAddressDeny=any
RestrictAddressFamilies=AF_UNIX AF_NETLINK AF_INET AF_INET6
ProtectKernelModules=yes
ProtectControlGroups=yes
ProtectClock=yes
ProtectHostname=yes
RestrictRealtime=yes
RestrictSUIDSGID=yes
LockPersonality=yes
SystemCallArchitectures=native
EOF
}

# Relance de l'assistant root à la demande (bouton « Actualiser » de
# l'interface, SPEC § 22) : l'agent touche un fichier de SON dossier, et
# systemd lance l'assistant (même programme, mêmes règles que le minuteur).
# L'agent ne peut que déclencher un passage, jamais choisir ce qui tourne ;
# il se limite à une demande par minute, et systemd coupe au-delà.
chemin_root() {
  cat <<EOF
[Unit]
Description=mirador-agent : mesures root à la demande (Actualiser)

[Path]
PathModified=$VARLIB/actualiser-racine
Unit=mirador-agent-root.service
TriggerLimitIntervalSec=60
TriggerLimitBurst=3

[Install]
WantedBy=paths.target
EOF
}

timer_root() {
  cat <<'EOF'
[Unit]
Description=mirador-agent : mesures root toutes les 15 minutes

[Timer]
OnBootSec=2min
OnUnitActiveSec=15min
RandomizedDelaySec=60
AccuracySec=1min

[Install]
WantedBy=timers.target
EOF
}

unite_retour() {
  cat <<'EOF'
# mirador-agent-retour — remet la version précédente si la nouvelle ne démarre
# pas (déclenché par OnFailure= de mirador-agent.service). Action locale :
# jamais commandée par le serveur.
[Unit]
Description=mirador-agent : retour à la version précédente
ConditionPathExists=/var/lib/mirador-agent/bin/essai.json

[Service]
Type=oneshot
User=mirador-agent
Group=mirador-agent
ExecStart=/var/lib/mirador-agent/bin/precedent -retour-arriere -dossier-bin /var/lib/mirador-agent/bin -config /etc/mirador-agent/agent.conf
ExecStartPost=+/usr/bin/systemctl reset-failed mirador-agent.service
ExecStartPost=+/usr/bin/systemctl start mirador-agent.service
NoNewPrivileges=yes
ProtectSystem=strict
ReadWritePaths=/var/lib/mirador-agent
EOF
}

if [ "$MODE" = afficher ]; then
  afficher_detection
  echo "----- $ETC/agent.conf"; config_agent
  echo "----- mirador-agent.service"; unite_agent
  echo "----- mirador-agent-root.service"; unite_root
  echo "----- mirador-agent-root.timer"; timer_root
  echo "----- mirador-agent-root.path"; chemin_root
  echo "----- mirador-agent-retour.service"; unite_retour
  exit 0
fi

# ---------------------------------------------------------------- binaire
# Sans --binaire : téléchargé en HTTPS depuis mirador-server, avec le jeton
# d'enrôlement, ou, pour une machine déjà enrôlée qui relance le script, son
# token (en-tête Authorization, empreinte SHA-256 vérifiée).
# Le jeton passe par un fichier de configuration de curl / wget (dans un
# dossier temporaire root 0700), jamais par la ligne de commande : il serait
# lisible par tout utilisateur local avec « ps » (revue du 11/11).
nouveau_tele() { # dossier temporaire root 0700, effacé à la sortie
  if [ -z "${TELE:-}" ]; then
    umask 077
    TELE=$(mktemp -d)
    trap 'rm -rf -- "$TELE"' EXIT
    umask 022
  fi
}
telecharger() { # URL FICHIER ENTETES
  if command -v curl >/dev/null 2>&1; then
    printf 'header = "Authorization: Bearer %s"\n' "$AUTH" > "$TELE/curl.conf"
    curl -fsS --proto =https --tlsv1.2 --retry 3 -K "$TELE/curl.conf" -D "$3" -o "$2" "$1"
  elif command -v wget >/dev/null 2>&1; then
    printf 'header = Authorization: Bearer %s\n' "$AUTH" > "$TELE/wgetrc"
    WGETRC="$TELE/wgetrc" wget -q --https-only -S -O "$2" "$1" 2>"$3"
  else
    die "curl ou wget est nécessaire pour télécharger l'agent (ou utiliser --binaire)"
  fi
}
# Jeton d'enrôlement, sinon token de la machine (--token, ou celui déjà en place).
AUTH="$JETON"
[ -n "$AUTH" ] || AUTH="$TOKEN"
if [ -z "$AUTH" ] && [ "$MIRADOR" = oui ] && [ -s "$VARLIB/token" ]; then
  AUTH=$(tr -d '[:space:]' < "$VARLIB/token")
fi
if [ -z "$BINAIRE" ] && [ -n "$AUTH" ]; then
  case "$(uname -m)" in
    x86_64|amd64)  ARCH=amd64 ;;
    aarch64|arm64) ARCH=arm64 ;;
    *) die "architecture $(uname -m) non prise en charge (amd64 ou arm64)" ;;
  esac
  nouveau_tele
  info "Téléchargement de mirador-agent ($ARCH) depuis $SERVEUR"
  telecharger "$SERVEUR/api/agent/v1/install/binary?arch=$ARCH" "$TELE/mirador-agent" "$TELE/entetes" \
    || die "téléchargement impossible : jeton expiré ou déjà utilisé (ou token révoqué), ou adresse de cette machine pas autorisée dans nginx"
  ATTENDU=$(tr -d '\r' < "$TELE/entetes" | sed 's/^ *//' | awk -F': ' 'tolower($1)=="x-mirador-sha256"{print $2}' | tail -1)
  OBTENU=$(sha256sum "$TELE/mirador-agent" | awk '{print $1}')
  [ -n "$ATTENDU" ] && [ "$ATTENDU" = "$OBTENU" ] || die "empreinte du binaire téléchargé fausse (attendu ${ATTENDU:-?}, obtenu $OBTENU)"
  chmod 0755 "$TELE/mirador-agent"
  BINAIRE="$TELE/mirador-agent"
fi
[ -n "$BINAIRE" ] || die "--binaire est obligatoire (sauf avec --jeton ou une machine déjà enrôlée : téléchargé depuis le serveur)"
[ -f "$BINAIRE" ] || die "binaire introuvable : $BINAIRE"
# Fichier téléchargé à la main (curl -O) : pas exécutable. Copie privée
# rendue exécutable, l'original n'est pas touché.
if [ ! -x "$BINAIRE" ]; then
  nouveau_tele
  cp -- "$BINAIRE" "$TELE/mirador-agent-binaire"
  chmod 0755 "$TELE/mirador-agent-binaire"
  BINAIRE="$TELE/mirador-agent-binaire"
fi
VERSION=$("$BINAIRE" -version 2>/dev/null | awk '{print $2}') || true
[ -n "$VERSION" ] || die "le binaire ne se lance pas : fichier abîmé, ou mauvaise architecture (cette machine : $(uname -m))"
echo "$VERSION" | grep -Eq '^[A-Za-z0-9._-]{1,40}$' || die "version illisible : $VERSION"

afficher_detection

# ---------------------------------------------------------------- utilisateur et dossiers
info "Utilisateur système $UTILISATEUR"
if ! id "$UTILISATEUR" >/dev/null 2>&1; then
  useradd --system --no-create-home --home-dir "$VARLIB" --shell /usr/sbin/nologin "$UTILISATEUR"
fi
install -d -o root -g "$UTILISATEUR" -m 0750 "$ETC"
install -d -o "$UTILISATEUR" -g "$UTILISATEUR" -m 0750 "$VARLIB"
install -d -o "$UTILISATEUR" -g "$UTILISATEUR" -m 0750 "$BIN_DIR"
install -d -o root -g root -m 0755 "$ROOT_DIR"

# Écritures dans les dossiers de l'agent : faites SOUS SON IDENTITÉ (runuser),
# contenu passé par l'entrée standard. Un agent compromis qui y aurait posé
# un lien (ex. token.tmp -> /etc/passwd) ne peut ainsi toucher que ses
# propres fichiers, jamais un fichier de root (revue de sécurité du 11/11).
ecrire_agent() { # FICHIER MODE < contenu
  runuser -u "$UTILISATEUR" -- sh -c 'umask 077; rm -f -- "$1.tmp" && cat > "$1.tmp" && chmod "$2" "$1.tmp" && mv -f -- "$1.tmp" "$1"' sh "$1" "$2"
}

info "Binaire $VERSION"
ecrire_agent "$BIN_DIR/mirador-agent-$VERSION" 0755 < "$BINAIRE" || die "copie du binaire impossible"
runuser -u "$UTILISATEUR" -- sh -c 'ln -sfn "$1" "$2/current.nouveau" && mv -Tf "$2/current.nouveau" "$2/current"' \
  sh "mirador-agent-$VERSION" "$BIN_DIR" || die "lien current impossible"
# Copie root de l'assistant : jamais modifiable par l'agent.
install -o root -g root -m 0755 "$BINAIRE" "$ROOT_DIR/mirador-agent"

# ---------------------------------------------------------------- token
if [ "$MIRADOR" = oui ]; then
  # Fichiers de l'agent (il écrit lui-même son token après l'enrôlement),
  # écrits sous son identité (voir ecrire_agent).
  if [ -n "$TOKEN" ]; then
    printf '%s\n' "$TOKEN" | ecrire_agent "$VARLIB/token" 0600 || die "écriture du token impossible"
    runuser -u "$UTILISATEUR" -- rm -f -- "$VARLIB/jeton"
  elif [ -n "$JETON" ]; then
    if [ -s "$VARLIB/token" ]; then
      info "Token déjà présent : jeton ignoré (machine déjà enrôlée)"
    else
      printf '%s\n' "$JETON" | ecrire_agent "$VARLIB/jeton" 0600 || die "écriture du jeton impossible"
    fi
  fi
else
if [ -z "$TOKEN" ] && [ -r "$ETC/token" ]; then
  TOKEN=$(tr -d '[:space:]' < "$ETC/token")
  [ -n "$TOKEN" ] && info "Token existant repris"
fi
if [ -z "$TOKEN" ]; then
  TOKEN=$(openssl rand -hex 24 2>/dev/null || head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n')
  info "Nouveau token généré"
fi
umask 027
printf '%s\n' "$TOKEN" > "$ETC/token.tmp"
chown root:"$UTILISATEUR" "$ETC/token.tmp"; chmod 0640 "$ETC/token.tmp"
mv -f "$ETC/token.tmp" "$ETC/token"

fi

# ---------------------------------------------------------------- clés de signature
# Clés PUBLIQUES qui vérifient les mises à jour (SPEC § 18.25) : fichier root
# 0644, lisible par l'agent, jamais modifiable par lui ni par le serveur.
# Posées une fois ; changées seulement avec --cle-publique (rotation voulue
# par l'administrateur). Une clé publique n'est pas un secret.
FICHIER_CLES="$ETC/cles-publiques.txt"
cle_valide() { # CLE : base64 de 32 octets
  echo "$1" | grep -Eq '^[A-Za-z0-9+/]{43}=$' && [ "$(printf '%s' "$1" | base64 -d 2>/dev/null | wc -c)" -eq 32 ]
}
empreinte_cle() { printf '%s' "$1" | base64 -d | sha256sum | cut -c1-16; }
recuperer_cles() { # adresse publique, sans jeton ; 1 Kio au plus
  if command -v curl >/dev/null 2>&1; then
    curl -fsS --proto =https --tlsv1.2 --retry 3 "$SERVEUR/api/agent/v1/cles" 2>/dev/null | head -c 1024
  elif command -v wget >/dev/null 2>&1; then
    wget -q --https-only -O - "$SERVEUR/api/agent/v1/cles" 2>/dev/null | head -c 1024
  fi
}
if [ "$MIRADOR" = oui ]; then
  CLES=""
  if [ -n "$CLE_PUBLIQUE" ]; then
    CLES=$(echo "$CLE_PUBLIQUE" | tr ', ' '\n\n' | sed '/^$/d')
    ORIGINE="option --cle-publique"
  elif [ -s "$FICHIER_CLES" ]; then
    ORIGINE=""
    info "Clé de signature déjà posée : inchangée (--cle-publique pour la changer)"
  else
    CLES=$(recuperer_cles | tr -d '\r' | sed '/^$/d' || true)
    ORIGINE="serveur $SERVEUR"
  fi
  if [ -n "$ORIGINE" ]; then
    if [ -z "$CLES" ]; then
      echo "ATTENTION : aucune clé publique de signature (à régler dans l'interface :"
      echo "  Mises à jour des agents › Clé publique, puis relancer ce script)."
      echo "  Sans clé, l'agent refuse toute mise à jour (le reste fonctionne)."
    else
      [ "$(echo "$CLES" | wc -l)" -le 2 ] || die "deux clés publiques au plus"
      for k in $CLES; do cle_valide "$k" || die "clé publique invalide : $k"; done
      { echo "# Clés PUBLIQUES de signature des versions de mirador-agent ($ORIGINE)"
        echo "# Fichier root : l'agent le lit, ne peut pas le modifier."
        echo "$CLES"; } > "$FICHIER_CLES.tmp"
      chown root:root "$FICHIER_CLES.tmp"; chmod 0644 "$FICHIER_CLES.tmp"
      mv -f "$FICHIER_CLES.tmp" "$FICHIER_CLES"
      info "Clé de signature posée ($ORIGINE)"
    fi
  fi
  if [ -s "$FICHIER_CLES" ]; then
    for k in $(grep -v '^#' "$FICHIER_CLES"); do
      echo "    empreinte $(empreinte_cle "$k") : à comparer avec l'interface (Mises à jour des agents)"
    done
  fi
fi

# ---------------------------------------------------------------- configuration
config_agent > "$ETC/agent.conf.tmp"
chown root:"$UTILISATEUR" "$ETC/agent.conf.tmp"; chmod 0640 "$ETC/agent.conf.tmp"
mv -f "$ETC/agent.conf.tmp" "$ETC/agent.conf"
umask 022

info "Vérification de la configuration (en tant que $UTILISATEUR)"
runuser -u "$UTILISATEUR" -- "$BIN_DIR/current" -verifier -config "$ETC/agent.conf" \
  || die "configuration refusée (voir le message ci-dessus)"

# ---------------------------------------------------------------- proxy Docker
if [ "$USE_DOCKER" = oui ] && [ -z "$DOCKER_PROXY" ]; then
  if docker inspect "$PROXY_NOM" >/dev/null 2>&1; then
    info "Proxy Docker $PROXY_NOM déjà en place"
  else
    info "Proxy Docker en lecture seule ($PROXY_IMAGE, 127.0.0.1:$PROXY_PORT)"
    docker run -d --name "$PROXY_NOM" --restart unless-stopped \
      --read-only --tmpfs /run --tmpfs /tmp \
      --security-opt no-new-privileges:true --memory 32m \
      -p "127.0.0.1:$PROXY_PORT:2375" \
      -v /var/run/docker.sock:/var/run/docker.sock:ro \
      -e CONTAINERS=1 -e POST=0 -e INFO=0 -e PING=1 -e VERSION=1 \
      "$PROXY_IMAGE" >/dev/null
  fi
fi

# ---------------------------------------------------------------- unités systemd
info "Unités systemd"
# Dossier du résultat de l'assistant root : créé à chaque démarrage par
# systemd-tmpfiles (avant les services), groupe mirador-agent transmis aux
# fichiers créés dedans (bit setgid).
printf 'd /run/mirador-agent 2750 root %s -\n' "$UTILISATEUR" > "$TMPFILES"
chmod 0644 "$TMPFILES"
systemd-tmpfiles --create "$TMPFILES"
unite_agent  > "$UNITES/mirador-agent.service"
unite_root   > "$UNITES/mirador-agent-root.service"
timer_root   > "$UNITES/mirador-agent-root.timer"
chemin_root  > "$UNITES/mirador-agent-root.path"
unite_retour > "$UNITES/mirador-agent-retour.service"
chmod 0644 "$UNITES"/mirador-agent*.service "$UNITES/mirador-agent-root.timer" "$UNITES/mirador-agent-root.path"
systemctl daemon-reload
systemctl enable --now mirador-agent-root.timer >/dev/null
systemctl enable --now mirador-agent-root.path >/dev/null || echo "   (relance à la demande indisponible : l'assistant passe toutes les 15 min)"
systemctl start mirador-agent-root.service || echo "   (assistant root en échec : journalctl -u mirador-agent-root)"
systemctl enable mirador-agent.service >/dev/null
systemctl reset-failed mirador-agent.service 2>/dev/null || true
systemctl restart mirador-agent.service

info "Attente du démarrage"
sleep 5
systemctl is-active --quiet mirador-agent.service \
  || die "mirador-agent ne tourne pas : journalctl -u mirador-agent -n 40 --no-pager"

# ---------------------------------------------------------------- consignes mirador
if [ "$MIRADOR" = oui ]; then
  echo
  echo "================================================================"
  echo " mirador-agent $VERSION installé sur $NOM (mode mirador-server)"
  echo "================================================================"
  echo
  if [ -s "$VARLIB/jeton" ]; then
    echo " L'agent attend son approbation. Sur le serveur :"
    echo
    echo "    docker exec mirador-server /mirador-server -config /etc/mirador/server.conf machines"
    echo "    docker exec mirador-server /mirador-server -config /etc/mirador/server.conf approuver $NOM"
    echo
    echo " (« approuver » limite le token à l'adresse de la demande ; -sans-ip sinon.)"
  fi
  echo " Mises à jour automatiques : $MAJ_AUTO"
  echo " Suivi : journalctl -u mirador-agent -f"
  echo
  exit 0
fi

# ---------------------------------------------------------------- consignes nginx
echo
echo "================================================================"
echo " mirador-agent $VERSION installé sur $NOM"
echo "================================================================"
echo
echo " 1. Sur le VPS, dans le bloc « map » de /etc/nginx/conf.d/00-obs-map.conf :"
echo
printf '    "Bearer %s"    "%s";\n' "$TOKEN" "$NOM"
echo
echo " 2. Puis : nginx -t && systemctl reload nginx"
echo
echo " Tant que la ligne n'est pas ajoutée, l'agent reçoit des 403 : il garde les"
echo " métriques en mémoire (8 Mio, environ 19 h) et réessaie, rien n'est perdu."
echo
echo " Vérification, deux minutes après le rechargement de nginx :"
echo "    journalctl -u mirador-agent --since '3 min ago' --no-pager"
echo
