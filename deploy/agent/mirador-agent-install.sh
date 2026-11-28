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
#   - si Docker est présent : relais de socket en lecture seule (conteneur),
#     sur son propre réseau Docker à adresse fixe (une seule règle de
#     pare-feu, la même sur toutes les machines).
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
#   --docker-reseau PLAGE  plage PRIVÉE du réseau du relais Docker (/24 à /29 ;
#                        défaut 172.31.254.0/29 : passerelle .1, relais .2)
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
#   --sans-verification-signature
#                        DÉCONSEILLÉ : installer le binaire téléchargé sans vérifier
#                        sa signature, même si une clé publique est connue (par
#                        défaut, la signature est vérifiée avec openssl 3 avant
#                        tout lancement du binaire ; refus si impossible)
#   --accepter-version-ancienne
#                        accepter un binaire signé PLUS ANCIEN que celui en place
#                        (refusé par défaut : une vieille version signée peut
#                        avoir une faille connue)
#   --sans-filtre-relais ne pas réserver le relais Docker aux comptes mirador-agent
#                        et root (règles iptables, unité mirador-agent-relais)
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
# Image du proxy de socket Docker, version figée (vérifiée le 08/11/2026),
# épinglée par son empreinte (revue du 28/11, M1 : un tag peut être déplacé).
# Empreinte de l'index multi-architecture de v0.5.0, vérifiée le 28/11/2026
# (Docker Hub, registre et « docker pull » : mêmes octets). L'image ne sert
# que de programme HAProxy : sa configuration est celle écrite par ce script.
PROXY_IMAGE="tecnativa/docker-socket-proxy:v0.5.0@sha256:1f5038b54f06c3e18422902cf00ba21803d1c97805aae032e5e6673d532d3459"
# Configuration HAProxy du relais (liste blanche, voir config_relais).
PROXY_CONFIG=$ETC/relais-docker.cfg
PROXY_NOM=mirador-docker-proxy
PROXY_PORT=12375
# Mémoire du relais : 64 Mio (retour du 25/11 : 29,8 Mio utilisés pour une
# limite de 32 Mio ; le même relais sans limite en prend autant).
PROXY_MEMOIRE=64m
# Réseau dédié au relais, adresse fixe (retour du 24/11 : avec une sortie
# filtrée, l'adresse variable du pont par défaut obligeait à ouvrir toute sa
# plage). L'agent passe toujours par 127.0.0.1:$PROXY_PORT (il refuse toute
# autre adresse : le relais n'est pas chiffré) ; Docker relaie vers l'adresse
# fixe : c'est cette sortie-là que le pare-feu doit laisser passer.
PROXY_RESEAU=mirador-docker
PROXY_PONT=br-mirador
PROXY_PLAGE=172.31.254.0/29

BINAIRE=""; SERVEUR=""; SITE=""; ENV_=""; NOM=""; TOKEN=""; CLE_PUBLIQUE=""; MAJ_RACINE=""
DOCKER_MODE=auto; DOCKER_PROXY=""; PAREFEU_MODE=auto; SMART_MODE=auto
LOGS=oui; METRIQUES=oui; MODE=installer; MIRADOR=non; JETON=""; MAJ_AUTO=non
VERIF_SIGNATURE=oui; VERSION_ANCIENNE=non; FILTRE_RELAIS=oui

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
    --docker-reseau)   PROXY_PLAGE="${2:-}"; shift 2 ;;
    --pare-feu)        PAREFEU_MODE="${2:-}"; shift 2 ;;
    --smart)           SMART_MODE="${2:-}"; shift 2 ;;
    --sans-logs)       LOGS=non; shift ;;
    --sans-metriques)  METRIQUES=non; shift ;;
    --auto-update)     MAJ_AUTO=oui; shift ;;
    --detection)       MODE=detection; shift ;;
    --afficher-config) MODE=afficher; shift ;;
    --desinstaller)    MODE=desinstaller; shift ;;
    --sans-verification-signature) VERIF_SIGNATURE=non; shift ;;
    --accepter-version-ancienne)   VERSION_ANCIENNE=oui; shift ;;
    --sans-filtre-relais)          FILTRE_RELAIS=non; shift ;;
    -h|--help)         sed -n '2,/^$/p' "$0"; exit 0 ;;
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

