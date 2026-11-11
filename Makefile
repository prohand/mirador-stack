# Makefile de mirador-stack
#
#   make test      tests unitaires (avec détection des accès concurrents)
#   make bench     benchmarks (mémoire et allocations)
#   make verif     go vet + gofmt + en-têtes et licences des dépendances
#   make agent     agent : binaires statiques linux/amd64 et linux/arm64 dans dist/
#   make serveur   serveur : binaires statiques linux/amd64 et linux/arm64 dans dist/
#   make release   mirador-release : windows/amd64 (poste de l'utilisateur) et linux/amd64
#   make propre    supprime le dossier dist/

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null)
# Binaires statiques et reproductibles : pas de cgo, chemins retirés, pas d'ID de build.
GOFLAGS_BUILD = -trimpath -buildvcs=false
# Les clés PUBLIQUES de signature des mises à jour (cles/publiques.txt) sont
# intégrées par go:embed (paquet mirador-stack/cles). La clé privée n'est
# jamais dans le dépôt (SPEC.md § 6.8, CLAUDE.md).
LDFLAGS = -s -w -buildid= \
	-X mirador-stack/internal/version.Version=$(VERSION) \
	-X mirador-stack/internal/version.Commit=$(COMMIT)

.PHONY: test bench verif agent serveur release propre olvid-pb

test:
	go test -race ./...

bench:
	go test -run XXX -bench . -benchmem ./...

verif:
	go vet ./...
	@test -z "$$(gofmt -l .)" || (echo "gofmt à passer sur :"; gofmt -l .; exit 1)
	@./scripts/verifier-licences.sh

agent:
	@mkdir -p dist
	for arch in amd64 arm64; do \
		CGO_ENABLED=0 GOOS=linux GOARCH=$$arch go build $(GOFLAGS_BUILD) \
			-ldflags "$(LDFLAGS)" -o dist/mirador-agent-linux-$$arch ./cmd/mirador-agent || exit 1; \
	done
	cd dist && sha256sum mirador-* > SHA256SUMS

serveur:
	@mkdir -p dist
	for arch in amd64 arm64; do \
		CGO_ENABLED=0 GOOS=linux GOARCH=$$arch go build $(GOFLAGS_BUILD) \
			-ldflags "$(LDFLAGS)" -o dist/mirador-server-linux-$$arch ./cmd/mirador-server || exit 1; \
	done
	cd dist && sha256sum mirador-* > SHA256SUMS

release:
	@mkdir -p dist
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build $(GOFLAGS_BUILD) \
		-ldflags "$(LDFLAGS)" -o dist/mirador-release-windows-amd64.exe ./cmd/mirador-release
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build $(GOFLAGS_BUILD) \
		-ldflags "$(LDFLAGS)" -o dist/mirador-release-linux-amd64 ./cmd/mirador-release
	cd dist && sha256sum mirador-* > SHA256SUMS

propre:
	rm -r -f ./dist

# Client gRPC du daemon Olvid (code généré, AGPL-3.0) : voir scripts/generer-olvid-pb.sh.
olvid-pb:
	scripts/generer-olvid-pb.sh
