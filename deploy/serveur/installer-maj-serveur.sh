#!/bin/sh
# SPDX-License-Identifier: GPL-3.0-or-later
#
# Installe mirador-maj-serveur sur le VPS (SPEC § 102) : mise à jour du
# serveur depuis l'interface. À lancer UNE fois, en root, dans le dossier du
# docker-compose.yml du serveur :
#
#   sudo ./installer-maj-serveur.sh --version 0.6.0 --cle-publique CLE [--dossier /opt/mirador]
#
#   --version       version du serveur EN SERVICE (celle de l'image actuelle)
#   --cle-publique  clé publique de signature (la même que pour les agents)
#   --dossier       dossier du compose (défaut : dossier courant)
#   --depot         dépôt GitHub (défaut : prohand/mirador-stack)
#
# Ce que fait le script (rien d'autre) :
#   1. télécharge mirador-maj-serveur de la Release GitHub de --version et
#      vérifie son empreinte (SHA256SUMS de la même Release) ;
#   2. l'installe dans /usr/local/sbin ;
#   3. crée les dossiers d'échange : DOSSIER/maj (propriétaire 65532, le
#      serveur y écrit) et DOSSIER/maj-etat (root, lu par le serveur) ;
#   4. écrit /etc/mirador-maj-serveur.conf s'il n'existe pas ;
#   5. ajoute MIRADOR_VERSION et MIRADOR_IMAGE au .env s'ils manquent ;
#   6. installe et active les unités systemd (.path et .service) ;
#   7. lance « mirador-maj-serveur verifier ».
# Le docker-compose.yml n'est pas modifié : le script dit quoi y changer.
set -eu

VERSION="" CLE="" DOSSIER="$(pwd)" DEPOT="prohand/mirador-stack"
while [ $# -gt 0 ]; do
  case "$1" in
    --version) VERSION="${2:-}"; shift 2 ;;
    --cle-publique) CLE="${2:-}"; shift 2 ;;
    --dossier) DOSSIER="${2:-}"; shift 2 ;;
    --depot) DEPOT="${2:-}"; shift 2 ;;
    *) echo "option inconnue : $1" >&2; exit 2 ;;
  esac
done
[ "$(id -u)" = 0 ] || { echo "à lancer en root (sudo)" >&2; exit 1; }
printf '%s' "$VERSION" | grep -Eq '^[0-9]{1,5}\.[0-9]{1,5}\.[0-9]{1,5}$' || { echo "--version X.Y.Z attendu (version du serveur en service)" >&2; exit 2; }
printf '%s' "$CLE" | grep -Eq '^[A-Za-z0-9+/]{43}=$' || { echo "--cle-publique : clé publique Ed25519 en base64 attendue (Mises à jour des agents › Clé publique)" >&2; exit 2; }
printf '%s' "$DEPOT" | grep -Eq '^[A-Za-z0-9][A-Za-z0-9-]{0,38}/[A-Za-z0-9._-]{1,100}$' || { echo "--depot invalide" >&2; exit 2; }
case "$DOSSIER" in /*) ;; *) echo "--dossier : chemin absolu attendu" >&2; exit 2 ;; esac
[ -f "$DOSSIER/docker-compose.yml" ] || [ -f "$DOSSIER/compose.yml" ] || { echo "pas de docker-compose.yml dans $DOSSIER" >&2; exit 1; }

case "$(uname -m)" in
  x86_64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) echo "architecture non prise en charge : $(uname -m)" >&2; exit 1 ;;
esac

# 1-2. Binaire, vérifié par SHA256SUMS (même Release, HTTPS).
TMP=$(mktemp -d)
trap 'rm -rf -- "$TMP"' EXIT
BASE="https://github.com/$DEPOT/releases/download/v$VERSION"
NOM="mirador-maj-serveur-$VERSION-linux-$ARCH"
echo "Téléchargement de $NOM…"
curl -fsSL --proto '=https' --tlsv1.2 -o "$TMP/$NOM" "$BASE/$NOM"
curl -fsSL --proto '=https' --tlsv1.2 -o "$TMP/SHA256SUMS" "$BASE/SHA256SUMS"
( cd "$TMP" && grep " $NOM\$" SHA256SUMS | sha256sum -c - ) || { echo "empreinte incorrecte : rien n'est installé" >&2; exit 1; }
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

# 6. Unités systemd (dossier du compose inscrit dedans).
for u in mirador-maj-serveur.path mirador-maj-serveur.service; do
  curl -fsSL --proto '=https' --tlsv1.2 -o "$TMP/$u" "https://raw.githubusercontent.com/$DEPOT/v$VERSION/deploy/serveur/systemd/$u"
  sed "s#DOSSIER#$DOSSIER#g" "$TMP/$u" > "/etc/systemd/system/$u"
  chmod 0644 "/etc/systemd/system/$u"
done
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