# Plage du relais Docker : privée (RFC 1918), de /24 à /29, alignée ;
# passerelle = 1re adresse (l'hôte), relais = 2e.
plage_relais() {
  local p=$1 a b c d l taille
  [[ "$p" =~ ^([0-9]{1,3})\.([0-9]{1,3})\.([0-9]{1,3})\.([0-9]{1,3})/(2[4-9])$ ]] \
    || die "--docker-reseau : plage invalide « $p » (ex. 172.31.254.0/29, de /24 à /29)"
  a=${BASH_REMATCH[1]}; b=${BASH_REMATCH[2]}; c=${BASH_REMATCH[3]}; d=${BASH_REMATCH[4]}; l=${BASH_REMATCH[5]}
  { [ "$a" -le 255 ] && [ "$b" -le 255 ] && [ "$c" -le 255 ] && [ "$d" -le 255 ]; } \
    || die "--docker-reseau : plage invalide « $p »"
  if ! { [ "$a" -eq 10 ] || { [ "$a" -eq 172 ] && [ "$b" -ge 16 ] && [ "$b" -le 31 ]; } \
         || { [ "$a" -eq 192 ] && [ "$b" -eq 168 ]; }; }; then
    die "--docker-reseau : « $p » n'est pas une plage privée (10.x, 172.16 à 172.31, 192.168.x)"
  fi
  taille=$(( 1 << (32 - l) ))
  [ $(( d % taille )) -eq 0 ] || die "--docker-reseau : « $p » ne commence pas au début d'un bloc /$l"
  PROXY_PASSERELLE="$a.$b.$c.$(( d + 1 ))"
  PROXY_IP="$a.$b.$c.$(( d + 2 ))"
}
plage_relais "$PROXY_PLAGE"

[ "$(id -u)" -eq 0 ] || die "à lancer en root (sudo)"

# ---------------------------------------------------------------- désinstallation
if [ "$MODE" = desinstaller ]; then
  info "Arrêt et suppression de mirador-agent"
  # mirador-agent-relais : son arrêt retire ses règles iptables.
  systemctl disable --now mirador-agent.service mirador-agent-root.timer mirador-agent-root.path \
    mirador-agent-relais.service >/dev/null 2>&1 || true
  rm -f "$UNITES/mirador-agent.service" "$UNITES/mirador-agent-root.service" \
        "$UNITES/mirador-agent-root.timer" "$UNITES/mirador-agent-root.path" "$UNITES/mirador-agent-retour.service" \
        "$UNITES/mirador-agent-relais.service"
  systemctl daemon-reload
  if command -v docker >/dev/null 2>&1 && docker inspect "$PROXY_NOM" >/dev/null 2>&1; then
    docker rm -f "$PROXY_NOM" >/dev/null
  fi
  if command -v docker >/dev/null 2>&1 && docker network inspect "$PROXY_RESEAU" >/dev/null 2>&1; then
    docker network rm "$PROXY_RESEAU" >/dev/null || true
  fi
  rm -f "$TMPFILES"
  rm -rf /run/mirador-agent
  rm -f "$ETC/agent.conf" "$ETC/token" "$ETC/cles-publiques.txt" "$PROXY_CONFIG"
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
# iptables : pour réserver le relais Docker à l'agent (unite_relais).
IPT=$(command -v iptables 2>/dev/null || true)

