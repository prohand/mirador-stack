# Installation de mirador-server sur le VPS (en parallèle de la stack actuelle)

Ce guide est à suivre **par vous**, sur le VPS. Rien n'est fait à distance.
La stack actuelle (`/opt/obs`, VictoriaMetrics, VictoriaLogs, Grafana…)
continue de tourner sans changement : mirador s'ajoute à côté, sur le même
réseau Docker `obs`, à l'adresse **172.30.0.20**.

Les adresses `172.30.0.x` de ce guide sont des **exemples** : remplacez-les
par celles de votre réseau Docker (celles déjà en place sur votre VPS).

Collez-moi les sorties marquées 📋 dans une nouvelle session.

## 0. Ce qu'il faut

- Docker et le réseau `obs` de la stack actuelle (déjà là).
- Les fichiers `deploy/serveur/docker-compose.yml` et
  `deploy/serveur/server.conf.exemple` du dépôt.
- L'image du serveur :
  - **soit** publiée par la CI : il faut un tag de version (ex. `v0.2.0`),
    à demander. Si le dépôt est privé, l'image l'est aussi : `docker login
    ghcr.io` avec un jeton GitHub ayant seulement le droit `read:packages` ;
  - **soit** construite sur le VPS depuis une copie du dépôt (aucun compte
    nécessaire) :

    ```bash
    cd /chemin/vers/mirador-stack && docker build -f deploy/serveur/Dockerfile --build-arg VERSION=0.2.0 -t ghcr.io/prohand/mirador-server:0.2.0 .
    ```

## 1. Fichiers et clé des secrets

```bash
mkdir -p /opt/mirador/config /opt/mirador/secrets && cd /opt/mirador
```

Copier `docker-compose.yml` dans `/opt/mirador/` et `server.conf.exemple`
dans `/opt/mirador/config/server.conf`.

```bash
openssl rand -base64 32 > secrets/mirador_cle && chown 65532:65532 secrets/mirador_cle && chmod 0400 secrets/mirador_cle
# Le serveur tourne en utilisateur 65532 (pas root) : il doit pouvoir lire
# son fichier de démarrage. Sinon : « open /etc/mirador/server.conf:
# permission denied » et le conteneur redémarre en boucle.
chown 65532:65532 config/server.conf && chmod 0400 config/server.conf
```

> **Copiez tout de suite le contenu de `secrets/mirador_cle` hors du VPS**
> (gestionnaire de mots de passe). Sans lui, aucune sauvegarde ne se restaure.

