# PROGRES.md — état d'avancement de mirador-stack

## Où on en est

| Étape | État | Branche |
|---|---|---|
| 1. Cahier des charges | **Validée le 07/11/2026** — SPEC v1.0 (PR #1) | `etape-1-cahier-des-charges` |
| 2. Agent | **Validée et fusionnée le 09/11/2026** (PR #2 et #3) | `etape-2-agent` |
| 3. Serveur et stockage | **Validée le 10/11/2026** (PR #4, fusion à confirmer) | `etape-3-serveur` |
| 4. Alertes et notifications | **Validée le 10/11/2026** (PR à fusionner) | `claude/admiring-lamport-nqtigt` |
| 5. Interface web | **faite, à valider** (parties 1 à 6) | `claude/admiring-wright-2n17eb` |
| 5 bis. Idées de Beszel : surveillance par machine, vue d'ensemble | **faite, à valider** (SPEC § 19) | `claude/laughing-sagan-7w7xrm` |
| 5 ter. Codes de secours de la 2FA et passkeys | **faite, à valider** (SPEC § 20) | `claude/laughing-sagan-7w7xrm` |
| 6. Équipements sans agent + `mirador-collector` | non commencée | — |
| 7. Bascule | non commencée | — |

## Étape 1 — fait

- v0.1 : `CLAUDE.md`, `SPEC.md`, `PROGRES.md`, 5 questions.
- v0.2 : nouveaux fichiers de `reference/`, 8 dashboards inventoriés, réponses
  intégrées, section Nyx (lu en entier, rien copié).
- v0.3 (réponses et mesures du 07/11) :
  - collecteur LAN : Alloy reste jusqu'à l'étape 6, puis binaire séparé
    `mirador-collector` ; l'agent n'ouvre jamais de port ;
  - Olvid optionnel, Telegram principal, test du daemon au début de l'étape 4 ;
  - 5 règles de logs = ajouts possibles ; alerte OOM activée par défaut, basée sur
    le compteur noyau `oom_kill` (pas de recherche dans les logs) ;
  - mesures de la stack actuelle intégrées (SPEC § 2.1) ;
  - collecte limitée aux 42 métriques `node_*` réellement utilisées (à valider) ;
  - plafond de stockage mirador : 8 Go (22 Go libres mesurés).
- v0.3 + Olvid : conception basée sur `prohand/gladys-olvid` (écrit par l'utilisateur : reprise libre, réécrite en Go) :
  - profil Olvid particulier suffisant, sans Enterprise ;
  - daemon Java (`-Xmx512m` chez Gladys) → RAM à mesurer à l'étape 4, comptée à
    part du budget serveur ;
  - daemon lancé par son propre `docker compose`, client gRPC Go côté serveur
    seulement, délais et keepalive sur chaque appel ;
  - à vérifier : détecter que le bot est coupé des serveurs Olvid (pas seulement
    du daemon).
- Aucun code. Rien touché sur l'infrastructure.

## Étape 2 — avancement

Réponses du 07/11 : test sur **srv-test**, envoi sous le nom
`srv-test-mirador`, alertes en double laissées partir, une ligne de token en
plus dans nginx (SPEC § 15.1).

| Partie | État |
|---|---|
| 1. Squelette Go, configuration typée, CI, compilation amd64/arm64 | **fait** |
| 2. Collecteurs `/proc` et `/sys`, encodeur *remote write*, file bornée, envoi | **fait** (métriques système ; journald et Docker viennent ensuite) |
| 3. systemd (D-Bus), mises à jour en attente, Docker par proxy | **fait** (D-Bus réel à vérifier sur srv-test : pas de systemd ici) |
| 4. Assistant root (pare-feu, SMART, **certificats**) | **fait** (iptables testé en vrai ici ; SMART avec des sorties d'exemple, `smartctl` absent ici) |
| 5a. journald, filtres, masquage, compteurs (OOM…), envoi vers VictoriaLogs | **fait** (vrai journalctl et VictoriaLogs à vérifier sur srv-test) |
| 5b. Logs Docker par le proxy | **fait** (testé avec un faux Docker ; vrai proxy à vérifier sur srv-test) |
| 6. Config distante, enrôlement, protocole versionné, mise à jour signée (désactivés) | **fait** (testé, non branché tant que le serveur n'existe pas) |
| 7. Script d'installation, unités systemd, commandes pour srv-test | **fait** (`deploy/agent/`, `docs/installation-agent.md`) |
| 8. Mesures sur srv-test | **fait** : valeurs identiques à Alloy, 24 h mesurées, objectifs tenus |

Contenu actuel :

- `cmd/mirador-agent` : binaire (`-verifier`, `-une-fois`, `-self-test`, `-version`).
- `internal/agent/config` : configuration `CLE=valeur` validée, token dans un fichier à part.
- `internal/agent/collect` : CPU, charge, mémoire, vmstat (dont `oom_kill`), PSI,
  disques, réseau, systèmes de fichiers, uname/heure, températures.
- `internal/agent/remotewrite` : encodeur protobuf + snappy.
- `internal/agent/envoi` : file bornée en mémoire, envoi HTTPS avec reprise.
- `internal/agent` : boucle de collecte, métriques internes `mirador_agent_*`.
- CI (`.github/workflows/ci.yml`) : gofmt, go vet, tests `-race`, fuzzing court,
  gosec, govulncheck, licences, compilation.
- Tests : 7 paquets, tous verts (`go test -race ./...`). Fuzzing : config, analyseurs `/proc`.
- `gosec` : 0 alerte restante (12 traitées : faux positifs annotés, 2 erreurs
  d'écriture corrigées). `govulncheck` : aucune faille.
- Binaires statiques et reproductibles (mêmes empreintes après vidage du cache).

## Étape 3 — avancement

| Partie | État |
|---|---|
| 1. Bancs d'essai stockage et décision chiffrée (SPEC § 16.1) | **fait** : TSDB pour les métriques, segments zstd pour les logs |
| 2. Squelette serveur : fichier de démarrage, SQLite + migrations, deux écoutes, image Docker | **fait** (12,7 Mo au repos ; image non construite ici : pas de Docker) |
| 3. Ingestion : remote write + jsonline, tokens hachés, quotas, cardinalité, débit, décalage d'horloge | **fait** (SPEC § 16.4) : ~1 µs/série, ~4,4 µs/ligne, 11,5 Mo au repos, 64 Mo au pire sous charge |
| 4. Stockage : TSDB + agrégats 5 min / 30 j, logs zstd + recherche bornée, rétention à chaud | **fait** (SPEC § 16.6) : 119 Mo sous 30 000 séries, logs 37 Mo/jour, mot rare sur 24 h en 8 ms |
| 5. Enrôlement, config distante, battements, mises à jour signées + `mirador-release` ; branchement côté agent | **fait** (SPEC § 16.7) : cycle complet testé de bout en bout (enrôlement → approbation → battement → configuration → mise à jour → confirmation / retour arrière) ; commandes du serveur ; `mirador-release` Windows |
| 6. Sauvegarde / restauration (testée en CI), migrations sûres | **fait** (SPEC § 16.9) : archive chiffrée, local + SFTP + S3, planifiée et à la demande, restauration sur instance vide testée en CI ; ~280 Mo/s |
| 7. Test de charge 50 agents (rapport RAM/CPU/I/O), installation sur le VPS en parallèle | **fait** (SPEC § 16.10) : 0,55 % CPU, 122 Mo de RAM propre (RSS max 157 Mo), ~19 Ko/s écrits, 0 erreur ; image Docker en CI ; guide d'installation prêt (à faire par l'utilisateur) |

Décision du 09/11 (SPEC § 16.5) : nginx filtre seulement par IP ; tokens gérés
par mirador ; **token lié à des adresses** réglables dans l'interface (fait
côté serveur : migration 0002, 403 hors liste, dernière adresse gardée).

Mesures de la partie 3 (machine de développement, 4 cœurs) :

| Mesure | Résultat | Objectif |
|---|---|---|
| CPU, métriques | ~1 µs par série (0,76 ms pour un lot de 800) → ~0,04 % d'un cœur pour 25 agents | < 5 % |
| CPU, logs | ~4,4 µs par ligne → ~6 s par jour pour 1,4 M lignes | < 5 % |
| RAM au repos | 11,5 Mo (2,1 Mo propres) | < 256 Mio |
| RAM, suivi des quotas | 2,7 Mo pour 40 000 séries | — |
| RAM, pire cas des requêtes | ~16 Mo par requête, 4 en même temps → 64 Mo | < 256 Mio avec la TSDB (~100 Mo) |
| Disque | 1 écriture SQLite par minute (`vu_le`) | — |

Comparatif DuckDB (10/11, SPEC § 16.8) : **décidé, on garde la TSDB et le
moteur de logs** (DuckDB : disque ×2,6 et écritures ×3 pour les métriques,
+140 à 170 Mo de RAM pour les logs). Encodage XOR2 de la TSDB activé :
−9 % de disque, rien d'autre ne change.

**Bilan de l'étape 3 contre les objectifs** (50 agents, 30 000 séries, test
de charge de la partie 7) :

| Objectif (SPEC § 7.1) | Mesuré | Verdict |
|---|---|---|
| RAM < 256 Mio | 122 Mo propres en moyenne, RSS max 157 Mo (compaction comprise) | ✅ |
| CPU < 5 % d'un cœur | 0,55 % (pic 1,1 %) | ✅ |
| Écritures < 1 Mo/s | ~19 Ko/s (~1,6 Go/jour) | ✅ |
| Disque ≤ 8 Go | ~1,1 Go métriques brutes (7 j, XOR2) + agrégats + ~1,1 Go logs (30 j) | ✅ |

À confirmer : mesure de 24 h sur le VPS avec les vraies machines
(`docs/installation-serveur.md` § 5).

Mesures de la partie 4 (SPEC § 16.6) :

| Mesure | Résultat | Objectif |
|---|---|---|
| RAM, serveur complet, 50 agents × 600 séries + logs (3 min) | ~119 Mo RSS, 96 Mo propres (générateur compris) | < 256 Mio |
| CPU, même charge | 0,43 % d'un cœur (générateur compris) | < 5 % |
| Logs, écriture | 2,3 µs/ligne ; 37 Mo/jour sur disque | — |
| Logs, recherche (1 jour) | mot rare 8 ms ; texte libre 0,66 s ; 500 dernières lignes 8 ms | — |
| Binaire du serveur | 24,6 Mo (12 Mo avant la TSDB) | — |
| I/O par jour sous charge réelle | à mesurer en partie 7 (24 h) | < 1 Mo/s |

Mesures de la partie 1 : voir SPEC § 16.1 (TSDB : ~100 Mo pour 30 000 séries
avec plafond Go, 0,04 % CPU ; logs zstd : 34 Mo/jour écrits contre 1,5 Go pour
SQLite FTS5).

Point à surveiller (vu en CI le 09/11) : sur une machine de la CI, un capteur
de `/sys/class/hwmon` répondait très lentement ; le test de fuite mémoire de
l'agent (3 300 cycles) dépassait 10 min. Corrigé côté test (`/sys` vide). Côté
agent : une lecture de capteur lente ralentit le cycle de collecte. À vérifier
sur une machine **physique** (srv-test est une VM, sans capteur) ; si besoin,
délai maximal sur les lectures de capteurs, comme pour les montages réseau.

## Étape 4 — avancement

| Partie | État |
|---|---|
| 1. Moteur (PromQL restreint), règles en base à chaud, historique, import des 70 règles, règles mirador, métriques internes, commandes | **fait** (SPEC § 17.1 à 17.5) |
| 2. Silences, inhibitions, dépendances, maintenance, accusés de réception, regroupement, rappels, mode ombre | **fait** (SPEC § 17.6) ; mode **ombre par défaut** ; coût négligeable (un tour / 5 s, aucune écriture hors envoi) |
| 3. Notifications : Telegram, ntfy, e-mail, bascule, file d'attente, historique, heartbeat HetrixTools | **fait** (SPEC § 17.7) : bascule testée de bout en bout avec pannes simulées (principal, puis secours, file d'attente, retour, inversion) ; e-mail non essayé contre un vrai serveur |
| 4. Olvid : test du daemon (par l'utilisateur), puis module | **fait** : tests concluants (10/11), module écrit (SPEC § 17.11) ; essai d'annulation à faire chez toi (`docs/olvid.md` § 4) |
| 5. Comparaison avec vmalert, test de charge avec alertes, bilan | **fait** (SPEC § 17.8, § 17.9) |

**Bilan de l'étape 4 contre les objectifs** (test de charge 50 agents + 75
règles, 12 min ; SPEC § 17.9) :

| Objectif (SPEC § 7.1) | Étape 3 | Étape 4 | Verdict |
|---|---|---|---|
| RAM < 256 Mio | 122 Mo propres, RSS max 157 Mo | **185 Mo** propres, **RSS max 218 Mo** (175 / 205 avec `GOGC=50`) | ✅ mais marge réduite (~38 Mo) |
| CPU < 5 % d'un cœur | 0,55 % | **1,00 %** | ✅ |
| Écritures < 1 Mo/s | ~19 Ko/s | ~23 Ko/s | ✅ |

⚠️ +60 Mo de RAM par rapport à l'étape 3. **Profil mémoire (10/11)** : le
moteur d'alertes n'y est pour rien (−3,5 Mo entre « avec » et « sans »
règles). La hausse vient de la voie « hors ordre » de la TSDB (~43 à 55 Mo) :
le rattrapage des agents arrive après les métriques internes, écrites dès le
démarrage. Cas réel après une coupure, mémoire rendue à la compaction.
Essai `OutOfOrderCapMax` 32 → 8 : RAM propre inchangée, écritures +29 % →
**on garde 32**. SPEC § 17.9.

Réponses du 10/11 (SPEC § 17.10) : PromQL **validé** ; accusé de réception
depuis Telegram **fait** ; collecteur = `miradorcollector` ; `GOGC=50`
**ajouté** au compose ; invitation Olvid à gérer dans l'interface web
(étape 5, décidé).

Olvid (tests du 10/11, SPEC § 17.10) : **concluant**. Tête-à-tête et
groupe ok avec `-Xmx128m` (~170 Mio, CPU ~0,15 %, 5 Mo de disque) ; pendant
une coupure, le daemon accepte le message, le garde et le livre 1 s après
le retour du réseau. Un envoi ne comptera comme réussi qu'avec la
confirmation `MessageUploaded`.

Réponses du 10/11 : licence AGPL **acceptée** ; message Olvid **annulé**
s'il n'est pas confirmé (l'alerte part par le secours).

Module Olvid : **fait** (SPEC § 17.11). Envoi confirmé par
`MessageUploaded` (30 s), sinon annulé dans le daemon et bascule ; testé
avec un faux daemon gRPC. Binaire 27,1 → 29,7 Mo.

Reporté (réponse du 10/11), à faire **lors de l'installation de
mirador-server sur le VPS** : brancher le daemon sur le réseau `obs`,
configurer le canal, `notif-tester olvid`, puis l'**essai d'annulation**
(`docs/olvid.md` § 2 et § 4). Non bloquant pour la validation de l'étape 4.

**Étape 4 validée le 10/11/2026.**

## Étape 5 — avancement

| Partie | État |
|---|---|
| 1. Socle sécurité (comptes Argon2id, 2FA TOTP, sessions, CSRF, verrouillage, CrowdSec, rôles, premier admin, liste d'adresses à confirmer) | **fait** (SPEC § 18.2) |
| 2. Vue d'ensemble, machines, enrôlement, mises à jour, tableaux (6 convertis de Grafana, 134 panneaux), courbes uPlot | **fait** |
| 3. Alertes, sourdine, envois, comparaison vmalert, règles (modèles, formulaire, test 24 h, YAML) | **fait** |
| 4. Logs, flux bloqués | **fait** (carte GéoIP comprise) |
| 5. Réglages : notifications, conservation, sauvegardes, agents, interface, audit, export / import | **fait** |
| 6. Mesures, documentation, CrowdSec | **fait** (charge complète à refaire, SPEC § 18.7) |

Mesures (SPEC § 18.4, 50 machines, machine de développement) :

| Mesure | Résultat | Objectif |
|---|---|---|
| Vue d'ensemble / tableau | 3 ms / 3 ms (max 7 ms) | — |
| Courbe 50 séries | 22 ms, JSON compressé | — |
| CPU, un tableau ouvert (rafraîchi chaque minute) | ~0,3 % d'un cœur | serveur < 5 % ✅ |
| Mémoire | tas +20 Mo en rafale, rendu ensuite ; Argon2id ≤ 38 Mio au pire | serveur < 256 Mio ⚠️ marge : étape 4 = 218 Mo au max → ~238 Mo |
| Binaire du serveur | 29,7 → 30,8 Mo | — |
| Écritures disque | aucune par requête (sessions en mémoire) | ✅ |

⚠️ Mémoire : la marge devient faible si quelqu'un regarde des courbes
pendant une compaction. Solutions, par ordre de coût : (1) cache des
requêtes 400 → 100 entrées ; (2) courbes limitées à 20 séries (au lieu de 50) ;
(3) `GOMEMLIMIT` déjà à 200 Mio (le ramasse-miettes passe plus tôt). À
trancher après la mesure complète.

Restes à faire (SPEC § 18.5) : vues personnalisables, carte du monde
(fond SVG).

### Réponses du 10/11 et suites

| Question | Réponse | Conséquence |
|---|---|---|
| Page Olvid | **maintenant** | **faite** (SPEC § 18.9) ; essai réel sur le VPS |
| Plafonds de taille par défaut | question pas claire | reformulée ci-dessous |
| Routage par gravité | question pas claire | reformulée ci-dessous |
| GéoIP | **automatique** | **fait** : téléchargée chaque mois par le serveur, 6,3 Mo seulement pendant l'usage (SPEC § 18.8) ; carte du monde encore à faire |
| Tag `v0.2.0` | **on attend** | rien |

Réponses du 10/11 (suite) :

| Question | Réponse | Conséquence |
|---|---|---|
| Place maximale sur le disque | **10 Go** | défaut : 5 Gio (métriques détaillées) + 1 Gio (résumées) + 4 Gio (logs), modifiable dans l'interface |
| Où partent les alertes | **un seul canal, choisi, bascule sur l'autre en cas de problème** | « critique sur tous les canaux » retiré ; principal / secours seulement (SPEC § 18.10) ; pas de destinataire par règle |

Aucune question en attente pour l'étape 5.

**Design (10/11, SPEC § 18.11)** : maquettes comparées, choix **B2** (menu
à gauche repliable ou en haut, thèmes clair et sombre) avec les cartes de
chiffres de la piste B ; appliqué à toute l'interface. Défaut grave trouvé
et corrigé au passage : blocage de toute l'interface après « Réglages ›
Interface » (lecture de la base pendant une transaction).

Tag `v0.2.0` posé par l'utilisateur le 10/11 ; image publiée par la CI.

**Retours d'affichage (10/11, SPEC § 18.12)** : majuscules partout,
tailles « 20,5 Go », formulaire Conservation aligné, adresse d'ingestion
réglable (commande d'installation complète avec le jeton), onglet Olvid
seulement si activé, heartbeat multi-services avec bouton de test.

**Olvid et tableaux (10/11, SPEC § 18.13 et § 18.14)** : daemon Olvid en
option dans le compose (profil `olvid`) et assistant d'installation dans
l'interface (clé admin donnée une fois, jamais gardée). Tableaux
modifiables par les admins : créer, dupliquer, importer / exporter,
supprimer, rétablir les 6 tableaux fournis ; rangées, panneaux (avec
aperçu), largeur et glisser-déposer. Tests ajoutés (faux daemon admin,
éditeur de bout en bout), gosec 0.

**Depuis la v0.2.3 (10-11/11)** : site et type choisis dans l'interface (plus
de `--site`/`--env`), sites et types gérés dans les réglages, tableaux
fournis proposés (catalogue) et icônes, conservation de l'audit, aide aux
requêtes PromQL (≈ 40 modèles dont températures, assistant, vérification en
direct), **installation de l'agent par téléchargement HTTPS** (script et
binaire servis par le serveur avec le jeton, SPEC § 18.19).

**Test de charge complet (11/11, SPEC § 18.7)** : 50 agents + logs + un
navigateur sur l'interface pendant 6 min, réglages mémoire de production :
**pic 211 Mio** (< 256 Mio, marge ~45 Mio, générateur de charge compris),
CPU 2,3 % d'un cœur. Reste avant validation : essai sur le VPS.

**Groupe et nom (11/11, SPEC § 18.23)** : groupes de configuration gérés
en liste et imposés comme étiquette `groupe` ; nom choisi au jeton et
corrigeable à l'approbation ; **historique rattaché au nouveau nom après un
renommage** (graphiques, tableaux, logs). GéoIP : téléchargement à la
demande, effacée quand la case est décochée. Suite : tag `v0.2.7` puis
enrôlement de srv-test sur le VPS.

**Enrôlement de srv-test (11/11, v0.2.7)** : jeton créé dans l'interface,
script téléchargé en HTTPS depuis le vhost d'ingestion, machine approuvée
(token limité à son adresse) ; métriques et logs reçus, graphiques et
logs affichés : **fonctionne**. Deux corrections faites pendant l'essai
(v0.2.8) : nom identique à celui d'Alloy refusé à tort vers mirador-server ;
site et type désormais proposés par la machine (SPEC § 18.23). Alloy
continue en parallèle (comparaison). Reste : mesure de 24 h (RAM / CPU /
disque de l'agent et du serveur), import des règles et comparaison avec
vmalert, notifications en mode ombre, puis validation de l'étape 5.

**Dépôt public (11/11)** : nouveau dépôt public `prohand/mirador-stack`,
un seul premier commit (détails de l'infrastructure retirés) ; l'ancien
dépôt reste privé (`mirador-stack-archive`, historique et `reference/`).
`v0.2.8` publiée depuis le nouveau dépôt et installée sur le VPS.

**Règles et comparaison (11/11)** : 69 règles importées de vmalert (68
exactes, `SyslogMuet` désactivée en attendant l'étape 6 ; `Watchdog`
remplacée par le heartbeat) + 8 règles mirador = 77 règles, 76 actives.
`syslog.yml` (LogsQL) : rien à importer (import corrigé pour le dire au
lieu d'une erreur YAML). Comparaison avec vmalert active, vmalert joint,
aucun écart au premier passage. Mesure de 24 h en cours sur l'agent.

**Mises à jour signées (11/11, SPEC § 18.25 et § 18.26)** : clé publique
posée à l'installation (fichier root) ; versions signées par la CI de GitHub
(environnement `signature` limité aux tags) et importées depuis
l'interface. `v0.2.9` : agent signé publié, importé (« signature vérifiée :
amd64, arm64 ») ; la machine de test passée à la main en 0.2.9 avec la clé posée et
`--auto-update`. Prochaine étape : `v0.2.10` (script relançable sans jeton
avec le token de la machine) assignée à la machine de test → première mise à jour
automatique.

**Bug trouvé (11/11, machine de test, Debian récente)** : après un redémarrage de
l'agent, la reprise du journal système échouait (« Please specify only one
of --since=, --cursor=, and --after-cursor= ») : `--after-cursor` et
`--since` refusés ensemble par les systemd récents ; logs système plus
envoyés depuis la réinstallation (métriques intactes). Corrigé en 0.2.11 :
`--after-cursor` seul, lignes de plus de 12 h ignorées par l'agent ; si
journalctl refuse le curseur, reprise à la fin du journal.

**Première mise à jour automatique (11/11)** : `v0.2.10` signée par la CI,
importée, assignée ; la machine de test est passée seule de 0.2.9 à 0.2.10
(signature vérifiée, redémarrage propre, essai de 10 min, « mise à jour
confirmée »).

**Installation sur le VPS (10/11)** : mirador-server 0.2.0 démarré, vhost
dédié à la console, **interface web fonctionnelle**. Points relevés et
corrigés dans la documentation : connexion à ghcr.io (token classic
`read:packages`), droits de `server.conf` (utilisateur 65532), vhost nginx
complet. Suite : `docs/installation-serveur.md` § 3 à 5 (ingestion,
srv-test, mesure de 24 h), puis règles, Telegram en mode ombre, Olvid.

## Essai sur srv-test (08/11)

Alloy envoie sous `srvtest`, mirador sous `srv-test-mirador`.

| Contrôle | Alloy | mirador | Verdict |
|---|---|---|---|
| `node_memory_MemAvailable_bytes` | 1 230 635 008 | 1 230 069 760 | ✅ écart 0,05 % (relevés décalés) |
| CPU sur 5 min | 2,383 % | 2,382 % | ✅ |
| Métriques utilisées absentes chez mirador | — | aucune | ✅ |
| Logs journal (10 min) | 49 lignes | 49 lignes | ✅ |
| Logs Docker (10 min) | 133 lignes | 133 lignes | ✅ (jsonline + gzip acceptés par VictoriaLogs) |
| Séries actives | 1 053 | 870 | ⚠️ −17 % seulement (à analyser : états systemd ?) |

Consommation, essai de 12 min (`mesurer.sh 0.2`, 2 cœurs) :

| Processus | RAM moy. (max) | CPU moyen | Écrit disque | Objectif |
|---|---|---|---|---|
| mirador-agent | 20,8 Mo (21,7) | 0,075 % | 8 Ko (curseur du journal) | < 30 Mio, < 1 % ✅ |
| journalctl (enfant de l'agent) | 50,8 Mo | 0,003 % | 0 | ⚠️ voir ci-dessous |
| proxy Docker | 18,8 Mo | 0 % | 0 | — |
| Alloy (comparaison) | 123,6 Mo | 0,76 % | 1 260 Ko | — |
| assistant root | 35 ms de CPU cumulé | | | ✅ |

- Agent seul : ~6 fois moins de RAM et ~10 fois moins de CPU qu'Alloy.
- Agent + journalctl = 71,6 Mo : **au-dessus des 30 Mio visés**. Hypothèse : la
  RSS de journalctl est surtout faite des fichiers du journal projetés en
  mémoire (cache récupérable par le système). À vérifier (`RssAnon` / `RssFile`).
  - Si `RssAnon` est faible : compter seulement cette part dans le budget (à
    écrire dans SPEC).
  - Sinon : (a) lecteur du journal écrit en Go (gros travail), ou (b) transfert
    du journal par journald vers un socket local (modifie la configuration de
    journald).

### Mesure sur 24 h (09/11, `mesurer.sh 24`, 2 cœurs)

| Processus | RAM min / moy / max | Mémoire propre | CPU moyen | Écrit en 24 h |
|---|---|---|---|---|
| mirador-agent | 19,4 / 21,5 / 23,5 Mo | — | 0,066 % | 1 148 Ko (= 288 écritures du curseur, une toutes les 5 min) |
| journalctl | 48,6 / 50,3 / 101,7 Mo | **292 Ko** (`RssAnon`) ; 52 Mo de cache de fichiers (`RssFile`) | 0,003 % | 0 |
| proxy Docker | 12,6 / 14,9 / 18,8 Mo | — | 0 % | 0 |
| assistant root | ~32 ms de CPU par passage | | | |
| Alloy (comparaison) | 123,8 / 146,1 / 160,4 Mo | — | 0,745 % | 165 028 Ko |

**Verdict par rapport aux objectifs (SPEC § 6.1 et § 15.3 bis)** :

- RAM : agent 21,5 Mo + journalctl 0,3 Mo ≈ **22 Mio < 30 Mio** ✅ (Alloy : 146 Mo).
- CPU : **0,066 %** d'un cœur < 1 % ✅ (Alloy : 0,745 %, 11 fois plus).
- Disque : **1,1 Mo/jour**, uniquement le curseur du journal ✅ (Alloy : 165 Mo/jour,
  150 fois plus). Option si besoin : curseur toutes les 15 min (0,4 Mo/jour,
  au prix de quelques lignes renvoyées en double après un arrêt brutal).
- Pas de fuite : RAM stable sur 24 h ✅.
- Proxy Docker : 15 Mo en plus sur les machines avec Docker (déjà prévu, SPEC § 6.5).

### Séries par métrique (09/11, `topk`)

| Métrique | Alloy | mirador (avant correction) |
|---|---|---|
| `node_systemd_unit_state` | 382 | 438 |
| `node_systemd_service_restart_total` | 153 | 172 |
| `node_systemd_timer_last_trigger_seconds` | 20 | — (inutilisée) |
| `node_cpu_seconds_total` | 16 | 16 |

Corrections faites le 09/11 (SPEC § 15.3) : unités non chargées ignorées, une
seule série par unité (`state="failed"`). Attendu : ~600 séries pour
`srv-test-mirador` contre 1 053 pour Alloy. À confirmer après la
réinstallation de la nouvelle version (facultatif avant la validation).

## Mesures

### Étape 1 (mirador)

- Sans objet (pas de code).

### Étape 2, parties 1 et 2 (mesuré dans l'environnement de développement)

Benchmarks Go (`make bench`) :

| Mesure | Résultat | Objectif |
|---|---|---|
| Ajout de 500 points au lot | 0 allocation | 0 par point ✅ |
| Encodage + compression de 576 points | 0,17 ms, 0 allocation | 0 par point ✅ |
| Cycle complet (lecture `/proc` + `/sys`, 145 points, encodage) | 0,18 à 0,22 ms, 43 allocations (≈ 2 Ko) | 0 par point ✅ (les 43 viennent de l'ouverture des fichiers) |

Essai d'endurance : agent réel, collecte toutes les **10 s** (3 fois plus souvent
que le défaut), envoi vers un faux serveur local, relevés dans `/proc/<pid>` :

| Durée | RAM résidente | CPU moyen | Écritures disque |
|---|---|---|---|
| 3 min | 11,1 → 11,8 Mio | ≈ 0,01 % d'un cœur | 0 octet |
| 15 min | 10,5 → 13,1 Mio | ≈ 0,015 % d'un cœur | 0 octet |
| 5 min, avec la partie 3 (systemd, apt, Docker activés) | 11,6 → 13,5 Mio | ≈ 0,02 % d'un cœur | 0 octet |

Objectifs : RAM < 30 Mio ✅, CPU < 1 % ✅, pas d'écriture disque ✅.
Limite de l'essai partie 3 : ici, ni systemd ni Docker ne tournent (les appels
échouent vite) ; `apt-get --simulate` a bien tourné. Le coût réel de D-Bus et de
Docker sera mesuré sur srv-test.

La RAM résidente monte doucement (≈ +2,6 Mio en 15 min). Vérification d'une fuite
(`TestPasDeFuiteMemoire`, lancé par la CI) : après 3 000 cycles complets
(≈ 25 h à 30 s), la mémoire réellement utilisée par les données est de **489 Ko**,
contre 922 Ko au départ → **pas de fuite**. La montée vient de la mémoire libérée
que Go garde un moment avant de la rendre au système. `GOMEMLIMIT` (25 Mio) borne
ce comportement ; la mesure sur plusieurs jours sur srv-test (partie 8) le
confirmera.
À confirmer sur srv-test (partie 8), et avec journald + Docker (parties 3 et 5),
qui coûteront plus.

### Stack actuelle (référence, mesurée le 07/11 sur le VPS)

| Élément | Valeur |
|---|---|
| RAM stack obs (6 conteneurs) | ≈ 770 Mio (Grafana 291, VictoriaMetrics 248, VictoriaLogs 187, Alertmanager 24, vmalert 14 + 5) |
| Proxy de socket Docker | 17,6 Mio |
| Disque VictoriaMetrics (30 j) | 1,1 Go |
| Séries vues sur 30 j | 94 953 |
| Logs / jour | ≈ 1,40 million de lignes, ≈ 262 Mo de texte (Docker 61 %) |
| Disque du VPS | 38 Go, 17 utilisés, 22 libres |

Objectifs mirador en face : serveur < 256 Mio (≈ 3 fois moins), agent < 30 Mio
(Alloy : 80 à 150 Mo par machine).

## Mesures complémentaires (non bloquantes pour l'étape 2)

Utiles pour dimensionner l'étape 3. À lancer sur le VPS quand tu as le temps.

Taille de VictoriaLogs sur disque (`du` n'existe pas dans l'image, on mesure le
volume depuis l'hôte) :

```bash
sudo du -sh "$(docker volume inspect obs_vl-data --format '{{.Mountpoint}}')"
```

Séries **actives** (dernières données de moins de 5 min) :

```bash
curl -s http://172.30.0.10:8428/api/v1/query --data-urlencode 'query=count({__name__!=""})'
```

Les 15 métriques qui ont le plus de séries :

```bash
curl -s http://172.30.0.10:8428/api/v1/query --data-urlencode 'query=topk(15, count by (__name__) ({__name__!=""}))'
```

Séries actives par machine :

```bash
curl -s http://172.30.0.10:8428/api/v1/query --data-urlencode 'query=sort_desc(count by (instance) ({__name__!=""}))'
```

Les 15 conteneurs les plus bavards sur 24 h :

```bash
curl -s http://172.30.0.11:9428/select/logsql/query --data-urlencode 'query=_time:24h job:docker | stats by (host, container) count() as lignes, sum_len(_msg) as octets | sort by (lignes desc) | limit 15'
```

Si le volume ne s'appelle pas `obs_vl-data`, la liste des volumes :

```bash
docker volume ls
```

## Questions en attente (pour l'étape 2)

1. **Machine de test** : sur quelle machine tester `mirador-agent` en parallèle
   d'Alloy ? Idéalement une VM ou un VPS Debian/Ubuntu **avec Docker**, sans rôle
   critique.
2. **Noms en double** : si Alloy et mirador envoient les mêmes séries avec le même
   `instance`, VictoriaMetrics mélange les points (débits faux). Proposition : pendant
   le test, mirador envoie avec `instance` / `host` = `<nom>-mirador` (même `job`,
   `site`, `env`). Les dashboards le montrent comme une machine de plus, et la
   comparaison est directe. D'accord ?
3. **Alertes en double** pendant le test : les règles se déclencheront aussi pour
   `<nom>-mirador`. On les laisse partir (utile pour comparer), ou tu ajoutes une
   route Alertmanager qui les envoie nulle part (je te donne les lignes) ?
4. **Accès d'ingestion** : il faudra une ligne de token en plus dans la `map` nginx
   pour l'agent de test (comme pour une nouvelle machine). Je te donnerai la ligne
   et la commande de génération ; tu confirmes que ça te va ?

Rappel : tu vérifies les règles de logs qui tournent chez toi ; je mettrai la SPEC
à jour si tu en trouves.

## Deuxième revue de sécurité et d'architecture (11/11)

- Branche `claude/architecture-security-login-sont9e` ; détail : SPEC § 18.27.
- **Corrigé** : verrou de connexion contournable par une rafale de requêtes
  simultanées (40 mots de passe essayés au lieu de 5, mesuré). Un essai est
  maintenant réservé avant chaque vérification (mot de passe, code 2FA,
  actions sensibles). Tests : `rafale_test.go` ; `go vet` et
  `go test -race ./...` verts.
- Coût : une table en mémoire limitée aux requêtes en cours (quelques
  octets) ; pas de CPU ni d'I/O en plus.
- **Fait après validation** (SPEC § 18.28) : messages de sécurité à
  l'administrateur (code 2FA faux après un bon mot de passe, compte
  verrouillé, connexion depuis une nouvelle adresse ; 1 par heure et par
  sujet) ; nom filtré dans le journal. Tests : `prevenir_test.go`,
  `TestPrevenir`.
- **À essayer sur le VPS** : se connecter depuis une autre adresse, taper un
  code 2FA faux ; le message doit arriver sur le canal principal.
- **Reste fait le 12/11** (SPEC § 18.29) :
  - verrou progressif (15 min → 24 h), 3 sessions partielles par compte,
    mots de passe trop simples refusés ;
  - paquet `internal/server/auth` sorti de `console` (sans changement de
    comportement) ;
  - écoute ouverte refusée sans `MIRADOR_PROXY` ni TLS ; démarrage
    simplifié ; Dependabot (mensuel, regroupé).
- **Mesure** : boucle de la console = 52 µs par passage toutes les 5 s,
  18 Ko alloués, aucune écriture disque → relecture des réglages gardée
  telle quelle. Les autres changements n'ajoutent ni RAM, ni CPU, ni I/O
  mesurables (un entier par compteur de verrou).
- **Pas fait** : actions GitHub figées par empreinte (à faire à la main) ;
  réseau Docker dédié (étape 7).
- **À vérifier sur le VPS avant de mettre à jour** : `config/server.conf`
  contient bien `MIRADOR_PROXY=…` (sinon le serveur refuse de démarrer).
- Non lancé ici : `gosec` (pas installé) ; la CI le fera.

## Idées reprises de Beszel (12/11, SPEC § 19)

Demande : choisir ce qui est surveillé sur chaque machine (détection par
l'agent, proposition, validation ou non), et un design plus lisible.
Maquette validée, pas de vérification de port.

| Partie | État |
|---|---|
| Agent : réglages de surveillance, ventilateurs, ZFS, RAID logiciel, détection | **fait** (fichiers d'exemple ; aucun vrai capteur, pool ou grappe ici) |
| Serveur : choix dans la configuration typée, anciens agents protégés, 3 règles | **fait** |
| Page machine : bloc « À surveiller », bandeau | **fait** |
| Vue d'ensemble : barres, tri, filtre, colonnes, alertes, « à valider » | **fait** |
| Documentation (`docs/interface-web.md` § 10), SPEC § 19 | **fait** |

Mesures (machine de développement, 4 cœurs) :

| Mesure | Résultat | Objectif |
|---|---|---|
| Agent, détection seule | 0,13 µs par passage (60 s), 0 allocation | < 1 % d'un cœur ✅ |
| Agent, tout activé | 21 µs par passage, 20 allocations | idem ✅ |
| Agent, écritures disque | aucune | ✅ |
| Serveur, page machine et vue d'ensemble | 3 à 5 ms (5 machines) | — |
| Serveur, écritures disque | seulement à l'enregistrement d'un choix | ✅ |
| RAM de l'agent et du serveur | **non mesurée en conditions réelles** | à faire sur srv-test et le VPS |

Tests : `go vet` et `go test -race ./...` verts. `gosec` non lancé ici (pas
installé) : la CI le fera. Règles : 80 après import (77 avant, + 3).

À vérifier chez toi (commandes à coller, sorties à me renvoyer) :

```bash
# Sur un Proxmox : le fichier d'état des pools existe-t-il, lisible sans root ?
sudo -u mirador-agent sh -c 'for f in /proc/spl/kstat/zfs/*/state; do echo "$f : $(cat "$f")"; done'
# Ventilateurs et RAID logiciel vus par le noyau
ls /sys/class/hwmon/*/fan*_input 2>/dev/null; ls -d /sys/block/md*/md 2>/dev/null
```

Ajouté ensuite (12/11, SPEC § 19.8) : recherche rapide Ctrl+K, sous-titres
de 18 graphiques fournis, filtre des courbes, empreinte de la machine (token
lié à la machine, option éteinte par défaut, migration 0013). Essayé dans
Chromium sur le serveur de démonstration.

Ordre de mise à jour : **serveur d'abord**, puis les agents (un agent
récent marche avec un ancien serveur, mais sans ces réglages).

## Codes de secours et passkeys (12/11, SPEC § 20)

| Partie | État |
|---|---|
| Codes de secours : 10 codes à usage unique, connexion et nouvelle 2FA | **fait** |
| Passkeys : création, connexion (à la place du code 2FA), suppression | **fait** |
| Tests : unitaires, fuzzing, bout en bout, vrai navigateur (Chromium) | **fait** |
| Essai avec un vrai téléphone ou une vraie clé, derrière nginx | **à faire chez toi** |

- Aucune dépendance ajoutée ; migration 0012 ; `go vet` et
  `go test -race ./...` verts ; `gosec` à voir en CI.
- Coût : ~0,1 ms par connexion par passkey ; rien en mémoire hors un défi
  par session en cours.
- Réponse du 12/11 (« je veux les 2 ») : la passkey remplace le code 2FA
  après le mot de passe, **et** peut servir seule, sans mot de passe
  (l'appareil doit alors vérifier l'utilisateur ; réglage « Sécurité » pour
  l'interdire). Les deux essayés dans Chromium.
- À essayer sur le VPS : créer des codes de secours, créer une passkey
  depuis le téléphone, se déconnecter, se reconnecter des deux façons.

## Prochaine étape

- Étape 3 **validée le 10/11/2026** (pull request #4).
- En parallèle, sur le VPS (par l'utilisateur, `docs/installation-serveur.md`) :
  installation de mirador-server à côté de la stack actuelle, passage de
  `srv-test-mirador` sur mirador-server, mesure de 24 h.
- À décider : poser le tag `v0.1.0` (publication de l'image sur ghcr.io), ou
  construire l'image sur le VPS.
- **Étape 4 validée le 10/11/2026** (branche `claude/admiring-lamport-nqtigt`,
  pull request vers `main`).
- À l'installation de mirador-server sur le VPS : essais Olvid réels
  (`docs/olvid.md` § 2 et § 4).
- Sur le VPS, quand mirador-server y tourne : importer les règles
  (`docs/administration-serveur.md`, § Alertes), configurer Telegram en
  **mode ombre**, activer la comparaison avec vmalert, regarder
  `comparer-vmalert` après quelques jours.
- **Étape 5 faite** (branche `claude/admiring-wright-2n17eb`) : à essayer
  sur le VPS (`docs/interface-web.md`), puis validation ; questions ci-dessus.
- Ensuite : étape 6 (équipements sans agent, `mirador-collector`).