afficher_detection() {
  echo
  echo "  Machine         : $NOM"
  [ -n "$NOM_ALLOY" ] && echo "  Nom chez Alloy  : $NOM_ALLOY"
  echo "  Étiquettes      : site=${SITE:-?}  env=${ENV_:-?}"
  echo "  Docker          : $A_DOCKER présent -> métriques et logs Docker : $USE_DOCKER"
  if [ "$USE_DOCKER" = oui ] && [ -z "$DOCKER_PROXY" ]; then
    echo "  Relais Docker   : 127.0.0.1:$PROXY_PORT -> $PROXY_IP:2375 (réseau $PROXY_RESEAU $PROXY_PLAGE, pont $PROXY_PONT)"
  fi
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
# Sans ce fichier (ancienne stack supprimée), la liste déjà écrite dans
# agent.conf est gardée : relancer le script ne la perd pas (retour du 24/11).
ignorer_systemd() {
  if [ -r /etc/obs-systemd-ignore ]; then
    sed 's/#.*//' /etc/obs-systemd-ignore | tr -s ' \t' '\n' | grep -E '\.service$' | paste -sd, - || true
  elif [ -r "$ETC/agent.conf" ]; then
    sed -n 's/^MIRADOR_SYSTEMD_IGNORER=//p' "$ETC/agent.conf" | head -1 | grep -E '^[A-Za-z0-9@_.:,-]*$' || true
  fi
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

# Durcissement : celui de l'agent (revue du 28/11), sans aucun réseau (le
# retour arrière ne fait que changer le lien « current »). Les deux lignes
# « + » ci-dessus (systemctl, en root) n'y sont pas soumises.
NoNewPrivileges=yes
CapabilityBoundingSet=
AmbientCapabilities=
ProtectSystem=strict
ProtectHome=read-only
ReadWritePaths=/var/lib/mirador-agent
PrivateTmp=yes
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
RestrictAddressFamilies=AF_UNIX
IPAddressDeny=any
SystemCallArchitectures=native
SystemCallFilter=@system-service
SystemCallFilter=~@privileged
SystemCallErrorNumber=EPERM
EOF
}

# Configuration HAProxy du relais Docker (revue de sécurité du 28/11, M1).
# L'image tecnativa, réglée par ses variables (CONTAINERS=1), laissait passer
# TOUT ce qui commence par /containers en lecture, dont /archive et /export
# (fichiers des conteneurs, volumes compris). Ici : liste blanche exacte des
# chemins dont l'agent a besoin, en GET seulement.
config_relais() {
  cat <<'EOF'
# relais-docker.cfg : configuration HAProxy du relais Docker de mirador-agent
# (écrite par mirador-agent-install.sh, montée en lecture seule).
# Lecture seule, LISTE BLANCHE : les seuls chemins de l'API Docker utilisés
# par l'agent (internal/agent/collect/docker.go, internal/agent/dockerlogs),
# en GET :
#   /_ping                  vérification par le script d'installation
#   /containers/json        liste des conteneurs
#   /containers/<id>/json   détail (nom, état, redémarrages, tty)
#   /containers/<id>/stats  CPU, mémoire, réseau, disque
#   /containers/<id>/logs   logs des conteneurs
# Tout le reste est refusé (403) : /archive, /export, /attach, /exec,
# /images, /volumes…, et toute écriture (POST, PUT, DELETE).
global
    log stdout format raw daemon info
    maxconn 4000

defaults
    mode http
    log global
    option httplog
    option dontlognull
    option http-server-close
    timeout http-request 10s
    timeout queue 1m
    timeout connect 10s
    # Logs suivis en continu : flux coupé après 10 min sans ligne, l'agent
    # le rouvre (mêmes valeurs que la configuration d'origine de l'image).
    timeout client 10m
    timeout server 10m
    timeout http-keep-alive 10s
    maxconn 3000

backend docker
    server socket /var/run/docker.sock

frontend relais
    bind :2375
    # <id> : identifiant hexadécimal du conteneur (12 à 64 caractères) ;
    # préfixe de version de l'API facultatif (/v1.47/...). Chemin exact.
    acl lecture method GET
    acl chemin_permis path -m reg '^(/v1[.][0-9]{1,3})?/(_ping|containers/json|containers/[0-9a-f]{12,64}/(json|stats|logs))$'
    http-request deny unless lecture chemin_permis
    # Seuls les refus sont journalisés (docker logs mirador-docker-proxy).
    http-request set-log-level silent
    default_backend docker
EOF
}

# Qui peut joindre le relais (revue de sécurité du 28/11, M1) : sans règle,
# tout compte local de la machine pouvait lire l'API Docker par
# 127.0.0.1:PORT (ou directement par l'adresse du relais). Règles iptables de
# SORTIE, dans une chaîne à part : une connexion NOUVELLE vers le relais
# n'est permise qu'aux comptes root (Docker lui-même) et mirador-agent.
# Ces règles ne font que REFUSER : elles n'ouvrent rien et ne touchent pas
# au reste du pare-feu (lu, jamais modifié, par l'agent : SPEC § 136).
# Unité oneshot : règles remises à chaque démarrage, retirées à l'arrêt.
unite_relais() {
  cat <<EOF
# mirador-agent-relais — relais Docker réservé aux comptes root et
# $UTILISATEUR (règles iptables, chaîne MIRADOR-RELAIS).
[Unit]
Description=mirador-agent : relais Docker réservé à $UTILISATEUR et root
After=docker.service nftables.service netfilter-persistent.service ufw.service firewalld.service

[Service]
Type=oneshot
RemainAfterExit=yes
# « - » devant la commande : erreur ignorée (chaîne déjà créée, règle absente).
ExecStart=-$IPT -w -N MIRADOR-RELAIS
ExecStart=$IPT -w -F MIRADOR-RELAIS
ExecStart=$IPT -w -A MIRADOR-RELAIS -m conntrack --ctstate ESTABLISHED,RELATED -j RETURN
ExecStart=$IPT -w -A MIRADOR-RELAIS -m owner --uid-owner 0 -j RETURN
ExecStart=$IPT -w -A MIRADOR-RELAIS -m owner --uid-owner $UTILISATEUR -j RETURN
ExecStart=$IPT -w -A MIRADOR-RELAIS -p tcp -j REJECT --reject-with tcp-reset
ExecStart=-$IPT -w -D OUTPUT -d 127.0.0.1/32 -p tcp --dport $PROXY_PORT -j MIRADOR-RELAIS
ExecStart=$IPT -w -I OUTPUT 1 -d 127.0.0.1/32 -p tcp --dport $PROXY_PORT -j MIRADOR-RELAIS
ExecStart=-$IPT -w -D OUTPUT -d $PROXY_IP/32 -p tcp --dport 2375 -j MIRADOR-RELAIS
ExecStart=$IPT -w -I OUTPUT 1 -d $PROXY_IP/32 -p tcp --dport 2375 -j MIRADOR-RELAIS
ExecStop=-$IPT -w -D OUTPUT -d 127.0.0.1/32 -p tcp --dport $PROXY_PORT -j MIRADOR-RELAIS
ExecStop=-$IPT -w -D OUTPUT -d $PROXY_IP/32 -p tcp --dport 2375 -j MIRADOR-RELAIS
ExecStop=-$IPT -w -F MIRADOR-RELAIS
ExecStop=-$IPT -w -X MIRADOR-RELAIS

[Install]
WantedBy=multi-user.target
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
  if [ "$USE_DOCKER" = oui ] && [ -z "$DOCKER_PROXY" ]; then
    echo "----- $PROXY_CONFIG"; config_relais
    if [ "$FILTRE_RELAIS" = oui ] && [ -n "$IPT" ]; then
      echo "----- mirador-agent-relais.service"; unite_relais
    fi
  fi
  exit 0
fi

# ---------------------------------------------------------------- binaire
# Sans --binaire : téléchargé en HTTPS depuis mirador-server, avec le jeton
# d'enrôlement, ou, pour une machine déjà enrôlée qui relance le script, son
# token (en-tête Authorization, empreinte SHA-256 vérifiée).
# Le jeton passe par un fichier de configuration de curl / wget (dans un
# dossier temporaire root 0700), jamais par la ligne de commande : il serait
# lisible par tout utilisateur local avec « ps » (revue du 11/11).
TELE=""; ESSAI=""
nettoyer() { # dossiers temporaires, effacés à la sortie du script
  if [ -n "$TELE" ]; then rm -rf -- "$TELE"; fi
  if [ -n "$ESSAI" ]; then rm -rf -- "$ESSAI"; fi
}
trap nettoyer EXIT
nouveau_tele() { # dossier temporaire root 0700
  if [ -z "$TELE" ]; then
    umask 077
    TELE=$(mktemp -d)
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

# ---------------------------------------------------------------- signature du binaire
# Revue de sécurité du 28/11 (M2) : quand une clé PUBLIQUE de signature est
# déjà connue sur cette machine (--cle-publique, ou fichier posé par une
# installation précédente), le binaire téléchargé n'est lancé et installé
# qu'après vérification de son manifeste signé (Ed25519), envoyé par le
# serveur avec lui (en-tête X-Mirador-Manifeste). Un serveur piraté ne peut
# donc plus faire installer, en root, un binaire qui n'a pas été signé sur le
# poste de l'administrateur. Vérification faite par openssl (3.0 ou plus).
# Ces cinq fonctions sont les mêmes dans mirador-agent-install.sh et
# mirador-collector-install.sh (vérifié par deploy/agent/script_test.go).

# valeur_entete FICHIER NOM : valeur du dernier en-tête NOM (en minuscules)
# du fichier d'en-têtes écrit par curl (-D) ou wget (-S).
valeur_entete() {
  tr -d '\r' < "$1" | sed 's/^ *//' | awk -F': ' -v n="$2" 'tolower($1)==n{print $2}' | tail -1
}

# champ_json JSON CLE : valeur d'un champ du manifeste (JSON écrit par
# mirador-server sur une ligne, sans espace) : texte entre guillemets, nombre
# ou true/false. Vide si le champ est absent.
champ_json() {
  printf '%s' "$1" | grep -oE "\"$2\":(\"[^\"]*\"|[a-z0-9]+)" | head -1 | sed -E 's/^"[a-z0-9_]+"://; s/^"(.*)"$/\1/' || true
}

# openssl_recent : openssl 3.0 ou plus est-il là ? (« pkeyutl -rawin »,
# nécessaire pour vérifier une signature Ed25519, n'existe pas avant.)
openssl_recent() {
  local v
  command -v openssl >/dev/null 2>&1 || return 1
  v=$(openssl version 2>/dev/null | awk '$1=="OpenSSL"{print $2}')
  [ "${v%%.*}" -ge 3 ] 2>/dev/null
}

# verifier_manifeste ENTETES BINAIRE PRODUIT ARCH CLES : vérifie, SANS lancer
# le binaire :
#   1. la signature du manifeste, avec une des clés publiques (CLES : une
#      par ligne, en base64) ;
#   2. que le manifeste décrit bien CE binaire : produit, architecture,
#      empreinte SHA-256 et taille.
# Les octets signés sont refaits comme Manifeste.MessageSigne() (fichier
# internal/protocole/protocole.go) : une ligne par champ, dans cet ordre :
#   produit, version, architecture, sha256, taille, oui|non (retour
#   autorisé), puis la date de fin du retour arrière si elle existe.
# Résultat : VERSION_SIGNEE (version du manifeste), RETOUR_SIGNE (oui|non)
# et RETOUR_JUSQUA (date Unix, 0 sans date). Arrêt du script sinon.
verifier_manifeste() {
  local entetes=$1 binaire=$2 produit=$3 arch=$4 cles=$5
  local brut json p v a s t r j sig k dir ok=non
  brut=$(valeur_entete "$entetes" x-mirador-manifeste)
  if [ -z "$brut" ]; then
    if [ "$(valeur_entete "$entetes" x-mirador-signe)" = non ]; then
      die "le serveur n'a qu'un binaire « embarqué » NON SIGNÉ, alors qu'une clé de signature est connue sur cette machine. Publier une version signée (Machines › Mises à jour des agents) puis relancer ; ou, en connaissance de cause, --sans-verification-signature (déconseillé)"
    fi
    die "le serveur n'a pas envoyé de manifeste signé (mirador-server trop ancien ?). Le mettre à jour puis relancer ; ou --sans-verification-signature (déconseillé)"
  fi
  # (Longueur testée à part : un « {20,4000} » rend grep très lent.)
  [ "${#brut}" -le 4000 ] && printf '%s' "$brut" | grep -Eq '^[A-Za-z0-9+/]+={0,2}$' || die "manifeste signé illisible"
  json=$(printf '%s' "$brut" | base64 -d 2>/dev/null | head -c 4096 | tr -d '\n') || die "manifeste signé illisible"
  # Un JSON piégé ne peut rien obtenir : ce sont ces MÊMES valeurs qui sont
  # vérifiées par la signature, puis comparées au binaire.
  p=$(champ_json "$json" produit); v=$(champ_json "$json" version)
  a=$(champ_json "$json" architecture); s=$(champ_json "$json" sha256)
  t=$(champ_json "$json" taille); r=$(champ_json "$json" retour_autorise)
  j=$(champ_json "$json" retour_jusqua); sig=$(champ_json "$json" signature)
  [ -n "$p" ] || p=mirador-agent # produit absent : manifeste de l'agent
  echo "$v" | grep -Eq '^[0-9]{1,5}\.[0-9]{1,5}\.[0-9]{1,5}$' || die "manifeste : version invalide « $v »"
  echo "$s" | grep -Eq '^[0-9a-f]{64}$' || die "manifeste : empreinte invalide"
  echo "$t" | grep -Eq '^[0-9]{1,9}$' || die "manifeste : taille invalide"
  echo "$sig" | grep -Eq '^[A-Za-z0-9+/]{86}==$' || die "manifeste : signature mal formée"
  case "$r" in true) r=oui ;; false) r=non ;; *) die "manifeste : champ retour_autorise invalide" ;; esac
  [ -n "$j" ] || j=0
  echo "$j" | grep -Eq '^[0-9]{1,12}$' || die "manifeste : date de retour invalide"
  # Octets signés, puis vérification avec chaque clé connue.
  dir=$(mktemp -d)
  printf '%s\n' "$p" "$v" "$a" "$s" "$t" "$r" > "$dir/message"
  [ "$j" = 0 ] || printf '%s\n' "$j" >> "$dir/message"
  printf '%s' "$sig" | base64 -d > "$dir/signature" 2>/dev/null || true
  for k in $cles; do
    # Clé publique brute (32 octets) -> format DER lu par openssl : en-tête
    # fixe des clés Ed25519 (RFC 8410), puis la clé.
    { printf '\060\052\060\005\006\003\053\145\160\003\041\000'; printf '%s' "$k" | base64 -d 2>/dev/null; } > "$dir/cle.der" || true
    if openssl pkeyutl -verify -pubin -keyform DER -inkey "$dir/cle.der" -rawin \
         -in "$dir/message" -sigfile "$dir/signature" >/dev/null 2>&1; then
      ok=oui
      break
    fi
  done
  rm -rf -- "$dir"
  [ "$ok" = oui ] || die "SIGNATURE INVALIDE : le binaire envoyé par le serveur n'est signé par aucune clé connue de cette machine. Rien n'a été installé. (Serveur piraté, ou clé changée : voir --cle-publique.)"
  # Le manifeste (authentique) décrit-il bien le fichier reçu ?
  [ "$p" = "$produit" ] || die "manifeste signé d'un autre produit ($p, attendu $produit)"
  [ "$a" = "$arch" ] || die "manifeste signé d'une autre architecture ($a, attendu $arch)"
  [ "$(sha256sum "$binaire" | awk '{print $1}')" = "$s" ] || die "le binaire reçu ne correspond pas au manifeste signé (empreinte)"
  [ "$(stat -c %s "$binaire")" = "$t" ] || die "le binaire reçu ne correspond pas au manifeste signé (taille)"
  VERSION_SIGNEE=$v; RETOUR_SIGNE=$r; RETOUR_JUSQUA=$j
}

