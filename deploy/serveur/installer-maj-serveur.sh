#!/bin/sh
# SPDX-License-Identifier: GPL-3.0-or-later
#
# Installe mirador-maj-serveur sur le VPS (SPEC § 102) : mise à jour du
# serveur depuis l'interface. À lancer UNE fois, en root, dans le dossier du
# docker-compose.yml du serveur :
#
#   sudo ./installer-maj-serveur.sh --version 0.6.0 --cle-publique CLE [--dossier /opt/mirador]
#
#   --version         version du serveur EN SERVICE (celle de l'image actuelle)
#   --cle-publique    clé publique de signature (la même que pour les agents)
#   --dossier         dossier du compose (défaut : dossier courant)
#   --depot           dépôt GitHub (défaut : prohand/mirador-stack)
#   --sans-signature  DÉCONSEILLÉ : installe même si le binaire n'est pas
#                     signé (versions publiées avant la signature de
#                     mirador-maj-serveur) ; seule l'empreinte SHA256SUMS,
#                     prise dans la même Release, est alors contrôlée
#
# Ce que fait le script (rien d'autre) :
#   1. télécharge mirador-maj-serveur de la Release GitHub de --version,
#      vérifie son empreinte (SHA256SUMS) puis la SIGNATURE de son manifeste
#      avec --cle-publique (OpenSSL 3 ou plus récent) : rien n'est installé
#      si la signature manque ou est fausse ;
#   2. l'installe dans /usr/local/sbin ;
#   3. crée les dossiers d'échange : DOSSIER/maj (propriétaire 65532, le
#      serveur y écrit) et DOSSIER/maj-etat (root, lu par le serveur) ;
#   4. écrit /etc/mirador-maj-serveur.conf s'il n'existe pas ;
#   5. ajoute MIRADOR_VERSION et MIRADOR_IMAGE au .env s'ils manquent ;
#   6. installe et active les unités systemd (.path et .service), écrites
#      par ce script lui-même (plus rien n'est téléchargé pour elles) ;
#   7. lance « mirador-maj-serveur verifier ».
# Le docker-compose.yml n'est pas modifié : le script dit quoi y changer.
set -eu

# --- début verifier_signature (aussi lancée par les tests Go : internal/release)
# verifier_signature BINAIRE MANIFESTE CLE_PUBLIQUE PRODUIT VERSION ARCH
# Vérifie la signature Ed25519 d'un manifeste produit par mirador-release.
# Seule la signature est lue dans le manifeste : le message signé est
# reconstruit ici à partir du binaire téléchargé et des valeurs attendues
# (même format que protocole.Manifeste.MessageSigne), rien d'autre du
# manifeste n'est donc cru. Renvoie 0 seulement si la signature est bonne.
verifier_signature() {
  v_bin=$1 v_man=$2 v_cle=$3 v_produit=$4 v_version=$5 v_arch=$6
  if ! openssl version 2>/dev/null | grep -Eq '^OpenSSL ([3-9]|[1-9][0-9])\.'; then
    echo "OpenSSL 3 (ou plus récent) requis pour vérifier la signature" >&2
    return 1
  fi
  # Une seule ligne « signature », en base64 de 64 octets (88 caractères).
  [ "$(grep -c '"signature"' "$v_man")" = 1 ] || { echo "manifeste invalide" >&2; return 1; }
  v_sig=$(sed -n 's/^[[:space:]]*"signature":[[:space:]]*"\([A-Za-z0-9+/]\{86\}==\)",\{0,1\}[[:space:]]*$/\1/p' "$v_man")
  [ -n "$v_sig" ] || { echo "signature absente ou mal formée dans le manifeste" >&2; return 1; }
  v_dir=$(mktemp -d)
  # Message signé : produit, version, architecture, SHA-256 (hexadécimal),
  # taille (octets), « non » (pas de retour arrière) ; une ligne chacun.
  v_sha=$(sha256sum "$v_bin" | cut -d ' ' -f 1)
  v_taille=$(wc -c < "$v_bin" | tr -d ' ')
  printf '%s\n%s\n%s\n%s\n%s\nnon\n' "$v_produit" "$v_version" "$v_arch" "$v_sha" "$v_taille" > "$v_dir/message"
  # Clé publique Ed25519 brute (32 octets, base64) → format PEM d'OpenSSL :
  # « MCowBQYDK2VwAyEA » est l'en-tête DER fixe d'une clé publique Ed25519.
  printf '%s\n%s\n%s\n' '-----BEGIN PUBLIC KEY-----' "MCowBQYDK2VwAyEA$v_cle" '-----END PUBLIC KEY-----' > "$v_dir/cle.pem"
  printf '%s' "$v_sig" | openssl base64 -d -A > "$v_dir/signature"
  if openssl pkeyutl -verify -pubin -inkey "$v_dir/cle.pem" -rawin \
       -in "$v_dir/message" -sigfile "$v_dir/signature" >/dev/null 2>&1; then
    rm -rf -- "$v_dir"
    return 0
  fi
  rm -rf -- "$v_dir"
  echo "SIGNATURE INVALIDE : ce binaire n'a pas été signé par cette clé" >&2
  return 1
}
# --- fin verifier_signature

