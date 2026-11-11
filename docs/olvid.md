# Olvid avec mirador-server

Le canal Olvid de mirador parle au daemon officiel pour bots
(`olvid/bot-daemon:2.0.1`) en gRPC, sur le réseau Docker interne `obs`. Le
daemon ne publie **aucun port**. Choix et mesures : SPEC.md § 10, § 17.10,
§ 17.11.

Le daemon de test (`/opt/olvid`, `docs/test-olvid.md`) peut être gardé : il
suffit de le brancher sur le réseau `obs`.

## 0. Le plus simple : l'assistant de l'interface

1. Dans le dossier du `docker-compose.yml` de mirador, en root :

   ```bash
   grep -q OLVID_ADMIN_CLIENT_KEY_CLI .env 2>/dev/null || echo "OLVID_ADMIN_CLIENT_KEY_CLI=$(openssl rand -hex 32)" >> .env
   chmod 600 .env
   docker compose --profile olvid up -d
   grep OLVID_ADMIN_CLIENT_KEY_CLI .env
   ```

2. Interface : **Réglages › Notifications**, carte Olvid, lien « assistant
   d'installation Olvid ». Adresse `mirador-olvid:50051`, coller la clé admin,
   code 2FA. mirador crée l'identité du bot et une clé client, puis active
   Olvid. La clé admin n'est **pas** gardée.
3. Page **Olvid** : inviter le bot depuis le téléphone, échanger les codes,
   choisir la discussion. Puis choisir Olvid comme principal ou secours.

Un daemon déjà installé (ex. `/opt/olvid`, ci-dessous) marche aussi avec
l'assistant : donner son adresse (`172.30.0.21:50051`) et sa clé admin
(dans son `.env`). Les § 1 et 2 restent la méthode à la main.

## 1. Daemon sur le réseau `obs`

`/opt/olvid/docker-compose.yml` (la clé admin reste dans `.env`, 0600) :

```yaml
services:
  daemon:
    image: olvid/bot-daemon:2.0.1
    container_name: olvid-daemon
    restart: unless-stopped
    environment:
      OLVID_ADMIN_CLIENT_KEY_CLI: ${OLVID_ADMIN_CLIENT_KEY_CLI}
      JAVA_TOOL_OPTIONS: "-Xmx128m"     # ~170 Mio mesurés, envois ok
    volumes:
      - ./data:/daemon/data             # identité du bot : à sauvegarder
      - ./backups:/daemon/backups
    mem_limit: 512m
    cpus: 1
    logging:
      options: { max-size: "10m", max-file: "3" }
    networks:
      default: {}
      obs:
        ipv4_address: 172.30.0.21       # adresse libre du réseau obs, à adapter

  cli:
    image: olvid/bot-python-runner:2.0.1
    entrypoint: olvid-cli
    profiles: [cli]
    environment:
      OLVID_ADMIN_CLIENT_KEY: ${OLVID_ADMIN_CLIENT_KEY}
      OLVID_DAEMON_URL: http://daemon:50051
    stdin_open: true
    tty: true

networks:
  obs:
    external: true
```

```bash
cd /opt/olvid
docker compose up -d
docker network inspect obs -f '{{range .Containers}}{{.Name}} {{.IPv4Address}}{{"\n"}}{{end}}'
```

Vérifie que `olvid-daemon` a bien l'adresse choisie, et qu'aucun port n'est
publié : `docker port olvid-daemon` ne doit rien afficher.

## 2. Canal dans mirador

But : dire à mirador-server où est le daemon et comment s'y connecter.

**Avant** : mirador-server doit tourner sur le VPS
(`docs/installation-serveur.md`). Vérifier :

```bash
docker ps --filter name=mirador-server --format '{{.Names}} {{.Status}}'
```

Rappel : `MS` = `docker exec mirador-server /mirador-server -config /etc/mirador/server.conf`
et `MSI` = la même chose avec `docker exec -i`.

### 2.1 Les trois informations à réunir