# version_plus_ancienne A B : A est-elle strictement plus ancienne que B ?
# (versions 1.2.3 ; sinon : non)
version_plus_ancienne() {
  echo "$1" | grep -Eq '^[0-9]{1,5}\.[0-9]{1,5}\.[0-9]{1,5}$' || return 1
  echo "$2" | grep -Eq '^[0-9]{1,5}\.[0-9]{1,5}\.[0-9]{1,5}$' || return 1
  [ "$1" != "$2" ] && [ "$(printf '%s\n%s\n' "$1" "$2" | sort -V | head -1)" = "$1" ]
}

# Clés publiques DÉJÀ connues sur cette machine : --cle-publique, sinon le
# fichier posé par une installation précédente. (À la toute première
# installation sans --cle-publique, la clé vient du serveur lui-même : elle
# ne prouverait rien sur son binaire, la vérification n'a donc pas lieu.)
FICHIER_CLES="$ETC/cles-publiques.txt"
cle_valide() { # CLE : base64 de 32 octets
  echo "$1" | grep -Eq '^[A-Za-z0-9+/]{43}=$' && [ "$(printf '%s' "$1" | base64 -d 2>/dev/null | wc -c)" -eq 32 ]
}
CLES_CONNUES=""
if [ -n "$CLE_PUBLIQUE" ]; then
  CLES_CONNUES=$(echo "$CLE_PUBLIQUE" | tr ', ' '\n\n' | sed '/^$/d')
  for k in $CLES_CONNUES; do cle_valide "$k" || die "clé publique invalide : $k"; done