VERSION="" CLE="" DOSSIER="$(pwd)" DEPOT="prohand/mirador-stack" SANS_SIGNATURE=non
while [ $# -gt 0 ]; do
  case "$1" in
    --version) VERSION="${2:-}"; shift 2 ;;
    --cle-publique) CLE="${2:-}"; shift 2 ;;
    --dossier) DOSSIER="${2:-}"; shift 2 ;;
    --depot) DEPOT="${2:-}"; shift 2 ;;
    --sans-signature) SANS_SIGNATURE=oui; shift ;;
    *) echo "option inconnue : $1" >&2; exit 2 ;;
  esac
done
[ "$(id -u)" = 0 ] || { echo "à lancer en root (sudo)" >&2; exit 1; }
printf '%s' "$VERSION" | grep -Eq '^[0-9]{1,5}\.[0-9]{1,5}\.[0-9]{1,5}$' || { echo "--version X.Y.Z attendu (version du serveur en service)" >&2; exit 2; }
printf '%s' "$CLE" | grep -Eq '^[A-Za-z0-9+/]{43}=$' || { echo "--cle-publique : clé publique Ed25519 en base64 attendue (Mises à jour des agents › Clé publique)" >&2; exit 2; }
printf '%s' "$DEPOT" | grep -Eq '^[A-Za-z0-9][A-Za-z0-9-]{0,38}/[A-Za-z0-9._-]{1,100}$' || { echo "--depot invalide" >&2; exit 2; }
# Chemin absolu, caractères simples seulement : il est écrit dans les unités
# systemd (un espace ou un retour à la ligne y changerait le sens).
printf '%s' "$DOSSIER" | grep -Eq '^/[A-Za-z0-9/._-]*$' || { echo "--dossier : chemin absolu attendu (lettres, chiffres, / . _ - seulement)" >&2; exit 2; }
[ -f "$DOSSIER/docker-compose.yml" ] || [ -f "$DOSSIER/compose.yml" ] || { echo "pas de docker-compose.yml dans $DOSSIER" >&2; exit 1; }

case "$(uname -m)" in
  x86_64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) echo "architecture non prise en charge : $(uname -m)" >&2; exit 1 ;;
esac

# 1-2. Binaire : empreinte (SHA256SUMS) puis signature (manifeste).
TMP=$(mktemp -d)
trap 'rm -rf -- "$TMP"' EXIT
BASE="https://github.com/$DEPOT/releases/download/v$VERSION"
NOM="mirador-maj-serveur-$VERSION-linux-$ARCH"
echo "Téléchargement de $NOM…"
curl -fsSL --proto '=https' --tlsv1.2 -o "$TMP/$NOM" "$BASE/$NOM"
curl -fsSL --proto '=https' --tlsv1.2 -o "$TMP/SHA256SUMS" "$BASE/SHA256SUMS"
( cd "$TMP" && grep " $NOM\$" SHA256SUMS | sha256sum -c - ) || { echo "empreinte incorrecte : rien n'est installé" >&2; exit 1; }
if [ "$SANS_SIGNATURE" = oui ]; then
  echo "ATTENTION : --sans-signature : signature NON vérifiée (déconseillé)." >&2
else
  if ! curl -fsSL --proto '=https' --tlsv1.2 -o "$TMP/$NOM.manifeste.json" "$BASE/$NOM.manifeste.json"; then
    echo "Pas de manifeste signé pour $NOM : rien n'est installé." >&2
    echo "(Version publiée avant la signature de mirador-maj-serveur : mettre d'abord le serveur à jour à la main, ou --sans-signature, déconseillé.)" >&2
    exit 1
  fi
  verifier_signature "$TMP/$NOM" "$TMP/$NOM.manifeste.json" "$CLE" mirador-maj-serveur "$VERSION" "$ARCH" \
    || { echo "rien n'est installé" >&2; exit 1; }
  echo "Signature vérifiée."
