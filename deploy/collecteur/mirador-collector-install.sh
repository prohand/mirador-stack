#!/usr/bin/env bash
# SPDX-License-Identifier: GPL-3.0-or-later
#
# mirador-collector-install.sh — installation de mirador-collector
#
# Le collecteur du LAN (SPEC.md § 23.13) relève les équipements sans agent
# (Synology, UniFi, imprimante…) et envoie au serveur mirador en HTTPS. À
# installer sur une machine Debian/Ubuntu du LAN (un LXC suffit) :
#   - utilisateur système « mirador-collector » (sans droits root) ;
#   - binaire dans /var/lib/mirador-collector/bin (lien « current ») ;
#   - configuration dans /etc/mirador-collector ;
#   - unités systemd durcies : collecteur, retour arrière, compagnon des
#     processus (sans réseau, compte « mirador-processus », binaire posé par
#     root dans /usr/local/libexec : le collecteur ne peut pas le remplacer) ;
#   - ping sans droits pour son groupe (net.ipv4.ping_group_range, sondes).
#
# Usage :
#   MIRADOR_JETON=mirador_enrol_… bash mirador-collector-install.sh \
#       --serveur https://mirador.example.org --nom collecteur-lan
#   (script et binaire téléchargés depuis le serveur :
#    curl -fsSL https://…/api/agent/v1/install/collecteur -o mirador-collector-install.sh)
#
# Options :
#   --serveur URL        adresse de mirador-server (https://…)       OBLIGATOIRE
#   --nom NOM            nom du collecteur (celui choisi dans l'interface)
#   --jeton VALEUR       jeton d'enrôlement (« Ajouter un collecteur ») ; mieux :
#                        variable MIRADOR_JETON (invisible dans « ps »)
#   --binaire FICHIER    binaire à installer (sinon téléchargé depuis le serveur,
#                        empreinte vérifiée)
#   --auto-update        mises à jour signées automatiques
#   --cle-publique CLE   clé PUBLIQUE de signature (base64) ; sinon demandée au
#                        serveur à la première installation, puis jamais changée
#   --sans-verification-signature
#                        DÉCONSEILLÉ : installer le binaire téléchargé sans vérifier
#                        sa signature, même si une clé publique est connue (par
#                        défaut, la signature est vérifiée avec openssl 3 avant
#                        tout lancement du binaire ; refus si impossible)
#   --accepter-version-ancienne
#                        accepter un binaire signé PLUS ANCIEN que celui en place
#   --afficher-config    affiche la configuration et les unités, et s'arrête
#   --desinstaller       arrête et supprime mirador-collector
#
# Rejouable : un second passage garde le token et le nom en place.

set -euo pipefail
export PATH="/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
umask 022

# ---------------------------------------------------------------- constantes
UTILISATEUR=mirador-collector
ETC=/etc/mirador-collector
VARLIB=/var/lib/mirador-collector
BIN_DIR=$VARLIB/bin
UNITES=/etc/systemd/system
# Compagnon des processus (SPEC § 120.1) : voit tous les processus, sans
# réseau, sous son propre compte. Binaire à root : remis à jour à chaque
# passage de ce script (pas par la mise à jour automatique du collecteur).
UTILISATEUR_PROC=mirador-processus
BIN_PROC=/usr/local/libexec/mirador-collector-processus

BINAIRE=""; SERVEUR=""; NOM=""; JETON=""; CLE_PUBLIQUE=""; MAJ_AUTO=non; MODE=installer
VERIF_SIGNATURE=oui; VERSION_ANCIENNE=non

die()  { echo "ERREUR : $*" >&2; exit 1; }
info() { echo "==> $*"; }

while [ $# -gt 0 ]; do
  case "$1" in
    --serveur)         SERVEUR="${2:-}"; shift 2 ;;
    --nom)             NOM="${2:-}"; shift 2 ;;
    --jeton)           JETON="${2:-}"; shift 2 ;;
    --binaire)         BINAIRE="${2:-}"; shift 2 ;;
    --auto-update)     MAJ_AUTO=oui; shift ;;
    --cle-publique)    CLE_PUBLIQUE="${2:-}"; shift 2 ;;
    --afficher-config) MODE=afficher; shift ;;
    --desinstaller)    MODE=desinstaller; shift ;;
    --sans-verification-signature) VERIF_SIGNATURE=non; shift ;;
    --accepter-version-ancienne)   VERSION_ANCIENNE=oui; shift ;;
    -h|--help)         sed -n '2,/^$/p' "$0"; exit 0 ;;
    *)                 die "option inconnue : $1 (voir --help)" ;;
  esac
