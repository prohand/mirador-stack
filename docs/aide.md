# Aide détaillée de l'interface

Les aides de l'interface tiennent en une ligne (SPEC § 26). Le détail est
ici : chaque lien « En savoir plus › » de l'interface pointe vers une
section de cette page.

<a id="securite-mode-admin"></a>
## Sécurité : mode administration

- Après un code 2FA juste, le code n'est plus demandé pendant la durée
  réglée (1 min à 1 h, défaut 10 min).
- `0` désactive le mode : le code est demandé à chaque action sensible.
- La sécurité des comptes (mots de passe, 2FA, passkeys, utilisateurs)
  demande **toujours** le code, même en mode administration.
- Il s'ouvre aussi à la connexion (le second facteur vient d'être donné).
- Un bandeau en haut de page indique l'heure de fin ; « Quitter » le coupe
  tout de suite.

<a id="securite-adresses"></a>
## Sécurité : adresses autorisées

- Vide = toutes les adresses. Sinon : adresses ou préfixes séparés par des
  virgules (ex. `192.168.1.0/24, 2001:db8::/32`).
- Un changement doit être **confirmé dans les 2 minutes** (bandeau en haut
  de page), sinon l'ancienne liste revient : on ne peut pas s'enfermer
  dehors par erreur.

<a id="conservation-plafonds"></a>
## Conservation des données

- Durées : `7d`, `30d`, `720h`, `1y`…
- Plafond atteint : les données les plus anciennes partent en premier
  (blocs ou fichiers entiers, par une tâche de fond).
- Journal d'audit et historique des alertes : dans la base, purgés une
  fois par jour, sans plafond de taille (quelques Mio par an).
- Une seule durée par type de données : une durée par groupe, par machine
  ou par type de log n'est pas possible avec les moteurs de stockage
  actuels (une rétention par base, SPEC § 18.6).

<a id="installation-agent"></a>
## Installer un agent

- Adresse utilisée par le script : celle d'**ingestion** (vhost des
  agents, réglée dans Réglages › Interface), pas celle de l'interface.
- Le script télécharge le binaire de l'agent (amd64 ou arm64, détecté) avec
  le jeton et vérifie son empreinte.
- Le jeton ne sert qu'une fois et expire. Passé par la variable
  `MIRADOR_JETON`, il n'apparaît pas dans la liste des processus ; l'espace
  en début de ligne le garde hors de l'historique bash.
- La machine doit être autorisée dans le vhost d'ingestion (nginx).
- Sans curl : `wget -O mirador-agent-install.sh …`. Hors ligne :
  `--binaire FICHIER`.
- Mises à jour automatiques : option `--auto-update` (gardée sur la
  machine) ; sans elle, relancer le script pour changer de version.
- Métriques et logs se règlent ensuite par machine, par groupe ou pour
  toutes : Machines › Configuration des agents (« Métriques de la
  machine », « Logs du journal », « Logs Docker »).

<a id="machines-en-attente"></a>
## Machines en attente d'approbation

- Une ligne par machine, la plus ancienne demande d'abord. Nom, site, type
  et groupe se corrigent sur la ligne avant **Approuver**.
- Approuver donne un token à la machine : toujours **une par une**.
- Au-delà de 5 demandes : lien « Voir les N autres ».
- Deux pastilles orange, de simples **indices** (rien n'est bloqué) :
  - **nom à changer ?** : le nom vient de la machine et il est trop
    courant (`debian`, `ubuntu`, `localhost`, `raspberrypi`, `pve`,
    `vps-…`, `ip-…`…). Une alerte « debian ne répond plus » ne dirait pas
    de quelle machine il s'agit, et un second `debian` serait refusé.
  - **adresse inconnue** : aucune machine approuvée n'a été vue sur ce
    réseau (même /24 en IPv4, même /64 en IPv6). Normal pour un nouvel
    hébergeur ; si vous n'attendiez pas de machine, refusez (le jeton a
    peut-être fuité). Rien n'est signalé pour la toute première machine.

<a id="cle-publique"></a>
## Mises à jour : clé publique de signature

- Clé **publique** affichée par `mirador-release cle` sur ton poste ; la
  clé privée n'en sort jamais.
- Le script d'installation la récupère sur le serveur et l'écrit sur la
  machine dans un fichier root (`/etc/mirador-agent/cles-publiques.txt`)
  que l'agent ne peut pas modifier.
- La changer dans l'interface ne touche **pas** les machines déjà
  installées : pour elles, relancer le script avec `--cle-publique`.
