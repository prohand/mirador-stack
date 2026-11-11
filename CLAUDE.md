# CLAUDE.md — règles de travail du projet mirador-stack

> À relire **à chaque session**, avec `docs/BRIEF.md` (référence complète :
> objectifs, contraintes, sécurité, performance, plan en 7 étapes).
> Puis lire `SPEC.md` (décisions) et `PROGRES.md` (où on en est).

## Langue et style
- Réponses en français, simples, sans longue introduction, en listes courtes.
- Code commenté en français, simple et lisible : l'utilisateur doit pouvoir le maintenir seul.
- Nom du projet partout : `mirador-stack` (module Go), binaires `mirador-agent`,
  `mirador-server`, `mirador-release`, et `mirador-collector` (étape 6, collecteur LAN).
- `mirador-agent` n'ouvre jamais de port. Seul `mirador-collector` écoute, sur le LAN uniquement.
- Licence : GPL-3.0-or-later (`LICENSE`). En-tête `// SPDX-License-Identifier: GPL-3.0-or-later`
  dans chaque fichier Go ; dépendances compatibles GPL-3.0 uniquement.

## Ce que je n'ai pas le droit de faire
- Aucun accès à l'infrastructure de l'utilisateur, et ne jamais essayer.
- Ne rien déployer, installer ni modifier chez lui. J'analyse et je propose, il valide.
- Pour tester sur ses machines : lui écrire les commandes, il colle les sorties
  dans une nouvelle session. Pour mes tests : simulations (serveur local, fichiers d'exemple).
- Jamais de secret (token, clé, URL privée) dans le code, les tests, les logs ou les commits.
- La clé privée de signature n'existe jamais dans le dépôt ni dans mes sessions.
- nyx-stack (`github.com/prohand/nyx-stack`, GPL-3.0) : lecture seule, inspiration
  seulement (architecture, interface, fonctionnalités). Ne jamais copier de code ;
  demander avant de reprendre un passage tel quel. Ce qui est retenu / écarté est
  dans `SPEC.md` § 12.
- gladys-olvid (`github.com/prohand/gladys-olvid`, AGPL-3.0) : référence pour Olvid.
  **L'utilisateur en est l'auteur** : sa logique, ses choix et ses textes peuvent
  être repris librement (réécrits en Go, le code source est en Node.js). Indiquer
  la provenance dans les commentaires (« repris de gladys-olvid »).
  Ce qui en est tiré est dans `SPEC.md` § 10.
- Jamais d'exécution de commande à distance sur les agents (ni shell, ni script, ni
  commande dans la configuration).

## Méthode
- Une branche par étape (ex. `etape-1-cahier-des-charges`), commits clairs,
  pull request. **Jamais de push direct sur `main`.**
- Travailler étape par étape. Chaque étape fonctionne seule et est **validée par
  l'utilisateur avant la suivante**.
- Écrire des tests unitaires et les lancer avant de présenter le résultat.
- Une étape n'est pas finie tant que sa consommation RAM / CPU / I/O n'est pas
  mesurée et reportée dans `PROGRES.md` (comparée aux objectifs, avec solutions si dépassement).
- Chaque choix technique est justifié dans `SPEC.md` par rapport au coût RAM/CPU/disque.
- Chaque étape décrit son modèle de menaces dans `SPEC.md`.
- Questions à l'utilisateur : maximum 5, regroupées.

## Fichiers à tenir à jour à la racine
- `CLAUDE.md` : ces règles (ce fichier).
- `SPEC.md` : cahier des charges et décisions prises.
- `PROGRES.md` : état d'avancement, mesures, prochaine étape.