done
if [ -z "$JETON" ] && [ -n "${MIRADOR_JETON:-}" ]; then
  JETON=$MIRADOR_JETON
fi
unset MIRADOR_JETON

[ "$(id -u)" -eq 0 ] || die "à lancer en root (sudo)"

# ---------------------------------------------------------------- désinstallation
if [ "$MODE" = desinstaller ]; then
  info "Arrêt et suppression de mirador-collector"
  systemctl disable --now mirador-collector.service mirador-collector-processus.service >/dev/null 2>&1 || true
  rm -f "$UNITES/mirador-collector.service" "$UNITES/mirador-collector-retour.service" "$UNITES/mirador-collector-processus.service"
  rm -f "$BIN_PROC"
  rm -f /etc/sysctl.d/60-mirador-collector-ping.conf
  systemctl daemon-reload
  rm -rf -- "$ETC" "$VARLIB"
  if id "$UTILISATEUR" >/dev/null 2>&1; then userdel "$UTILISATEUR" 2>/dev/null || true; fi
  if id "$UTILISATEUR_PROC" >/dev/null 2>&1; then userdel "$UTILISATEUR_PROC" 2>/dev/null || true; fi
  info "Fait. Retirer aussi le collecteur dans l'interface (Machines)."
  exit 0
fi

# ---------------------------------------------------------------- contrôles
[ -n "$SERVEUR" ] || die "--serveur est obligatoire"
echo "$SERVEUR" | grep -Eq '^https://[A-Za-z0-9.-]+(:[0-9]{1,5})?$' || die "--serveur : https://nom[:port] attendu, sans chemin"
if [ -z "$NOM" ] && [ -r "$ETC/collector.conf" ]; then
  NOM=$(sed -n 's/^MIRADOR_NOM=//p' "$ETC/collector.conf" | head -1)
fi
[ -n "$NOM" ] || NOM=$(hostname -s 2>/dev/null || true)
echo "$NOM" | grep -Eq '^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$' || die "--nom : lettres, chiffres, . _ - (63 caractères)"
if [ -n "$JETON" ]; then
  echo "$JETON" | grep -Eq '^mirador_enrol_[A-Za-z0-9_-]{20,200}$' || die "jeton d'enrôlement illisible"
fi

config_collecteur() {
  cat <<EOF
# /etc/mirador-collector/collector.conf — généré par mirador-collector-install.sh
# Format CLE=valeur. Une clé inconnue empêche le démarrage (sécurité).
# Le token est dans $VARLIB/token, jamais ici. Les réglages (équipements,
# syslog, SNMP…) viennent de l'interface de mirador.

MIRADOR_NOM=$NOM
MIRADOR_SERVEUR=$SERVEUR
MIRADOR_FICHIER_TOKEN=$VARLIB/token
MIRADOR_FICHIER_JETON=$VARLIB/jeton
MIRADOR_FICHIER_CLES=$ETC/cles-publiques.txt
MIRADOR_FICHIER_CONFIG_DISTANTE=$VARLIB/config-distante.json
MIRADOR_DOSSIER_BIN=$BIN_DIR
MIRADOR_MAJ_AUTO=$MAJ_AUTO
EOF
}