| Information | Où la trouver | Exemple |
|---|---|---|
| Adresse du daemon | celle choisie au § 1 + port 50051 | `172.30.0.21:50051` |
| Clé client du bot | affichée par `identity new`, gardée dans `/opt/olvid/client.key` | longue suite de caractères |
| Numéro de discussion | dans le CLI : `discussion get` (le `id:`) | `1` |

Si la clé client n'a pas été gardée : dans le CLI, `key --help` montre
comment en créer une nouvelle pour l'identité du bot.

### 2.2 Écrire les réglages

1. Récupérer les réglages actuels dans un fichier :

   ```bash
   cd /root
   MS notif > notif.json
   nano notif.json
   ```

2. Dans le fichier, trouver la ligne `"canaux": {` et ajouter juste en
   dessous ce bloc (avec tes trois valeurs ; garder la virgule à la fin) :

   ```json
       "olvid": {
         "actif": true,
         "adresse": "172.30.0.21:50051",
         "cle": "COLLER-ICI-LA-CLÉ-CLIENT",
         "discussion": 1,
         "confirmation": "30s"
       },
   ```

   Ne rien changer d'autre pour le premier essai. (Si Telegram est déjà
   configuré, tu peux aussi remplacer `"secours": ""` par
   `"secours": "olvid"`.)

3. Envoyer le fichier à mirador, puis l'effacer (il contient la clé en clair) :

   ```bash
   MSI notif-reglages - < notif.json
   rm notif.json
   ```

   `fait` = accepté. Sinon, le message dit quel champ est faux.

4. Attendre 10 s (le serveur relit ses réglages), puis :

   ```bash
   MS notif-etat              # la ligne « olvid » doit apparaître, état ok
   MS notif-tester olvid      # attendu : « message de test envoyé par olvid »
   ```

   Le message « 🧪 Message de test de mirador-server » arrive sur ton
   téléphone.

📋 À coller : la sortie de `notif-etat` et de `notif-tester olvid`.

Plus tard (après l'essai du § 4), Olvid en principal et Telegram en secours :
dans `notif.json`, `"principal": "olvid"` et `"secours": "telegram"`.

## 3. Comment mirador juge un envoi Olvid

- Le daemon répond « envoyé » même s'il ne joint plus les serveurs Olvid
  (essai du 10/11). mirador attend donc la confirmation **MessageUploaded**
  (message arrivé sur le serveur Olvid) pendant `confirmation` (30 s par
  défaut).
- Sans confirmation : le message est **annulé dans le daemon** (il ne
  partira pas plus tard), l'envoi compte comme un échec, et l'alerte part par
  le canal de secours. Après 3 échecs de suite, Olvid est « en panne » et le
  secours le dit.
- La vérification régulière (chaque minute, sans message) teste que le
  daemon répond et que la clé est acceptée. Elle ne peut pas savoir si le
  daemon joint les serveurs Olvid : aucun appel de l'API ne le donne.

## 4. Essai de l'annulation (à faire une fois)

But : vérifier qu'un message annulé pendant une coupure **n'arrive pas** au
retour du réseau (sinon : doublon avec le secours).

```bash
# Confirmation courte pour l'essai : "confirmation": "10s" (notif-reglages).
IP=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{"\n"}}{{end}}' olvid-daemon); echo "$IP"
```

Le daemon a deux adresses (réseau `default` et `obs`) : bloquer la sortie
Internet des deux.

```bash
for ip in $IP; do sudo iptables -I DOCKER-USER -s "$ip" ! -d 172.16.0.0/12 -j DROP; done
date
MS notif-tester olvid          # attendu : échec ; « MS envois » montre « non confirmé … message annulé »
sleep 60
for ip in $IP; do sudo iptables -D DOCKER-USER -s "$ip" ! -d 172.16.0.0/12 -j DROP; done
date
sleep 180
MS envois -limite 5
```

📋 À coller : la sortie de `notif-tester`, celle de `envois`, et surtout :
**le message de test est-il arrivé sur ton téléphone après le retour du
réseau ?** (attendu : non). Remets ensuite `"confirmation": "30s"`.
