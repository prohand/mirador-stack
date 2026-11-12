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

Verrou progressif : chaque nouveau verrou de la même adresse ou du même
compte dure deux fois plus longtemps (15 min, 30 min, 1 h… 24 h au plus).
Tout lever : `utilisateur-secours` (§ 5).

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
| Vue d'ensemble | machines, muettes, alertes, canaux, sauvegarde, stockage, état du parc (rafraîchi chaque minute, en pause si l'onglet est caché) ; tableau des machines avec barres, tri, filtre, colonnes masquables, alertes et éléments « à valider » (§ 10) |
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

## 10. Choisir ce qui est surveillé sur chaque machine

Sur la page d'une machine, bloc « À surveiller sur cette machine »
(SPEC § 19).

- **Détecté, à valider** : ce que l'agent a trouvé et qui n'est pas encore
  surveillé (ventilateurs, pools ZFS, RAID logiciel, nouveaux services).
  Choisir « Surveiller », « Ignorer » ou laisser « Plus tard ».
- **Surveillé** : ce que la machine envoie. Un point rouge signale un
  problème. « Retirer » arrête l'envoi (et donc les alertes liées) ;
  l'historique reste.
- **Ignoré ou absent** : « Rétablir » remet la valeur par défaut.
- Taper le **code 2FA**, puis « Enregistrer les choix » (ou « Tout
  surveiller » / « Tout ignorer »). La machine applique le choix à sa
  prochaine demande (5 min au plus) ; l'agent redémarre seul.
- « Appliquer à : tout le groupe » enregistre le choix pour toutes les
  machines du groupe de configuration. Les services restent par machine.
- **Services systemd** : par défaut, tous les services démarrés au boot sont
  suivis automatiquement. Avec « Nouveaux services : me les proposer avant
  de les surveiller », ceux suivis aujourd'hui le restent, et chaque nouveau
  service apparaît dans « à valider ».
- Un agent trop ancien affiche un message : les réglages nouveaux sont
  grisés jusqu'à sa mise à jour ; le reste fonctionne.
- Retirer « Disques SMART », « Pare-feu » ou « Certificats » arrête l'envoi,
  pas l'assistant root de la machine. Pour l'arrêter aussi : `MIRADOR_SMART=non`
  (ou `MIRADOR_PAREFEU=non`) dans `/etc/mirador-agent/agent.conf`.

Vue d'ensemble :

- un clic sur « Machine », « CPU », « Mémoire », « Disque » ou « Alertes »
  trie ; un deuxième inverse ; un troisième revient à l'ordre par gravité ;
- « Filtre » cherche dans le nom, le site, le type et le groupe ;
- « Colonnes » masque des colonnes ; tri et colonnes sont retenus dans le
  navigateur.

## 11. Codes de secours et passkeys (« Mon compte »)

SPEC § 20.

**Codes de secours** (téléphone de la 2FA perdu) :

- « Mon compte » › « Codes de secours » › code 2FA › « Créer 10 nouveaux
  codes ». Ils ne sont affichés **qu'une fois** : les ranger dans le
  gestionnaire de mots de passe ou sur papier.
- À la connexion, après le mot de passe : « Téléphone perdu ? Utiliser un
  code de secours ». Un code ne sert qu'une fois.
- Une fois connecté : « Double authentification » › « Téléphone perdu » pour
  préparer une nouvelle 2FA avec un autre code de secours.
- Chaque usage envoie un message sur le canal de notification.
- Plus de code, plus de téléphone : `utilisateur-secours NOM` sur le serveur
  (`docs/administration-serveur.md`).

**Passkeys** :

- « Mon compte » › « Passkeys » › nom + code 2FA › « Continuer », puis
  « Créer la passkey sur cet appareil ».
- À la connexion, deux façons :
  - mot de passe, puis « Utiliser une passkey » à la place du code ;
  - « Se connecter avec une passkey », sans mot de passe : l'appareil
    demande alors son code PIN, l'empreinte ou le visage.
- Pour interdire la passkey seule : « Sécurité » › décocher « Autoriser la
  connexion par passkey seule ».
- Il faut ouvrir l'interface par son **nom de domaine en HTTPS** (pas par
  une adresse IP). nginx doit transmettre l'en-tête `Host` (déjà le cas avec
  la configuration du § 2).
- Une passkey ne marche que sous le domaine où elle a été créée : si le
  domaine de l'interface change, il faut la recréer.
- Le code 2FA reste demandé pour les actions sensibles.

## 12. Raccourcis et lien du token à la machine

- **Ctrl+K** (ou « Aller à… » dans le menu) : aller à une machine, un
  tableau ou une page en tapant quelques lettres.
- **Filtre des courbes** : sur un graphique de 6 courbes ou plus, le champ
  « Filtrer les courbes… » ne garde que celles dont le nom contient le texte.
- **Lier le token à la machine** (page de la machine › « Modifier la
  machine ») : le token est alors refusé s'il est copié sur une autre
  machine. À délier avant de réinstaller ou de cloner la machine, ou de
  revenir à un agent plus ancien. Si une machine liée devient muette après
  une réinstallation : « Délier », attendre une minute, « Lier ».