unite_collecteur() {
  cat <<'EOF'
# mirador-collector — collecteur du LAN (sans root)
[Unit]
Description=mirador-collector (équipements du LAN)
Documentation=https://github.com/prohand/mirador-stack
Wants=network-online.target
After=network-online.target
# Après 5 échecs en 5 minutes (nouvelle version qui ne démarre pas) :
# retour à la version précédente.
StartLimitIntervalSec=300
StartLimitBurst=5
OnFailure=mirador-collector-retour.service

[Service]
User=mirador-collector
Group=mirador-collector
# Ping sans droits (sondes, SPEC § 67) : réglage réappliqué à chaque démarrage
# (« + » : en root, hors du bac à sable ; « - » : sans fichier, rien). Un
# conteneur LXC n'applique pas /etc/sysctl.d au démarrage.
ExecStartPre=-+/usr/sbin/sysctl -q -p /etc/sysctl.d/60-mirador-collector-ping.conf
ExecStart=/var/lib/mirador-collector/bin/current -config /etc/mirador-collector/collector.conf -dossier-bin /var/lib/mirador-collector/bin
Restart=always
RestartSec=10
Environment=GOMEMLIMIT=35MiB

Nice=10
CPUWeight=20
MemoryMax=120M

# Durcissement. Le syslog (partie 4) écoute sur un port > 1024 : aucun droit
# particulier n'est nécessaire.
NoNewPrivileges=yes
CapabilityBoundingSet=
AmbientCapabilities=
ProtectSystem=strict
ProtectHome=yes
ReadWritePaths=/var/lib/mirador-collector
StateDirectory=mirador-collector
StateDirectoryMode=0750
PrivateTmp=yes
PrivateDevices=yes
ProtectKernelModules=yes
ProtectKernelTunables=yes
ProtectKernelLogs=yes
ProtectControlGroups=yes
ProtectClock=yes
ProtectHostname=yes
ProtectProc=invisible
# Repère du direct pour le compagnon des processus (fichier vide ; le
# compagnon n'en lit que la date).
RuntimeDirectory=mirador-collector
RuntimeDirectoryMode=0755
RestrictNamespaces=yes
RestrictRealtime=yes
RestrictSUIDSGID=yes
LockPersonality=yes
MemoryDenyWriteExecute=yes
RemoveIPC=yes
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
# Ports d'écoute (revue du 28/11, B15) : le collecteur n'écoute qu'en TCP
# (syslog TLS), sur le port réglé dans l'interface, de 1024 à 65535 (même
# borne que la vérification de la configuration). Toute autre écoute (UDP,
# port < 1024) est refusée par le noyau.
# Pas de « SocketBindDeny=any » : « any » refuserait aussi le bind explicite
# sur le port 0 (port choisi par le noyau) que fait la bibliothèque SNMP
# (0.0.0.0:0 en UDP, vérifié avec strace) : le SNMP serait cassé. Les ports
# 1 à 65535 en UDP sont refusés, le port 0 reste permis. Le ping (socket
# ICMP) n'est pas concerné. systemd 249 ou plus (sinon ligne ignorée).
SocketBindDeny=udp:1-65535
SocketBindDeny=tcp:1-1023
SystemCallArchitectures=native
SystemCallFilter=@system-service
SystemCallFilter=~@privileged
SystemCallErrorNumber=EPERM

[Install]
WantedBy=multi-user.target
EOF
}