Dans `config/server.conf`, vérifier l'adresse de nginx vue depuis le
conteneur : la passerelle du réseau `obs` (déjà dans l'exemple). C'est la
seule adresse autorisée à transmettre l'IP réelle des agents.

```
MIRADOR_PROXY=172.30.0.1
```

## 2. Démarrage

```bash
cd /opt/mirador && docker compose up -d
```

```bash
sleep 30 && docker compose ps && docker compose logs --tail 20 mirador-server
```

📋 Attendu : `healthy`, et la ligne `mirador-server démarré`.

## 3. nginx : ingestion de mirador

Deux possibilités. **3 bis (vhost dédié) est la plus simple** : le vhost
d'ingestion actuel (Alloy → VictoriaMetrics) n'est pas touché.

### 3 bis. Vhost dédié (choix du 10/11)

`/etc/nginx/conf.d/02-mirador.conf` (contexte `http`), ajouter :

```nginx
limit_req_zone $binary_remote_addr zone=mirador_ingestion:1m rate=20r/s;
```

`/etc/nginx/sites-available/INGESTION` (remplacer INGESTION par le nom choisi) :

```nginx
server {
  listen 80;
  listen [::]:80;
  server_name INGESTION;
  location /.well-known/acme-challenge/ { root /var/www/html; }
  location / { return 301 https://$host$request_uri; }
}

server {
  listen 443 ssl;
  listen [::]:443 ssl;
  http2 on;
  server_name INGESTION;
  ssl_certificate     /etc/letsencrypt/live/INGESTION/fullchain.pem;
  ssl_certificate_key /etc/letsencrypt/live/INGESTION/privkey.pem;
  ssl_protocols TLSv1.2 TLSv1.3;
  server_tokens off;
  add_header Strict-Transport-Security "max-age=31536000; includeSubDomains" always;

  include /etc/nginx/snippets/obs-allow.conf;   # adresses des machines ; token vérifié par mirador

  location /api/agent/ {
    limit_req zone=mirador_ingestion burst=50 nodelay;
    proxy_pass         http://172.30.0.20:9440;
    proxy_http_version 1.1;
    proxy_set_header   Host $host;
    proxy_set_header   X-Forwarded-For $remote_addr;
    proxy_set_header   Connection "";
    client_max_body_size 8m;
    proxy_read_timeout 600s;                    # mises à jour de l'agent
  }
  location = /sante { proxy_pass http://172.30.0.20:9440; }
  location / { return 404; }
}
```

### 3 ter. Ou : ajouter mirador au vhost d'ingestion actuel (filtre IP seulement)


Dans le vhost d'ingestion actuel, **déplacer** le contrôle du token dans les
deux anciennes `location`, puis ajouter celle de mirador. Le reste ne change
pas (même liste d'IP `obs-allow.conf`, même certificat) :

```nginx
  # Couche 1 : filtrage par IP source (inchangé, pour tout le vhost)
  include /etc/nginx/snippets/obs-allow.conf;

  # (supprimer ici la ligne « if ($obs_site = "") { return 403; } »)

  location ~ ^/(api/v1/write|api/v2/write|write|opentelemetry/v1/metrics)$ {
    if ($obs_site = "") { return 403; }      # token de la map : seulement pour VictoriaMetrics
    proxy_pass         http://172.30.0.10:8428;
    # … (le reste inchangé)
  }

  location /insert/ {
    if ($obs_site = "") { return 403; }      # idem pour VictoriaLogs
    proxy_pass         http://172.30.0.11:9428;
    # … (le reste inchangé)
  }

  # mirador-server : tokens vérifiés par mirador (pas de map), IP filtrée ci-dessus
  location /api/agent/ {
    proxy_pass         http://172.30.0.20:9440;
    proxy_http_version 1.1;
    proxy_set_header   Host $host;
    proxy_set_header   X-Forwarded-For $remote_addr;   # mirador lit l'IP réelle ici
    proxy_set_header   Connection "";
    client_max_body_size 8m;
    proxy_read_timeout 600s;                           # binaires de mise à jour
  }
```

```bash
nginx -t && systemctl reload nginx
```

Vérification depuis une machine de la liste d'IP (sans token : refus attendu
**par mirador**, ce qui prouve que la requête l'atteint) :

```bash
curl -s -o /dev/null -w '%{http_code}\n' -X POST https://VOTRE-VHOST-INGESTION/api/agent/v1/metrics
```

📋 Attendu : `401`.

## 4. Brancher une machine (ex. srv-test) sur mirador-server

Fait le 11/11 pour srv-test. Tout se fait dans l'interface.

1. **Machines › Ajouter une machine** : nom (facultatif), site, type,
   groupe, « Envoyer », durée, code 2FA → **Créer un jeton**. La page
   affiche les deux lignes à coller.
2. Sur la machine, en root (`sudo -i`), coller les deux lignes affichées :

   ```bash
   curl -fsSL https://VOTRE-VHOST-INGESTION/api/agent/v1/install -o mirador-agent-install.sh
    MIRADOR_JETON=mirador_enrol_… bash mirador-agent-install.sh --serveur https://VOTRE-VHOST-INGESTION
   ```

   Adresse : celle du **vhost d'ingestion** (agents), pas celle de
   l'interface. Le script est public ; il télécharge le binaire (amd64 ou
   arm64) avec le jeton et vérifie son empreinte SHA-256. Le nom proposé
   est le nom d'hôte (ou `--nom`) ; le même nom qu'Alloy est permis
   (bases séparées). Hors ligne : `--binaire FICHIER`.

3. **Machines › En attente d'approbation** : vérifier (ou corriger) nom,
   site, type, groupe, puis **Approuver** (token limité à l'adresse de la
   demande si la case est cochée ; à décocher si l'adresse publique change).

4. Une minute plus tard : « Dernière donnée » récente dans Machines,
   graphiques dans la fiche, lignes dans Logs (filtre Machine).

Si l'agent de l'étape 2 envoyait encore vers VictoriaMetrics, sa ligne dans
la `map` nginx ne sert plus : vous pouvez la retirer.

## 4 bis. Règles, mode ombre et comparaison avec vmalert

Rien n'est envoyé tant que le **mode ombre** est actif (défaut) : chaque
message qui serait parti est seulement noté dans **Alertes › Envois**
(statut « ombre »). vmalert et Alertmanager continuent de prévenir.

1. **Importer les règles de vmalert** : copier sur votre poste les fichiers
   `/opt/obs/rules/system.yml` et `/opt/obs/rules/equipements.yml` du VPS,
   puis **Règles › Importer** (un fichier à la fois). Le rapport classe
   chaque règle : *exacte*, *approchée* (à relire) ou *impossible* (non
   importée). `/opt/obs/rules-logs/syslog.yml` (LogsQL) : à importer aussi,
   le rapport dit ce qui passe.
2. **Comparaison** : **Réglages › Notifications › Comparaison avec vmalert** :
   - cocher « Comparer toutes les 5 min » ;
   - adresse : `http://obs-vmalert:8880` (même réseau Docker `obs` ;
     ou l'IP fixe `http://172.30.0.12:8880`) ;
   - correspondances : une ligne `nom-mirador=nom-vmalert` par machine dont
     le nom diffère (ex. `srv-test=srvtest`) ;
   - enregistrer.
3. **Alertes › Comparaison** : présentes des deux côtés, « manquante chez
   mirador », « en trop chez mirador ». Laisser tourner au moins 24 h, puis
   coller l'export (même page, ou
   `docker exec mirador-server /mirador-server -config /etc/mirador/server.conf comparer-vmalert`).

📋 Collez le rapport d'import (nombres exacte / approchée / impossible) et,
le lendemain, la comparaison.

## 5. Mesure sur 24 h

Sur le VPS, à lancer dans un `tmux` ou `screen` :

```bash
for i in $(seq 1 1440); do printf '%s,' "$(date +%H:%M)"; docker stats --no-stream --format '{{.MemUsage}},{{.CPUPerc}},{{.BlockIO}}' mirador-server; sleep 60; done > /root/mirador-24h.csv
```

Le lendemain :

```bash
tail -3 /root/mirador-24h.csv && du -sm /var/lib/docker/volumes/mirador_mirador-donnees/_data && docker exec mirador-server /mirador-server -config /etc/mirador/server.conf sauvegardes | head -20
```

📋 Collez la sortie.

## 6. Retirer mirador (si besoin)

```bash
cd /opt/mirador && docker compose down
```

Le volume `mirador_mirador-donnees` est gardé tant qu'il n'est pas supprimé
(`docker volume rm mirador_mirador-donnees`, définitif). Remettre la ligne
`if ($obs_site = "") { return 403; }` au niveau du vhost si vous retirez la
`location /api/agent/`.