elif [ -s "$FICHIER_CLES" ]; then
  CLES_CONNUES=$(grep -v '^#' "$FICHIER_CLES" | sed '/^$/d' || true)
fi
SIGNATURE_VERIFIEE=non; VERSION_SIGNEE=""; RETOUR_SIGNE=non; RETOUR_JUSQUA=0

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
  ATTENDU=$(valeur_entete "$TELE/entetes" x-mirador-sha256)
  OBTENU=$(sha256sum "$TELE/mirador-agent" | awk '{print $1}')
  [ -n "$ATTENDU" ] && [ "$ATTENDU" = "$OBTENU" ] || die "empreinte du binaire téléchargé fausse (attendu ${ATTENDU:-?}, obtenu $OBTENU)"
  # Signature : vérifiée AVANT tout lancement du binaire.
  if [ -n "$CLES_CONNUES" ] && [ "$VERIF_SIGNATURE" = oui ]; then
    openssl_recent || die "openssl 3.0 ou plus est nécessaire pour vérifier la signature du binaire (ici : $(openssl version 2>/dev/null || echo 'openssl absent')). Installer openssl 3 (Debian 12, Ubuntu 22.04 ou plus récents), ou, en connaissance de cause, --sans-verification-signature (déconseillé)"
    verifier_manifeste "$TELE/entetes" "$TELE/mirador-agent" mirador-agent "$ARCH" "$CLES_CONNUES"
    SIGNATURE_VERIFIEE=oui
    info "Signature du binaire vérifiée (version $VERSION_SIGNEE)"
  elif [ -n "$CLES_CONNUES" ]; then
    echo "ATTENTION : signature du binaire NON vérifiée (--sans-verification-signature)."
  fi
  BINAIRE="$TELE/mirador-agent"
