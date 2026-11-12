# Administration de mirador-server (en attendant l'interface web)

Toutes les commandes se lancent dans le conteneur (rien à installer) :

```bash
docker exec mirador-server /mirador-server -config /etc/mirador/server.conf aide
```

Dans la suite, `MS` remplace `docker exec mirador-server /mirador-server -config /etc/mirador/server.conf`.
Les options se placent **avant** le nom de la machine. Chaque action est notée
dans le journal d'audit (jamais les tokens ni les jetons).

## Machines

| Besoin | Commande |
|---|---|
| Voir les machines (statut, version, dernière réception, adresses) | `MS machines` |
| Jeton d'enrôlement (usage unique, 24 h par défaut) | `MS jeton` ou `MS jeton -duree 2h` |
| Approuver une machine en attente | `MS approuver NOM` (token limité à l'adresse de la demande) ou `MS approuver -sans-ip NOM` |
| Refuser une machine | `MS refuser NOM` |
| Couper l'accès d'une machine | `MS revoquer NOM` (effet en 60 s au plus) |
| Token sans enrôlement (installation manuelle) | `MS token NOM` (affiché une seule fois ; les anciens sont révoqués) |
| Adresses autorisées pour le token | `MS ips NOM "203.0.113.7, 2001:db8:1:2::/64"` ; `MS ips NOM ""` = toutes |
| Site, environnement, groupe | `MS etiquettes -site bureau -env vm -groupe dns NOM` |

Une machine qui change d'adresse reçoit des 403 : elle garde ses données et
réessaie. `MS machines` montre sa nouvelle adresse (« DERNIÈRE IP ») ; il
suffit de corriger la liste avec `MS ips`.

## Configuration des agents

Document JSON avec seulement les réglages à changer (mêmes noms que la
configuration distante de l'agent, SPEC § 6.7). Portées : `defaut`,
`groupe:NOM`, `machine:NOM` ; la plus précise l'emporte, réglage par réglage.

```bash
echo '{"intervalle":"60s","logs_debit":100}' | docker exec -i mirador-server /mirador-server -config /etc/mirador/server.conf config defaut -
MS voir-config srv-test-mirador
echo '{}' | docker exec -i mirador-server /mirador-server -config /etc/mirador/server.conf config defaut -
```

Un réglage inconnu ou hors bornes est refusé tout de suite. L'agent récupère
sa configuration toutes les 5 min et redémarre proprement pour l'appliquer.

## Mises à jour signées de l'agent

Les versions sont **signées par la CI de GitHub** à chaque tag `vX.Y.Z`
(SPEC § 18.26) et importées depuis l'interface. Le serveur n'a jamais la
clé privée : il vérifie seulement la signature.

### Une seule fois : la clé

1. Sur le PC Windows (ou tout poste de confiance) :
   `mirador-release-windows-amd64.exe cle` → crée `cle-mirador.json`
   (chiffré par une phrase de passe) et affiche la clé **publique** et son
   empreinte. Garder une copie de `cle-mirador.json` et de la phrase hors du
   PC : une clé perdue ne se retrouve pas.
2. GitHub, dépôt › **Settings › Environments › New environment** :
   `signature`.
   - **Deployment branches and tags** → *Selected branches and tags* →
     **Add deployment branch or tag rule** → *Tag* → `v*`.
   - **Environment secrets** :
     - `MIRADOR_CLE_SIGNATURE` = tout le contenu de `cle-mirador.json` ;
     - `MIRADOR_PHRASE` = la phrase de passe.
3. Interface : **Mises à jour des agents › Clé publique de signature** :
   coller la clé publique (code 2FA) ; vérifier l'empreinte.

Les machines installées ensuite reçoivent la clé (fichier root
`/etc/mirador-agent/cles-publiques.txt`). Celles installées **avant** :
relancer le script une fois (il reprend tout le reste) :

```bash
sudo bash mirador-agent-install.sh --serveur https://VOTRE-VHOST-INGESTION --mirador \
  --cle-publique CLE_PUBLIQUE --auto-update
```

### À chaque version

1. Poser le tag `vX.Y.Z` sur `main` (GitHub › Releases › Draft a new release).
   La CI publie l'image du serveur **et** ajoute à la Release l'agent signé
   (`mirador-agent-X.Y.Z-linux-amd64/arm64` et leurs `.manifeste.json`).
2. Interface : **Mises à jour des agents › Importer une version signée
   depuis GitHub** : saisir `vX.Y.Z`, code 2FA, **Importer**. Le serveur
   télécharge, vérifie la signature et l'empreinte, puis publie.
3. Assigner la version, d'abord à une machine pour essayer, puis à toutes
   (voir ci-dessous).

Fork : régler son propre dépôt (« Changer de dépôt ») et sa propre clé ;
sans les secrets, sa CI publie l'agent sans signature.

### Sans GitHub (à la main)

1. Signer sur le PC :

   ```bash
   mirador-release-windows-amd64.exe signer -cle cle-mirador.json -version 1.2.0 -arch amd64 mirador-agent-linux-amd64
   ```

2. Publier le binaire et son manifeste dans l'interface (Publier une version
   à la main), ou sur le VPS :

   ```bash
   docker exec -i mirador-server /mirador-server -config /etc/mirador/server.conf \
     publier "$(cat mirador-agent-linux-amd64.manifeste.json)" - < mirador-agent-linux-amd64
   ```

### Assistant root

L'assistant root (certificats, SMART, pare-feu) se met à jour **tout seul**
depuis la version signée et confirmée de l'agent, quand les mises à jour
automatiques sont activées : il revérifie lui-même la signature avec la clé
de `/etc/mirador-agent/cles-publiques.txt` (SPEC § 22.5). Pour l'interdire
sur une machine : `MIRADOR_MAJ_RACINE=non` dans `/etc/mirador-agent/agent.conf`
(ou `--sans-maj-racine` à l'installation) ; il ne change alors qu'en
relançant le script d'installation.

### Changer de clé

Régler les **deux** clés dans l'interface (l'actuelle et la nouvelle),
relancer le script avec `--cle-publique ANCIENNE,NOUVELLE` sur chaque
machine, remplacer les secrets GitHub par la nouvelle clé, puis retirer
l'ancienne. Une clé posée sur une machine ne change jamais sans cette
option : même un serveur piraté ne peut pas y mettre la sienne.

### Assigner une version

D'abord à une machine pour essayer (fiche de la machine › Version de
l'agent), puis à toutes (Mises à jour des agents › Version de toutes les
machines). En ligne de commande :

