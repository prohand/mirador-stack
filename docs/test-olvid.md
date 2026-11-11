# Test du daemon Olvid (étape 4, partie 4)

But : savoir si Olvid peut servir de canal (secours, puis principal) **avant**
d'écrire le module dans mirador. À faire sur le VPS. Rien ne touche à la stack
actuelle ni à mirador-server. Tu colles les sorties marquées 📋 dans une
nouvelle session.

Sources : documentation officielle <https://doc.bot.olvid.io> (version
2.0.1) et ton intégration Gladys (`prohand/gladys-olvid`).

## 1. Préparer (5 min)

```bash
sudo mkdir -p /opt/olvid && cd /opt/olvid
sudo chown "$USER" /opt/olvid
# Clé admin : aléatoire, gardée dans un fichier à part (jamais dans le compose).
umask 077
openssl rand -hex 32 > admin.key
printf 'OLVID_ADMIN_CLIENT_KEY_CLI=%s\nOLVID_ADMIN_CLIENT_KEY=%s\n' "$(cat admin.key)" "$(cat admin.key)" > .env
```

Fichier `/opt/olvid/docker-compose.yml` (aucun port publié : le daemon n'est
joignable que par le réseau Docker interne) :

```yaml
services:
  daemon:
    image: olvid/bot-daemon:2.0.1
    container_name: olvid-daemon
    restart: unless-stopped
    environment:
      OLVID_ADMIN_CLIENT_KEY_CLI: ${OLVID_ADMIN_CLIENT_KEY_CLI}
      # Option standard de Java : taille maximale de la mémoire du programme.
      JAVA_TOOL_OPTIONS: "-Xmx256m"
    volumes:
      - ./data:/daemon/data
      - ./backups:/daemon/backups
    mem_limit: 1g
    cpus: 1
    logging:
      options: { max-size: "10m", max-file: "3" }

  cli:
    image: olvid/bot-python-runner:2.0.1
    entrypoint: olvid-cli
    profiles: [cli]
    environment:
      OLVID_ADMIN_CLIENT_KEY: ${OLVID_ADMIN_CLIENT_KEY}
      OLVID_DAEMON_URL: http://daemon:50051
    stdin_open: true
    tty: true
```

## 2. Démarrer

```bash
cd /opt/olvid
docker compose up -d
docker compose logs --tail 50 daemon
```

📋 Les 50 lignes de log (vérifie qu'aucune clé n'y apparaît ; sinon masque-la).

## 3. Identité du bot et contact (10 min)

```bash
docker compose run --rm cli
```

Dans le CLI :

1. `identity new mirador` — la **clé client** s'affiche **une seule fois** :
   copie-la dans `/opt/olvid/client.key` (umask 077). Ne la colle pas ici.
2. Réponds `yes` à l'ajout aux contacts, ouvre le lien d'invitation, scanne le
   QR code avec **ton** Olvid (profil perso), échange les deux codes.
3. `message send 1 Test mirador 1` — le message doit arriver sur ton téléphone.
4. `help` — 📋 colle la liste des commandes (pour savoir comment créer un
   groupe et lister les discussions).
5. Si une commande de groupe existe : crée un groupe avec toi dedans et
   envoie-y un message. 📋 dis si ça marche.

Quitter : `exit`.

## 4. Mesure de la consommation (15 min, sans rien faire)

```bash
cd /opt/olvid
for i in $(seq 1 30); do
  docker stats --no-stream --format '{{.Name}} {{.MemUsage}} {{.CPUPerc}}' olvid-daemon
  sleep 30
done | tee mesure-repos.txt
sudo du -sh data backups
docker image ls olvid/bot-daemon
```

📋 `mesure-repos.txt` (30 lignes) et les tailles.

Puis la même chose avec `JAVA_TOOL_OPTIONS: "-Xmx128m"` (modifier, puis
`docker compose up -d`, puis renvoyer un message du CLI pour vérifier) :
📋 les 30 lignes et « le message est arrivé : oui / non ».

## 5. Coupure du réseau Olvid (15 min) — à refaire

Premier essai (10/11) : le CLI était sur l'**identité 1** (invite « 1 > »)
alors que le bot et sa discussion sont sur l'**identité 2** (« 2 > »), d'où
`NOT_FOUND: Discussion not found` : le message n'est jamais parti, le test
ne dit donc rien encore. Avant de recommencer :

```bash
cd /opt/olvid
docker compose run --rm cli
```

Dans le CLI :

1. `identity --help` puis la sous-commande qui liste les identités : 📋 colle
   la liste (à quoi correspond l'identité 1 ?) ;
2. passe sur l'identité du bot (2) avec la sous-commande « current » ou
   équivalente (`identity --help` la donne) : l'invite doit afficher « 2 > » ;
3. `discussion get` : 📋 note le numéro de ta discussion en tête-à-tête ;
4. `exit`.

Puis la coupure (remplace `N` par le numéro de discussion) :

```bash
IP=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' olvid-daemon); echo "$IP"
sudo iptables -I DOCKER-USER -s "$IP" ! -d 172.16.0.0/12 -j DROP
date
docker compose run --rm cli    # vérifier « 2 > », puis : message send N Test coupure ; exit
sleep 120
docker compose logs --since 5m daemon > coupure.txt
sudo iptables -D DOCKER-USER -s "$IP" ! -d 172.16.0.0/12 -j DROP
date
sleep 180
docker compose logs --since 4m daemon > retour.txt
```

📋 :
- la réponse du CLI à `message send` pendant la coupure ;
- le message arrive-t-il sur ton téléphone après le rétablissement, et
  au bout de combien de temps ?
- `coupure.txt` et `retour.txt` (masque toute clé).

Questions en plus (premier essai) :
- 📋 avec `-Xmx128m`, le message de test est-il bien arrivé ?
- 📋 groupe : tu as été ajouté au groupe 1 en « membre en attente » ; après
  acceptation sur ton téléphone, `message send` vers la discussion du groupe
  (numéro dans `discussion get`) arrive-t-il ?
- 📋 `sudo du -sh /opt/olvid/data /opt/olvid/backups`.

## 6. Après le test

- Si tu gardes Olvid : laisse le daemon tourner. Le dossier `data/` contient
  l'identité du bot : à inclure dans tes sauvegardes (sinon il faudra refaire
  l'invitation).
- Sinon : `docker compose down && sudo rm -rf /opt/olvid`.
- Dans les deux cas, garde `client.key` et `admin.key` hors du dépôt et hors
  des messages : ce sont des secrets.