unite_retour() {
  cat <<'EOF'
# mirador-collector-retour — remet la version précédente si la nouvelle ne
# démarre pas (OnFailure= de mirador-collector.service). Action locale :
# jamais commandée par le serveur.
[Unit]
Description=mirador-collector : retour à la version précédente
ConditionPathExists=/var/lib/mirador-collector/bin/essai.json

[Service]
Type=oneshot
User=mirador-collector
Group=mirador-collector
ExecStart=/var/lib/mirador-collector/bin/precedent -retour-arriere -dossier-bin /var/lib/mirador-collector/bin -config /etc/mirador-collector/collector.conf
ExecStartPost=+/usr/bin/systemctl reset-failed mirador-collector.service
ExecStartPost=+/usr/bin/systemctl start mirador-collector.service

# Durcissement : celui du collecteur (revue du 28/11, B15), sans aucun
# réseau (le retour arrière ne fait que changer le lien « current »). Les
# deux lignes « + » ci-dessus (systemctl, en root) n'y sont pas soumises.
NoNewPrivileges=yes
CapabilityBoundingSet=
AmbientCapabilities=
ProtectSystem=strict
ProtectHome=yes
ReadWritePaths=/var/lib/mirador-collector
PrivateTmp=yes
PrivateDevices=yes
ProtectKernelModules=yes
ProtectKernelTunables=yes
ProtectKernelLogs=yes
ProtectControlGroups=yes
ProtectClock=yes
ProtectHostname=yes
ProtectProc=invisible
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

unite_processus() {
  cat <<'EOF'
# mirador-collector-processus — compagnon du collecteur : relève les
# processus de la machine (nom, CPU, mémoire, compte ; jamais les lignes de
# commande) pour sa page « Machine » et la « cause probable » des alertes.
# Aucun réseau, aucun token, aucune configuration ; écrit seulement dans
# /run/mirador-processus (lisible par tous, comme « ps »).
[Unit]
Description=mirador-collector : relevé des processus (sans réseau)
Documentation=https://github.com/prohand/mirador-stack

[Service]
User=mirador-processus
Group=mirador-processus
ExecStart=/usr/local/libexec/mirador-collector-processus -processus
Restart=always
RestartSec=10
Environment=GOMEMLIMIT=12MiB
Nice=10
CPUWeight=10
MemoryMax=40M
RuntimeDirectory=mirador-processus
RuntimeDirectoryMode=0755
UMask=0022

# Aucun réseau : ni IPv4/IPv6 (familles refusées), ni adresse permise.
RestrictAddressFamilies=AF_UNIX
IPAddressDeny=any
# Rien du collecteur n'est visible (token, configuration).
InaccessiblePaths=-/var/lib/mirador-collector -/etc/mirador-collector
NoNewPrivileges=yes
CapabilityBoundingSet=
AmbientCapabilities=
ProtectSystem=strict
ProtectHome=yes
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
RemoveIPC=yes
SystemCallArchitectures=native
SystemCallFilter=@system-service
SystemCallFilter=~@privileged
SystemCallErrorNumber=EPERM

[Install]
WantedBy=multi-user.target
EOF
}

if [ "$MODE" = afficher ]; then
  echo "----- $ETC/collector.conf"; config_collecteur
  echo "----- mirador-collector.service"; unite_collecteur
  echo "----- mirador-collector-retour.service"; unite_retour
  echo "----- mirador-collector-processus.service"; unite_processus
  exit 0
fi

# ---------------------------------------------------------------- binaire
# Téléchargé en HTTPS depuis mirador-server avec le jeton (ou le token déjà
# en place), empreinte SHA-256 vérifiée. Le jeton passe par un fichier de
# configuration de curl / wget (dossier root 0700), jamais par la ligne de
# commande (lisible avec « ps »).
umask 077
TELE=$(mktemp -d)
umask 022
ESSAI=""
nettoyer() { # dossiers temporaires, effacés à la sortie du script
  rm -rf -- "$TELE"
  if [ -n "$ESSAI" ]; then rm -rf -- "$ESSAI"; fi
}
trap nettoyer EXIT
telecharger() { # URL FICHIER ENTETES
  if command -v curl >/dev/null 2>&1; then
    printf 'header = "Authorization: Bearer %s"\n' "$AUTH" > "$TELE/curl.conf"
    curl -fsS --proto =https --tlsv1.2 --retry 3 -K "$TELE/curl.conf" -D "$3" -o "$2" "$1"
  elif command -v wget >/dev/null 2>&1; then
    printf 'header = Authorization: Bearer %s\n' "$AUTH" > "$TELE/wgetrc"
    WGETRC="$TELE/wgetrc" wget -q --https-only -S -O "$2" "$1" 2>"$3"
  else
    die "curl ou wget est nécessaire (ou utiliser --binaire)"
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
cle_valide() { echo "$1" | grep -Eq '^[A-Za-z0-9+/]{43}=$' && [ "$(printf '%s' "$1" | base64 -d 2>/dev/null | wc -c)" -eq 32 ]; }
CLES_CONNUES=""
if [ -n "$CLE_PUBLIQUE" ]; then
  CLES_CONNUES=$(echo "$CLE_PUBLIQUE" | tr ', ' '\n\n' | sed '/^$/d')
  for k in $CLES_CONNUES; do cle_valide "$k" || die "clé publique invalide : $k"; done
elif [ -s "$FICHIER_CLES" ]; then
  CLES_CONNUES=$(grep -v '^#' "$FICHIER_CLES" | sed '/^$/d' || true)
fi
SIGNATURE_VERIFIEE=non; VERSION_SIGNEE=""; RETOUR_SIGNE=non; RETOUR_JUSQUA=0

AUTH="$JETON"
if [ -z "$AUTH" ] && [ -s "$VARLIB/token" ]; then
  AUTH=$(tr -d '[:space:]' < "$VARLIB/token")
fi
if [ -z "$BINAIRE" ]; then
  [ -n "$AUTH" ] || die "jeton d'enrôlement absent (MIRADOR_JETON=… ou --jeton), ou --binaire"
  case "$(uname -m)" in
    x86_64|amd64)  ARCH=amd64 ;;
    aarch64|arm64) ARCH=arm64 ;;
    *) die "architecture $(uname -m) non prise en charge (amd64 ou arm64)" ;;
  esac
  info "Téléchargement de mirador-collector ($ARCH) depuis $SERVEUR"
  telecharger "$SERVEUR/api/agent/v1/install/binary?arch=$ARCH&produit=mirador-collector" "$TELE/mirador-collector" "$TELE/entetes" \
    || die "téléchargement impossible : jeton expiré ou déjà utilisé, ou adresse de cette machine pas autorisée dans nginx"
  ATTENDU=$(valeur_entete "$TELE/entetes" x-mirador-sha256)
  OBTENU=$(sha256sum "$TELE/mirador-collector" | awk '{print $1}')
  [ -n "$ATTENDU" ] && [ "$ATTENDU" = "$OBTENU" ] || die "empreinte du binaire téléchargé fausse (attendu ${ATTENDU:-?}, obtenu $OBTENU)"
  # Signature : vérifiée AVANT tout lancement du binaire.
  if [ -n "$CLES_CONNUES" ] && [ "$VERIF_SIGNATURE" = oui ]; then
    openssl_recent || die "openssl 3.0 ou plus est nécessaire pour vérifier la signature du binaire (ici : $(openssl version 2>/dev/null || echo 'openssl absent')). Installer openssl 3 (Debian 12, Ubuntu 22.04 ou plus récents), ou, en connaissance de cause, --sans-verification-signature (déconseillé)"
    verifier_manifeste "$TELE/entetes" "$TELE/mirador-collector" mirador-collector "$ARCH" "$CLES_CONNUES"
    SIGNATURE_VERIFIEE=oui
    info "Signature du binaire vérifiée (version $VERSION_SIGNEE)"
  elif [ -n "$CLES_CONNUES" ]; then
    echo "ATTENTION : signature du binaire NON vérifiée (--sans-verification-signature)."
  fi
  BINAIRE="$TELE/mirador-collector"