fi
[ -n "$BINAIRE" ] || die "--binaire est obligatoire (sauf avec --jeton ou une machine déjà enrôlée : téléchargé depuis le serveur)"
[ -f "$BINAIRE" ] || die "binaire introuvable : $BINAIRE"
if [ "$SIGNATURE_VERIFIEE" = non ] && [ -n "$CLES_CONNUES" ] && [ "$VERIF_SIGNATURE" = oui ]; then
  echo "    (binaire local donné par --binaire : signature non vérifiée, c'est votre fichier)"
fi

# ---------------------------------------------------------------- utilisateur et dossiers
info "Utilisateur système $UTILISATEUR"
if ! id "$UTILISATEUR" >/dev/null 2>&1; then
  useradd --system --no-create-home --home-dir "$VARLIB" --shell /usr/sbin/nologin "$UTILISATEUR"
fi
install -d -o root -g "$UTILISATEUR" -m 0750 "$ETC"
install -d -o "$UTILISATEUR" -g "$UTILISATEUR" -m 0750 "$VARLIB"
install -d -o "$UTILISATEUR" -g "$UTILISATEUR" -m 0750 "$BIN_DIR"
install -d -o root -g root -m 0755 "$ROOT_DIR"

# ---------------------------------------------------------------- version du binaire
# Lancé SOUS LE COMPTE de l'agent, jamais en root (revue du 28/11), et
# seulement après la vérification de signature. Copie à root dans un dossier
# lisible par l'agent (celui du téléchargement est réservé à root).
ESSAI=$(mktemp -d)
chmod 0755 "$ESSAI"
install -o root -g root -m 0755 "$BINAIRE" "$ESSAI/mirador-agent"
VERSION=$(runuser -u "$UTILISATEUR" -- timeout 30 "$ESSAI/mirador-agent" -version 2>/dev/null | awk '{print $2}') || true
[ -n "$VERSION" ] || die "le binaire ne se lance pas : fichier abîmé, ou mauvaise architecture (cette machine : $(uname -m))"
echo "$VERSION" | grep -Eq '^[A-Za-z0-9._-]{1,40}$' || die "version illisible : $VERSION"
if [ "$SIGNATURE_VERIFIEE" = oui ]; then
  [ "$VERSION" = "$VERSION_SIGNEE" ] || die "le binaire annonce la version $VERSION, son manifeste signé la version $VERSION_SIGNEE : refusé"
  # Pas de retour vers une version plus ancienne que celle en place (une
  # vieille version signée peut avoir une faille connue), sauf retour
  # arrière signé encore valable, ou --accepter-version-ancienne.
  EN_PLACE=$(readlink "$BIN_DIR/current" 2>/dev/null | sed -n 's/^mirador-agent-//p' || true)
  if [ "$VERSION_ANCIENNE" = non ] && version_plus_ancienne "$VERSION" "$EN_PLACE"; then
    if [ "$RETOUR_SIGNE" = oui ] && [ "$RETOUR_JUSQUA" -gt "$(date +%s)" ]; then
      info "Retour à la version $VERSION (retour arrière signé)"
    else
      die "le serveur envoie la version $VERSION, plus ancienne que celle en place ($EN_PLACE) : refusé. Si c'est voulu : --accepter-version-ancienne"
    fi
  fi
