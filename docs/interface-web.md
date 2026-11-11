# Interface web de mirador-server

Référence : SPEC § 18. Rien ici n'est appliqué automatiquement : tu lances
les commandes toi-même.

## 1. Premier administrateur

- Aucun compte n'existe au départ. Au démarrage, le serveur écrit un
  **jeton à usage unique** dans son journal :

  ```bash
  docker logs mirador-server 2>&1 | grep premier
  ```

- Ouvrir `https://CONSOLE/premier-admin`, coller le jeton, choisir un nom
  et un mot de passe (12 caractères au moins).
- Se connecter : la **2FA est obligatoire** ; scanner le QR code avec une
  application (Aegis, FreeOTP…) et saisir un code.
- Le jeton ne sert qu'une fois ; il change à chaque redémarrage tant
  qu'aucun compte n'existe.

## 2. nginx (vhost de la console)

Un vhost **dédié** à l'interface (port 9441). Les agents restent sur le vhost
d'ingestion (`location /api/agent/` → 9440) : ne pas mettre `/api/agent/` ici.

`/etc/nginx/conf.d/02-mirador.conf` (contexte `http`) :

```nginx
limit_req_zone $binary_remote_addr zone=mirador_connexion:1m rate=10r/m;
```

`/etc/nginx/sites-available/CONSOLE` (remplacer CONSOLE par le nom choisi) :

```nginx
server {
  listen 80;
  listen [::]:80;
  server_name CONSOLE;
  location /.well-known/acme-challenge/ { root /var/www/html; }
  location / { return 301 https://$host$request_uri; }
}

server {
  listen 443 ssl;
  listen [::]:443 ssl;
  http2 on;
  server_name CONSOLE;

  ssl_certificate     /etc/letsencrypt/live/CONSOLE/fullchain.pem;
  ssl_certificate_key /etc/letsencrypt/live/CONSOLE/privkey.pem;
  ssl_protocols TLSv1.2 TLSv1.3;
  ssl_session_cache shared:TLSmirador:2m;

  server_tokens off;
  # mirador pose déjà CSP, X-Frame-Options, nosniff, Referrer-Policy.
  add_header Strict-Transport-Security "max-age=31536000; includeSubDomains" always;
  add_header X-Robots-Tag "noindex, nofollow, noarchive" always;

  # Filtrage par adresses (à adapter) : allow / deny, geo, etc.

  client_max_body_size 2m;
  proxy_http_version 1.1;
  proxy_set_header   Host $host;                   # contrôle d'origine (CSRF)
  proxy_set_header   X-Forwarded-For $remote_addr;  # vraie adresse du client
  proxy_set_header   X-Forwarded-Proto $scheme;
  proxy_set_header   Connection "";

  location = /connexion      { limit_req zone=mirador_connexion burst=5 nodelay; proxy_pass http://172.30.0.20:9441; }
  location = /connexion/code { limit_req zone=mirador_connexion burst=5 nodelay; proxy_pass http://172.30.0.20:9441; }
  location = /mises-a-jour/publier { client_max_body_size 32m; proxy_pass http://172.30.0.20:9441; }
  location / { proxy_pass http://172.30.0.20:9441; }
}
```

- `Host $host` est **nécessaire** : sans lui, les formulaires peuvent être
  refusés (« requête refusée (origine) ») sur un vieux navigateur.
- `MIRADOR_PROXY` (fichier de démarrage) = passerelle du réseau `obs`
  (`docker network inspect obs --format '{{(index .IPAM.Config 0).Gateway}}'`) ;
  sinon tous les clients semblent venir de nginx (verrouillage commun à tous).

Vérification :

```bash
curl -sI https://CONSOLE/connexion | grep -iE '^HTTP|content-security|x-frame|strict-transport'
curl -s https://CONSOLE/sante     # {"etat":"ok"}
```

## 3. Rôles et actions sensibles

- **admin** : tout ; **lecture** : consultation seule.
- Le **code 2FA est redemandé** pour : comptes, sécurité, jetons et tokens,
  suppression de machines ou de règles, mises à jour des agents, réduction
  de la conservation, réglages des notifications et des sauvegardes, import
  de configuration.
- Changer la **liste d'adresses** ou **son propre rôle** : à confirmer dans
  les 2 minutes (bandeau rouge en haut), sinon l'ancienne valeur revient.

## 4. CrowdSec

Chaque échec est écrit dans le journal du serveur, sous une forme fixe :

```json
{"level":"WARN","msg":"console : échec de connexion","ip":"203.0.113.9","utilisateur":"admin","raison":"mot de passe faux"}
```

Un texte saisi dans le champ « Nom » qui n'a pas la forme d'un nom de compte
est écrit `"utilisateur":"(invalide)"` (un mot de passe tapé au mauvais
endroit ne va pas dans le journal).

Messages de sécurité (canal de notification principal, 1 par heure et par
sujet au plus) :

- code 2FA faux après un mot de passe juste ;
- compte verrouillé ;
- connexion réussie depuis une nouvelle adresse (aussi à la première
  connexion après un redémarrage du serveur).

Fichiers prêts (à relire et adapter) : `deploy/crowdsec/mirador-logs.yaml`
(parseur) et `deploy/crowdsec/mirador-bf.yaml` (scénario). Acquisition :

