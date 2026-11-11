#!/usr/bin/env bash
# SPDX-License-Identifier: GPL-3.0-or-later
#
# Vérifie deux choses (SPEC.md § 14) :
#  1. chaque fichier Go porte l'en-tête SPDX GPL-3.0-or-later ;
#  2. chaque dépendance Go COMPILÉE dans un binaire (./cmd/...) figure dans
#     THIRD-PARTY.md (licence relue et
#     compatible GPL-3.0). Une dépendance absente fait échouer la vérification.
set -euo pipefail
cd "$(dirname "$0")/.."

erreur=0
# Exception : le client Olvid généré depuis Olvid-Bot-Protobuf (AGPL-3.0,
# accord de l'utilisateur du 10/11, SPEC § 17.11) porte l'en-tête AGPL.
OLVID_PB=./internal/server/notif/olvid/pb/
while IFS= read -r f; do
  attendu='^// SPDX-License-Identifier: GPL-3.0-or-later$'
  case "$f" in "$OLVID_PB"*) attendu='^// SPDX-License-Identifier: AGPL-3.0-only$' ;; esac
  if ! head -1 "$f" | grep -q "$attendu"; then
    echo "en-tête SPDX absent : $f"
    erreur=1
  fi
done < <(find . -name '*.go' -not -path './dist/*')

while IFS= read -r mod; do
  [ -n "$mod" ] || continue
  if ! grep -q -- "\`$mod\`" THIRD-PARTY.md; then
    echo "dépendance non déclarée dans THIRD-PARTY.md : $mod"
    erreur=1
  fi
done < <(go list -deps -f '{{with .Module}}{{if not .Main}}{{.Path}}{{end}}{{end}}' ./cmd/... | sort -u)

if [ "$erreur" -ne 0 ]; then
  exit 1
fi
echo "licences : OK"
