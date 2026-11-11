Tu es mon assistant de développement. Je suis administrateur systèmes et réseau. Réponds en français, simplement, sans longue introduction, avec des listes courtes.

## Projet : mirador-stack
Écrire en Go un outil de supervision perso, que je peux modifier moi-même, pour remplacer à terme ma stack actuelle (Grafana Alloy + VictoriaMetrics + VictoriaLogs + vmalert + Alertmanager + Grafana). Mode push uniquement.
- Dépôt et module Go : mirador-stack. Binaires : mirador-agent (sur les machines), mirador-server (sur le VPS) et mirador-release (signature des versions de l'agent).
- Utilise ce nom partout (code, documentation, interface, service systemd, messages).

## Ma stack actuelle (référence, dossier reference/)
- Agents Grafana Alloy installés par obs-agent-install.sh, qui envoient en HTTPS vers un VPS OVH (nginx en entrée : token par machine, liste d'IP autorisées).
- 72 alertes dans system.yml et equipements.yml (règles vmalert), routage dans alertmanager.yml (Telegram, heartbeat HetrixTools "Watchdog").
- Sources sans agent : Synology (SNMP v3), OPNsense (Telegraf), Proxmox (metric server), UniFi (collecteur LAN), syslog.
- Labels utilisés : host, site, env, job, unit, container.

## Inspiration
- Le dépôt https://github.com/midnights-ra1n/nyx-stack (Rust, GPL-3.0) sert d'inspiration pour l'architecture, l'organisation de l'interface et les idées de fonctionnalités.
- Lis-le pour comprendre, mais NE COPIE PAS de code : tout est réécrit en Go, avec ma propre conception. Si tu penses qu'un passage mérite d'être repris tel quel, demande-moi d'abord (licence GPL-3.0).
- À ne PAS reprendre : l'exécution de commandes à distance sur les agents. Chez moi, l'agent n'exécute jamais de commande à distance.
- S'il t'est impossible d'accéder à ce dépôt, dis-le-moi et je te collerai les passages utiles.
- Dans SPEC.md, liste ce que tu retiens de Nyx et ce que tu écartes, avec la raison.

## Contraintes techniques
- Go uniquement (agent et serveur), binaires statiques, dépendances minimales.
- Push uniquement : aucune connexion entrante vers mes machines. L'agent n'ouvre aucun port.
- L'agent n'accepte JAMAIS d'ordre arbitraire (pas de shell, pas de commande, pas de script reçu). Il envoie ses données et interroge le serveur en sortie (HTTPS) uniquement pour : récupérer sa configuration (valeurs typées et validées, jamais de commande) et récupérer des mises à jour signées (voir section dédiée).
- Authentification par token, TLS.
- Pas de VPN. Je déploie à la main avec des scripts (ni Ansible ni Semaphore).
- Code simple, lisible, commenté en français, que je peux maintenir seul.
- Compilation croisée linux/amd64 et linux/arm64.

## Performance : impact minimum partout
Le moindre coût en RAM, CPU et disque (I/O) est une exigence de premier rang, sur l'agent comme sur le serveur. Chaque choix technique doit être justifié par rapport à ça dans SPEC.md.

Agent :
- Objectifs chiffrés à valider : moins de 30 Mio de RAM résidente et moins de 1 % d'un cœur en moyenne. Plafond dur à 150 Mio (limite systemd MemoryMax).
- Priorité de service basse : Nice=19, CPUWeight bas, IOSchedulingClass=idle.
- Lecture directe de /proc et /sys, sans lancer de commandes externes quand c'est évitable. Pas de fork répété.
- Collecte à intervalle raisonnable (par défaut 30 s à 60 s), et intervalles plus longs pour les données qui changent peu (SMART, mises à jour, certificats).
- Envoi groupé (batch), compressé, avec un tampon mémoire borné. Si le serveur est injoignable : tampon borné avec abandon des plus anciens points, jamais de croissance sans limite, jamais d'écriture disque intensive.
- Logs du journal : lecture en flux, filtrage à la source, pas de relecture complète, pas de buffer illimité.
- Aucune écriture disque en régime normal (ou très rare et groupée).
- Pas de réflexion lourde, pas d'allocations inutiles dans les boucles de collecte.
- La récupération de configuration et la recherche de mise à jour se font rarement (ex. toutes les 5 à 10 minutes avec un délai aléatoire), avec requêtes conditionnelles (ETag) pour ne rien transférer si rien n'a changé.

Serveur :
- SQLite en mode WAL, écritures groupées par lots (transactions), index limités à ce qui sert vraiment.
- Données agrégées et purgées automatiquement (rétention configurable), VACUUM planifié hors heures de pointe.
- Cache mémoire borné, requêtes de lecture paginées, pas de chargement complet en mémoire.
- Moteur d'alertes : évaluation incrémentale, pas de rescan complet de l'historique à chaque cycle.
- Interface web : fichiers statiques embarqués dans le binaire, compressés, mise à jour des graphiques par requêtes légères, pas de rafraîchissement agressif.

Vérification obligatoire :
- Des benchmarks Go (testing.B) et des mesures (pprof, mémoire, allocations) pour la collecte, l'envoi, l'insertion en base et l'évaluation des alertes.
- Un test de charge simulant 50 agents pendant plusieurs minutes, avec rapport de la RAM, du CPU et des I/O mesurés.
- Pour chaque étape, indique dans PROGRES.md la consommation mesurée et compare-la aux objectifs. Si un objectif n'est pas tenu, signale-le et propose des solutions.

## Stockage (choix à justifier dans SPEC.md)
- SQLite (modernc.org/sqlite, pur Go, mode WAL) pour : config, règles, silences, utilisateurs, tokens, historique des alertes, journal d'audit.
- Métriques : Prometheus TSDB (bibliothèque Go) en première option. Mesure sa RAM avec 50 machines (~500 séries chacune, intervalle 30 s) et dis-moi si elle respecte mes objectifs.
- Logs : comparatif chiffré avant de choisir entre (a) segments zstd + petit index (machine, unité, niveau, temps), (b) SQLite FTS5. Mesure RAM, I/O, taille disque et vitesse de recherche sur un jeu de logs simulé.
- Tout le stockage est derrière une interface Go (écrire, lire, purger), pour pouvoir changer de moteur, y compris brancher VictoriaMetrics/VictoriaLogs comme stockage.
- Rétention configurable depuis l'interface web (voir étape 5), jamais figée dans le code.
- Le moteur de stockage doit permettre de changer la rétention à chaud (sans redémarrage) et de supprimer par blocs entiers (par période), pas ligne par ligne, pour limiter l'I/O. Si le moteur choisi ne le permet pas, signale-le dans SPEC.md.

## Configuration : un maximum de choses depuis l'interface web
Principe : tout ce qui n'est pas strictement nécessaire au démarrage se règle dans l'interface, jamais dans le code ni dans un fichier.
- Reste dans un fichier (démarrage) : adresse d'écoute, TLS, chemin de la base, clé de chiffrement des secrets. Rien d'autre.
- Configurable dans l'interface, au minimum :
  - Machines : approuver, renommer (nom, site, env, groupe), supprimer avec ou sans historique, générer/révoquer le token, groupes et étiquettes.
  - Agents (par défaut global, avec surcharge par groupe ou par machine) : intervalles de collecte, collecteurs activés ou non, exclusions (unités systemd, points de montage, interfaces réseau, conteneurs), filtres et niveaux de logs, masquage de données sensibles, tailles de tampon, intervalle de récupération de configuration.
  - Alertes : règles, seuils, durées, gravités, modèles, silences, inhibitions, dépendances, fenêtres de maintenance, plages horaires silencieuses.
  - Notifications : canaux (Telegram, Olvid, ntfy, e-mail), canal principal et canal de secours (inversables), destinataires, routage par gravité, groupe ou machine, réglages de bascule, heartbeat HetrixTools (URL, intervalle), format des messages, test d'envoi.
  - Conservation des données (voir étape 5).
  - Équipements sans agent : cibles SNMP, sources Proxmox, OPNsense, UniFi, syslog.
  - Quotas par agent et limites (voir section dédiée).
  - Sauvegardes : fréquence, destination, rétention.
  - Sécurité : durée des sessions, seuils de verrouillage, liste d'IP autorisées, politique 2FA, utilisateurs et rôles.
  - Interface : thème, langue, vues et tableaux de bord personnalisables, fuseau horaire, unités, mode debug temporaire.
- Chaque réglage a : une valeur par défaut, des bornes validées, un bouton « rétablir la valeur par défaut », et une entrée dans le journal d'audit (qui, quand, ancienne et nouvelle valeur).
- Les changements s'appliquent à chaud, sans redémarrage.
- Les réglages qui peuvent m'enfermer dehors (liste d'IP, 2FA, rôles) demandent une confirmation puis s'annulent automatiquement au bout de 2 minutes si je ne les confirme pas depuis la nouvelle configuration.
- Les secrets (jeton Telegram, URL HetrixTools, communautés SNMP, clés Olvid) se saisissent dans l'interface, sont stockés chiffrés, ne sont jamais ré-affichés (écriture seule) ni journalisés.
- Toute la configuration est exportable et importable en YAML (sans les secrets, ou avec secrets chiffrés au choix).
- La configuration envoyée aux agents est strictement typée et validée. Elle ne contient jamais de commande, de chemin d'exécutable ni de script. Les commandes externes éventuelles (ex. smartctl) ont un chemin et des arguments figés dans le code.

## Notifications : plusieurs canaux et canal de secours
Contexte : une fois, mes alertes Telegram ont été bloquées par mon propre pare-feu sans que je m'en rende compte. Le système doit être fiable même si un canal tombe.
- Les canaux sont des modules derrière une interface Go commune : Telegram (existant, groupe), Olvid, ntfy, e-mail (SMTP). On peut en ajouter d'autres sans toucher au reste.
- Le canal principal et le canal de secours sont des réglages de l'interface, inversables en un clic. Au départ : Telegram principal. À terme, je veux pouvoir mettre Olvid en principal et Telegram en secours, sans modifier le code.
- Olvid (messagerie sécurisée française) : utiliser le daemon officiel pour bots (image Docker olvid/bot-daemon, API gRPC, description protobuf publiée dans le dépôt Olvid-Bot-Protobuf). Génère un client Go à partir du protobuf. Lis la documentation officielle (doc.bot.olvid.io) pour relever le port, l'authentification, la licence, la création de l'identité du bot et l'envoi à un contact ou à un groupe. Je n'ai pas vérifié ces points : ne suppose rien. Mesure la RAM et le CPU du daemon et inclus-les dans le budget. Si le coût est trop élevé ou si l'usage est trop contraignant, dis-le-moi et propose une alternative.
- Surveillance de l'état de chaque canal, en continu :
  - Olvid : vérification régulière (ex. toutes les 30 à 60 s) que le daemon répond et que le bot est connecté aux serveurs Olvid. Un échec d'envoi compte aussi comme une panne, car le daemon peut tourner alors que le réseau Olvid est inaccessible.
  - Telegram : vérification de joignabilité de l'API sans envoyer de message (ex. toutes les 5 minutes), pour détecter un blocage réseau ou pare-feu avant d'en avoir besoin.
- Bascule automatique : si le canal principal est jugé en panne (N vérifications ou envois en échec, réglable), les alertes partent sur le canal de secours. Le message de secours mentionne le canal défaillant.
- Dès que le canal principal tombe, un message « canal principal en panne » est envoyé sur le canal de secours (ex. Olvid principal en panne, alerte sur Telegram). Un message de retour à la normale est envoyé quand il revient, puis les alertes reprennent sur le principal.
- Si le canal de secours tombe à son tour, une alerte part sur le canal principal. Si tous les canaux sont en panne, les alertes sont gardées en file d'attente (bornée, sur disque léger) et renvoyées dès qu'un canal revient, avec l'heure d'origine.
- Anti-rebond : la bascule et le retour demandent plusieurs vérifications consécutives (réglables) pour éviter les allers-retours, et les messages de panne ne sont pas répétés en boucle.
- Les canaux sont optionnels, chacun peut être activé ou non dans l'interface.
- Routage par gravité (ex. critique sur plusieurs canaux en même temps, information sur un seul).
- Bouton « envoyer un test » pour chaque canal, et historique des envois (réussi, échec, canal utilisé, durée).
- Messages avec un lien direct vers l'alerte dans l'interface, et un contenu clair (machine, gravité, valeur, durée, depuis quand).
- Le heartbeat externe (HetrixTools) reste indépendant des canaux : il me prévient si le serveur lui-même tombe.

## Supervision de la supervision
- Le serveur surveille sa propre santé : RAM, disque, retard d'ingestion, erreurs d'écriture, taille de la base, durée d'évaluation des alertes, état des canaux de notification, expiration de son certificat.
- Le serveur et l'agent exposent leurs propres métriques internes (consommation, erreurs, files d'attente), protégées par authentification.
- Le heartbeat externe (HetrixTools, URL configurable dans l'interface) est conservé : si le serveur meurt, je suis prévenu de l'extérieur.
- Si un agent cesse de répondre, c'est détecté par le serveur. Si le serveur lui-même ne peut plus évaluer ses règles, le heartbeat s'arrête et je suis prévenu.
- Mode debug activable temporairement depuis l'interface (retour automatique au mode normal après une durée), logs structurés avec niveaux.

## Bascule depuis ma stack actuelle
- Les deux systèmes fonctionnent en parallèle pendant la migration. Ma stack actuelle reste en place tant que je ne l'ai pas validée.
- Mode « ombre » : mirador évalue les alertes sans envoyer de notification, et un écran compare ses alertes à celles de vmalert (alertes présentes des deux côtés, manquantes, en trop), avec un rapport exportable.
- Import des 72 règles existantes (system.yml, equipements.yml) vers le format de mirador, avec rapport des règles qui n'ont pas d'équivalent exact.
- Possibilité d'importer les noms de machines, sites, envs et labels depuis ma stack actuelle.
- Aucune coupure de l'ancienne stack sans ma validation explicite.

## Sauvegarde et restauration
- Sauvegarde cohérente à chaud de la configuration et de la base SQLite, planifiée et déclenchable depuis l'interface, chiffrée, avec rétention configurable et envoi vers une destination que je choisis (dossier local, SFTP ou S3).
- Les métriques et logs sont sauvegardables en option (volumineux), la configuration toujours.
- Procédure de restauration documentée et testée automatiquement dans la CI (sauvegarde, restauration sur une instance vide, vérification).
- Alerte si la dernière sauvegarde est trop ancienne ou a échoué.

## Enrôlement des machines
- Une nouvelle machine apparaît dans l'interface « en attente d'approbation » après avoir utilisé un token d'enrôlement à usage unique et de durée limitée, généré dans l'interface.
- Une fois approuvée, la machine reçoit son token définitif (haché côté serveur, révocable). Le nom, le site, l'env et le groupe se définissent dans l'interface, et se modifient en un clic (plus besoin de relancer un script ni de modifier une liste de tokens dans nginx).
- Le script d'installation de l'agent reste simple : une commande avec le token d'enrôlement et l'adresse du serveur, sans autre choix à faire.
- nginx n'a plus qu'un rôle de reverse proxy (TLS, liste d'IP, limites), plus de correspondance token vers nom.

## Mise à jour des agents depuis l'interface (agents non Docker)
Les agents Docker se mettent à jour par leur image : hors périmètre. Le serveur n'est pas mis à jour par ce mécanisme.
Principe : l'agent interroge le serveur en sortie. Le serveur n'envoie jamais rien à l'agent de sa propre initiative.
- Signature : chaque version de mirador-agent est signée en Ed25519. La clé privée reste chez moi, hors serveur et hors dépôt. La clé publique est intégrée dans le binaire de l'agent (avec possibilité d'une seconde clé pour la rotation). Le serveur vérifie aussi la signature avant d'accepter une version.
- Publication : l'outil en ligne de commande mirador-release signe et prépare une version, que j'envoie dans l'interface (ou que je dépose sur le serveur). Le serveur ne signe jamais lui-même.
- Désactivé par défaut : le script d'installation de l'agent propose --auto-update. Sans cette option, l'agent ne cherche jamais de mise à jour.
- Fonctionnement de l'agent : il demande au serveur quelle version lui est assignée. Si elle est plus récente que la sienne : téléchargement dans un dossier temporaire, vérification de la somme SHA-256 et de la signature, refus de toute version plus ancienne (anti-retour arrière, sauf instruction signée explicite), test du nouveau binaire (--self-test), remplacement atomique, redémarrage par systemd.
- Retour arrière automatique : l'ancien binaire est conservé. Si le nouvel agent ne se signale pas au serveur dans les minutes qui suivent, l'ancien est restauré et l'échec est reporté.
- Installation : le script d'installation prépare un dossier appartenant à l'utilisateur du service (ex. /var/lib/mirador-agent/bin, avec un lien « current » utilisé par l'unité systemd) pour que l'agent puisse se mettre à jour sans être root si possible. Documente dans SPEC.md le compromis choisi.
- Dans l'interface : version de chaque agent, version cible par machine ou par groupe, bouton « mettre à jour », déploiement par étapes (d'abord une machine test, puis le reste), fenêtre horaire autorisée, historique, bouton « revenir à la version précédente », possibilité de bloquer les mises à jour sur une machine, alerte si un agent est en retard de version ou si une mise à jour a échoué.
- Journal d'audit de toute action de mise à jour.

## Alertes plus intelligentes
- Dépendances entre équipements : je peux déclarer qu'une machine dépend d'une autre (ex. les VM dépendent de Proxmox, un site dépend de son OPNsense). Si le parent est en panne, les alertes des enfants sont regroupées ou supprimées automatiquement pour éviter le déluge.
- Fenêtres de maintenance planifiées (ponctuelles ou récurrentes), par machine ou groupe, avec reprise automatique.
- Accusé de réception (ack) d'une alerte, avec commentaire, depuis l'interface (et, si le canal le permet, depuis la notification).
- Regroupement et déduplication des notifications, rappels périodiques configurables tant qu'une alerte n'est pas acquittée.
- Chaque alerte garde son historique : début, fin, durée, valeurs, qui l'a acquittée.

## Protection contre les débordements
- Quotas par agent, réglables : nombre maximum de séries, volume de logs par minute, taille des requêtes. Au-delà, les données excédentaires sont refusées ou échantillonnées, et une alerte est émise.
- Limitation de débit par token et par IP sur les points d'entrée.
- Limite de cardinalité des labels (une étiquette qui explose ne doit pas saturer le stockage).
- Détection du décalage d'horloge d'une machine (comparaison avec l'heure du serveur), avec alerte, et horodatage corrigé ou rejeté si le décalage est trop grand.

## Données sensibles dans les logs
- Masquage à la source, dans l'agent, avant l'envoi : mots de passe, tokens, clés API, en-têtes d'autorisation, numéros de carte, adresses e-mail (au choix).
- Règles de masquage configurables dans l'interface (modèles prêts à l'emploi + expressions personnalisées avec limites de coût pour éviter les expressions abusives).
- Test du masquage dans l'interface (je colle une ligne, je vois le résultat).

## Compatibilité et évolution
- Le protocole agent-serveur est versionné. Un ancien agent doit continuer à fonctionner avec un serveur plus récent pendant une période de compatibilité documentée, et le serveur signale les agents trop anciens.
- Migrations de la base SQLite automatiques, versionnées, testées, avec sauvegarde automatique avant migration et possibilité de revenir en arrière.
- Les formats d'export YAML sont versionnés.

## Sécurité de l'interface web (exigence de premier rang, accessible depuis Internet)
- Mot de passe Argon2id + 2FA obligatoire (TOTP ou WebAuthn). Aucun compte par défaut : le premier admin se crée avec un jeton à usage unique affiché en console.
- Limitation des tentatives, verrouillage temporaire, journal des échecs de connexion dans un format lisible par CrowdSec.
- Rôles lecture seule et admin. Journal d'audit de toutes les modifications. Les actions sensibles (mise à jour d'agents, suppression de données, changement de sécurité) demandent une confirmation et une nouvelle saisie du 2FA.
- Cookies HttpOnly, Secure, SameSite=Strict, sessions courtes stockées côté serveur, rotation, protection CSRF.
- Ports/listeners séparés pour les agents (token) et pour l'interface. Prévu derrière nginx (TLS 1.3, HSTS, liste d'IP optionnelle).
- CSP stricte sans script inline ni CDN externe (tout embarqué), X-Frame-Options, nosniff, Referrer-Policy.
- Les logs affichés sont du contenu non fiable : échappement systématique (XSS). Requêtes SQL paramétrées. Limites de taille et de durée sur la recherche de logs.
- Tokens d'agents hachés, un par machine, révocables. Secrets chiffrés au repos, jamais affichés ni journalisés.
- Règles d'alertes en langage restreint, sans exécution de code. Webhooks protégés contre le SSRF.
- Un serveur compromis ne doit pas pouvoir prendre le contrôle des agents : aucun binaire non signé n'est jamais installé, et la configuration envoyée aux agents ne peut contenir aucune commande.
- Processus non-root, durcissement systemd (NoNewPrivileges, ProtectSystem=strict, etc.).
- Interface utilisable sur téléphone (mise en page adaptative), car je consulte les alertes depuis mon mobile.
- CI : go vet, gosec, govulncheck, tests de sécurité (en-têtes, accès sans session, CSRF, signature invalide refusée, version plus ancienne refusée, configuration malformée refusée), dépendances figées.
- Chaque étape décrit son modèle de menaces dans SPEC.md.