else
  [ -f "$BINAIRE" ] || die "binaire introuvable : $BINAIRE"
  cp -- "$BINAIRE" "$TELE/mirador-collector"
  BINAIRE="$TELE/mirador-collector"
  if [ -n "$CLES_CONNUES" ] && [ "$VERIF_SIGNATURE" = oui ]; then
    echo "    (binaire local donné par --binaire : signature non vérifiée, c'est votre fichier)"
  fi
fi
chmod 0755 "$BINAIRE"

# ---------------------------------------------------------------- utilisateur et dossiers
info "Utilisateur système $UTILISATEUR"
if ! id "$UTILISATEUR" >/dev/null 2>&1; then
  useradd --system --no-create-home --home-dir "$VARLIB" --shell /usr/sbin/nologin "$UTILISATEUR"
fi
install -d -o root -g "$UTILISATEUR" -m 0750 "$ETC"
if ! id "$UTILISATEUR_PROC" >/dev/null 2>&1; then
  useradd --system --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin "$UTILISATEUR_PROC"
fi

# ---------------------------------------------------------------- version du binaire
# Lancé SOUS LE COMPTE du collecteur, jamais en root (revue du 28/11), et
# seulement après la vérification de signature. Copie à root dans un dossier
# lisible par lui (celui du téléchargement est réservé à root).
ESSAI=$(mktemp -d)
chmod 0755 "$ESSAI"
install -o root -g root -m 0755 "$BINAIRE" "$ESSAI/mirador-collector"
VERSION=$(runuser -u "$UTILISATEUR" -- timeout 30 "$ESSAI/mirador-collector" -version 2>/dev/null | awk '$1=="mirador-collector"{print $2}') || true
[ -n "$VERSION" ] || die "ce n'est pas mirador-collector, ou il ne se lance pas (cette machine : $(uname -m))"
echo "$VERSION" | grep -Eq '^[A-Za-z0-9._-]{1,40}$' || die "version illisible : $VERSION"
if [ "$SIGNATURE_VERIFIEE" = oui ]; then
  [ "$VERSION" = "$VERSION_SIGNEE" ] || die "le binaire annonce la version $VERSION, son manifeste signé la version $VERSION_SIGNEE : refusé"
  # Pas de retour vers une version plus ancienne que celle en place, sauf
  # retour arrière signé encore valable, ou --accepter-version-ancienne.
  EN_PLACE=$(readlink "$BIN_DIR/current" 2>/dev/null | sed -n 's/^mirador-collector-//p' || true)
  if [ "$VERSION_ANCIENNE" = non ] && version_plus_ancienne "$VERSION" "$EN_PLACE"; then
    if [ "$RETOUR_SIGNE" = oui ] && [ "$RETOUR_JUSQUA" -gt "$(date +%s)" ]; then
      info "Retour à la version $VERSION (retour arrière signé)"
    else
      die "le serveur envoie la version $VERSION, plus ancienne que celle en place ($EN_PLACE) : refusé. Si c'est voulu : --accepter-version-ancienne"
    fi
  fi