fi
install -o root -g root -m 0755 "$TMP/$NOM" /usr/local/sbin/mirador-maj-serveur

# 3. Dossiers d'échange.
install -d -o 65532 -g 65532 -m 0755 "$DOSSIER/maj"
install -d -o root -g root -m 0755 "$DOSSIER/maj-etat"
install -d -o root -g root -m 0700 /var/lib/mirador-maj-serveur

# 4. Réglages (gardés s'ils existent).
if [ ! -f /etc/mirador-maj-serveur.conf ]; then
  umask 022
  cat > /etc/mirador-maj-serveur.conf <<CONF
# Mise à jour du serveur depuis l'interface (SPEC § 102).
DOSSIER_COMPOSE=$DOSSIER
CLES_PUBLIQUES=$CLE
DEPOT=$DEPOT
IMAGE=ghcr.io/${DEPOT%%/*}/mirador-server
CONF
  echo "Réglages écrits : /etc/mirador-maj-serveur.conf"
else
  echo "Réglages gardés : /etc/mirador-maj-serveur.conf"
fi

# 5. .env du compose : version en service et image.
ENV="$DOSSIER/.env"
[ -f "$ENV" ] || install -o root -g root -m 0600 /dev/null "$ENV"
grep -q '^MIRADOR_VERSION=' "$ENV" || echo "MIRADOR_VERSION=$VERSION" >> "$ENV"
grep -q '^MIRADOR_IMAGE=' "$ENV" || echo "MIRADOR_IMAGE=ghcr.io/${DEPOT%%/*}/mirador-server:$VERSION" >> "$ENV"
chmod 0600 "$ENV"

# 6. Unités systemd, écrites ici (revue du 28/11 : avant, téléchargées depuis
# le dépôt par tag, qui peut être déplacé). Dossier du compose inscrit dedans.
umask 022
cat > /etc/systemd/system/mirador-maj-serveur.path <<UNITE
# Mise à jour du serveur depuis l'interface (SPEC § 102) : quand le serveur
# dépose une demande, mirador-maj-serveur la traite (puis l'efface).
# Écrit par installer-maj-serveur.sh.
[Unit]
Description=mirador : demande de mise à jour du serveur

[Path]
PathExists=$DOSSIER/maj/demande.json
Unit=mirador-maj-serveur.service

[Install]
WantedBy=multi-user.target
UNITE
cat > /etc/systemd/system/mirador-maj-serveur.service <<UNITE
# Mise à jour du serveur depuis l'interface (SPEC § 102). Lancé par
# mirador-maj-serveur.path ; root car il pilote Docker (docker pull, compose,
# cp), mais le reste du système est en lecture seule pour lui.
# Écrit par installer-maj-serveur.sh.
[Unit]
Description=mirador : mise à jour du serveur (demandée dans l'interface)
After=docker.service
Requires=docker.service

[Service]
Type=oneshot
ExecStart=/usr/local/sbin/mirador-maj-serveur appliquer -config /etc/mirador-maj-serveur.conf
TimeoutStartSec=30min
# Pas de configuration Docker dans /root (caché ci-dessous).
Environment=DOCKER_CONFIG=/var/lib/mirador-maj-serveur/docker
NoNewPrivileges=yes
PrivateTmp=yes
ProtectHome=yes
ProtectSystem=strict
ReadWritePaths=$DOSSIER /var/lib/mirador-maj-serveur
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
RestrictSUIDSGID=yes
LockPersonality=yes
UNITE
chmod 0644 /etc/systemd/system/mirador-maj-serveur.path /etc/systemd/system/mirador-maj-serveur.service
systemctl daemon-reload
systemctl enable --now mirador-maj-serveur.path

# 7. Contrôle.
echo
/usr/local/sbin/mirador-maj-serveur verifier -config /etc/mirador-maj-serveur.conf || {
  echo
  echo "À faire dans $DOSSIER/docker-compose.yml (service mirador-server), puis « docker compose up -d » :"
  echo "  image: \${MIRADOR_IMAGE}"
  echo "  volumes:"
  echo "    - ./maj:/var/lib/mirador-maj"
  echo "    - ./maj-etat:/var/lib/mirador-maj-etat:ro"
  echo "Puis relancer : sudo mirador-maj-serveur verifier -config /etc/mirador-maj-serveur.conf"
}
