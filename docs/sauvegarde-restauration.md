# Sauvegarde et restauration de mirador-server

`MS` = `docker exec mirador-server /mirador-server -config /etc/mirador/server.conf`
(voir `docs/administration-serveur.md`).

## Ce qui est sauvegardé

| Contenu | Quand | Taille |
|---|---|---|
| Configuration : base SQLite (machines, tokens hachés, réglages, audit…), binaires de l'agent publiés, fichier de démarrage (pour mémoire) | toujours | quelques Mo |
| Métriques (instantané cohérent) | option `metriques` | ~1 à 1,5 Go pour 7 jours |
| Logs | option `logs` | ~1 Go pour 30 jours |

L'archive est **chiffrée** avec la clé des secrets du serveur
(`/run/secrets/mirador_cle`). La clé n'est jamais dans l'archive.

> **Gardez une copie de la clé des secrets hors du VPS** (gestionnaire de mots
> de passe, clé USB). Sans elle, aucune archive ne peut être restaurée, et les
> secrets de la base (SFTP, S3, plus tard les canaux de notification) sont
> illisibles.

## Réglages

Par défaut : une sauvegarde de la configuration chaque nuit à 02:43 (UTC), 14
archives gardées dans le volume (`/var/lib/mirador/archives`).

Pour changer (les secrets sont donnés en clair et chiffrés à l'enregistrement ;
`"***"` garde le secret déjà enregistré) :

```bash
docker exec -i mirador-server /mirador-server -config /etc/mirador/server.conf sauvegarde-reglages - <<'EOF'
{
  "actif": true,
  "heure": "02:43",
  "conserver": 14,
  "metriques": false,
  "logs": false,
  "sftp": {
    "hote": "sauvegardes.example.org:22",
    "utilisateur": "mirador",
    "dossier": "/srv/sauvegardes/mirador",
    "empreinte_hote": "SHA256:…",
    "cle_ssh": "-----BEGIN OPENSSH PRIVATE KEY-----\n…\n-----END OPENSSH PRIVATE KEY-----\n"
  }
}
EOF
```

- `empreinte_hote` est **obligatoire** : sur le serveur SFTP,
  `ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub`. Une autre empreinte =
  connexion refusée (faux serveur).
- La clé SSH : une clé dédiée, sans phrase de passe
  (`ssh-keygen -t ed25519 -N "" -f mirador-sauvegarde`), autorisée seulement sur
  le compte de sauvegarde.
- Mirador garde aussi `conserver` archives sur le serveur SFTP.

Pour S3 (Scaleway, OVH, AWS…), à la place de `sftp` ou en plus :

```json
"s3": {
  "adresse": "https://s3.fr-par.scw.cloud",
  "region": "fr-par",
  "seau": "mes-sauvegardes",
  "prefixe": "mirador/",
  "cle_acces": "SCW…",
  "secret": "…",
  "style_hote": false
}
```

- Mirador **n'efface rien** sur S3 : donnez-lui une clé qui peut seulement
  **écrire** (même un serveur compromis ne pourra pas détruire les
  sauvegardes), et réglez la durée de conservation par une **règle de cycle de
  vie** du compartiment (ex. expiration après 30 jours).
- Un envoi S3 est limité à 5 Gio : au-delà (métriques et logs inclus sur une
  longue période), utilisez SFTP.

Voir les réglages (secrets masqués), l'historique et les archives :

```bash
MS sauvegardes
```

Une destination distante en échec fait échouer la sauvegarde (l'archive
locale est gardée) : l'historique montre l'erreur, et l'alerte « sauvegarde
en échec ou trop ancienne » (étape 4) préviendra.

## Sauvegarder maintenant

```bash
MS sauvegarder
MS sauvegarder -metriques -logs
```

La sauvegarde est faite par le serveur en marche (copie cohérente, sans
arrêt). Débit mesuré : ~280 Mo/s, soit quelques secondes même avec métriques
et logs.

Récupérer une archive locale sur le VPS (elle reste chiffrée) :

```bash
docker cp mirador-server:/var/lib/mirador/archives/mirador-20261110-024300.sauvegarde .
```

## Vérifier une archive (sans rien restaurer)

```bash
docker exec -i mirador-server /mirador-server -config /etc/mirador/server.conf verifier-sauvegarde - < mirador-20261110-024300.sauvegarde
```

Déchiffre tout, contrôle chaque empreinte, puis efface la copie de travail.

## Restaurer

La restauration se fait **toujours dans un volume vide** : elle ne remplace
jamais des données existantes.

Sur une machine neuve (ou après la perte du VPS) :

1. Remettre `docker-compose.yml`, `config/server.conf` et **la même clé des
   secrets** (fichier du secret `mirador_cle`).
2. Copier l'archive sur la machine.
3. Restaurer **avant** le premier démarrage (le volume est encore vide).
   L'archive passe par l'entrée standard (`-T`) : pas de dossier à monter ni
   de droits de fichier à régler :

   ```bash
   docker compose run --rm -T mirador-server restaurer - < mirador-20261110-024300.sauvegarde
   ```

4. Démarrer :

   ```bash
   docker compose up -d
   ```

Sur la même machine (données abîmées) : arrêter (`docker compose down`),
changer le nom du volume dans `docker-compose.yml` (ex. `mirador-donnees-2` :
l'ancien est gardé tant que vous ne le supprimez pas), puis étapes 3 et 4.

Après la restauration :

- les agents continuent avec leur token (rien à refaire sur les machines) ;
- le fichier de démarrage d'origine est dans
  `/var/lib/mirador/restauration/server.conf`, pour comparaison ;
- une archive d'une version plus récente du serveur est refusée : mettre le
  serveur à jour d'abord.

## Ce que la CI vérifie à chaque envoi

Test `TestSauvegardeRestauration` : un serveur reçoit des données (machine,
token, configuration, métriques, logs) ; sauvegarde à chaud par la commande ;
arrêt ; vérification ; restauration sur une instance vide ; redémarrage ; le
token marche, la configuration, les métriques et les logs sont retrouvés ; une
autre clé et un dossier non vide sont refusés.