fi

# ---------------------------------------------------------------- ping sans droits
# Sondes ping (SPEC § 48, § 67) : le collecteur, sans droits, ouvre un socket
# ICMP « datagramme ». Le noyau ne l'accepte que pour les groupes de
# net.ipv4.ping_group_range ; souvent vide (« 1 0 ») dans un conteneur LXC.
# La plage est élargie au groupe du collecteur, jamais réduite ; gardée dans
# /etc/sysctl.d et réappliquée au démarrage du collecteur (unité).
FICHIER_PING=/etc/sysctl.d/60-mirador-collector-ping.conf
GID_COLL=$(id -g "$UTILISATEUR")
PLAGE=$(sysctl -n net.ipv4.ping_group_range 2>/dev/null | tr -s ' \t' ' ' || true)
BAS=${PLAGE%% *}; HAUT=${PLAGE##* }
if [ -n "$PLAGE" ] && [ "$BAS" -le "$GID_COLL" ] && [ "$GID_COLL" -le "$HAUT" ]; then
  info "Ping sans droits déjà permis au groupe $UTILISATEUR ($PLAGE)"
elif [ -n "$PLAGE" ]; then
  if [ "$BAS" -gt "$HAUT" ]; then
    BAS=$GID_COLL; HAUT=$GID_COLL # plage vide : le seul groupe du collecteur
  else
    if [ "$GID_COLL" -lt "$BAS" ]; then BAS=$GID_COLL; fi
    if [ "$GID_COLL" -gt "$HAUT" ]; then HAUT=$GID_COLL; fi
  fi
  { echo "# mirador-collector : ping sans droits pour le groupe $UTILISATEUR (sondes ping)"
    echo "net.ipv4.ping_group_range = $BAS $HAUT"; } > "$FICHIER_PING"
  chmod 0644 "$FICHIER_PING"
  if sysctl -q -p "$FICHIER_PING"; then
    info "Ping sans droits permis au groupe $UTILISATEUR (net.ipv4.ping_group_range = $BAS $HAUT)"
  else
    echo "ATTENTION : réglage net.ipv4.ping_group_range refusé : les sondes ping de ce collecteur échoueront."
  fi
fi
install -d -o "$UTILISATEUR" -g "$UTILISATEUR" -m 0750 "$VARLIB"
install -d -o "$UTILISATEUR" -g "$UTILISATEUR" -m 0750 "$BIN_DIR"

# Écritures dans les dossiers du collecteur : SOUS SON IDENTITÉ (runuser), un
# lien posé par un collecteur compromis ne peut toucher que ses fichiers.
ecrire_collecteur() { # FICHIER MODE < contenu
  runuser -u "$UTILISATEUR" -- sh -c 'umask 077; rm -f -- "$1.tmp" && cat > "$1.tmp" && chmod "$2" "$1.tmp" && mv -f -- "$1.tmp" "$1"' sh "$1" "$2"
}

info "Binaire $VERSION"
ecrire_collecteur "$BIN_DIR/mirador-collector-$VERSION" 0755 < "$BINAIRE" || die "copie du binaire impossible"
runuser -u "$UTILISATEUR" -- sh -c 'ln -sfn "$1" "$2/current.nouveau" && mv -Tf "$2/current.nouveau" "$2/current"' \
  sh "mirador-collector-$VERSION" "$BIN_DIR" || die "lien current impossible"

# Compagnon : copie à root, jamais dans les dossiers du collecteur.
install -d -o root -g root -m 0755 "$(dirname "$BIN_PROC")"
install -o root -g root -m 0755 "$BINAIRE" "$BIN_PROC.nouveau" && mv -f "$BIN_PROC.nouveau" "$BIN_PROC" \
  || die "copie du compagnon des processus impossible"

if [ -n "$JETON" ]; then
  if [ -s "$VARLIB/token" ]; then
    info "Token déjà présent : jeton ignoré (collecteur déjà enrôlé)"
  else
    printf '%s\n' "$JETON" | ecrire_collecteur "$VARLIB/jeton" 0600 || die "écriture du jeton impossible"
  fi
fi

# ---------------------------------------------------------------- clés de signature
# Clés PUBLIQUES qui vérifient les mises à jour : fichier root 0644, jamais
# modifiable par le collecteur ni par le serveur ; posées une fois.
# (FICHIER_CLES et cle_valide : définis plus haut, section « binaire ».)
CLES=""; ORIGINE=""
if [ -n "$CLE_PUBLIQUE" ]; then
  CLES=$(echo "$CLE_PUBLIQUE" | tr ', ' '\n\n' | sed '/^$/d'); ORIGINE="option --cle-publique"
elif [ ! -s "$FICHIER_CLES" ]; then
  if command -v curl >/dev/null 2>&1; then
    CLES=$(curl -fsS --proto =https --tlsv1.2 --retry 3 "$SERVEUR/api/agent/v1/cles" 2>/dev/null | head -c 1024 | tr -d '\r' | sed '/^$/d' || true)
  else
    CLES=$(wget -q --https-only -O - "$SERVEUR/api/agent/v1/cles" 2>/dev/null | head -c 1024 | tr -d '\r' | sed '/^$/d' || true)
  fi
  ORIGINE="serveur $SERVEUR"
fi
if [ -n "$ORIGINE" ]; then
  if [ -z "$CLES" ]; then
    echo "ATTENTION : aucune clé publique de signature : le collecteur refusera toute mise à jour."
  else
    [ "$(echo "$CLES" | wc -l)" -le 2 ] || die "deux clés publiques au plus"
    for k in $CLES; do cle_valide "$k" || die "clé publique invalide : $k"; done
    { echo "# Clés PUBLIQUES de signature des versions de mirador ($ORIGINE)"; echo "$CLES"; } > "$FICHIER_CLES.tmp"
    chown root:root "$FICHIER_CLES.tmp"; chmod 0644 "$FICHIER_CLES.tmp"
    mv -f "$FICHIER_CLES.tmp" "$FICHIER_CLES"
    info "Clé de signature posée ($ORIGINE)"
  fi
fi

# ---------------------------------------------------------------- configuration
umask 027
config_collecteur > "$ETC/collector.conf.tmp"
chown root:"$UTILISATEUR" "$ETC/collector.conf.tmp"; chmod 0640 "$ETC/collector.conf.tmp"
mv -f "$ETC/collector.conf.tmp" "$ETC/collector.conf"
umask 022
info "Vérification de la configuration (en tant que $UTILISATEUR)"
runuser -u "$UTILISATEUR" -- "$BIN_DIR/current" -verifier -config "$ETC/collector.conf" \
  || die "configuration refusée (voir le message ci-dessus)"

# ---------------------------------------------------------------- unités systemd
info "Unités systemd"
unite_collecteur > "$UNITES/mirador-collector.service"
unite_retour     > "$UNITES/mirador-collector-retour.service"
unite_processus  > "$UNITES/mirador-collector-processus.service"
chmod 0644 "$UNITES"/mirador-collector*.service
systemctl daemon-reload
systemctl enable mirador-collector.service mirador-collector-processus.service >/dev/null
systemctl reset-failed mirador-collector.service mirador-collector-processus.service 2>/dev/null || true
systemctl restart mirador-collector.service mirador-collector-processus.service
sleep 5
systemctl is-active --quiet mirador-collector.service \
  || die "mirador-collector ne tourne pas : journalctl -u mirador-collector -n 40 --no-pager"
# Le compagnon n'est pas indispensable : sans lui, la page « Machine » ne
# montre que le collecteur dans la liste des processus.
systemctl is-active --quiet mirador-collector-processus.service \
  || echo "ATTENTION : compagnon des processus arrêté : journalctl -u mirador-collector-processus -n 40 --no-pager"

echo
echo "================================================================"
echo " mirador-collector $VERSION installé ($NOM)"
echo "================================================================"
if [ -s "$VARLIB/jeton" ]; then
  echo " Il attend son approbation : Machines › en attente, dans l'interface."
fi
echo " Mises à jour automatiques : $MAJ_AUTO"
echo " Suivi : journalctl -u mirador-collector -f"
echo " Processus (compagnon sans réseau) : systemctl status mirador-collector-processus"
echo
