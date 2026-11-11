# Composants tiers

mirador-stack est sous GPL-3.0-or-later (`LICENSE`). Chaque dépendance ci-dessous
a été relue : sa licence est compatible avec la GPL-3.0. Le script
`scripts/verifier-licences.sh` (lancé par la CI) refuse toute dépendance absente
de cette liste.

| Module | Version | Licence | Utilisé par | Pourquoi |
|---|---|---|---|---|
| `github.com/klauspost/compress` | v1.20.3 | BSD-3-Clause (+ parties Apache-2.0 et MIT) | mirador-agent | compression snappy du format *remote write* (et zstd pour les logs) ; plus rapide que les versions de la bibliothèque standard, sans cgo |
| `github.com/godbus/dbus/v5` | v5.2.4 | BSD-2-Clause | mirador-agent | lecture de l'état des services systemd par D-Bus, sans lancer `systemctl` |
| `golang.org/x/sys` | v0.49.0 | BSD-3-Clause | agent, serveur, mirador-release | appels système (Linux, Windows) |
| `golang.org/x/term` | v0.48.0 | BSD-3-Clause | mirador-release | saisie de la phrase de passe sans l'afficher (Windows et Linux) |
| `rsc.io/qr` | v0.2.0 | BSD-3-Clause | mirador-server | QR code de la 2FA (et de l'invitation Olvid) dessiné par le serveur, sans service externe ; ~1 500 lignes, aucune dépendance |
| `github.com/pkg/sftp` | v1.13.12 | BSD-2-Clause | mirador-server | copie des sauvegardes sur un serveur SFTP |
| `github.com/kr/fs` | v0.1.0 | BSD-3-Clause | mirador-server (via pkg/sftp) | |
| `modernc.org/sqlite` | v1.62.0 | BSD-3-Clause (SQLite : domaine public) | mirador-server | base de configuration, pur Go (pas de cgo, binaire statique) |
| `modernc.org/libc` | v1.80.1 | BSD-3-Clause (parties musl : MIT) | mirador-server (via sqlite) | bibliothèque C traduite en Go |
| `modernc.org/mathutil` | v1.7.1 | BSD-3-Clause | mirador-server (via sqlite) | |
| `modernc.org/memory` | v1.12.1 | BSD-3-Clause | mirador-server (via sqlite) | |
| `github.com/remyoudompheng/bigfft` | 2023-01-29 | BSD-3-Clause | mirador-server (via sqlite) | |
| `github.com/google/uuid` | v1.6.0 | BSD-3-Clause | mirador-server (via sqlite) | |
| `github.com/dustin/go-humanize` | v1.1.0 | MIT | mirador-server (via sqlite) | |
| `github.com/edsrzf/mmap-go` | 2024-12-12 | BSD-3-Clause | mirador-server (via prometheus/promql) | journal des requêtes du moteur PromQL (non utilisé par mirador) |
| `github.com/facette/natsort` | 2018-12-10 | BSD-3-Clause | mirador-server (via prometheus/promql) | tri naturel (fonctions sort_by_label) |

### Base de séries temporelles de Prometheus et ses dépendances (mirador-server)

La bibliothèque `tsdb` de Prometheus importe le paquet de configuration de
Prometheus, qui à son tour tire les SDK des nuages (AWS, Azure, Google), gRPC
et des morceaux de Kubernetes. Ce code est **compilé mais jamais appelé** par
mirador (seules ses fonctions d'initialisation tournent). Coût mesuré : +12,6 Mo
de binaire, ~+1 Mo de RAM (SPEC § 16.6). Licences relues : toutes Apache-2.0,
MIT ou BSD, compatibles GPL-3.0. `govulncheck` (CI) signale les failles du code
réellement appelé.

| Module | Version | Licence | Utilisé par | Pourquoi |
|---|---|---|---|---|
| `cloud.google.com/go/auth` | v0.24.1-0.20261001053825-dbc26066f70a | Apache-2.0 | mirador-server (via prometheus/tsdb) |  |
| `cloud.google.com/go/auth/oauth2adapt` | v0.3.0 | Apache-2.0 | mirador-server (via prometheus/tsdb) |  |
| `cloud.google.com/go/compute/metadata` | v0.10.0 | Apache-2.0 | mirador-server (via prometheus/tsdb) |  |
| `github.com/Azure/azure-sdk-for-go/sdk/azcore` | v1.23.3 | MIT | mirador-server (via prometheus/tsdb) |  |
| `github.com/Azure/azure-sdk-for-go/sdk/azidentity` | v1.15.0 | MIT | mirador-server (via prometheus/tsdb) |  |
| `github.com/Azure/azure-sdk-for-go/sdk/internal` | v1.13.0 | MIT | mirador-server (via prometheus/tsdb) |  |
| `github.com/AzureAD/microsoft-authentication-library-for-go` | v1.10.1 | MIT | mirador-server (via prometheus/tsdb) |  |
| `github.com/alecthomas/units` | v0.0.0-20240927000941-0f3dac36c52b | MIT | mirador-server (via prometheus/tsdb) |  |
| `github.com/aws/aws-sdk-go-v2` | v1.47.3 | Apache-2.0 | mirador-server (via prometheus/tsdb) |  |
| `github.com/aws/aws-sdk-go-v2/config` | v1.33.9 | Apache-2.0 | mirador-server (via prometheus/tsdb) |  |
| `github.com/aws/aws-sdk-go-v2/credentials` | v1.20.9 | Apache-2.0 | mirador-server (via prometheus/tsdb) |  |
| `github.com/aws/aws-sdk-go-v2/feature/ec2/imds` | v1.20.3 | Apache-2.0 | mirador-server (via prometheus/tsdb) |  |
| `github.com/aws/aws-sdk-go-v2/internal/configsources` | v1.5.6 | Apache-2.0 | mirador-server (via prometheus/tsdb) |  |
| `github.com/aws/aws-sdk-go-v2/internal/endpoints/v2` | v2.8.6 | Apache-2.0 | mirador-server (via prometheus/tsdb) |  |
| `github.com/aws/aws-sdk-go-v2/internal/v4a` | v1.5.6 | Apache-2.0 | mirador-server (via prometheus/tsdb) |  |
| `github.com/aws/aws-sdk-go-v2/service/internal/accept-encoding` | v1.13.21 | Apache-2.0 | mirador-server (via prometheus/tsdb) |  |
| `github.com/aws/aws-sdk-go-v2/service/internal/presigned-url` | v1.14.6 | Apache-2.0 | mirador-server (via prometheus/tsdb) |  |
| `github.com/aws/aws-sdk-go-v2/service/signin` | v1.10.4 | Apache-2.0 | mirador-server (via prometheus/tsdb) |  |
| `github.com/aws/aws-sdk-go-v2/service/sso` | v1.38.4 | Apache-2.0 | mirador-server (via prometheus/tsdb) |  |
| `github.com/aws/aws-sdk-go-v2/service/ssooidc` | v1.43.4 | Apache-2.0 | mirador-server (via prometheus/tsdb) |  |
| `github.com/aws/aws-sdk-go-v2/service/sts` | v1.51.4 | Apache-2.0 | mirador-server (via prometheus/tsdb) |  |
| `github.com/aws/smithy-go` | v1.28.5 | Apache-2.0 | mirador-server (via prometheus/tsdb) |  |
| `github.com/bboreham/go-loser` | v0.0.0-20230920113527-fcc2c21820a3 | Apache-2.0 | mirador-server (via prometheus/tsdb) |  |
| `github.com/beorn7/perks` | v1.0.1 | MIT | mirador-server (via prometheus/tsdb) |  |
| `github.com/cenkalti/backoff/v5` | v5.0.3 | MIT | mirador-server (via prometheus/tsdb) |  |
| `github.com/cespare/xxhash/v2` | v2.3.0 | MIT | mirador-server (via prometheus/tsdb) |  |
| `github.com/dennwc/varint` | v1.0.0 | MIT | mirador-server (via prometheus/tsdb) |  |
| `github.com/felixge/httpsnoop` | v1.1.0 | MIT | mirador-server (via prometheus/tsdb) |  |
| `github.com/go-logr/logr` | v1.4.4 | Apache-2.0 | mirador-server (via prometheus/tsdb) |  |
| `github.com/go-logr/stdr` | v1.2.2 | Apache-2.0 | mirador-server (via prometheus/tsdb) |  |
| `github.com/golang-jwt/jwt/v5` | v5.3.1 | MIT | mirador-server (via prometheus/tsdb) |  |
| `github.com/golang/snappy` | v1.0.0 | BSD-3-Clause | mirador-server (via prometheus/tsdb) |  |
| `github.com/google/go-cmp` | v0.7.0 | BSD-3-Clause | mirador-server (via prometheus/tsdb) |  |
| `github.com/google/s2a-go` | v0.1.11 | Apache-2.0 | mirador-server (via prometheus/tsdb) |  |
| `github.com/googleapis/enterprise-certificate-proxy` | v0.3.23 | Apache-2.0 | mirador-server (via prometheus/tsdb) |  |
| `github.com/googleapis/gax-go/v2` | v2.26.2 | BSD-3-Clause | mirador-server (via prometheus/tsdb) |  |
| `github.com/grafana/regexp` | v0.0.0-20250905093917-f7b3be9d1853 | BSD-3-Clause | mirador-server (via prometheus/tsdb) |  |
| `github.com/jpillora/backoff` | v1.0.0 | MIT | mirador-server (via prometheus/tsdb) |  |
| `github.com/kylelemons/godebug` | v1.1.0 | Apache-2.0 | mirador-server (via prometheus/tsdb) |  |
| `github.com/munnerz/goautoneg` | v0.0.0-20191010083416-a7dc8b61c822 | BSD-3-Clause | mirador-server (via prometheus/tsdb) |  |
| `github.com/mwitkow/go-conntrack` | v0.0.0-20190716064945-2f068394615f | Apache-2.0 | mirador-server (via prometheus/tsdb) |  |
| `github.com/oklog/ulid/v2` | v2.1.2 | Apache-2.0 | mirador-server (via prometheus/tsdb) |  |
| `github.com/pkg/browser` | v0.0.0-20240102092130-5ac0b6a4141c | BSD-2-Clause | mirador-server (via prometheus/tsdb) |  |
| `github.com/prometheus/client_golang` | v1.25.0 | Apache-2.0 | mirador-server (via prometheus/tsdb) |  |
| `github.com/prometheus/client_golang/exp` | v0.0.0-20261019101427-217707f26b14 | Apache-2.0 | mirador-server (via prometheus/tsdb) |  |
| `github.com/prometheus/client_model` | v0.6.3 | Apache-2.0 | mirador-server (via prometheus/tsdb) |  |
| `github.com/prometheus/common` | v0.72.1 | Apache-2.0 | mirador-server (via prometheus/tsdb) |  |
| `github.com/prometheus/otlptranslator` | v1.0.0 | Apache-2.0 | mirador-server (via prometheus/tsdb) |  |
| `github.com/prometheus/procfs` | v0.22.0 | Apache-2.0 | mirador-server (via prometheus/tsdb) |  |
| `github.com/prometheus/prometheus` | v0.316.0 | Apache-2.0 | mirador-server (via prometheus/tsdb) | base de séries temporelles (TSDB) des métriques, choisie sur mesures (SPEC § 16.1) |
| `github.com/prometheus/sigv4` | v0.5.0 | Apache-2.0 | mirador-server (via prometheus/tsdb) |  |
| `github.com/stretchr/testify` | v1.12.1 | MIT | mirador-server (via prometheus/tsdb) |  |
| `go.opentelemetry.io/auto/sdk` | v1.2.1 | Apache-2.0 | mirador-server (via prometheus/tsdb) |  |
| `go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp` | v0.72.0 | Apache-2.0 | mirador-server (via prometheus/tsdb) |  |
| `go.opentelemetry.io/otel` | v1.47.0 | Apache-2.0 | mirador-server (via prometheus/tsdb) |  |
| `go.opentelemetry.io/otel/log` | v1.47.0 | Apache-2.0 | mirador-server (via prometheus/tsdb) |  |
| `go.opentelemetry.io/otel/metric` | v1.47.0 | Apache-2.0 | mirador-server (via prometheus/tsdb) |  |
| `go.opentelemetry.io/otel/trace` | v1.47.0 | Apache-2.0 | mirador-server (via prometheus/tsdb) |  |
| `go.uber.org/atomic` | v1.12.0 | MIT | mirador-server (via prometheus/tsdb) |  |
| `go.uber.org/goleak` | v1.3.0 | MIT | mirador-server (via prometheus/tsdb) |  |
| `go.yaml.in/yaml/v2` | v2.4.4 | Apache-2.0 | mirador-server (via prometheus/tsdb) |  |
| `go.yaml.in/yaml/v3` | v3.0.5 | Apache-2.0 | mirador-server (via prometheus/tsdb) |  |
| `golang.org/x/crypto` | v0.58.0 | BSD-3-Clause | mirador-server, mirador-release | serveur : SSH des sauvegardes SFTP ; mirador-release : Argon2id (chiffrement de la clé privée) |
| `golang.org/x/exp` | v0.0.0-20261014143641-f0251eb4aa3e | BSD-3-Clause | mirador-server (via prometheus/tsdb) |  |
| `golang.org/x/net` | v0.62.0 | BSD-3-Clause | mirador-server (via prometheus/tsdb) |  |
| `golang.org/x/oauth2` | v0.37.0 | BSD-3-Clause | mirador-server (via prometheus/tsdb) |  |
| `golang.org/x/sync` | v0.24.0 | BSD-3-Clause | mirador-server (via prometheus/tsdb) |  |
| `golang.org/x/text` | v0.43.0 | BSD-3-Clause | mirador-server (via prometheus/tsdb) |  |
| `golang.org/x/time` | v0.16.0 | BSD-3-Clause | mirador-server (via prometheus/tsdb) |  |
| `google.golang.org/api` | v0.305.0 | BSD-3-Clause | mirador-server (via prometheus/tsdb) |  |
| `google.golang.org/genproto/googleapis/rpc` | v0.0.0-20261012220534-4341edf3f941 | Apache-2.0 | mirador-server (via prometheus/tsdb) |  |
| `google.golang.org/grpc` | v1.84.0 | Apache-2.0 | mirador-server (via prometheus/tsdb ; utilisé directement par le canal Olvid) |  |
| `google.golang.org/protobuf` | v1.36.12 | BSD-3-Clause | mirador-server (via prometheus/tsdb ; utilisé directement par le canal Olvid) |  |
| `k8s.io/apimachinery` | v0.37.1 | Apache-2.0 | mirador-server (via prometheus/tsdb) |  |
| `k8s.io/client-go` | v0.37.1 | Apache-2.0 | mirador-server (via prometheus/tsdb) |  |
| `k8s.io/klog/v2` | v2.140.0 | Apache-2.0 | mirador-server (via prometheus/tsdb) |  |
| `k8s.io/utils` | v0.0.0-20260626114624-be93311217bd | Apache-2.0 | mirador-server (via prometheus/tsdb) |  |

Les autres modules qui apparaissent dans `go.mod` (outils de génération de
modernc, bibliothèques de test) ne sont **pas compilés** dans les binaires : la
vérification ne porte que sur ceux qui le sont (`go list -deps ./cmd/...`).

## Données

| Donnée | Licence | Remarque |
|---|---|---|
| Base GéoIP « IP to Country Lite » de DB-IP | CC BY 4.0 | serveur uniquement (étape 3) ; mention « IP Geolocation by DB-IP » affichée dans l'interface |

### Code généré inclus dans le dépôt

| Source | Version | Licence | Où | Pourquoi |
|---|---|---|---|---|
| `olvid-io/Olvid-Bot-Protobuf` | v2.0.1 (`6009f7e`) | **AGPL-3.0** | `internal/server/notif/olvid/pb/` (client gRPC généré) | canal Olvid du serveur ; compatible GPL-3.0 (article 13) ; ce dossier reste sous AGPL-3.0 (accord de l'utilisateur du 10/11, SPEC § 17.11) |

| uPlot (`leeoniya/uPlot`, npm `uplot`) | 1.6.32 | MIT | `internal/server/console/statique/uplot/` (licence jointe) | courbes de l'interface web (canvas, 51 Ko), embarquées dans le binaire |
| Police Inter (npm `@fontsource/inter`) | 5.3.0 | SIL Open Font License 1.1 | `internal/server/console/statique/polices/` (licence jointe) | texte de l'interface ; embarquée (pas de Google Fonts : CSP stricte) |
| Police Space Grotesk (npm `@fontsource/space-grotesk`) | 5.3.0 | SIL Open Font License 1.1 | `internal/server/console/statique/polices/` (licence jointe) | titres et chiffres de l'interface |
| Natural Earth, *Admin 0 – Countries* 1:110m (`nvkelso/natural-earth-vector`) | 5.1.2 | domaine public | `internal/server/console/carte_monde.go` (généré par `scripts/generer-carte.py`) | fond de la carte du monde des flux bloqués (contours simplifiés, noms français) |
| Base GéoIP DB-IP « country lite » | téléchargée chaque mois | CC BY 4.0 | dossier des données du serveur (jamais dans le dépôt ni le binaire) | pays des flux bloqués ; mention « IP Geolocation by DB-IP » sur les pages |