```yaml
# /etc/crowdsec/acquis.d/mirador.yaml
source: docker
container_name:
  - mirador-server
labels:
  type: mirador
```

## 5. Secours (on ne peut plus entrer)

```bash
# 2FA perdue ou mot de passe oublié : mot de passe provisoire, 2FA effacée
docker exec mirador-server /mirador-server -config /etc/mirador/server.conf utilisateur-secours admin
# liste d'adresses fausse
docker exec mirador-server /mirador-server -config /etc/mirador/server.conf console-ips-vider
# comptes
docker exec mirador-server /mirador-server -config /etc/mirador/server.conf utilisateurs
```

Effet en 10 s au plus, sans redémarrage. Un redémarrage du serveur ferme
toutes les sessions (elles sont en mémoire).

## 6. Pages

| Page | Contenu |
|---|---|
| Vue d'ensemble | machines, muettes, alertes, canaux, sauvegarde, stockage, état du parc (rafraîchi chaque minute, en pause si l'onglet est caché) |
| Machines | jeton (nom facultatif, site, type, groupe), approbation (nom corrigeable), étiquettes, renommage (historique de l'ancien nom rattaché au nouveau dans les graphiques et les logs), adresses, token, version cible, suppression ; graphiques de la machine |
| Mises à jour | versions publiées, version globale, fenêtre horaire, publication (manifeste signé + binaire) |
| Alertes | en cours, historique, détail, accusé de réception, silence en un clic ; silences, maintenances, inhibitions, dépendances ; envois ; comparaison vmalert |
| Règles | liste et santé, modèles, formulaire, test sur 24 h, import / export YAML, modifications |
| Logs | recherche bornée, répétitions repliées, palmarès ; flux bloqués |
| Tableaux | Serveurs Linux, Proxmox, OPNsense, Synology, UniFi, Certificats (convertis des tableaux Grafana), et ceux créés ici ; tous modifiables (§ 9) |
| Réglages | notifications, conservation, sauvegardes, sites / types / groupes de configuration, agents (par défaut → groupe → machine), interface (GéoIP : téléchargement à la demande, effacée si décochée), export / import |
| Utilisateurs, Sécurité, Audit | comptes, sessions, verrouillage, liste d'adresses, journal d'audit |

## 7. Olvid depuis l'interface

1. Réglages › Notifications › Olvid › **assistant d'installation**
   (docs/olvid.md § 0) : lancer le daemon (profil `olvid` du compose), coller
   sa clé admin ; mirador crée le bot et active Olvid. L'onglet « Olvid »
   n'apparaît qu'une fois Olvid activé.
2. Réglages › Olvid : dans l'application Olvid du téléphone, *ajouter un
   contact* et scanner le QR code du bot.
3. La demande apparaît dans « Invitations en cours » : **Accepter**, puis
   échanger les codes à 4 chiffres (celui affiché ici, à saisir sur le
   téléphone ; celui du téléphone, à saisir ici).
4. Choisir la discussion qui reçoit les alertes (code 2FA), puis « Tester
   Olvid » dans Réglages › Notifications.

## 8. GéoIP

- Automatique : le serveur télécharge la base DB-IP chaque mois (premier
  essai 5 min après le démarrage). Il doit pouvoir joindre
  `https://download.db-ip.com` en sortie.
- État et désactivation : Réglages › Interface.

## 9. Modifier les tableaux (admins)

- **Tableaux** › « Gérer les tableaux » : créer, dupliquer, exporter (JSON),
  importer (export Grafana ou tableau mirador), supprimer (code 2FA).
- Les 6 tableaux **fournis** sont proposés (en bas de la page Tableaux) :
  « Aperçu » puis « Installer ». Une fois installés, ils se modifient et se
  suppriment comme les autres ; « Rétablir l'original » remet la version
  livrée ; un tableau fourni supprimé est de nouveau proposé.
- Dans un tableau, « Modifier ce tableau » :
  - **glisser-déposer** les panneaux (d'une rangée à l'autre aussi) et les
    rangées par leur poignée ⠿ ; enregistré au lâcher ;
  - **largeur** de chaque panneau : auto, ¼, ⅓, ½, ⅔, ¾, pleine ;
  - rangées : ajouter, renommer, replier, supprimer ;
  - requêtes : **requête toute faite** (liste par thème, dont les
    températures), **assistant sans PromQL** (métrique, calcul, regroupement,
    filtre), et sous chaque requête une **vérification en direct** (erreur,
    explication en français, étiquettes cliquables pour la légende, unité
    proposée) ;
  - panneau : type (courbe, chiffre, barres, tableau), requêtes PromQL,
    unité, seuils de couleur, textes à la place des valeurs, colonnes ;
    bouton **Aperçu** avant d'enregistrer ;
  - propriétés : titre, description, période, filtres.
- Toute modification est vérifiée (langage PromQL restreint) et écrite dans
  l'audit. Deux onglets ouverts : la seconde modification est refusée
  (« le tableau a changé entre-temps »).
- Sur téléphone, le glisser-déposer n'est pas garanti : utiliser un
  ordinateur pour la disposition.