- Deux clés au plus : l'actuelle et la suivante (pour changer de clé).

<a id="collecte-machine"></a>
## Machine : ce qui est relevé

- Processeur et mémoire sont toujours envoyés avec « Système ».
- Jamais listés : disques virtuels (loop, ram, dm-…), interfaces internes
  (docker, veth, lo…) et montages système. Réglage : Machines ›
  Configuration des agents.
- Les choix partent dans la configuration typée de l'agent, appliquée à sa
  prochaine demande (5 min au plus) ; aucune commande n'est envoyée.
- « Retirer » arrête l'envoi et donc les alertes liées ; l'historique reste.
  Chaque changement est inscrit dans le journal d'audit.

<a id="certificats-agent"></a>
## Machine : certificats surveillés

- Toujours cherchés (fichiers locaux de la machine) :
  `/etc/letsencrypt/live/*/cert.pem`, `/etc/ssl/local/*.crt`,
  `/etc/ssl/local/*.pem`, `/etc/pve/local/pveproxy-ssl.pem`,
  `/etc/proxmox-backup/proxy.pem`.
- Chemins ajoutés : sous `/etc`, `/opt`, `/srv`, `/var/lib` ou
  `/usr/local` ; nom en `.pem`, `.crt`, `.cer` ou `.cert` ; jamais une clé
  privée ; 3 jokers `*` au plus ; 20 chemins.
- Lus au prochain passage de l'assistant root (15 min au plus).
- Une machine peut refuser ces chemins avec
  `MIRADOR_CERTIFICATS_DISTANTS=non` dans son fichier local.

<a id="unifi"></a>
## Collecteur : contrôleur UniFi

- Compte local en **lecture seule** : UniFi › Admins › ajouter, rôle
  « View Only », accès local seulement. Ou une clé API (UniFi OS).
- Empreinte du certificat (certificat fait maison), depuis le collecteur :

  ```bash
  openssl s_client -connect IP:443 </dev/null 2>/dev/null | openssl x509 -noout -fingerprint -sha256
  ```

  Les « : » sont retirés tout seuls.
- Mots de passe et clé API : chiffrés sur le serveur, jamais réaffichés.

<a id="modele-listes"></a>
## Modèles personnels : listes à tester

- Chaque liste se remplit ensuite par groupe ou par machine, une valeur par
  ligne.
- Chaque ligne devient un test de l'agent, étiqueté `modele="<nom>"`.
- Plusieurs champs sur une ligne : les séparer par « | ».

<a id="modele-regles"></a>
## Modèles personnels : règles

- Expression PromQL (même lecteur que la page « Règles »).
- Mesures des tests : `mirador_check_ok`, `mirador_check_duree_secondes`,
  `mirador_check_http_code`… avec l'étiquette `modele="…"`.
- « Remplace » : nom d'une règle importée qui se tait sur les machines du
  modèle (évite les alertes en double).

<a id="heartbeat"></a>
## Notifications : heartbeat

- Un simple GET vers l'adresse réglée, à chaque intervalle.
- Indépendant des canaux : il s'arrête si le serveur ne peut plus évaluer
  ses règles, et le service externe te prévient alors.
- Service sur ton réseau local (Uptime Kuma…) : cocher « Autoriser … une
  adresse privée » plus haut dans la page.

<a id="panneau-requetes"></a>
## Tableaux : requêtes d'un panneau

- Une ligne vide est ignorée ; enregistrer ajoute deux lignes libres
  (12 requêtes au plus).
- Variables : `$__rate_interval`, et les filtres du tableau
  (`instance=~"$instance"`).
- Légende : `{{instance}}`, `{{device}}`…

<a id="application"></a>
## Notifications : application (push)

- **Ce que c'est** : les alertes arrivent en notification sur le téléphone
  ou le navigateur, et l'icône de l'application porte le nombre d'alertes
  en cours (pastille).
