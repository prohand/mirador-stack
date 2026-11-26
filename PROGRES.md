# PROGRES.md — état d'avancement de mirador-stack

## Où on en est

| Étape | État | Branche |
|---|---|---|
| 1. Cahier des charges | **Validée le 07/11/2026** — SPEC v1.0 (PR #1) | `etape-1-cahier-des-charges` |
| 2. Agent | **Validée et fusionnée le 09/11/2026** (PR #2 et #3) | `etape-2-agent` |
| 3. Serveur et stockage | **Validée le 10/11/2026** (PR #4, fusion à confirmer) | `etape-3-serveur` |
| 4. Alertes et notifications | **Validée le 10/11/2026** (PR à fusionner) | `claude/admiring-lamport-nqtigt` |
| 5. Interface web | **Validée le 13/11/2026** (parties 1 à 6 ; SPEC § 36 à 38 validés le 16/11) | `claude/admiring-wright-2n17eb` |
| 5 bis. Idées de Beszel : surveillance par machine, vue d'ensemble | **Validée le 12/11/2026** (SPEC § 19) | `claude/laughing-sagan-7w7xrm` |
| 5 ter. Codes de secours de la 2FA et passkeys | **Validée le 12/11/2026** (SPEC § 20) | `claude/laughing-sagan-7w7xrm` |
| 5 quater. Élément par élément, signal « à valider », Réglages rangés | **Validée le 12/11/2026** (SPEC § 21, retours corrigés : § 22) | `claude/affectionate-albattani-izl0lb` |
| 6. Équipements sans agent + `mirador-collector` | **Validée le 26/11/2026** : collecteur sur le LAN, agents sur tout le parc, mesures tenues | `claude/intelligent-cori-yh3n7c` |
| 7. Bascule | **Validée le 26/11/2026** : ancienne stack (Grafana, VictoriaMetrics, VictoriaLogs, vmalert, Alertmanager, Alloy) supprimée, mirador seul en service | `claude/intelligent-cori-yh3n7c` |

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

### Mesure réelle du serveur (VPS, 12 → 13/11, 18 h 40)

`docker stats` chaque minute (1120 relevés, une nuit complète avec la
sauvegarde de 02:43), 1 machine avec agent, mode ombre, comparaison vmalert
active. Deux redémarrages pour mise à jour (vers 10:24, puis avant 14:41).

| Mesure | Résultat | Objectif |
|---|---|---|
| RAM | 60 à 75 Mio au calme, **113 Mio au plus** (pointes toutes les ~2 h : rangement des métriques, puis mémoire rendue) | < 256 Mio ✅ (stack actuelle ~770 Mio : ~7 fois moins) |
| CPU | **1,27 % d'un cœur en moyenne**, 9,2 % au plus (pointe courte) | < 5 % ✅ |
| Écritures disque | ~100 Mo en 14 h 40, dont l'archive de sauvegarde (51 Mo) ; ~5,7 Mo/h hors sauvegarde | ✅ |
| Lectures disque | ~140 Mo en 14 h 40, surtout la sauvegarde (relit les versions de l'agent) | ⚠️ voir dessous |
| Données | métriques 14 Mo, logs 5 Mo, base 3 Mo, GéoIP 5 Mo | ✅ |
| **Versions de l'agent** | **166 Mo** (toutes les versions importées, amd64 + arm64) | ⚠️ |
| Archives de sauvegarde | 8 Ko → 19 Mo → 51 Mo en 3 jours (elles contiennent les versions) | ⚠️ 14 jours gardés : jusqu'à ~1 Go |

⚠️ Solutions proposées (par ordre d'effet) : (1) garder seulement les
versions utiles (cible, versions en service, 2 dernières), purge automatique
et bouton « Supprimer » ; les autres se réimportent depuis GitHub (signées) ;
(2) n'importer que les architectures présentes dans le parc (amd64 ici) ;
(3) la sauvegarde ne contient alors que ces versions-là. Effet attendu :
versions ~30 Mo, archive ~15 Mo par jour.

**Fait en 0.2.21 (SPEC § 22.6), mesuré le 13/11** : au démarrage, 16
binaires supprimés (toutes les arm64, versions 0.2.9 à 0.2.13) ; dossier
`versions` 166 → 55 Mo. Le reste part au fil des jours (versions importées
depuis moins de 24 h gardées).

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
Deuxième (12/11) : 0.2.10 → 0.2.11 de la même façon ; plus d'erreur
journalctl après le redémarrage (bug de reprise du journal corrigé).

**Mesure de 24 h de l'agent avec mirador-server (11-12/11, `mesurer.sh 24`,
machine de test, 2 cœurs, Alloy en parallèle)**

| Processus | RSS moy / max | Mémoire propre moy. | CPU moyen | Écrit en 24 h |
|---|---|---|---|---|
| mirador-agent | 21,0 / 23,5 Mo | 13,6 Mo | ≈ 0,09 % d'un cœur (systemd : 8,7 s en 2 h 49) | 32 Ko |
| journalctl (enfant de l'agent) | 52,5 / 100,7 Mo | 1,0 Mo (le reste : cache des fichiers du journal, partagé) | ≈ 0 | 0 |
| proxy Docker | 15,2 / 18,8 Mo | 9,1 Mo | ≈ 0 | 0 |
| **Alloy** (référence) | 154,2 / 157,7 Mo | 77,8 Mo | 0,75 % | 165 Mo |

- Objectif agent < 30 Mio : **atteint** (21 Mo, ~7 fois moins qu'Alloy) ;
  total côté mirador (agent + journalctl propre + proxy) ≈ 31 à 37 Mo.
- CPU : le chiffre du script (0,002 %) est faussé par 3 redémarrages de
  l'agent pendant la mesure (réinstallation, deux mises à jour) ; valeur
  retenue : celle de systemd. À remesurer sur 24 h sans mise à jour si
  besoin d'un chiffre précis.
- Comparaison avec vmalert sur 24 h : aucun écart (aucune alerte des deux
  côtés).

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
2. **Alertes en double** pendant le test : les règles se déclencheront aussi pour
   `<nom>-mirador`. On les laisse partir (utile pour comparer), ou tu ajoutes une
   route Alertmanager qui les envoie nulle part (je te donne les lignes) ?
3. **Accès d'ingestion** : il faudra une ligne de token en plus dans la `map` nginx
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

## Élément par élément, « à valider », Réglages (12/11, SPEC § 21)

| Partie | État |
|---|---|
| Agent : disques, interfaces, montages, certificats retirés un par un ; chemins de certificats depuis l'interface | **fait** |
| Assistant root : lecture des chemins venus du serveur, avec garde-fous (§ 21.3) | **fait, écart de sécurité validé le 12/11** (testé ici : lien, tube, clé privée, dossier interdit) |
| Serveur : capacité `elements`, anciens agents protégés | **fait** |
| Page machine : détail Système et Certificats | **fait** (vu dans Chromium, démonstration) |
| Signal « à valider » : menu, liste, en-tête | **fait** |
| Vue d'ensemble : tri État / À valider / Vu, filtre d'état | **fait** |
| Réglages : onglets et cartes depuis une seule liste | **fait** |

Mesures (SPEC § 21.4) : agent sans coût mesurable ; assistant root 0,3 ms
pour 20 certificats toutes les 15 min ; serveur 5,4 µs par machine pour le
bloc, compteur du menu calculé au plus une fois par minute. RAM réelle
toujours à mesurer sur srv-test et le VPS.

À vérifier chez toi après mise à jour (serveur d'abord, puis l'agent) :

```bash
# L'assistant root a-t-il lu les chemins ajoutés dans l'interface ? (erreurs éventuelles)
sudo cat /run/mirador-agent/root.json | python3 -m json.tool | grep -A3 -E '"erreurs"|"chemin"'
```

### Étape 6 : mesure de référence du collecteur actuel (13/11)

Alloy : 155 Mo résidents (179 Mo pour systemd), 0,86 % d'un cœur en
moyenne ; unpoller : 26 Mo. Référence ~180 Mo ; objectif de
`mirador-collector` : < 40 Mio (SPEC § 23.6).

Commandes utilisées (pour refaire la mesure plus tard) :

Sur le collecteur LAN (Alloy + unpoller), pour comparer avec
`mirador-collector` (SPEC § 23.3) :

```bash
systemctl show alloy -p MemoryCurrent -p CPUUsageNSec
ps -o rss=,etime=,cmd= -C alloy -C unpoller -C unifi-poller
free -m
```


## Modèles de surveillance (SPEC § 24, demande du 14/11, avant la suite de l'étape 6)

| Partie | État |
|---|---|
| A. Serveur, détection, interface (7 modèles fournis, machine et groupe, seuils par machine, règles importées remplacées, services attendus) | **fait**, testé (données simulées, interface) ; **validé le 16/11** |
| A bis. Plus simple (14/11) : mode administration 10 min, cases à cocher, application automatique, tableau modèles × groupes | **fait**, testé, captures du serveur de démonstration ; **validé le 16/11** |
| B. Catalogue de checks dans l'agent (HTTP, TCP, processus, fichier, dossier, DNS, ping) + 5 modèles (Serveur web, Sauvegarde, Ports TCP, Processus, Joignabilité) avec listes par groupe et par machine | **fait** (14/11), testé (serveurs et fichiers simulés, interface) ; **validé le 16/11** |
| C. Modèles personnels dans l'interface : créer, dupliquer un modèle livré, modifier, exporter / importer en JSON, supprimer (SPEC § 24.11) | **fait** (14/11), testé (base simulée, interface, rendu vérifié en capture) ; **validé le 16/11** |

Mesure partie C : 1 à 5 Ko par modèle personnel (200 au plus) gardés en
mémoire par le serveur, une requête de détection par minute et par modèle
qui en a une ; rien côté agent. Objectif serveur inchangé ✅.

Mesure partie B (banc d'essai, 40 checks) : 6,5 ms par passage toutes les
60 s (~0,01 % d'un cœur), 1,8 Mo de mémoire temporaire par passage, aucune
écriture disque. Objectif agent (< 30 Mio, < 1 % d'un cœur) : inchangé ✅.

## Design « A bis » et thème par utilisateur (SPEC § 25, 14/11)

| Partie | État |
|---|---|
| Maquettes : A épuré, B compact, C doux, D technique, A bis (menu noir) | faites ; choix : **A bis** |
| Thème Jour / Nuit / Auto par utilisateur, en bas du menu | **fait**, testé ; **validé le 16/11** |
| Design A bis (feuille de style, menu noir, pastille « mode ombre ») | **fait**, captures jour / nuit / menu replié ; **validé le 16/11** |

Mesure : une colonne en base, une requête par changement de thème, feuille
de style de même taille ; objectif serveur inchangé ✅.

## Aides allégées (SPEC § 26, 14/11, choix « A b d »)

| Partie | État |
|---|---|
| Maquettes A+B+D et C | faites ; choix : **A + B + D** |
| Bouton « Avec aides / Sans aides » par utilisateur | **fait**, testé ; **validé le 16/11** |
| 143 aides relues : 81 raccourcies (« En savoir plus » vers `docs/aide.md`), 3 retirées, avertissements gardés | **fait**, captures avant / après ; **validé le 16/11** |

Mesure : une colonne en base, une requête par clic ; objectif serveur
inchangé ✅.

## Retours du 14/11 au soir (SPEC § 27)

| Retour | État |
|---|---|
| Gestionnaire de mots de passe qui remplit « Mode administration » | **corrigé** (libellé et marques « à ignorer ») ; à vérifier chez toi |
| Mode administration perdu après déconnexion / reconnexion | **corrigé** : ouvert à la connexion |
| Modèles pas cochés sur les machines déjà en place | **corrigé** : cochés d'office (à enregistrer) + bouton « Les cocher sur ces machines » |
| Retirer un modèle doit aussi arrêter ses métriques | **fait** : décoché → alertes et relevés du modèle coupés pour la machine ; recoché → rallumés |
| Passkey Bitwarden sur iPhone | **confirmé** par toi le 15/11 ✅ |
| Règles manquantes dans les modèles | **fait** : 19 règles ajoutées (Docker, Linux, physique, certificats, ZFS, RAID), remplacent les règles importées sur les machines du modèle |
| Services suggérés absurdes (cochés d'office) | **corrigé** : actifs, propres au modèle, hors services de distribution, non cochés |
| Règles liées aux modèles (choix « 1. Oui, 2. auto ») | **fait** : règles sans seuil limitées au modèle ; Linux + pare-feu, trafic ; modèles NAS Synology, OPNsense, UniFi appliqués à la détection ; page Règles : retirer les règles importées qui ne servent plus |
| Règles de modèle gérées par le modèle seul | **fait** : remises à jour au démarrage (choix « désactivée » gardé) ; page Règles : désactiver seulement, modification et suppression renvoyées au modèle |
| Passkey Bitwarden sur iPhone : `NotAllowedError` | **corrigé** : demande « preferred », vérification de l'utilisateur contrôlée par le serveur |

## Pages Alertes et Audit (SPEC § 28, 15/11)

| Partie | État |
|---|---|
| Maquettes (recommandations) | validées (« Oui code ») |
| Alertes : tuiles, nouvelles, groupes, actions sur la ligne, mini-courbes, rechargement 30 s | **fait**, testé ; **validé le 16/11** |
| Audit : catégories, phrases, changements, jours, filtres, sensibles, CSV | **fait**, testé ; **validé le 16/11** |
| Historique : frise 24 h / 7 jours, règles les plus bruyantes | **fait**, testé ; **validé le 16/11** |
| Conditions « en attente » montrées comme des alertes | **corrigé** : bloc « En attente » à part, temps restant affiché |

Mesure : 30 requêtes de plage au plus par affichage de la page Alertes
(page ouverte seulement) ; objectif serveur inchangé ✅.

## Page d'une machine et graphiques (SPEC § 29, 15/11)

| Partie | État |
|---|---|
| Maquette (recommandations, ordre de la page) | validée |
| En ce moment, ordre, sommaire, périodes, 2 colonnes, vides regroupés, replis retenus | **fait**, testé ; **validé le 16/11** |
| Graphiques : légende française cliquable, aire, échelle, survol synchronisé, zoom, seuil du modèle | **fait** ; **validé le 16/11** |
| Accents des tableaux fournis | **fait** |

Mesure : 7 à 8 requêtes instantanées de plus par affichage d'une machine ;
objectif serveur inchangé ✅.

## Acquittement (SPEC § 30, 15/11)

| Partie | État |
|---|---|
| État « acquittée » lu en base (défaut : l'alerte semblait ne pas changer) | **corrigé** |
| Durée 4 h / 24 h / jusqu'à la fin, rappel à l'échéance | **fait**, testé ; **validé le 16/11** |
| Bloc « Prises en charge », par qui, Reprendre | **fait**, testé ; **validé le 16/11** |
| Escalade vers le canal de secours (coupée par défaut) | **fait**, testé (aiguillage) ; envoi réel à vérifier |

## Métriques et graphiques (SPEC § 31, 15/11)

| Partie | État |
|---|---|
| Livraison 1 : alertes et événements sur les courbes, âge des données, « plein dans X j » | **fait**, testé ; **validé le 16/11** |
| Livraison 2 : semaine dernière, plein écran, empilé, export, zoom dans l'adresse | **fait**, testé ; **validé le 16/11** |
| Livraison 3 : plus gourmands (conteneurs), carte de chaleur 24 h, écran mural | **fait**, testé ; **validé le 16/11** (processus non relevés par l'agent) |

Mesure (livraison 1) : prévision des disques **44 ms et 28 Mio passagers**
pour 50 systèmes de fichiers sur 7 jours, une fois par 10 min (cache) ;
annotations : 3 petites requêtes par page machine, gardées 30 s. Pas de
mémoire permanente notable (caches bornés à 500 et 100 entrées).
Livraison 2 : « −7 j » double la requête d'une courbe, seulement au clic ;
2 courbes empilées de plus par page machine ; le reste dans le navigateur.
Livraison 3 : carte de chaleur **46 ms et 26 Mio passagers** pour
50 machines, une fois par 5 min ; plus gourmands : 3 petites requêtes par
page machine.

## Certificats : réglage général et avancement (SPEC § 32, 15/11)

| Partie | État |
|---|---|
| Réglages › Certificats (compte DNS commun, liste des collecteurs, Renouveler) | **fait**, testé ; **validé le 16/11** |
| Avancement pas à pas (6 pas, étape Let's Encrypt en cours, écoute confirmée par les mesures) | **fait**, testé ; **validé le 16/11** |
| `--nom` dans la commande d'installation | **fait**, testé |

Coût : une écriture SQLite par étape (6 par certificat, tous les 60 jours).

## Page Logs refaite (SPEC § 33, 16/11)

| Partie | État |
|---|---|
| Filtres rangés, pastilles, niveau « et plus grave », exclusion (moteur) | **fait**, testé ; **validé le 16/11** |
| Histogramme + zoom, lignes compactes, surlignage, détail au clic | **fait**, testé ; **validé le 16/11** |
| Messages regroupés, contexte « Voir autour », mode direct, export, recherches enregistrées | **fait**, testé ; **validé le 16/11** |

Mesure : 44 ms et 4 Mio passagers pour analyser 2 000 lignes ; mode direct :
une petite recherche toutes les 5 s par onglet actif (30 min au plus).

## Audit du 16/11 : sécurité, graphique, téléphone, PWA (SPEC § 34)

Branche `claude/wonderful-goodall-aaqrgl`. Aucune faille critique ni haute.

| Partie | État |
|---|---|
| Connexion : verrou de compte (table des comptes à part), 10 défis passkey par adresse | **fait**, testé ; **validé le 16/11** |
| Console : codes 2FA manquants, import « securite », secrets liés à l'adresse, rôle avant le corps, masquage des secrets | **fait**, testé ; **validé le 16/11** |
| Machine muette : plus de `ServiceDuModeleAbsent`, alertes inhibées dans un bloc replié | **fait**, testé ; à vérifier sur le VPS |
| Graphique : logo de la connexion, 12 px au moins, accords, légende et aide des courbes | **fait**, vu dans Chromium ; **validé le 16/11** |
| Application installable : manifeste, icônes, encoche (sans service worker) | **fait** ; à essayer sur ton téléphone |
| Téléphone : barre du bas, en-tête compact, cartes, courbes à l'affichage | **fait**, vu en écran d'iPhone 13 simulé ; **validé le 16/11** |
| Connexion depuis un lien : page demandée gardée, page relais, passkey en premier | **fait**, vérifié dans Chromium ; **validé le 16/11** |
| Notifications push et pastille de l'icône (SPEC § 35, demande du 16/11) | **fait**, testé ; **validé le 16/11** |
| Option : page hors ligne | **non faite** (pas demandée) |

Mesures (Chromium, serveur de démonstration, machine de développement) :

| Mesure | Avant | Après | Objectif |
|---|---|---|---|
| Courbes demandées à l'ouverture d'une page machine (13 courbes), écran 1440 × 900 | 13 | **4** | moins de requêtes ✅ |
| Idem sur téléphone (avant défilement) | 13 | **0** | ✅ |
| Largeur de la page machine sur un écran de 390 px | 520 px (débord) | **390 px** | ✅ |
| Champs de saisie sur téléphone | 14 px, 36 px de haut | **16 px, 44 px** | ✅ |
| Mémoire du serveur | — | + 1 Mo au pire (table des noms inconnus, bornée) | < 256 Mio ✅ |
| Écritures disque | — | aucune en plus | ✅ |
| Binaire du serveur | — | + ~60 Ko (icônes, styles, scripts) | — |

Notifications push (SPEC § 35) : **0,15 ms et 17 Ko passagers par appareil
et par notification** (mesuré), une requête HTTPS sortante par appareil,
rien de permanent en mémoire, aucune dépendance ajoutée. Vérifié : vecteur
de la RFC 8291, déchiffrement par une autre bibliothèque, Chromium,
**envoi réel accepté par le service de push de Google**. Non vérifié ici :
l'arrivée sur un vrai téléphone et la pastille.

CI du 16/11 : `gosec` signalait une « redirection ouverte » sur le retour
à la page demandée (faux positif : chemin local vérifié) ; revérifié juste
avant la redirection et annoté. `gosec` relancé ici : 0 alerte.

RAM et CPU réels du serveur : **non remesurés** (rien n'est ajouté en
continu) ; à confirmer sur le VPS avec `docker stats` après la mise à jour.

Tests : `go vet`, `go test ./...` et `gosec` (0 alerte) verts ici.
`govulncheck` : la CI le fera.

À vérifier chez toi après la mise à jour du serveur :

- la page Alertes d'une machine muette ne montre plus que `AgentMuet` (les
  `ServiceDuModeleAbsent` en cours se ferment en ~5 min) ;
- installer l'application sur le téléphone, ouvrir un lien d'alerte reçu
  par Telegram ;
- désactiver une règle hors mode administration : la boîte demande le code.
- notifications : Réglages › Notifications › Application (Actif, Copie),
  puis « Mon compte » sur le téléphone › Activer › notification d'essai ;
  vérifier la pastille sur l'icône. Le serveur doit pouvoir sortir en HTTPS
  vers les services de push (`docs/aide.md`, « Notifications : application »).

Seconde passe (lecture ciblée, pas un audit complet) sur l'ingestion,
l'enrôlement, les mises à jour et l'assistant root : rien d'exploitable
trouvé ; un durcissement possible noté dans SPEC § 34.

## Page Machines plus claire (SPEC § 36, 16/11)

Branche `claude/vibrant-meitner-a0xe1m`. Choix : B (liste par site, filtres)
+ C (ajout guidé, page à part) + les deux pastilles.

| Partie | État |
|---|---|
| Liste par site / groupe / liste, filtres et recherche dans l'adresse, problèmes en premier, équipements sous leur collecteur | **fait**, testé ; **validé le 16/11** |
| « À approuver » : une ligne par machine, 5 puis « Voir les N autres », pastilles « nom à changer ? » et « adresse inconnue » | **fait**, testé ; **validé le 16/11** |
| Page `/machines/ajouter` en 4 étapes, bouton Copier, noms réservés | **fait**, testé ; **validé le 16/11** |
| Bug : `<body data-alertes>` cassait mirador.js (cartes téléphone, tri, Copier) | **corrigé**, test de non-régression |
| Vu dans Chromium (écran large et iPhone 13 simulé) | **fait** |

Mesures (banc de test, 200 machines sur 8 sites, 20 en attente, HTTPS
local compris) :

| Page | Temps | HTML | Mémoire allouée (libérée ensuite) |
|---|---|---|---|
| `/machines` (par site) | 8 ms | 65 Ko | 2,4 Mo |
| `/machines?vue=liste&filtre=problemes&q=m1` | 4 ms | 28 Ko | 1,1 Mo |
| `/machines/ajouter` | 4 ms | 18 Ko | 0,7 Mo |

Rien en base, rien en mémoire entre deux pages, une requête SQL en plus.
Négligeable devant les objectifs du serveur (SPEC § 2).

### Suite du 16/11 (SPEC § 37)

| Partie | État |
|---|---|
| « Le parc sur 24 h » : tri par pic, 10 lignes puis « Voir les N autres », 100 au plus avec « + N non affichées », suit les filtres du tableau | **fait**, testé, vu dans Chromium (30 machines) ; **validé le 16/11** |
| Import d'une version : liste des 5 dernières versions de GitHub (flux Atom, gardé 10 min), champ « ou autre » | **fait**, testé avec un faux GitHub ; à vérifier sur le VPS |
| Écran mural : bloc « Prises en charge », « Rien à traiter » au lieu de « Tout va bien » | **fait**, testé, vu dans Chromium ; **validé le 16/11** |

Coût : carte du parc, aucune requête en plus ; liste des versions, une
requête HTTPS vers github.com au plus toutes les 10 min (quelques dizaines
de Ko), seulement à l'ouverture de la page par un admin ; écran mural, rien
de plus (les alertes prises en charge étaient déjà lues).

### Réglages, Utilisateurs, Sécurité (SPEC § 38, 16/11)

| Partie | État |
|---|---|
| Raccourcis « Logs » et « Flux bloqués » retirés de Tableaux | **fait**, testé |
| Bloc « Général » rangé : mode en haut, adresse et fuseau dans Interface, dépendances dans Silences | **fait**, testé (formulaire renvoyé tel quel : rien n'est perdu) ; **validé le 16/11** |
| Notifications : cartes de canaux, résumé, barre d'enregistrement en bas | **fait**, testé, vu dans Chromium (écran large et téléphone) ; **validé le 16/11** |
| Accueil des Réglages : état du moment sur chaque carte | **fait**, testé, vu ; **validé le 16/11** |
| Utilisateurs : pastilles, boutons par action, création sur une ligne | **fait**, testé, vu ; **validé le 16/11** |
| Sécurité : bilan, 3 cartes, durées avec unité, « Fermer toutes les autres » | **fait**, testé, vu ; **validé le 16/11** |

Coût : affichage seulement (quelques lectures en base à l'ouverture des
pages) ; rien de collecté ni gardé en mémoire.

## Idées reprises de New Relic (18/11)

Demande du 18/11 : idées 1 à 6 à faire, maquettes validées ; idée 7
(motifs de logs) à mesurer avant de décider.

### Idée 1 — Si les données disparaissent (SPEC § 46) : fait, à valider

- Réglage par règle : garder (défaut), fermer, ouvrir ; pastille
  « Données absentes depuis… » ; ligne dans le message.
- Tests : `TestAbsenceDeDonnees` (garder 10 min à 3 h puis retour, fermer,
  machine supprimée, ouvrir après le délai sans doublon, machine qui
  répond sans alerte).
- Coût : aucune requête de plus sauf pour les règles « ouvrir ».

### Idée 2 — Suivi des changements (SPEC § 47) : fait, à valider

- Agent : paquets installés lus dans dpkg.log (`apt_package_installed`),
  image des conteneurs (`node_docker_container_info`) ; liste « à mettre
  à jour » recalculée 2 min après une installation. **Agent à mettre à
  jour** pour les paquets et les images.
- Serveur : comparaison chaque minute (paquets, redémarrage + noyau,
  agent, conteneur recréé ou redémarré à la main), table `changements`
  (90 jours, migration 0027).
- Fiche machine : bloc « Changements » (7 jours, audit compris) ; traits
  sur les courbes ; ligne « 🔧 12m avant : … » dans le message d'alerte.
- Tests : `TestJournalDpkg` (lecture, ajout, rotation, 48 h),
  `TestDetecteur` (premier tour muet, nuit de mises à jour complète,
  plantage ignoré, regroupement des paquets, redémarrage du serveur sans
  doublon), `TestChangementEtAbsenceDansLeMessage`.
- Coût : agent, un stat() par minute ; serveur, 7 requêtes instantanées
  par minute.

### Idée 3 — Sondes de services (SPEC § 48) : fait, à valider

- HTTP(S), TCP, ping, DNS ; depuis un collecteur (LAN) ou le serveur
  (extérieur, adresses privées refusées) ; page Supervision › Sondes ;
  règles SondeEnEchec et SondeLente.
- **Collecteur à mettre à jour** pour les sondes du LAN.
- Tests : `internal/sondes` (validation, HTTP code/contenu/lente/refus,
  TCP, DNS avec un faux serveur, ping sauté ici faute de droit),
  `TestSondesCollecteur` (liste, part du collecteur, collecteur ancien,
  audit), `TestLotSonde`, `TestPageSondes`.
- Coût : un essai par sonde et par minute ; 4 à 5 séries par sonde.

### Idée 4 — Disponibilité (SPEC § 49) : fait, à valider

- Alertes › Disponibilité : machines et sondes, 30 j / 90 j / ce mois-ci,
  objectifs réglables (99,5 % par défaut), maintenances retirées.
- Tests : `TestIntervallesDispo`, `TestPageDisponibilite` (panne de 2 h
  dont 1 h en maintenance, objectif 99,9 % dépassé de 16 min).
- Coût : aucune nouvelle donnée ; deux requêtes SQL à l'ouverture.

### Idée 6 — Services (SPEC § 50) : fait, à valider

- Supervision › Services : regroupements machines / conteneurs / sondes,
  éléments clés, état, disponibilité 30 j ; série
  `mirador_service_etat` et règles ServiceEnPanne / ServiceDegrade.
- Tests : `TestLignes`, `TestCalculer` (5 services : panne, dégradé, OK,
  élément inconnu), `TestPageServices` (création, état, renommage,
  refus, suppression). Le test a trouvé un défaut de format des
  pourcentages (« 100 » affiché « 10 ») : corrigé, aussi sur la page
  Disponibilité.
- Coût : 5 requêtes par minute au plus, une série par service.

### Idée 5 — Ruche (SPEC § 51) : fait, à valider

- Onglet Résumé / Ruche de la vue d'ensemble (choix B du 18/11), choix
  retenu par le navigateur ; filtres Machines, Conteneurs, Sondes, Tout.
- Tests : `TestRuche` (résumé par défaut, ruche des machines et des
  conteneurs, fragment « tout »), vérifié dans Chromium (ordinateur et
  téléphone, onglet retenu).

### Idée 7 — Motifs de logs (SPEC § 52) : fait, à valider

- Onglet « Messages regroupés » amélioré : mots qui changent fusionnés
  (MOT), colonnes Évolution et Vu la 1re fois, « nouveau », tri
  « Nouveaux d'abord » ; registre des motifs alimenté à l'ingestion.
- Mesure dans le serveur : 7 µs par ligne (25 µs avant optimisation),
  ≈ 0,01 % d'un cœur au débit actuel ; 4 Mo au plus.
- Tests : `TestRegroupeur`, `TestRegistre` (relu après redémarrage,
  nouveau, apprentissage), `TestRegistrePlafond`, `TestRegrouper`,
  `TestRegrouperMotifs`.
- Mesure d'avant la décision (prototype) :

- Regroupement façon Drain, écrit pour la mesure : nombres, adresses et
  identifiants remplacés par ‹n›, mots qui changent par ‹*›.
- Jeu réaliste d'une journée (1,4 million de lignes, profil du § 1 :
  traefik, Home Assistant, zigbee2mqtt, Nextcloud, journal, sshd, dnsmasq…) :
  - **1,2 µs par ligne** (1,7 s pour la journée entière) ;
  - 33 motifs, lisibles (« Invalid user ‹*› from ‹n› port ‹n› »,
    « DHCPACK(eth‹n›) ‹n› ‹n› ‹*› ») ;
  - pire cas (lignes toutes différentes, 5 000 motifs, éviction
    continue) : 0,8 à 3,9 µs par ligne.
- Au débit actuel (16 lignes/s) : **≈ 0,002 % d'un cœur** ; à 500 lignes/s :
  0,06 % (0,2 % au pire).
- Mémoire : **4,2 Mo** au plus (5 000 motifs, plafond), 192 octets
  passagers par ligne.
- Disque : compteurs par heure gardés en base toutes les 5 min (quelques
  centaines de lignes), négligeable.
- Conclusion : coût faible, **faisable**. Décision à prendre par
  l'utilisateur.

## Tâches : prochaine exécution et « Voir l'erreur » (SPEC § 92, § 93, 20/11) : fait, à valider

- Colonne « Prochaine » (annoncée ou estimée).
- Bouton « Voir l'erreur » : journal du service ou du conteneur autour de
  l'échec, erreurs des sauvegardes Proxmox, message des signaux curl.
- À faire chez l'utilisateur : agent à jour sur les nœuds Proxmox
  (assistant root format 6) pour les lignes « ERROR » des sauvegardes.

## Sondes allégées, tâches : nom affiché et détail Proxmox (SPEC § 89, 20/11) : fait, à valider

- Sondes : colonne « Cible » en pastilles, jours restants du certificat.
- Tâches : nom affiché ; sauvegardes vzdump : VM (OK / échec, durée,
  taille), stockage, planning Proxmox, « Échec partiel ».
- Mesures : un journal vzdump (256 Kio au plus) lu par passage de
  l'assistant root ; ~5 séries par VM sauvegardée.
- À faire chez l'utilisateur : mettre l'agent à jour sur les nœuds Proxmox
  (assistant root format 5).
- Corrigé aussi (§ 90) : « Context canceled » sur les tableaux.

## Tâches planifiées : description et page machine (SPEC § 88, 20/11) : fait, à valider

- Description en clair de chaque tâche (vzdump, PBS, timers…) ; résumé
  dans l'onglet Vue d'ensemble de la machine, pastille sur Activité.

## Sondes : machine liée visible (20/11, retour) : fait, à valider

- Colonne « Machine » dans la liste des sondes ; machine en haut de la
  fiche de la sonde et dans ses liens ; champ « Machine liée » à l'ajout
  manuel et dans « Modifier la sonde » (on peut la changer ou la retirer) ;
  « Essai fait depuis » (serveur ou collecteur) en haut de la fiche.

## Téléphone : barre du haut à la iOS (SPEC § 86, 20/11) : fait, à valider

- Grand titre en haut de page, verre dépoli et titre centré au défilement,
  barre d'état et menu « Plus » aux couleurs de la page ; ordinateur inchangé.

## Téléphone « Verre noir » (SPEC § 85, 20/11) : fait, à valider

- En-tête noir qui reste en haut, barre du bas flottante et arrondie,
  « Accueil » au lieu de « Vue d'ensemble » ; ordinateur inchangé.

## Page de statut publique (SPEC § 84, 20/11) : fait, à valider

- Réglages › Interface : adresse secrète `/statut/<jeton>`, applications
  choisies, 90 jours de disponibilité, incidents ; sans compte, sans
  script ni cookie ; jamais de machine, d'IP ni de log.
- Mesures : rien tant que la page n'est pas ouverte ; une lecture SQL et
  une requête de métriques par affichage.
- Non fait : note d'incident publiée à la main.

## Vue d'ensemble réorganisable (SPEC § 83, 20/11) : fait, à valider

- « Modifier la vue » : déplacer ou masquer les tuiles et les blocs ; une
  disposition pour l'ordinateur, une pour le téléphone, avec le compte.
- Mesures : rien de mesurable en plus (une lecture du compte déjà chargé).

## Modèles des messages (SPEC § 82, 20/11) : fait, à valider

- Réglages › Notifications › Modèles : titre et texte par type de message
  et par canal, aperçu par canal, essai, retour au défaut.
- Messages par défaut : libellés lisibles des règles (« CPU élevé »).
- Mesures : rien en plus sans modèle ; avec modèle, 27 µs et 17 Kio
  alloués par message envoyé (banc d'essai).
- Non fait : e-mail en HTML.

## Petits ajouts repris de Checkmate (SPEC § 81, 20/11) : fait, à valider

- Maintenances : chaque jour / semaine / mois, prochaine fenêtre affichée,
  cible `site:NOM` (tout un site, équipements du collecteur compris).
- Sondes HTTP : champ JSON attendu ; alerte N jours avant l'expiration du
  certificat du site.
- Canal Webhook : JSON mirador, Discord, Gotify, Matrix, Slack / Mattermost.
- Tâches planifiées : entrée de menu à part + bloc sur la page machine.
- Mesures : rien de mesurable en plus (une requête SQL au rechargement de
  la sourdine ; JSON décodé seulement si demandé ; un appel HTTP par
  message webhook).

## Tâches planifiées (SPEC § 80, 20/11) : fait, à valider

- Page Sondes › Tâches planifiées : l'agent voit seul les timers systemd,
  les conteneurs Docker ponctuels et les sauvegardes Proxmox VE / PBS ;
  le reste se signale par `curl` sur une adresse secrète.
- Alertes « en échec » et « en retard », avertissement ou critique.
- Mesures : 1,3 ms par lecture du journal Proxmox (toutes les 15 min) ;
  2 appels D-Bus par timer toutes les 5 min ; négligeable côté serveur.
- Sur Proxmox, l'assistant root passe au format 4 : il se met à jour avec
  l'agent (mises à jour automatiques) ou par la commande habituelle.

## Menu réorganisable (SPEC § 79, 20/11) : fait, à valider

- « Modifier le menu » : ordre, entrées masquées, 4 raccourcis du bas ;
  un menu pour l'ordinateur, un pour le téléphone, gardés avec le compte.

## Appareils de confiance (SPEC § 78, 20/11) : fait, à valider

- « Rester connecté sur cet appareil » (coché d'office en PWA) : 30 j au
  plus, 7 j sans usage, survit aux redémarrages ; liste et « Oublier » dans
  Sécurité ; actions sensibles toujours avec la 2FA.

## Logs plus lisibles (SPEC § 77, 20/11) : fait, à valider

- Trait et pastille pour erreurs et avertissements, machine + origine,
  IP et mots d'échec colorés, lignes semblables repliées, filtres d'un clic.

## Courbe d'une alerte et températures de la machine (SPEC § 76, 19/11) : fait, à valider

- Page d'une alerte : vraie courbe (axes, °C, seuil du modèle en pointillé).
- Page machine : rangée « Températures » (capteurs, disques SMART).

## Six améliorations d'affichage (SPEC § 75, 19/11) : fait, à valider

- Courbes vides, onglets sur une ligne, aides repliées, audit lisible,
  zones tactiles, menu en icônes sur tablette.

## Audit d'affichage de toute l'interface (SPEC § 74, 19/11) : fait, à valider

- 300 captures (ordinateur, tablette, téléphone PWA, clair et sombre) :
  13 pages plus larges que l'écran avant, aucune après.
- Tableaux sur tablette et téléphone, frise des incidents, thème sombre,
  libellés bruts : corrigés.

## Noms d'alertes lisibles ; CI plus rapide (SPEC § 73, 19/11) : fait, à valider

- « MiseAJourAgentEchouee » s'affiche « Mise à jour de l'agent échouée »
  (nom technique en infobulle). Coût : quelques µs par ligne affichée.
- CI : fuzzing en 2 tâches parallèles, ~6 min 45 → ~4 min.

## Dernières alertes de chaque élément (SPEC § 72, 19/11) : fait, à valider

- Machines (onglet Activité), sondes, applications, conteneurs : 10
  dernières alertes avec le retour à la normale.
- Colonne « Mises à jour » : se cache maintenant quand on la décoche.

## Colonnes Site et Type du parc (SPEC § 71, 19/11) : fait, à valider

- Triables, dans « Colonnes » ; montrées d'office s'il y a plusieurs sites
  ou plusieurs types.

## Ajouter une sonde depuis la page Sondes ; majuscules (SPEC § 69, 19/11) : fait, à valider

- « + Ajouter une sonde » en haut : « Depuis une machine » (même bloc que la
  page machine) ou « À la main ».
- Majuscules : pastilles et sortes d'éléments.
- Couleurs de la page Sondes (SPEC § 70) : tuiles comme la page Alertes,
  pastilles de type, trait par ligne, disponibilité colorée.

## Liens des fiches filtrés sur l'élément (SPEC § 68, 19/11) : fait, à valider

- Fiches sonde, application, conteneur : Alertes, Incidents, Événements,
  Disponibilité, page de la liste… filtrés sur l'élément, bandeau
  « Filtré sur … · tout voir ».

## Ping refusé sur le collecteur, code HTTP affiché (SPEC § 67, 19/11) : fait, à valider

- « ping impossible sans droits » : plage `net.ipv4.ping_group_range` vide
  sur le collecteur (LXC). Le script d'installation l'élargit au groupe du
  collecteur et l'unité la réapplique au démarrage. Collecteur déjà
  installé : commandes données à l'utilisateur.
- Sondes HTTP(S) : « OK · 200 · 5 ms ».

## Page d'une machine en onglets (SPEC § 66, 19/11) : fait, à valider

- Vue d'ensemble · Activité · Sondes et liens · Réglages (Collecte pour un
  collecteur) ; alertes toujours en haut ; pastilles sur les onglets ;
  onglet retenu et dans l'adresse. Vérifié dans Chromium (ordinateur et
  téléphone), sans erreur JS.

## « Services » renommés « Applications » (SPEC § 65, 19/11) : fait, à valider

- Menu, page, fiches, Ctrl+K, incidents ; anciennes adresses redirigées.
- Règles ApplicationEnPanne / ApplicationDegradee (renommées au démarrage du
  serveur, réglages gardés).
- À vérifier après mise à jour du serveur : Alertes › Règles, plus de
  ServiceEnPanne, ApplicationEnPanne avec la note « § 65 ».

## Modifier et renommer une sonde (SPEC § 64, 19/11) : fait, à valider

- Fiche de la sonde › « Modifier la sonde » (lien « Modifier » sur la page
  Sondes et dans le bloc Sondes de la machine) : cible HTTPS vers le nom du
  site, renommage (suivi par les services et les étiquettes).
- Bloc Sondes de la machine : décocher une sonde ou un ping déjà créé puis
  « Enregistrer » le retire.

## Code 2FA de la boîte « Action protégée » et Bitwarden (19/11) : corrigé, à valider

- Bitwarden ne proposait pas le code dans la boîte (champ sans nom ni id,
  contrairement à la page de connexion). Champ maintenant `id="totp-boite"`,
  `name="totp"`, `autocomplete="one-time-code"`, et compte `name="username"`.
- À vérifier : clic sur une action protégée, l'icône Bitwarden propose le
  code dans le champ.

## Bloc « Changements » compact (SPEC § 63, 19/11) : fait, à valider

- 5 lignes montrées, le reste replié ; identiques du même jour regroupés
  (×N) ; détail long coupé ; une ligne d'explication.

## « cet appareil » affiché sur tous les appareils (19/11) : corrigé, à valider

- Mon compte › Notifications : la pastille « cet appareil » s'affichait sur
  l'iPhone en étant sur le PC. Cause : `.pastille { display: inline-flex }`
  l'emportait sur l'attribut `hidden`. Correction : `[hidden] { display:
  none !important; }` pour toute la console (vérifié dans Chromium).

## Adresses à pinguer, plusieurs pings, battement immédiat (SPEC § 62, 19/11) : fait, à valider

- Adresse à pinguer vide : l'agent ne pouvait pas lire ses interfaces
  (netlink interdit par le service durci). Lues maintenant dans `/proc`.
- Page machine › Sondes › Ping : adresses de l'agent en cases à cocher
  (route par défaut en premier), « Autres adresses », un ping par adresse.
- La mise à jour forcée marchait (proxmoxbackup en 0.3.8 en 10 s) ; la
  page tardait : le serveur écrit maintenant tout de suite un battement qui
  change quelque chose (nouvelle version, redémarrage).
- À faire chez l'utilisateur : nouvelle version de l'agent (tag) pour les
  adresses ; serveur pour le reste.

## Alerte « mise à jour échouée » sur un HTTP 502 (SPEC § 61, 19/11) : corrigé, à valider

- Vu sur proxmoxbackup (déjà en 0.3.7) : la vérification de version a reçu
  un 502 du proxy (serveur indisponible un instant), signalée à tort comme
  un échec de mise à jour. L'alerte se ferme seule au prochain essai
  (15 min).
- Corrigé : l'agent ne signale une vérification impossible qu'après 6 h ;
  la règle attend 30 min (règle déjà en base corrigée au démarrage).
- Vérification : après mise à jour du serveur, page Alertes › Règles ›
  MiseAJourAgentEchouee : « pendant 30m » et la note « § 61 ».

## Sondes depuis la page machine (SPEC § 60, 19/11) : fait, à valider

- Agent : adresses IP et ports ouverts (lus dans `/proc/net/tcp`), protocole
  détecté une fois par port (HTTPS, HTTP, SSH, TCP).
- Page machine › **Sondes** : « Activer le ping » depuis le collecteur du
  site (ou le site choisi si la machine n'en a pas), sondes proposées
  cochées d'office (HTTPS, HTTP, SSH), ajoutées en un clic.
- Mesures : agent 0,28 ms et 300 Kio passagers toutes les 5 min (40 ports,
  2 000 connexions) ; serveur : 2 requêtes de plus à l'ouverture de la page
  machine. Objectifs tenus.
- À faire chez l'utilisateur : mettre à jour les agents (adresses et ports)
  et le serveur ; les collecteurs n'ont pas besoin de changer.
- Vérification proposée : page d'une machine du site du collecteur ›
  Sondes : le collecteur est choisi d'office, les ports apparaissent
  quelques minutes après la mise à jour de l'agent.

## Design et architecture repris de New Relic (18/11, SPEC § 53 à 59)

Demande du 18/11 : « fais tout ». Ordre : journal d'événements, incident,
registre des éléments, fiches, période commune, Ctrl+K, étiquettes.

- § 53 Journal d'événements : fait (`TestEvenements`).
- § 54 Incidents : fait (`TestIncidents` : regroupement par service et par
  site, alerte isolée, frise avec le changement d'avant, note, lien depuis
  l'alerte, note dans le journal).
- § 55 Registre des éléments, § 56 fiches, § 59 étiquettes : faits
  (`TestElementsFichesEtiquettes`, `TestRuche` qui passe par le registre).
- § 57 Période commune et § 58 Ctrl+K : faits, vérifiés dans Chromium
  (période reprise par Logs et une autre machine, case « Comparer »
  retenue, favori, récents, Entrée sur une sonde → sa fiche ; aucune
  erreur JavaScript).

## AgentMuet au-delà de 24 h (SPEC § 45, 18/11)

- Défaut : la règle se fermait seule après 24 h sans données (`lag(...[1d])`).
- Correction : série interne `mirador_machine_derniere_metrique_timestamp_seconds`
  et expression en deux parties ; règle en base corrigée au démarrage.
- Tests : `TestAgentMuetAuDelaDe24h` (2 min à 70 h de silence : alerte
  continue, mêmes étiquettes ; machine qui répond : rien),
  `TestCorrigerRegles`, contrôle de bout en bout. `go test ./internal/...` OK.
- Mesures :
  - évaluation 3,3 à 3,6 ms contre 3,8 à 4,0 ms avant (20 machines) ;
  - une série et un point par minute par machine ; RAM : une entrée de
    16 octets par machine.

## Coupure d'internet du site (16/11, SPEC § 39 et § 40)

Coupure d'internet chez l'utilisateur du **16/11 10:30 au 18/11 10:11**
(environ 47 h 40, heure du retour donnée par l'utilisateur le 18/11).
La page Coupures affiche une fin au 17/11 10:30 (23 h 59) : fausse, due au
défaut d'AgentMuet (fermée seule 24 h après la dernière donnée, SPEC § 45).
Le bilan (0 % de relevés reçus, file vide) a donc été lu trop tôt.
L'historique de cette coupure reste faux ; les suivantes seront justes.
`ProxmoxMuet` absent chez l'utilisateur : rien à désactiver.

| Partie | État |
|---|---|
| Un seul message « Site X injoignable » quand toutes les machines d'un site se taisent, un seul au retour ; bandeau sur l'accueil et Alertes | **fait**, testé (simulation : 3 machines, retour échelonné) ; à valider |
| Page Alertes › Coupures : coupures retrouvées dans l'historique, bilan par machine (relevés rattrapés, file d'attente, lots jetés, autonomie du tampon) | **fait**, testé (métriques simulées) ; à valider sur la coupure du 16/11 |
| Guide de l'étape 7 (`docs/bascule-finale.md`) | **écrit** ; à valider |
| SPEC § 18.27 : titre remis à jour (propositions traitées) | **fait** |

Mesures (banc d'essai) :

| Quoi | Mesure | Objectif serveur (SPEC § 7.1) |
|---|---|---|
| Calcul « site injoignable » (toutes les 5 s) | 57 µs pour 300 machines toutes en panne, 53 Ko alloués puis libérés (~0,001 % d'un cœur) | négligeable ✅ |
| Page des coupures | 3 à 9 ms, 18 Ko de HTML (2 machines, 5 h de relevés) ; 7 requêtes par machine, seulement à l'ouverture | négligeable ✅ |
| Base, disque | rien de nouveau en base ni sur disque ; une requête SQL de plus par minute | ✅ |

Au retour d'internet (à coller dans une nouvelle session) :
- heure de retour ;
- copie (ou capture) de Alertes › Coupures après la mise à jour du
  serveur : la coupure du 16/11 y apparaît (si les machines ont leur site) ;
- si la coupure dépasse ~19 h, des lots auront été jetés : la page le dira.

Vu sur la capture de la comparaison vmalert (SPEC § 39.3) : `ProxmoxMuet`
manquante chez mirador est normale (mirador ne reçoit pas le metric
server) : à désactiver dans Règles.

## Réessayer / forcer une mise à jour (SPEC § 44, 18/11)

| Partie | État |
|---|---|
| Fiche machine : bloc « Mise à jour » (raison de l'échec, bouton Réessayer / Mettre à jour maintenant), lien depuis la liste | **fait**, testé, vu dans Chromium ; à valider |
| Serveur : fenêtre ignorée 24 h, agent réveillé, `reessai` envoyé seulement aux agents qui le comprennent | **fait**, testé |
| Agent : capacité `reessai`, échec oublié si la demande est plus récente, vérification réveillée par l'actualisation | **fait**, testé ; à valider après mise à jour des agents |
| Conteneurs : « — » et légende au lieu de « pas de test » | **fait**, vu |

Pour proxmoxbackup (agent 0.3.1, trop ancien pour `reessai`) : une fois
sur la machine `sudo rm /var/lib/mirador-agent/bin/echec.json`, puis
« Réessayer la mise à jour » sur sa fiche.

Coût : une colonne en base, une lecture par demande de mise à jour de
l'agent (toutes les 15 min par machine) ; rien d'autre.

## Pages de connexion refaites (SPEC § 43, 17/11)

| Partie | État |
|---|---|
| Style « Nuit » (choix A parmi 4 maquettes) : carte centrée sur fond bleu nuit, toujours sombre | **fait**, vu dans Chromium (écran large, iPhone 13) ; à valider |
| Code 2FA, code de secours, création de la 2FA, premier administrateur : même cadre | **fait**, connexion complète testée (mot de passe faux, puis bon mot de passe et code) ; à valider |

Coût : CSS et SVG en ligne seulement, aucun script ni image.

## Conteneurs, Services, Paquets, détail au clic (SPEC § 42, 17/11)

Idées de Beszel, maquettes validées le 17/11 ; « fais tout et le détail au clic ».

| Partie | État |
|---|---|
| Page Conteneurs (menu, compteur, Ctrl K, lien depuis la machine, filtres, tri, Logs) | **fait**, testé, vu dans Chromium (écran large et téléphone) ; à valider |
| Bloc Services (replié, ouvert d'office si échec, onglets, Logs) | **fait**, testé, vu ; à valider |
| Bloc Paquets (agent : liste de 200 au plus ; serveur : bloc replié) | **fait**, testé, vu ; à valider après mise à jour des agents |
| Panneau de détail (CPU par cœur, mémoire, disques, réseau) | **fait**, testé, vu (Échap, onglets, téléphone) ; à valider |
| Même flèche › / ⌄ sur tous les blocs repliables | **fait** |

Mesures (banc, 10 machines, 200 conteneurs) : page Conteneurs 86 ms et
4,3 Mo temporaires ; page machine 26 ms ; panneau CPU 5 ms. Agent : une
série de plus par conteneur en marche, une par paquet en attente (200 au
plus). Négligeable devant les objectifs ✅.

À faire de ton côté : mettre à jour serveur **et agents** (liste des
paquets, « Depuis » des conteneurs).

## Derniers logs dans les messages (SPEC § 41, 17/11)

Idée reprise de Beszel (idée seulement). Choix du 17/11 : « fais le 1 uniquement ».

| Partie | État |
|---|---|
| Service en échec, conteneur, OOM du noyau : 5 dernières lignes de logs dans le message (alertes nouvelles) | **fait**, testé (faux stockage et vrai stockage des logs) ; à valider |
| Case dans Réglages › Notifications (cochée par défaut) | **fait**, testé |

Mesure (banc, 24 h de logs, 200 000 lignes, 10 machines) : **1,5 ms** et
1,5 Mo de mémoire temporaire par recherche ; au plus 3 recherches par
message, abandonnées après 3 s. Rien en base. Négligeable devant les
objectifs du serveur ✅.

## Étape 6 — avancement (SPEC § 23)

Étape 5 **validée le 13/11**.

| Partie | État |
|---|---|
| 1a. Proxmox dans l'agent : invités lus dans les cgroups, liste et noms par l'assistant root ; disques des VM (volumes ZFS/LVM, ou assistant root au rythme réglé pour les qcow2) | **fait**, chemins vérifiés sur un nœud Proxmox 9.2 ; agent à installer sur le nœud |
| 1b. Tableau « Proxmox VE » et 3 règles fournies | **fait** ; 13/11 : agent 0.2.23 sur un nœud Proxmox 9.2, assistant au format 3, invités nommés dans le tableau ; disque des VM qcow2 et mesure 24 h à confirmer |
| 2. Entrée InfluxDB (OPNsense) | **fait** (14/11), testé + fuzzing ; à brancher sur OPNsense (`docs/administration-serveur.md`) |
| 3. `mirador-collector` : squelette, enrôlement (type au jeton), configuration, mise à jour signée par produit, équipements rattachés et contrôlés à l'arrivée, script d'installation | **fait** (14/11), testé (simulations, interface) ; à installer sur le LAN |
| 4. Syslog TLS (adresse privée, émetteurs filtrés avant le TLS, RFC 5425/5424/3164, fuzzing) + certificat ACME (Let's Encrypt, défi DNS fait par le serveur chez deSEC, Cloudflare, OVHcloud, Gandi, Infomaniak ou CSC, renouvellement, « Renouveler maintenant ») | **fait** (14/11), testé (TLS réel, faux ACME et faux fournisseurs DNS, interface) ; à brancher sur le LAN |
| 5. SNMP v3 (authPriv, OID figés, noms de snmp_exporter) et sondes de certificats ; mots de passe chiffrés en base | **fait** (14/11), testé (NAS simulé, TLS réel, interface) ; OID à vérifier sur le NAS |
| 6. Client UniFi natif (compte local ou clé API, certificat vérifié ou épinglé, noms d'unpoller) | **fait** (14/11), testé (contrôleur simulé : UniFi OS, logiciel, clé API) ; à brancher |
| 7. Mesures sur le LAN, comparaison avec Alloy, retrait d'Alloy et d'unpoller **sur validation** | à faire avec toi (`docs/bascule-collecteur.md`) |

Première mesure sur le nœud Proxmox (13/11, 37 min après le démarrage, à
refaire sur 24 h) : agent **21 Mo** résidents (14 Mo de mémoire propre ; les
137 Mo de plus vus par systemd sont du cache de fichiers du journal, rendu
au besoin). Sur ce nœud, Alloy + smartctl_exporter : ~161 Mo (~7 fois plus).
Disques des VM qcow2 : lecture de /proc/<pid>/io refusée (groupe de
l'assistant) → 0.2.24 (groupe root) pas suffisant : QEMU protégé par le
noyau ; 0.2.25 : `CAP_SYS_PTRACE` pour l'assistant (choix du 13/11), script
relancé : **disques des VM qcow2 mesurés** (vérifié le 13/11).

Mesure partie 6 : 16 µs par site UniFi lu (hors réseau), 5 requêtes HTTPS
par minute ; unpoller (26 Mo) pourra être retiré.

Mesure partie 5 : 16 µs par relevé Synology (hors réseau), binaire du
collecteur 8,8 Mo.

À vérifier sur le NAS (lecture seule, depuis le collecteur, utilisateur SNMP
v3 créé dans DSM) : `snmpwalk -v3 -l authPriv -u UTILISATEUR -a SHA -A
'MOTDEPASSE' -x AES -X 'CLE' ADRESSE-NAS 1.3.6.1.4.1.6574.1` (les lignes
`…6574.1.7.1.0` et `…6574.1.7.2.0` = CPU et mémoire ; coller la sortie).

Mesure partie 4 (banc d'essai) : 7 µs par ligne syslog (lecture, masquage,
mise en lot) ; ~0,0005 % d'un cœur au rythme actuel, 0,35 % à 500 lignes/s ;
mémoire bornée (file de 1 000 lignes, lots de 512 Kio, envoi 4 Mio).

Mesure partie 3 (binaire réel, au repos, 40 s) : collecteur **12,5 Mo**
résidents (4,5 Mo de mémoire propre), < 10 ms de CPU, binaire 7,7 Mo.
Objectif collecteur (< 40 Mio, < 1 % d'un cœur) ✅ ; référence Alloy +
unpoller ≈ 180 Mo.

Mesure partie 1a (banc d'essai, 30 invités) : 0,83 ms par cycle de 30 s
(~0,003 % d'un cœur), ~68 Ko de mémoire temporaire par cycle, aucune
écriture disque. Objectif agent (< 30 Mio, < 1 % d'un cœur) : inchangé ✅.

## Éléments sur téléphone (SPEC § 94, 20/11)
- Onglets en pastilles, liens un par ligne, espace du compteur, filtre pleine largeur.
- Vérifié iPhone 13 (clair et sombre) : aucun débord à droite, aucune erreur JS.
- Coût : CSS seulement.

## Tâches planifiées : historique, anomalies, VM oubliées, PBS (SPEC § 95, 20/11)
- Historique des exécutions (frise des 10 dernières, durée habituelle).
- Anomalies : durée anormale, tourne trop longtemps, taille de sauvegarde en
  baisse ; règle `TacheAnormale`.
- VM et conteneurs sans sauvegarde planifiée (assistant root, format 7) ;
  règle `VMSansSauvegarde`.
- Détail des tâches Proxmox Backup Server (nettoyage, vérification, élagage,
  synchronisation).
- Page Tâches par machine, filtre « En problème » ; carte « Sauvegardes des
  dernières 24 h » sur la vue d'ensemble.
- Mesures : serveur 21 ms/min pour 50 tâches, 2 Mo de base au pire ; agent
  4 ms par journal PBS (toutes les 15 min). Objectifs tenus.
- Tests : historique, anomalies, journaux PBS, VM oubliées, bout en bout
  (règles : 178, dont 177 actives). Captures ordinateur et iPhone 13 vérifiées.
- À faire chez l'utilisateur : mettre à jour le serveur, puis l'agent des
  nœuds Proxmox et du PBS (assistant root format 7). Pour confirmer les
  formes de journaux PBS, coller la sortie de :
  `ls /var/log/proxmox-backup/tasks | head` et
  `tail -n 3 /var/log/proxmox-backup/tasks/archive`.

## Alertes sur téléphone (SPEC § 96, 20/11)
- Onglets sur une ligne avec fondu au bord (les pastilles prenaient trop de place), heures de l'historique lisibles, cartes titrées par
  le nom de l'alerte, « En attente » sans « · ».
- Bug corrigé : historique des alertes coupé par le style de la frise des incidents.
- Alertes en cours : titre à gauche, gravité à droite, plus de carte dans la carte.
- Vérifié iPhone 13 clair et sombre (Alertes, Envois, Incident, Règles) :
  aucun débord, aucune erreur JS.

## Agents et temps réel, partie 1 : « En direct » (SPEC § 97, 20/11)
- Page machine : CPU, mémoire, réseau, disques chaque seconde, top 10 des
  processus ; pause, arrêt 30 s après fermeture, 15 min au plus.
- Agent : relevé /proc (sans root, sans commande), envoyé seulement quand on
  regarde ; serveur : relevés en mémoire (5 min), jamais stockés.
- Barre iOS : titre sans pastilles (« pve1 »).
- Mesures : agent ~1 ms par relevé (82 processus), ~1,2 Ko/s par machine
  regardée ; serveur ~300 Ko par machine regardée. Rien hors direct.
- Tests : hub, protocole, relevé /proc simulé, boucle agent, réveil de
  l'agent et relevés côté serveur, flux de la page ; captures ordinateur et
  iPhone (démo). Bug trouvé par les tests : l'agent en attente pouvait ne
  pas être réveillé (état oublié) — corrigé.
- À faire chez l'utilisateur : mettre à jour serveur puis agents (capacité
  « direct »).
- Suite : partie 2 (pannes vues tout de suite, cause probable), partie 3
  (logs en direct), partie 4 (tampon disque, envoi allégé).

## Agents et temps réel, partie 2 : pannes vues tout de suite, cause probable (SPEC § 98, 20/11)
- Agent : signal systemd (service en échec) et compteur OOM toutes les 2 s :
  relevé immédiat et serveur prévenu ; processus les plus gourmands chaque minute.
- Serveur : pannes notées (journal des événements, « Vu tout de suite » sur
  l'alerte), règles évaluées 2 s après ; « Cause probable » sur les alertes
  de CPU, charge, mémoire.
- Mesures : 31 µs / 2 s (OOM), 0,9 ms / min (processus). Objectifs tenus.
- Tests : veilleur (signaux, OOM), pannes côté serveur (limite, réveil),
  cause probable, page d'alerte, journal ; démo vérifiée.

## Agents et temps réel, partie 3 : logs en direct poussés (SPEC § 99, 20/11)
- Page Logs : « Direct » reçoit les lignes dès leur arrivée (1 à 2 s) au lieu
  de redemander toutes les 5 s ; lignes en attente quand on lit plus bas.
- Serveur : signal à chaque écriture de logs, aucune recherche sans nouvelle ligne.
- Tests : flux (lignes déjà là, nouvelle ligne poussée, filtres) ; démo
  vérifiée (ordinateur, iPhone).

## Agents et temps réel, partie 4 : tampon disque, envoi allégé (SPEC § 100, 20/11)
- Tampon disque 20 Mo pendant les coupures (ordre gardé, repris après
  redémarrage) ; rien écrit tant que le réseau marche.
- Envoi allégé : jauges inchangées renvoyées toutes les 2 min seulement ;
  −20 % d'octets mesurés sur la machine de test, 60 ns par point.
- Tests : tampon (débordement, ordre, quota, reprise, arrêt), allégement
  (garde, changement, compteurs jamais allégés).
- À mesurer chez l'utilisateur : gain réel sur un nœud Proxmox
  (`mirador_agent_points_economises_total`).

## CI revue et workflow de release (SPEC § 101, 20/11)
- CI : durée maximale par tâche, noms en majuscules, plus de passage sur les tags.
- Release : bouton (version saisie) ou tag ; contrôles et tests, puis
  binaires signés + serveur + outil + SHA256SUMS, notes générées, image
  `:X.Y.Z` et `:latest`.
- Release : la CI n'est plus relancée, son résultat est vérifié (~8 min de
  moins par version) ; tag posé seulement après la vérification.
- Branches : fuzzing, image et arm64 seulement sur main (et le dimanche) :
  ~20 → ~8 min facturées par push de branche.
- actionlint sans remarque.
- À faire une fois chez l'utilisateur : environnement « signature » :
  autoriser la branche `main` (pour le bouton).

## Retours du 20/11 et mise à jour du serveur depuis l'interface (SPEC § 102)

- Affichage : carte « Sauvegardes 24 h » retirée de la vue d'ensemble ;
  avertissements en jaune (trait `--attention-trait`) ; messages regroupés
  des logs en cartes sur téléphone ; cible des sondes à droite avec le
  cadenas ; tri des sondes (et menu « Trier » des tableaux en cartes).
- Page machine : « En direct » et « En ce moment » réunis, courbes lisibles
  (maquette validée).
- Mise à jour du serveur depuis l'interface (option B) : image signée par la
  release, `mirador-maj-serveur` sur le VPS (vérifie, sauvegarde, bascule,
  retour automatique), page Réglages › Mise à jour du serveur.

| Mesure | Valeur | Objectif |
|---|---|---|
| Direct, navigateur (script) | 1,4 ms par seconde, tas 1,4 Mo | négligeable |
| Page machine (démo) | 185 ms, 5 requêtes de plus | < 500 ms |
| `mirador-maj-serveur` au repos | 0 (pas de processus, unité .path) | 0 |
| `mirador-maj-serveur` sans demande | 4 ms, 12,7 Mio RSS | — |
| Binaire `mirador-maj-serveur` | 6,9 Mio | — |
| Serveur : lecture de la dernière version | 1 HEAD par heure au plus, page ouverte | — |

Tests : `internal/majserveur` (Docker simulé : réussite, refus, retour
arrière avec base remise, lien symbolique), `internal/release` (image
signée), console (page, code 2FA, audit), configuration, présence.
actionlint 1.7.7 sans remarque.

Validé le 21/11 : release 0.6.0 publiée ; serveur passé en 0.6.0 (dernière
fois à la main), `mirador-maj-serveur` installé sur le VPS (« Tout est
prêt ») ; agents en 0.6.0 ; « En ce moment » en direct vérifié sur les
machines. Premier essai du bouton : à la prochaine version (0.6.1).

## Conservation des données : page revue (SPEC § 103, 21/11)

Maquette validée le 21/11 : barre du disque, une carte par type, durées en
clair, plafonds en Go, prévision pendant la saisie, avertissement seulement
en cas de baisse, téléphone corrigé (plafonds visibles). Coût : 2 `stat` de
plus par affichage de la page. Tests : page, hausse, baisse refusée sans
confirmation puis programmée, prévision et plafond.

## Réglages revus (SPEC § 105, 21/11)

Notifications, Sauvegardes, Certificats et Interface refaits d'après les
maquettes validées ; Page de statut dans son propre onglet. Interface :
blocs Général / Apparence / GéoIP détaillé (version, tailles, test d'une
adresse) / Dépannage replié. Coût : un `stat` de plus par affichage
d'Interface, rien en tâche de fond. Tests : bloc Général (fuseau, lien,
ingestion sans code si inchangée, refus sans code si elle change, fuseau
inconnu refusé), page de statut dans son onglet, certificats.

## Mise à jour du serveur : page sans erreur pendant le redémarrage (SPEC § 106, 21/11)

Rechargement en fond : la page reste affichée avec un message tant que le
serveur ne répond pas, puis reprend toute seule. Vérifié dans Chromium
(simulation) : 502 du proxy, connexion refusée, retour du serveur (bloc
remplacé, aucune navigation), fin (un seul rechargement complet). Coût :
inchangé (une requête par période sur ces pages seulement).

## Sites, types et groupes : renommer, fusionner, ranger (SPEC § 107, 21/11)

Maquette v2 validée le 21/11 : onglets, machines en pastilles, « Aussi
utilisé par », renommer avec ce qui change (une transaction, audit), fusion
confirmée, règles/tableaux à vérifier listés, « Machines… ». Coût : rien en
tâche de fond ; quelques COUNT à l'affichage. Tests : renommage (machines,
maintenance, liste, audit), fusion refusée sans confirmation puis faite,
fusion de groupes (réglages et modèles gardés), nom invalide, « Machines… ».

## Sondes : « Vérifier maintenant » (SPEC § 108, 21/11)

Bouton sur la page Sondes et la fiche : essai tout de suite (serveur ou
collecteur, via la demande de configuration déjà ouverte ; capacité
`sonder`, collecteur à mettre à jour). Limite : un essai par sonde toutes
les 10 s. Coût : nul hors usage. Tests : protocole (aller-retour des noms,
valeurs mal formées), serveur (réveil immédiat, rien pour un agent,
oubli après 5 min), sondes (essai sur signal), collecteur (nom inconnu
ignoré), lien (en-tête envoyé et lu), console (bouton grisé, limite 10 s,
rechargement 30 s).

## Mises à jour des agents : page revue (SPEC § 109, 21/11)

Maquette validée : tuiles, déploiement en 3 étapes (importer, essayer sur
une machine, déployer), machines par état avec filtres, cartes de
versions, réglages repliés. Audit : lignes séparées comme les logs.
Coût : nul (mêmes lectures). Tests : états, étapes 1 à 4, cartes, filtre.

## Mode administration : rouvrir facilement (SPEC § 110, 21/11)

Bandeau « fermé · Ouvrir » sur toutes les pages ; fin du mode suivie par le
navigateur (page restée ouverte). Vérifié sur iPhone simulé (horloge avancée
de 11 min, bandeau qui bascule, réouverture avec le code). Test Go :
bandeaux ouvert / fermé / désactivé, retour sur la page.

## Relecture de l'agent et du collecteur (SPEC § 111, 21/11)

Quatre relectures en parallèle, ~35 points signalés, 30 vérifiés et
corrigés (sécurité de l'assistant root, syslog, sondes, mise à jour,
tampon d'envoi, collecte). 4 laissés de côté, à décider (§ 111). Tests
ajoutés pour chaque correction (la plupart échouent sans elle) ;
`go test -race` sans alerte ; gosec sans alerte. Coût : négligeable.

## Suites de la relecture (SPEC § 112, 21/11)

Retour arrière signé valable 7 jours (date dans la signature, migration
0036) ; longues lignes des conteneurs TTY gardées (début). Le reste laissé
tel quel, sur décision. Tests : retour expiré / sans date refusé, date
modifiée = signature cassée, ligne TTY longue.

## Relecture de l'interface (SPEC § 113, 21/11)

Quatre relectures (sécurité, machines, alertes/logs/réglages, navigateur
ordinateur + iPhone, 524 pages visitées). ~45 points vérifiés et corrigés,
2 laissés de côté (§ 113). Tests Go ajoutés (la plupart échouent sans la
correction) ; corrections du navigateur revérifiées dans la démo (modèles,
édition de la vue, tri des alertes). `go vet`, tests et gosec sans alerte.
Coût : négligeable.

## Suites : fiches, vue d'ensemble, images du serveur (SPEC § 113, § 114, 21/11)

Fiche d'un élément : événements cherchés dans les requêtes (test : un
événement d'il y a 3 jours retrouvé malgré 400 plus récents). Vue
d'ensemble : compte « à valider » en cache 60 s ; fiches et page Éléments :
1 requête de métriques au lieu de 9. `mirador-maj-serveur` : étiquettes de
version et ménage des images après succès (test avec un Docker simulé :
`latest` et anciennes images retirées, précédente gardée, image utilisée
jamais supprimée). Tests, vet et gosec sans alerte.

## Performances et fuites (SPEC § 115, § 116, 21/11)

| Mesure | Avant | Après | Objectif |
|---|---|---|---|
| CPU, 50 agents + 2 h 30 d'historique | 3,75 % | **1,27 %** | < 5 % ✅ |
| Tas vivant sous charge | 170 Mo | **117 Mo** | — |
| RSS max sous charge | 223 Mo | 221 Mo (Go garde la place permise) | < 256 Mio ✅ |
| Gabarits HTML | 38,9 Mo | 6,1 Mo | — |
| Serveur au repos (1 agent, 45 min) | — | 76 Mo RSS, plat ; 0,14 % CPU | ✅ |
| Agent (intervalle 10 s, 45 min) | — | 17,5-18,6 Mo, plat ; 0,06 % CPU | < 30 Mio ✅ |

Fuites corrigées (chacune prouvée par un test) : agent 5 (journalctl en
boucle, configuration en boucle, lot de logs, /proc/net/tcp, dpkg/apt-get),
serveur 4 (fin d'alerte perdue, marques d'escalade, conteneurs jetables,
index de l'historique). Points faibles faits ensuite (§ 116) : ménage des
machines, sondes et tâches disparues, purge par âge de 8 tables, anciens
noms oubliés, tampons d'envoi réutilisés, compteur de plantages borné dans
le temps.

## Collecteur : relecture et épreuve réelle (SPEC § 117, 21/11)

Épreuve du vrai binaire (38 min, syslog TLS 200/s, SNMP v3, UniFi, sondes,
pannes simulées) : RSS max 23,4 Mo (< 30 Mio ✅), aucune fuite. 12 points de
relecture corrigés (syslog ISO, écoute jamais rouverte, sessions UniFi,
SNMP colonne par colonne, certificats des sondes…) + relance sur place après
un changement de configuration (agent et collecteur : plus de blocage au
6ᵉ changement en 5 min). Coût réel du syslog corrigé dans § 23.14 (~120 µs
par ligne, pas 7).

## Page du collecteur, nouvelle présentation (22/11)

- Maquette validée, codée (SPEC § 119) : cases d'état, tableau équipement × méthode, cartes par méthode, ajout et retrait ligne par ligne.
- Tests : `TestOngletCollecte`, `TestSaisieGardeeApresRefus` (réécrit), suite console complète verte avec `-race`.
- Coût : 6 requêtes instantanées au plus par affichage de la page, en cache ; aucun changement côté collecteur.
- Captures ordinateur et téléphone vérifiées (aucun débordement).

## Page Machines et vue d'ensemble, nouvelle présentation (22/11)

- Maquettes validées (sans « Tâches de la nuit »), codées (SPEC § 120).
- Machines : cases d'état, bandeau d'approbation, tuiles par site, bascule tuiles / tableau gardée par le navigateur.
- Vue d'ensemble : bandeau, 5 cases, « À regarder », « Le parc d'un coup d'œil », ligne du bas.
- Tests : `TestAccueilARegarder` (nouveau), `TestPageMachines`, `TestPostAccueil`, `TestRuche` mis à jour ; suite console complète verte avec `-race`.
- Coût : 3 requêtes instantanées en plus sur la vue d'ensemble (en cache) ; la page Machines réutilise les mesures de la vue d'ensemble.
- Captures ordinateur et téléphone vérifiées (aucun débordement).

## Mesures du collecteur contre Alloy + unpoller (obscollector, 23/11)

Relevé du 23/11 vers 1 h 15, collecteur 0.6.13 démarré depuis 6 min
(**provisoire** : à refaire après 24 h ; le CPU du démarrage pèse lourd).

| Service | Mémoire (cgroup) | CPU moyen | Durée |
|---|---|---|---|
| mirador-collector 0.6.13 | 11,9 Mio | 0,17 % | 0,1 h |
| mirador-collector-processus | 8,1 Mio | 0,18 % | 0,1 h |
| **mirador (total)** | **20,0 Mio** | 0,35 % (démarrage) | |
| Alloy | 176,7 Mio (RSS 154,6) | 0,83 % | 9 j 19 h |
| unpoller | 24,0 Mio (RSS 21,6) | 0,07 % | 28 j 22 h |
| **ancienne pile (total)** | **200,7 Mio** | 0,90 % | |

Deuxième relevé, 23/11 vers 12 h (collecteur 0.6.16 redémarré par sa mise
à jour 15 min avant ; compagnon en marche depuis 11,9 h) :

| Service | Mémoire (cgroup) | RSS | CPU moyen | Durée |
|---|---|---|---|---|
| mirador-collector 0.6.16 | 13,7 Mio | 20,8 Mio | 0,09 % | 0,2 h |
| mirador-collector-processus | 11,3 Mio | 9,5 Mio | **0,02 %** | 11,9 h |
| **mirador (total)** | **25,0 Mio** | | | |
| Alloy | 159,2 Mio | 159,2 Mio | 0,83 % | 246,7 h |
| unpoller | 25,6 Mio | 24,8 Mio | 0,07 % | 706,3 h |
| **ancienne pile (total)** | **184,8 Mio** | | 0,90 % | |

Machine : 961 Mio de mémoire, 402 Mio disponibles, 206 Mio de swap utilisés ;
arrêter Alloy et unpoller rendrait environ 185 Mio.

Relevé de référence, 24/11 (collecteur 0.6.16 en marche depuis 28,4 h) :

| Service | Mémoire (cgroup) | RSS | CPU moyen | Durée |
|---|---|---|---|---|
| mirador-collector 0.6.16 | 19,6 Mio | 18,4 Mio | **0,08 %** | 28,4 h |
| mirador-collector-processus | 11,2 Mio | 9,2 Mio | **0,02 %** | 40,1 h |
| **mirador (total)** | **30,8 Mio** | | **0,10 %** | |
| Alloy | 142,8 Mio | 132,9 Mio | 0,83 % | 274,8 h |
| unpoller | 28,4 Mio | 26,1 Mio | 0,07 % | 734,5 h |
| **ancienne pile (total)** | **171,2 Mio** | | **0,90 %** | |

**Objectifs tenus** (< 40 Mio, < 1 % d'un cœur) : 5,6 fois moins de mémoire
et 9 fois moins de CPU que l'ancienne pile.

Objectif du collecteur : < 40 Mio (SPEC § 23.3) : tenu (20 à 31 Mio avec le
compagnon). Compagnon : 0,02 % de CPU sur 12 h. Le CPU du collecteur sur 24 h
reste à mesurer : chaque mise à jour automatique le redémarre et remet le
compteur à zéro (relevé à refaire 24 h après la dernière mise à jour). Mémoire : 10 fois moins que l'ancienne pile. CPU : à juger sur
24 h (le compagnon scanne /proc chaque seconde seulement pendant le direct).

## Conteneurs, nouvelle présentation (SPEC § 123, 23/11)

- Maquette validée (ajouts de l'agent : oui ; vue d'office : par projet), codée.
- Agent : ports publiés, code de sortie, sortie du test de santé (masquée comme les logs).
- Page : bandeau avec la raison, 5 cases, recherche et filtres, vue Par projet / Liste, petite courbe du processeur sur 24 h, image changée.
- Fiche : pastilles, bandeau « pourquoi » avec les 3 dernières erreurs des logs, graphiques du tableau `conteneur` comme ceux d'une machine.
- Tests : `TestPageConteneurs` (réécrit), `TestFicheConteneur`, `TestRaisonConteneur`, `TestDocker` complété ; suites `internal/...` vertes, go vet et gosec propres.
- Mesures (banc, 200 conteneurs sur 10 machines) :

| Mesure | Valeur | Objectif |
|---|---|---|
| Page Conteneurs | 85 à 100 ms | < 300 ms |
| Mémoire allouée par affichage | 76 Mio, rendus aussitôt (courbes 24 h gardées 2 min) | pas de hausse durable |
| HTML | 583 Kio (deux vues), non compressé | — |
| Fiche d'un conteneur | 5 à 6 ms | < 300 ms |

  Pour un parc réel de 30 à 40 conteneurs : environ 5 fois moins. Si la page pèse trop un jour : ne rendre la vue Liste qu'à la demande.
- Captures ordinateur et téléphone vérifiées (aucun débordement).

## Agent et assistant root, affichage plus clair (SPEC § 124, 23/11)

- Chroot écarté (pas une barrière pour root, plus complexe) ; deux morceaux gardés.
- Santé : les deux morceaux expliqués côte à côte, lignes Agents / Assistants root séparées, tableau par machine, carte Envois directs.
- Page machine › Réglages : bloc « mirador sur cette machine » (agent + assistant root ; collecteur + compagnon).
- Tests : `TestBlocMirador` (nouveau), `TestPageSante` mis à jour ; suites `internal/server/...` vertes.
- Coût : 7 requêtes instantanées en cache (20 s) ; aucun changement de l'agent.
- Captures ordinateur et téléphone vérifiées.

## Agent dans un conteneur LXC (SPEC § 125, 24/11)

- Cause : dans un LXC, `/sys/class/hwmon` montre les capteurs de l'hôte ;
  pbs et pdm envoyaient les températures du nœud (modèle « Machine
  physique » détecté).
- Correction : l'agent repère le conteneur (`/run/systemd/container`,
  `/.dockerenv`) et ne lit plus capteurs, ventilateurs, ZFS ni RAID ; ligne
  « Conteneur » sur la page machine. Tests : détection (5 cas), bloc de la
  page machine ; tous les tests et gosec au vert.
- Après la mise à jour : décocher « Machine physique » sur les conteneurs
  où il reste coché (requête `count by (instance) (node_hwmon_temp_celsius)`).

## Relais Docker à adresse fixe (SPEC § 126, 24/11)

- Cause : sup, mail, proxy ont la sortie en DROP ; Docker relaie
  `127.0.0.1:12375` vers le relais par `docker0` (`172.19.0.1` →
  `172.19.0.2:2375`), jeté (journal `DROP_OUT_v4`). Adresse non fixe sur ce pont.
- Correction (script d'installation, arrive avec le serveur) : réseau
  `mirador-docker`, pont `br-mirador`, `172.31.254.0/29`, relais en
  `172.31.254.2` ; une seule règle de sortie partout ; test du relais en fin
  d'installation ; liste des services ignorés gardée en relançant le script.
  Testé : calcul et refus des plages (9 cas), test du relais (joignable /
  absent), placement du relais avec un faux docker (neuf, ancien, déjà bon,
  autre plage), playbook Semaphore en local.
- **Appliqué le 24/11 au soir** (0.6.18) : serveur à jour, règle `br-mirador`
  sur sup, mail, proxy, playbook `relais-docker-mirador.yml` passé ; alertes
  « Docker : démon injoignable » levées.

## Vérifier maintenant, délais, bandeau, paquets (SPEC § 127, 25/11)

- **0.6.19 publiée et installée le 25/11** (serveur, puis agents) : essais
  validés par l'utilisateur (« c'est ok »).

- Maquettes validées le 25/11 (alerte, bloc Services, bandeau, bloc Paquets).
- Retour à la normale d'un service : signalé par l'agent (D-Bus) comme la
  panne, alerte fermée en quelques secondes au lieu de 2 min au plus ;
  règle « service en échec » évaluée toutes les 15 s (agents anciens).
- « Vérifier maintenant » : page d'une alerte et lignes en échec du bloc
  Services ; relevé complet + assistant root, une fois par minute ; attente
  puis résultat affichés. Agent 0.6.19 requis (sinon « agent à mettre à jour »).
- Paquets : nouveau calcul après un apt update, 15 min après un échec,
  motif et âge du calcul affichés (le message « agent trop ancien » était
  faux ; proxmoxbackup : simulation correcte dans les limites du service,
  62 Mo au plus, pas de séries refusées : liste revenue seule).
- Nombre de paquets faux (573 sur la page Machines) : l'agent comptait
  aussi les nouveaux paquets de `dist-upgrade` (76 au lieu de 68 sur
  proxmoxbackup, vérifié avec `apt list --upgradable`). Corrigé : seules
  les mises à jour comptent, nouveaux paquets à part ; case « Machines à
  mettre à jour » (nombre de machines, total dessous, filtre).
- Bandeau : « Tout va bien » seulement sans avertissement ni pastille
  orange ou rouge ; points sans urgence et infos comptés à part.
- Téléphone : titre « Services » plus coupé (64 px de large, une ligne).
- Tests : veille (retour, déjà en échec), canal agent ↔ serveur (deux
  côtés), calcul des paquets (échec, réessai 15 min, listes apt), page
  machine (bouton, une par minute, attente, agent ancien), résultats,
  bandeau (avertissements, machine à regarder, infos) ; `go test ./...`,
  `go vet`, gosec au vert. Captures sur la démo conformes aux maquettes.

## Bilan mémoire après la suppression de l'ancienne stack (25/11)

| Machine | Avant | Après |
|---|---|---|
| VPS (sup, 3,8 Go) | ancienne stack ≈ 770 Mio (07/11 : Grafana 291, VictoriaMetrics 248, VictoriaLogs 187, Alertmanager 24, vmalert 19) | **mirador-server 187 Mio** (limite 300, objectif < 256 tenu) + relais Docker 30 Mio ; 2 512 Mio disponibles |
| obscollector (961 Mio) | 402 Mio disponibles, 206 Mio de swap (Alloy 143 + unpoller 28) | **459 Mio disponibles, 157 Mio de swap** (collecteur 20 + compagnon 11) |

- Gain sur le VPS : environ **580 Mio** (770 de l'ancienne stack contre 187
  pour mirador). Restent sur le VPS, hors mirador : uptime-kuma (279 + 181
  Mio), arcane (139 + 30 Mio).
- Relais Docker de mirador : 29,8 Mio utilisés pour une limite de 32 Mio
  (jamais tué faute de mémoire). Limite passée à **64 Mio** (0.6.20,
  publiée et appliquée le 25/11 par le playbook ; vérifié sur gladys-dev :
  67108864 octets).

## Sauvegarde téléchargeable, VM exclue (SPEC § 128, 25/11)

- Réglages › Sauvegardes : bouton « Télécharger » (archive chiffrée, admin, audit).
- « VM sans sauvegarde planifiée » : bouton « Ne pas surveiller » (page de
  l'alerte et Tâches planifiées), retour en arrière possible.
- Mesures : rien de mesurable (une série par VM exclue, une lecture de
  réglage par minute ; le téléchargement lit le fichier en flux).
- Tests : `TestTelechargerSauvegarde`, `TestInvitesExclus`,
  `TestMettreAJourVMSansSauvegarde`, `TestVMExclueSansAlerte` (vraie règle
  PromQL), `TestExclureVM` (interface).
- Livré en 0.6.21 (serveur seul) ; validé le 25/11 : alerte de la VM 200 pbs
  fermée après « Ne pas surveiller », téléchargement d'une sauvegarde fait.

## Connexion bloquée renouvelée, direct journalisé (SPEC § 129, 25/11)

- Agent : ping HTTP/2 (20 s / 10 s), connexions fermées après 2 délais
  dépassés d'affilée, arrêt du direct écrit au journal.
- Tests : `TestRenouveleur`, `TestExpediteurRenouvelleConnexion` (serveur
  qui ne répond plus puis revient), `TestTransportServeurPing`,
  `TestBoucleDirectArretJournal`.
- Mesures : rien de mesurable (un ping par connexion silencieuse toutes
  les 20 s).
- Playbook `effacer-echecs-ancienne-stack.yml` (alloy.service en échec sur
  le parc), essayé ici avec un faux systemctl (avec et sans échec).
- Playbook passé sur le parc le 25/11 (alloy.service effacé).
- 0.6.22 publiée, installée sur le parc et validée le 25/11.

## « À regarder » : raison et bons liens (SPEC § 130, 26/11)

- Bandeau avec la raison, liens « Voir » vers le disque, le certificat de
  la machine, l'onglet Activité ; filtre « Problèmes » aligné sur la case
  « À regarder ».
- Tests : `TestBandeauAvertissements` (disque), `TestCaseCertificatLien`,
  `TestProblemeAvecPointOrange`.
- Mesures : rien de mesurable (mêmes requêtes).
- 0.6.23 installée le 25/11 au soir : bandeau, certificat, page machine OK ;
  « Voir » du disque ouvrait l'onglet Activité (ancre `r-disques`),
  corrigé (vu dans le navigateur sur le serveur de démonstration).
- 0.6.24 installée et validée le 25/11 au soir (« Voir » du disque OK).
- dns2 (26/11, minuit) : mise à jour d'agent en échec « HTTP 403 ». Cause :
  jeton limité à son adresse IPv6 ; une minute en IPv4 (185.x) refusée par
  mirador (« adresse non autorisée », pas nginx). Les deux adresses ajoutées
  par l'utilisateur. À vérifier sur les autres VPS en double pile (mail,
  proxy, dns1, sup) : fait, tout est OK.
- Bouton « Autoriser cette adresse » sur la page machine (SPEC § 131),
  `TestAutoriserAdresseRefusee`, `TestAdresseAutorisee` ; à livrer avec la
  prochaine version (après la mesure de 24 h).
- sup (26/11) : journal local 2,9 Go → 461 Mo (limite 500 Mo) ; le bruit
  venait des paquets bloqués journalisés par le pare-feu (voisinage IPv6
  bloqué, règles ICMPv6 ajoutées par l'utilisateur). Playbook
  `limiter-journal.yml` (docs/deploiement-agents.md § 5.5) pour le parc,
  essayé ici avec de faux journalctl / systemctl (2 passages et --check).
- Playbook passé sur le parc le 26/11 (26 machines, aucun échec) : environ
  70 Go de journaux locaux avant, 12 Go après, **≈ 58 Go libérés**.

## Courbes longues jusqu'à maintenant, prévision disque (SPEC § 132, 26/11)

- 7 j / 30 j : agrégats + base brute pour les dernières 24 h (avant : courbes
  arrêtées 24 h plus tôt, vu sur le tableau OPNsense).
- « Plein dans N j » : perte de place sur 7 j et sur le dernier jour.
- Tests : `TestLectureLongueJusquaMaintenant` (échoue sans la correction :
  dernier point il y a 24 h 55), `TestPrevisionDisqueLibereHier`.
- Tableaux : maquettes validées le 26/11, codées (SPEC § 133) ;
  `TestResumeTableau`, `TestPageTableauPresentation`, captures sur la
  démonstration (ordinateur et téléphone).
- À livrer dans la prochaine version (après la mesure de 24 h).

## Mesure du serveur sur le parc complet (VPS, 26/11)

Serveur 0.6.24, 26 agents + collecteur + fw (Telegraf), relevé `docker stats`
chaque minute du 26/11 00:06 au 26/11 16:41 (978 points, **16 h 35** au lieu
de 24 h : nuit complète comprise, arrêtée pour installer la 0.6.25 ; aucun
redémarrage, aucun OOM).

| Mesure | Résultat | Objectif |
|---|---|---|
| RAM (`docker stats`, cache de fichiers actif compris) | **213 Mio en moyenne, 252 Mio au plus** (145 Mio au départ) | < 256 Mio ✅ **de justesse** (limite du conteneur 300 Mio) |
| CPU | **4,86 % d'un cœur en moyenne**, 9,9 % au plus | < 5 % ✅ **de justesse** |
| Écritures disque | 679 Mo en 16 h 35 (~41 Mo/h, sauvegarde de la nuit comprise) | ✅ |
| Volume des données | 774 Mo (métriques, logs, archives de sauvegarde, versions) | plafonds : métriques 5 Go + 1 Go, logs 4 Go |
| VPS | 2,3 Go disponibles sur 3,8 Go | ✅ |

Les deux objectifs sont tenus mais sans marge, alors que le parc a été
multiplié par 13 depuis la mesure du 14/11 (1,27 % CPU, 113 Mio).
**Mémoire, relevé du 26/11 au soir** : 159 Mio de mémoire propre (`anon`) ;
le reste est du cache de fichiers (50 Mio comptés par `docker stats`, 54 Mio
non comptés) que le noyau reprend. Objectif jugé sur la mémoire propre :
**marge ~95 Mio**, rien à changer. **CPU** : compteurs par partie ajoutés
(SPEC § 134). **Relevé du 26/11 à 19 h 37** (0.6.26, en marche depuis
1 h 43, 15 dernières minutes) : **3,8 % d'un cœur** au total ; règles
d'alerte 0,8 %, ramasse-miettes 0,8 %, réception des logs 0,5 %, réception
des mesures 0,3 %, tâches de fond 0,2 %, interface 0,0 % ; reste ≈ 1,2 %
non attribué (base des mesures, TLS, direct). Rien ne domine : **rien à
changer** (choix de l'utilisateur, 26/11). Mémoire propre 169,8 Mio.

Pistes si ça dépasse :
- RAM : `docker stats` compte le cache des fichiers de la base de mesures
  que le noyau reprend en cas de besoin ; mesurer la part réelle
  (`memory.stat` : anon contre file). Si c'est le tas Go : `GOMEMLIMIT` à
  180 Mio (le ramasse-miettes rend la mémoire plus tôt), moins de requêtes
  en même temps pour les pages lourdes.
- CPU : profil de 30 s pour savoir qui consomme (logs, règles, ingestion) ;
  ensuite, au choix : règles du modèle Linux évaluées toutes les minutes au
  lieu de 15 s, journaux des agents filtrés à la source (niveau info et
  plus), regroupement des envois de logs.

## Prochaine étape

Mise à jour le 25/11 au soir (serveur 0.6.21, agents 0.6.22) ; serveur en
0.6.26 depuis le 26/11 (0.6.25 et 0.6.26 installées et validées : courbes
7 j jusqu'à maintenant, cartes des tableaux en chiffres clés, temps de
calcul par partie). L'ancien
texte (bascule pas à pas, 22-24/11) est dans l'historique Git.

**Fait (étapes 6 et 7, à clore après la mesure de 24 h)**
- Collecteur sur le LAN (obscollector) : SNMP du NAS, UniFi, `fw`, syslog
  TLS, sondes de certificats ; mesures 24 h, objectifs tenus (24/11).
- Agents sur tout le parc par Semaphore (27 machines, approuvées le 24/11),
  en 0.6.22 depuis le 25/11.
- Ancienne stack supprimée (24/11) : Grafana, VictoriaMetrics,
  VictoriaLogs, vmalert, Alertmanager (VPS) ; Alloy, snmpd, scripts et
  minuteries `obs-*` (machines, obscollector) ; nginx, certificats, DNS.
  Échecs systemd restants effacés par Semaphore (25/11). Gain sur le VPS :
  environ 580 Mio.
- Gardé exprès : `01-obs-geo.conf`, `obs-allow.conf`, minuteries
  `obs-geo-fr` et `obs-geoip-update`.
- Alertes Telegram en mode actif depuis le 13/11 ; signal de vie OK.

**Reste**
1. Mesure du serveur : faite le 26/11 (16 h 35, voir plus haut), objectifs
   tenus de justesse.
2. Étapes 6 et 7 : **validées le 26/11** (tableau « Où on en est »).
3. SPEC : 74 sections validées le 25/11 ; 8 « en service, pas encore
   essayé » (§ 39 site injoignable, § 49 disponibilité, § 52 motifs de
   logs, § 54 incidents, § 81 maintenance récurrente et webhook, § 82
   modèles des messages, § 84 page de statut publique, § 122 applications
   d'après Docker Compose), à essayer au fil de l'eau.
4. Archives `/root/ancienne-stack.tgz` (machines) et
   `/root/grafana-sauvegarde.tgz` (VPS) : **effacées le 26/11**.
5. Facultatif : essai réel d'Olvid (`docs/olvid.md`).

Historique : 0.2.20 validée le 13/11 (l'assistant root suit l'agent seul) ;
mode **actif** (Telegram) depuis le 13/11.