## Qualité et documentation
- Tests de robustesse (fuzzing Go) sur tout ce qui lit des données externes : syslog, SNMP, journald, formats Telegraf, entrées de l'interface.
- Tests de bout en bout : un agent simulé, un serveur, des alertes, des notifications vers un faux canal.
- Documentation d'exploitation en français dans docs/ : installation du serveur et de l'agent, mise à jour, sauvegarde et restauration, rotation des clés et des tokens, bascule depuis l'ancienne stack, dépannage courant (canal de notification en échec, agent muet, disque plein), architecture.
- Builds reproductibles autant que possible, liste des dépendances (SBOM) générée à chaque version.

## Règles de travail (importantes)
- Tu tournes dans un environnement cloud isolé : tu n'as aucun accès à mon infrastructure et tu ne dois jamais essayer. Tu ne déploies, n'installes ni ne modifies rien chez moi. Tu analyses et tu proposes, je valide.
- Pour tester sur mes machines, écris-moi les commandes à lancer, je te colle les sorties dans une nouvelle session. Pour tes propres tests, utilise des simulations (serveur de test local, fichiers d'exemple).
- Travaille sur une branche par étape (ex. etape-1-cahier-des-charges), fais des commits clairs, ouvre une pull request. Ne pousse jamais directement sur main.
- Tu perds le contexte entre deux sessions. Maintiens à la racine :
  - CLAUDE.md : ces règles de travail, avec un renvoi vers docs/BRIEF.md (relis les deux à chaque session),
  - SPEC.md : cahier des charges et décisions prises,
  - PROGRES.md : état d'avancement et prochaine étape.
- Travaille par étapes. Chaque étape doit fonctionner seule et être validée par moi avant la suivante.
- Écris des tests unitaires et lance-les avant de me présenter le résultat.
- Une étape n'est pas terminée tant que sa consommation RAM/CPU/I/O n'a pas été mesurée et reportée dans PROGRES.md.
- Les secrets (tokens, clés, URL privées) ne sont jamais écrits dans le code, les tests, les logs ni les commits. La clé privée de signature n'existe jamais dans le dépôt ni dans tes sessions.

## Plan
1. Cahier des charges : lis reference/, dresse la liste de ce qu'il faut garder (métriques, 72 alertes, dashboards, sources, labels). Écris SPEC.md et présente-le-moi. NE CODE RIEN avant ma validation.
2. Agent Go (mirador-agent) : collecte système (CPU, mémoire, swap, disque, réseau, systemd, uptime), Docker, SMART, certificats, mises à jour en attente, logs du journal avec masquage des données sensibles. Il envoie vers VictoriaMetrics et VictoriaLogs avec des métriques compatibles node_exporter, pour que mes dashboards et alertes actuels continuent de marcher. Prévois dès cette étape la structure de configuration distante (valeurs typées), l'enrôlement, le protocole versionné et le mécanisme de mise à jour signée, mais laisse-les désactivés tant que le serveur n'existe pas. Test sur une seule machine, en parallèle d'Alloy.
3. Serveur Go (mirador-server) et stockage : points bruts quelques jours, puis agrégés 30 jours, logs avec recherche, derrière mon nginx. Ajoute l'enrôlement des machines, la distribution de configuration et des mises à jour signées (avec mirador-release), les quotas, la détection de décalage d'horloge, les migrations de base, la sauvegarde/restauration et la supervision interne.
4. Moteur d'alertes en YAML (seuil, durée, absence de données, tendance), dépendances entre équipements, fenêtres de maintenance, accusés de réception, silences, inhibitions, mode « ombre » avec comparaison à vmalert, import des 72 règles existantes. Notifications multi-canaux (Telegram, Olvid, ntfy, e-mail), heartbeat HetrixTools. Les règles sont stockées dans la base (ou des fichiers versionnés) et rechargées sans redémarrage.
   Les canaux principal et secours sont inversables dans l'interface (Telegram principal au début, Olvid principal ensuite), avec surveillance de chaque canal et bascule automatique testée (simulation de panne dans les tests de bout en bout).
5. Interface web : vue d'ensemble, détail machine, alertes, logs, et configuration de tout ce qui est listé dans « Configuration », « Notifications », « Enrôlement » et « Mise à jour des agents ».
   Gestion des alertes depuis l'interface, facilement :
   - créer, modifier, activer/désactiver et supprimer une règle sans écrire de YAML (formulaire : métrique, machine ou groupe, seuil, durée, gravité, destinataire),
   - modèles prêts à l'emploi (CPU, mémoire, disque, service systemd, conteneur, certificat, machine muette),
   - bouton « tester la règle » qui montre quelles machines elle aurait déclenchées sur les dernières 24 h, avant de l'enregistrer,
   - création d'un silence en un clic depuis une alerte, accusé de réception, fenêtre de maintenance,
   - historique des alertes et des modifications de règles (qui, quand),
   - import/export en YAML pour sauvegarde et suivi dans Git.
   Gestion de la conservation des données depuis l'interface (réservée au rôle admin) :
   - durée de conservation des métriques brutes, des métriques agrégées et des logs, réglable séparément,
   - taille maximale du stockage (métriques et logs) : quand elle est atteinte, les plus anciennes données sont supprimées en premier,
   - réglages par défaut globaux, avec possibilité de les adapter par groupe ou par machine, et par type de log (ex. journal systemd, docker, syslog),
   - affichage de l'état réel : espace disque utilisé, nombre de séries, volume de logs par jour, date de la donnée la plus ancienne,
   - estimation avant validation : « avec ces réglages, le stockage utilisera environ X Go » (calculée d'après le volume réel mesuré),
   - purge automatique appliquée sans redémarrage, par tâche de fond à faible priorité (pas de pic de CPU ni d'I/O),
   - garde-fous : valeurs minimales et maximales, confirmation avant toute réduction qui supprimerait des données, jamais de suppression immédiate sur un simple clic (suppression différée avec possibilité d'annuler quelques minutes),
   - chaque changement est inscrit dans le journal d'audit (qui, quand, ancienne et nouvelle valeur),
   - alerte automatique si le disque se remplit plus vite que prévu,
   - réglages exportables en YAML avec le reste de la configuration.
6. Équipements sans agent : SNMP v3, Telegraf, metric server Proxmox, UniFi, syslog TLS.
7. Bascule : période de fonctionnement en parallèle, rapport de comparaison avec la stack actuelle, puis arrêt progressif de l'ancienne stack, uniquement sur ma validation. Optionnel, à étudier et désactivé par défaut : une API de lecture compatible Prometheus pour continuer à utiliser Grafana en parallèle (mesure son coût en RAM avant de me la proposer).

## Première tâche
1. Enregistre d'abord ce texte tel quel dans docs/BRIEF.md (c'est la référence qui survit entre les sessions).
2. Fais uniquement l'étape 1, sur une branche etape-1-cahier-des-charges. Crée CLAUDE.md, SPEC.md et PROGRES.md, analyse reference/ et pose-moi les questions dont tu as besoin (maximum 5, regroupées). Ouvre une pull request avec SPEC.md pour que je la relise.