- **Deux réglages** (Réglages › Notifications › Application) :
  - *Actif* : le canal existe ; il peut être choisi comme canal principal
    ou de secours ;
  - *Recevoir aussi les alertes en notification sur les appareils, en plus
    de Telegram* (le nom du canal principal s'affiche) : quand un autre
    canal porte le message, les appareils le reçoivent aussi. Ces
    notifications en plus ne comptent pas pour la bascule.
- **Chaque personne abonne ses appareils** dans « Mon compte » ›
  Notifications sur cet appareil (code 2FA ou mode administration). Dix
  appareils au plus par compte.
- **iPhone, iPad** : installer d'abord mirador sur l'écran d'accueil
  (Partager › Sur l'écran d'accueil), puis activer depuis l'application
  (iOS 16.4 ou plus récent).
- **Par où passe le message** : par le service de push du navigateur
  (Google, Apple, Mozilla, Microsoft). Il est chiffré pour l'appareil : ce
  service ne peut pas le lire. Il connaît l'adresse de l'interface et
  l'heure des envois.
- **Le serveur doit pouvoir sortir en HTTPS** vers `fcm.googleapis.com`,
  `*.push.apple.com`, `*.push.services.mozilla.com` et
  `*.notify.windows.com` (pare-feu du serveur).
- **Pastille** : mise à jour à chaque notification, et à chaque ouverture
  de l'application. Sans notification (mode ombre, case « Recevoir aussi les alertes… » décochée), elle
  ne bouge qu'à l'ouverture.
- **Un appareil ne reçoit plus rien** : « Envoyer une notification
  d'essai » ; sinon le retirer et le réabonner. Un abonnement refusé par le
  service (application désinstallée) est effacé tout seul.
- **Serveur restauré sans sa base, ou clé des secrets changée** : la clé
  des notifications change, tous les appareils doivent être réabonnés.


## Changements

- **Ce que c'est** : bloc de la page d'une machine (SPEC § 47) : ce qui a
  changé sur 7 jours :
  - paquets installés ou mis à jour (lus dans `/var/log/dpkg.log` par
    l'agent) ;
  - redémarrages et noyau ;
  - version de l'agent ;
  - conteneurs recréés ou redémarrés à la main ;
  - réglages faits dans mirador pour la machine.
- **Affichage** : les 5 plus récents ; les autres repliés (« N plus
  anciens »). Les changements identiques du même jour sont regroupés (×N).
  Un détail long (liste de paquets) se déplie d'un clic.
- **Sur les courbes** : traits pointillés (survol pour le nom).
- Ouvert d'office s'il y a eu un changement depuis 24 h.

## Sondes

- **Ce que c'est** : un essai régulier d'un service, comme le ferait un
  utilisateur (SPEC § 48) :
  - *HTTP(S)* : la page répond-elle avec le bon code (200 à 399 par défaut),
    et contient-elle le texte attendu ;
  - *TCP* : le port est-il ouvert (SSH, base de données…) ;
  - *Ping* : la machine répond-elle ;
  - *DNS* : le serveur DNS répond-il pour un nom (et avec la bonne adresse).
- **Faite depuis** :
  - un *collecteur* : services du LAN (Home Assistant, NAS, box…) ;
  - le *serveur mirador* : vue de l'extérieur (Nextcloud, site public…).
    Adresses locales et privées refusées.
- **Alertes fournies** : *SondeEnEchec* (deux essais ratés de suite),
  *SondeLente* (plus lente que le seuil pendant 5 min). La raison d'un
  échec est sur la page Sondes.
- **Ping depuis un collecteur** : autorisé sans droits par le réglage
  `net.ipv4.ping_group_range` (ouvert par défaut sur Debian 12 et Ubuntu).
  Vérifier : `sysctl net.ipv4.ping_group_range` doit afficher
  `0 2147483647`.
- **Modifier, renommer** : lien « Modifier » (page Sondes, bloc Sondes de
  la machine) ou fiche de la sonde. Le nouveau nom suit dans les services et
  les étiquettes ; l'historique des mesures repart sous le nouveau nom.
- **Collecteur ancien** : il ne connaît pas les sondes ; elles lui sont
  retirées de sa configuration jusqu'à sa mise à jour.
- **Depuis la page d'une machine** (bloc *Sondes*, SPEC § 60) :
  - *Activer le ping* : fait depuis le collecteur du site de la machine,
    choisi d'office ; si la machine n'a pas de site, choisir le site (le
    collecteur) dans la liste ; cocher une ou plusieurs adresses données par
    l'agent (celle de la route par défaut en premier), ou en taper d'autres
    (séparées par des virgules) : un ping par adresse ;
  - *Sondes proposées* : l'agent liste les ports ouverts et essaie chacun
    une fois, en local, pour savoir s'il parle HTTPS, HTTP ou SSH ; cocher
    et *Ajouter les sondes cochées* ;
  - HTTPS par adresse IP : certificat non vérifié (il ne peut pas
    correspondre à une adresse) ; pour vérifier le certificat : « Modifier »
    (fiche de la sonde), viser le nom du site et décocher « Ne pas
    vérifier » ;
  - un agent ancien (avant 0.3.9) ne donne pas ses adresses : le ping
    reste possible en tapant l'adresse.
