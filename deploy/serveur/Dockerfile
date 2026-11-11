# SPDX-License-Identifier: GPL-3.0-or-later
#
# Image de mirador-server (SPEC.md § 16.2) : binaire statique seul, sans shell
# ni outil, utilisateur non root, système de fichiers en lecture seule (voir
# docker-compose.yml). Construite par la CI à partir du dépôt.

# --- construction -------------------------------------------------------------
FROM golang:1.27.4-bookworm AS construction
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG COMMIT=
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -buildvcs=false \
      -ldflags "-s -w -buildid= -X mirador-stack/internal/version.Version=${VERSION} -X mirador-stack/internal/version.Commit=${COMMIT}" \
      -o /mirador-server ./cmd/mirador-server
# Binaires de l'agent embarqués (même version) : une machine les télécharge
# en HTTPS à l'installation, avec son jeton d'enrôlement (SPEC § 18.19).
RUN for arch in amd64 arm64; do \
      CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath -buildvcs=false \
        -ldflags "-s -w -buildid= -X mirador-stack/internal/version.Version=${VERSION} -X mirador-stack/internal/version.Commit=${COMMIT}" \
        -o /out/usr/share/mirador/agent/mirador-agent-linux-$arch ./cmd/mirador-agent || exit 1; \
    done
# Dossier des données préparé ici : l'image finale n'a pas de shell pour le créer.
RUN mkdir -p /out/var/lib/mirador

# --- image finale ---------------------------------------------------------------
FROM scratch
COPY --from=construction /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=construction /mirador-server /mirador-server
COPY --from=construction /out/usr/share/mirador/agent /usr/share/mirador/agent
# Licence GPL-3.0 et composants tiers (l'image est redistribuée).
COPY --from=construction /src/LICENSE /src/THIRD-PARTY.md /licences/
# Le volume nommé reprend ce propriétaire à sa création : le serveur (non
# root) peut y écrire, sans droits sur le reste du système.
COPY --from=construction --chown=65532:65532 /out/var/lib/mirador /var/lib/mirador
ARG VERSION=dev
LABEL org.opencontainers.image.title="mirador-server" \
      org.opencontainers.image.source="https://github.com/prohand/mirador-stack" \
      org.opencontainers.image.licenses="GPL-3.0-or-later" \
      org.opencontainers.image.version="${VERSION}"
# Utilisateur non root, sans compte (uid/gid 65532).
USER 65532:65532
VOLUME ["/var/lib/mirador"]
EXPOSE 9440 9441
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
  CMD ["/mirador-server", "-sante", "-config", "/etc/mirador/server.conf"]
ENTRYPOINT ["/mirador-server", "-config", "/etc/mirador/server.conf"]
