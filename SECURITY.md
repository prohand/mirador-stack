# Sécurité

## Signaler une faille

**Merci de ne pas ouvrir d'issue publique.**

Utilisez le signalement privé de GitHub : onglet **Security** du dépôt ›
**Report a vulnerability**. Seul le mainteneur voit le message.

Indiquez si possible :

- le composant touché (mirador-agent, mirador-server, interface web, script
  d'installation, image Docker) et la version (`mirador-server -version`,
  `mirador-agent -version`) ;
- les étapes pour reproduire, et ce qu'un attaquant peut obtenir ;
- une proposition de correction, si vous en avez une.

## Ce qui se passe ensuite

C'est un projet personnel, maintenu sur le temps libre :

- réponse visée sous 7 jours ;
- correction publiée dans une nouvelle version, puis annonce dans l'avis de
  sécurité GitHub (avec votre nom si vous le souhaitez).

Merci de laisser le temps de corriger avant toute publication (90 jours au
plus).

## Versions suivies

Seule la **dernière version** publiée (tag `v*`) reçoit des corrections.

## Dans le périmètre

- Contournement de l'authentification de l'interface (mot de passe, 2FA,
  session, CSRF), élévation du rôle lecture vers admin.
- Exécution de code ou lecture de fichiers sur le serveur ou sur un agent.
- Envoi de données au nom d'une autre machine, contournement de l'enrôlement.
- Mise à jour d'agent non signée acceptée.
- Fuite de secrets (tokens, clés, mots de passe) dans les journaux, l'interface
  ou les sauvegardes.

## Hors périmètre

- Attaques qui demandent déjà un accès administrateur (interface ou root).
- Déni de service par volume brut (à filtrer en amont, par nginx).
- Configuration de nginx, du pare-feu ou de l'hôte propre à chaque
  installation.

## Principes de conception

Résumé des choix de sécurité (détails et modèles de menaces dans `SPEC.md`) :

- l'agent n'ouvre aucun port et n'exécute jamais de commande reçue ;
- les versions de l'agent sont signées (Ed25519) hors du serveur ;
- 2FA obligatoire, actions sensibles confirmées par un nouveau code ;
- secrets chiffrés au repos, sauvegardes chiffrées.
