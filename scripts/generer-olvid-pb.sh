#!/usr/bin/env bash
# SPDX-License-Identifier: GPL-3.0-or-later
#
# Régénère le client gRPC du daemon Olvid depuis Olvid-Bot-Protobuf
# (AGPL-3.0, SPEC.md § 17.11). Demande git et Go. Les outils (buf,
# protoc-gen-go, protoc-gen-go-grpc) sont installés dans un dossier
# temporaire, effacé à la fin.
set -euo pipefail
cd "$(dirname "$0")/.."

VERSION="${1:-v2.0.1}"
CIBLE=internal/server/notif/olvid/pb
MODULE=mirador-stack/$CIBLE

TMP="$(mktemp -d)"
trap 'rm -rf -- "$TMP"' EXIT

GOBIN="$TMP/outils" go install github.com/bufbuild/buf/cmd/buf@v1.50.0
GOBIN="$TMP/outils" go install "google.golang.org/protobuf/cmd/protoc-gen-go@$(go list -m -f '{{.Version}}' google.golang.org/protobuf)"
GOBIN="$TMP/outils" go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.5.1

git clone -q --depth 1 --branch "$VERSION" https://github.com/olvid-io/Olvid-Bot-Protobuf "$TMP/src"
printf 'version: v2\n' > "$TMP/src/buf.yaml"
cat > "$TMP/src/buf.gen.yaml" <<EOF
version: v2
managed:
  enabled: true
  override:
    - file_option: go_package_prefix
      value: $MODULE
plugins:
  - local: $TMP/outils/protoc-gen-go
    out: sortie
    opt: module=$MODULE
  - local: $TMP/outils/protoc-gen-go-grpc
    out: sortie
    opt: module=$MODULE
EOF
(cd "$TMP/src" && "$TMP/outils/buf" generate)

# Remplacement du code généré (chemin fixe), licence AGPL recopiée.
rm -rf -- "$CIBLE/olvid"
cp -r "$TMP/src/sortie/olvid" "$CIBLE/"
cp "$TMP/src/LICENSE" "$CIBLE/LICENSE"
find "$CIBLE" -name '*.go' | while read -r f; do
  {
    echo '// SPDX-License-Identifier: AGPL-3.0-only'
    echo "// Généré depuis olvid-io/Olvid-Bot-Protobuf $VERSION (AGPL-3.0) : voir pb/LISEZMOI.md."
    echo
    cat "$f"
  } > "$f.tmp"
  mv "$f.tmp" "$f"
done
echo "client Olvid régénéré ($VERSION)"