```bash
MS cible -machine srv-test-mirador 1.2.0
MS cible 1.2.0
```

Seuls les agents installés avec `--auto-update` se mettent à jour. Chacun
vérifie la signature, l'empreinte, et refuse une version plus ancienne (sauf
manifeste signé avec `-retour-autorise`). Il teste la nouvelle version
(`-self-test`) avant de basculer. S'il ne joint pas le serveur dans les
10 minutes, il revient à l'ancienne version et ne retente pas celle-ci ;
`MS machines` affiche l'échec.

## Sauvegardes

Voir `docs/sauvegarde-restauration.md` (réglages, SFTP, S3, restauration).

## Alertes

Les règles sont en base et relues par le serveur en 10 s au plus après une
modification. Les règles mirador (OOM, sauvegardes, horloge, disque du
serveur…) sont posées au premier démarrage.

1. Importer les règles actuelles de vmalert, par l'entrée standard
   (`MSI` = `docker exec -i mirador-server /mirador-server -config
   /etc/mirador/server.conf`, avec `-i`) — essai d'abord :

   ```bash
   MSI regles-importer -essai - < /opt/obs/rules/system.yml
   MSI regles-importer - < /opt/obs/rules/system.yml
   MSI regles-importer - < /opt/obs/rules/equipements.yml
   ```

   Le rapport classe chaque règle : `exact` (même expression), `approché`
   (adaptée, voir la note), `impossible` (non importée, ou importée
   désactivée). `-desactivees` importe tout en désactivé.

2. Consulter :

   ```bash
   MS regles                  # liste, gravité, active ou non
   MS etat-regles             # durée, résultats et erreurs de chaque règle
   MS alertes-en-cours        # actives et en attente
   MS alertes -limite 100     # historique
   ```