fi

afficher_detection

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
# (FICHIER_CLES et cle_valide : définis plus haut, section « binaire ».)
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
# relais_joignable : le relais répond-il à /_ping, par le chemin de l'agent
# (127.0.0.1:PORT, puis Docker vers l'adresse fixe) ? 5 essais, sans curl.
relais_joignable() {
  local i
  for i in 1 2 3 4 5; do
    if timeout 3 bash -c "exec 3<>/dev/tcp/127.0.0.1/$PROXY_PORT && printf 'GET /_ping HTTP/1.0\r\n\r\n' >&3 && head -c 12 <&3" 2>/dev/null \
         | grep -q ' 200'; then
      return 0
    fi
    sleep 1
  done
  return 1
}
# retirer_filtre_relais : arrête l'unité des règles du relais (son arrêt
# retire les règles) et la supprime, si elle existe.
retirer_filtre_relais() {
  if [ -f "$UNITES/mirador-agent-relais.service" ]; then
    systemctl disable --now mirador-agent-relais.service >/dev/null 2>&1 || true
    rm -f "$UNITES/mirador-agent-relais.service"
    systemctl daemon-reload
  fi
}
if [ "$USE_DOCKER" = oui ] && [ -z "$DOCKER_PROXY" ]; then
  # Réseau dédié : adresse fixe, pont nommé, conteneurs isolés entre eux.
  if docker network inspect "$PROXY_RESEAU" >/dev/null 2>&1; then
    plage_actuelle=$(docker network inspect "$PROXY_RESEAU" -f '{{range .IPAM.Config}}{{.Subnet}}{{end}}')
    [ "$plage_actuelle" = "$PROXY_PLAGE" ] \
      || die "le réseau Docker $PROXY_RESEAU existe déjà avec la plage $plage_actuelle (attendue $PROXY_PLAGE) : relancer avec --docker-reseau $plage_actuelle"
  else
    info "Réseau Docker $PROXY_RESEAU ($PROXY_PLAGE, pont $PROXY_PONT)"
    docker network create --driver bridge --subnet "$PROXY_PLAGE" --gateway "$PROXY_PASSERELLE" \
      -o com.docker.network.bridge.name="$PROXY_PONT" \
      -o com.docker.network.bridge.enable_icc=false \
      --label mirador=relais-docker "$PROXY_RESEAU" >/dev/null \
      || die "création du réseau $PROXY_RESEAU impossible (plage déjà prise ? --docker-reseau AUTRE_PLAGE)"
  fi
  # Configuration HAProxy (liste blanche) : fichier root 0644, monté en
  # lecture seule dans le conteneur ; son empreinte (avec l'image) est posée
  # en étiquette sur le conteneur pour savoir s'il faut le recréer.
  config_relais > "$PROXY_CONFIG.tmp"
  chown root:root "$PROXY_CONFIG.tmp"; chmod 0644 "$PROXY_CONFIG.tmp"
  mv -f "$PROXY_CONFIG.tmp" "$PROXY_CONFIG"
  EMPREINTE_RELAIS=$({ cat "$PROXY_CONFIG"; echo "$PROXY_IMAGE"; } | sha256sum | cut -c1-16)
  # Relais déjà en place, au bon endroit, même configuration ? Sinon
  # (ancienne installation : pont par défaut, adresse changée, relais réglé
  # par variables avant le 28/11, conteneur arrêté) : recréé.
  place=""; etiquette=""
  if docker inspect "$PROXY_NOM" >/dev/null 2>&1; then
    place=$(docker inspect "$PROXY_NOM" -f '{{range $n, $r := .NetworkSettings.Networks}}{{$n}}={{$r.IPAddress}} {{end}}')
    etiquette=$(docker inspect "$PROXY_NOM" -f '{{index .Config.Labels "mirador.relais"}}' 2>/dev/null || true)
  fi
  if [ "$place" = "$PROXY_RESEAU=$PROXY_IP " ] && [ "$etiquette" = "$EMPREINTE_RELAIS" ]; then
    info "Relais Docker $PROXY_NOM déjà en place ($PROXY_IP)"
    # Limite de mémoire remise à jour (ancienne installation : 32 Mio).
    docker update --memory "$PROXY_MEMOIRE" --memory-swap "$PROXY_MEMOIRE" "$PROXY_NOM" >/dev/null \
      || echo "   (limite de mémoire du relais non changée : docker update a échoué)"
  else
    if docker inspect "$PROXY_NOM" >/dev/null 2>&1; then docker rm -f "$PROXY_NOM" >/dev/null; fi
    info "Relais Docker en lecture seule, liste blanche ($PROXY_IMAGE, 127.0.0.1:$PROXY_PORT -> $PROXY_IP:2375)"
    # L'image lance HAProxy avec NOTRE configuration (-f) au lieu de la
    # sienne : son entrée (docker-entrypoint.sh) ajoute seulement « -W -db ».
    docker run -d --name "$PROXY_NOM" --restart unless-stopped \
      --label "mirador.relais=$EMPREINTE_RELAIS" \
      --network "$PROXY_RESEAU" --ip "$PROXY_IP" \
      -p "127.0.0.1:$PROXY_PORT:2375" \
      --read-only --tmpfs /run --tmpfs /tmp \
      --security-opt no-new-privileges:true --memory "$PROXY_MEMOIRE" --memory-swap "$PROXY_MEMOIRE" \
      -v /var/run/docker.sock:/var/run/docker.sock:ro \
      -v "$PROXY_CONFIG:/usr/local/etc/haproxy/mirador.cfg:ro" \
      "$PROXY_IMAGE" haproxy -f /usr/local/etc/haproxy/mirador.cfg >/dev/null \
      || die "démarrage du relais Docker impossible (docker logs $PROXY_NOM)"
  fi
  # Relais réservé aux comptes root et mirador-agent (règles iptables).
  if [ "$FILTRE_RELAIS" = oui ] && [ -n "$IPT" ]; then
    systemctl stop mirador-agent-relais.service 2>/dev/null || true # anciennes règles retirées
    unite_relais > "$UNITES/mirador-agent-relais.service"
    chmod 0644 "$UNITES/mirador-agent-relais.service"
    systemctl daemon-reload
    if systemctl enable --now mirador-agent-relais.service >/dev/null 2>&1; then
      info "Relais Docker réservé aux comptes root et $UTILISATEUR (iptables, chaîne MIRADOR-RELAIS)"
    else
      echo "   ATTENTION : règles iptables du relais refusées (journalctl -u mirador-agent-relais) :"
      echo "   tout compte local de cette machine peut lire l'API Docker par le relais."
    fi
  else
    retirer_filtre_relais
    if [ "$FILTRE_RELAIS" = oui ]; then
      echo "   ATTENTION : iptables absent : tout compte local de cette machine peut lire l'API"
      echo "   Docker par le relais (127.0.0.1:$PROXY_PORT)."
    fi
  fi
  if ! relais_joignable; then
    echo "   ATTENTION : le relais Docker ne répond pas (127.0.0.1:$PROXY_PORT -> $PROXY_IP:2375)."
    echo "   Cause habituelle : un pare-feu qui bloque la SORTIE (OUTPUT en DROP)."
    echo "   Règle à ajouter (puis à rendre permanente) :"
    echo "     iptables -I OUTPUT 1 -o $PROXY_PONT -s $PROXY_PASSERELLE -d $PROXY_IP -p tcp --dport 2375 -j ACCEPT"
    echo "   (sans elle : pas de métriques ni de logs Docker, alerte « Docker : démon injoignable »)"
  fi
else
  # Pas de relais créé par ce script (sans Docker, ou --docker-proxy) :
  # règles d'une installation précédente retirées.
  retirer_filtre_relais
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