3. Modifier : exporter, éditer le YAML, réimporter (seules les règles
   changées sont enregistrées, avec une ligne d'audit) :

   ```bash
   MS regles-exporter > regles.yml
   MSI regles-importer - < regles.yml
   MS regle-desactiver TemperatureElevee
   MS regle-activer TemperatureElevee
   MS regle-supprimer TemperatureElevee
   ```

Une règle supprimée qui était une règle mirador ne revient pas au
redémarrage. Le langage est PromQL (celui de vmalert), avec `lag()` en plus ;
détails et limites : SPEC.md § 17.1.

## Sourdine et notifications

Par défaut, mirador est en **mode ombre** : il calcule tout et note dans
l'historique ce qu'il aurait envoyé, sans rien envoyer (vmalert et
Alertmanager continuent de prévenir pendant la période en parallèle).

```bash
MS envois                       # ce qui est parti (ou serait parti en mode ombre)
MS mode actif                   # envoyer pour de bon (canaux : partie 3)
MS mode ombre
```

Silences (sélecteur d'étiquettes, comme dans les règles) :

```bash
MS silence -pendant 3h -commentaire "migration" '{instance="pve1"}'
MS silence -debut "2026-11-12 22:00" -pendant 1h '{alertname="CpuEleve"}'
MS silences
MS silence-annuler 4
```

Maintenances (machine, groupe ou sélecteur ; ponctuelles ou récurrentes) :

```bash
MS maintenance -debut "2026-11-12 22:00" -pendant 2h machine:pve1
MS maintenance -jours dim -heure 03:00 -pendant 2h groupe:proxmox
MS maintenances
MS maintenance-supprimer 2
```

Inhibitions et dépendances (déjà posées : celles d'alertmanager.yml, et
« équipements via=collecteur → miradorcollector ») :

```bash
MS inhibitions
MS dependances
MS dependance groupe:vms pve1                 # les VM dépendent de Proxmox
MS dependance-supprimer 1
```

Accusé de réception (arrête les rappels ; le numéro est dans `MS alertes`) :

```bash
MS acquitter -commentaire "je regarde" 12
```

Réglages (regroupement, attentes, rappels, lien vers l'interface, fuseau) :

```bash
MS notif > notif.json                         # modifier puis :
MSI notif-reglages - < notif.json
```

## Canaux de notification

1. Afficher les réglages, les enregistrer dans un fichier, ajouter le canal :

   ```bash
   MS notif > notif.json
   ```

   Dans `"canaux"`, ajouter par exemple (le jeton est celui du bot actuel,
   le `chat` celui du groupe Telegram) :

   ```json
   "telegram": {"actif": true, "jeton": "123456789:ABC…", "chat": "-100…",
                "acquitter": true, "acquitteurs": []},
   "heartbeat": {"actif": true, "url": "https://…hetrixtools…", "intervalle": "1m"}
   ```

   Pour le heartbeat, créer un **nouveau** moniteur HetrixTools pour mirador
   (celui d'Alertmanager reste pour l'ancienne stack).

2. Enregistrer (les secrets sont chiffrés ; ensuite `notif` affiche `***`,
   et on peut renvoyer le fichier avec `***` pour garder un secret) :

   ```bash
   MSI notif-reglages - < notif.json
   rm notif.json        # il contenait les secrets en clair
   ```

3. Tester, puis consulter l'état :

   ```bash
   MS notif-tester telegram
   MS notif-etat           # canaux, panne ou non, file d'attente, heartbeat
   MS envois               # historique
   ```

Bouton « Acquitter » : avec `"acquitter": true`, chaque alerte Telegram
porte un bouton ; un appui arrête les rappels (noté « telegram:ID » dans
l'historique). `"acquitteurs": [123456789]` limite le bouton à ces comptes
Telegram (vide = tout membre du groupe). Le bot ne lit que les appuis sur
ses boutons, jamais les messages du groupe. Le chat doit être l'identifiant
numérique du groupe (`-100…`).

Secours et inversion : `"principal": "telegram", "secours": "ntfy"` ;
pour inverser, échanger les deux valeurs et renvoyer le fichier.
`"tous": ["critical"]` envoie les critiques sur tous les canaux actifs.
Un ntfy ou un serveur SMTP du réseau local demande `"reseau_prive": true`.

Le mode reste **ombre** tant que `MS mode actif` n'a pas été lancé (refusé
si le canal principal n'est pas configuré).

## Comparaison avec vmalert (période en parallèle)

```bash
echo '{"actif": true, "url": "http://IP-DE-VMALERT:8880", "suffixe": "-mirador"}' | MSI vmalert-reglages -
MS comparer-vmalert          # écarts maintenant + épisodes des 7 derniers jours
MS comparer-vmalert -csv > ecarts.csv
```

Seules les machines approuvées dans mirador sont comparées. Le nom
`srv-test-mirador` est ramené à `srv-test` (suffixe retiré) ; une autre
correspondance se règle par `"correspondances": {"nom-mirador": "nom-vmalert"}`.
L'adresse de vmalert : `docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' obs-vmalert`.

## Interface web (secours)

L'interface remplace la plupart de ces commandes (`docs/interface-web.md`).
Si on ne peut plus y entrer :

- `utilisateurs` : liste des comptes ;
- `utilisateur-secours NOM` : mot de passe provisoire (affiché une fois), 2FA
  effacée, compte réactivé ;
- `console-ips-vider` : vide la liste d'adresses autorisées de l'interface.
