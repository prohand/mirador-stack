# Installer mirador-agent en parallèle d'Alloy (étape 2)

Guide pour l'essai sur **srv-test**. Alloy reste en place et continue d'envoyer
ses données : mirador-agent envoie les siennes sous le nom
**`srv-test-mirador`**, vers le même VictoriaMetrics et le même VictoriaLogs.
Les deux apparaissent côte à côte dans les dashboards.

Noms à comparer :

| Agent | Nom (`instance` pour les métriques, `host` pour les logs) |
|---|---|
| Alloy | `srvtest` (valeur de `OBS_NAME` dans `/etc/default/alloy`) |
| mirador-agent | `srv-test-mirador` |

Rien de ce guide ne touche à Alloy, à VictoriaMetrics ni à VictoriaLogs.
Sur le VPS, une seule modification : une ligne de token dans la `map` nginx.

---

## 1. Récupérer le binaire

> Aujourd'hui, la façon normale d'installer un agent est la commande
> affichée par l'interface (Machines › Ajouter une machine) : le script et
> le binaire viennent du serveur. Ce guide garde l'essai historique de
> l'étape 2.

La CI ne garde plus de binaires à télécharger (stockage GitHub limité,
16/11). Deux façons de les avoir :

- **Version publiée** : page *Releases* du dépôt, fichier
  `mirador-agent-X.Y.Z-linux-amd64` (signé, avec son `.manifeste.json`) ;
- **Compilés sur ton poste** (Go installé) : `make agent`, puis
  `cd dist && sha256sum -c SHA256SUMS`.

Le script d'installation et le script de mesure sont dans `deploy/agent/`.

Copier sur srv-test (adapter l'utilisateur) :

```bash
scp dist/mirador-agent-linux-amd64 deploy/agent/mirador-agent-install.sh deploy/agent/mesurer.sh admin@srv-test:/tmp/
```

## 2. Installer sur srv-test

Reprendre le site et l'env utilisés par Alloy, et vérifier son nom (`srvtest`) :

```bash
sudo grep -E '^OBS_(NAME|SITE|ENV)=' /etc/default/alloy
```

Le script refuse un nom identique à celui d'Alloy (les séries des deux agents se
mélangeraient).

Voir ce que le script va détecter (ne modifie rien) :

```bash
sudo bash /tmp/mirador-agent-install.sh --detection --nom srv-test-mirador --site <site> --env <env>
```

Installer (remplacer `<site>`, `<env>` et l'adresse d'ingestion) :

```bash
sudo bash /tmp/mirador-agent-install.sh --binaire /tmp/mirador-agent-linux-amd64 --serveur https://ingest.<ton-domaine> --site <site> --env <env> --nom srv-test-mirador
```

À la fin, le script affiche **une ligne à ajouter dans nginx**, avec le token.

Ce que fait le script :

- crée l'utilisateur système `mirador-agent` (sans droits root) ;
- installe le binaire dans `/var/lib/mirador-agent/bin/` et une copie root pour
  l'assistant dans `/usr/local/lib/mirador-agent/` ;
- écrit `/etc/mirador-agent/agent.conf` et `/etc/mirador-agent/token` (0640) ;
- si Docker est présent : lance le conteneur `mirador-docker-proxy`
  (`tecnativa/docker-socket-proxy:v0.5.0`, épinglé par son empreinte
  `sha256:1f5038b5…`, `127.0.0.1:12375`), sur son propre réseau Docker
  `mirador-docker` (`172.31.254.0/29`, pont `br-mirador`, relais en
  `172.31.254.2` ; autre plage privée : `--docker-reseau`). **Pare-feu qui
  filtre la sortie** (OUTPUT en DROP) : autoriser
  `iptables -I OUTPUT 1 -o br-mirador -s 172.31.254.1 -d 172.31.254.2 -p tcp --dport 2375 -j ACCEPT`
  (le script le signale s'il ne joint pas le relais) ;
- relais en **liste blanche** (revue du 28/11) : sa configuration HAProxy,
  écrite dans `/etc/mirador-agent/relais-docker.cfg`, ne laisse passer qu'en
  GET `/_ping`, `/containers/json` et `/containers/<id>/json|stats|logs` (les
  seuls chemins utilisés par l'agent) ; tout le reste (`/archive`, `/export`,
  `/attach`, `/exec`, écritures…) reçoit un 403 ;
- relais **réservé aux comptes root et `mirador-agent`** : unité
  `mirador-agent-relais.service` (règles iptables de sortie, chaîne
  `MIRADOR-RELAIS`, qui ne font que refuser ; remises à chaque démarrage).
  `--sans-filtre-relais` pour s'en passer. Si le pare-feu de la machine est
  rechargé (`iptables-restore`, `ufw reload`, `systemctl restart nftables`…),
  les règles peuvent disparaître : `systemctl restart mirador-agent-relais` ;
- installe et démarre `mirador-agent.service` et `mirador-agent-root.timer`.

## 3. Autoriser l'agent dans nginx (VPS)

Ajouter la ligne affichée par le script dans le bloc `map` de
`/etc/nginx/conf.d/00-obs-map.conf`, puis :

```bash
sudo nginx -t && sudo systemctl reload nginx
```

Tant que la ligne n'est pas là, l'agent reçoit des 403 : il garde ses métriques
en mémoire (8 Mio, environ 19 h) et réessaie. Rien n'est perdu.

## 4. Vérifier sur srv-test

État des services :

```bash
systemctl status mirador-agent.service mirador-agent-root.timer --no-pager
```

Journal de l'agent (aucune ligne « envoi en échec » attendue après nginx) :

```bash
journalctl -u mirador-agent --since '-5min' --no-pager
```

Une collecte, affichée sans rien envoyer :

```bash
sudo -u mirador-agent /var/lib/mirador-agent/bin/current -une-fois -config /etc/mirador-agent/agent.conf | grep -c .
```

Résultat de l'assistant root (pare-feu, SMART, certificats) :

```bash
sudo cat /run/mirador-agent/root.json
```

## 5. Comparer avec Alloy (sur le VPS, lecture seule)

Nombre de séries de chaque côté :

```bash
curl -sG http://172.30.0.10:8428/api/v1/query --data-urlencode 'query=count by (instance) ({instance=~"srvtest|srv-test-mirador"})'
```

Quelques valeurs à comparer (doivent être très proches) :

```bash
curl -sG http://172.30.0.10:8428/api/v1/query --data-urlencode 'query=node_memory_MemAvailable_bytes{instance=~"srvtest|srv-test-mirador"} or node_load5{instance=~"srvtest|srv-test-mirador"} or 100 * (1 - avg by (instance) (rate(node_cpu_seconds_total{instance=~"srvtest|srv-test-mirador", mode="idle"}[5m])))'
```

Métriques présentes chez Alloy mais absentes chez mirador (parmi celles utilisées) :

```bash
curl -sG http://172.30.0.10:8428/api/v1/query --data-urlencode 'query=count by (__name__) ({instance="srvtest", __name__=~"node_.*|ssl_.*|smartctl_.*"}) unless on (__name__) count by (__name__) ({instance="srv-test-mirador"})'
```

Logs des 10 dernières minutes de chaque côté :

```bash
curl -s http://172.30.0.11:9428/select/logsql/query --data-urlencode 'query=_time:10m host:~"srvtest|srv-test-mirador" | stats by (host, job) count() as lignes'
```

Alertes en cours pour la machine mirador (elles partent normalement, c'est voulu) :

```bash
curl -s http://172.30.0.12:8880/api/v1/alerts | python3 -m json.tool | grep -B2 -A8 'srv-test-mirador'
```

## 6. Mesurer la consommation (srv-test)

Essai rapide (12 minutes) :

```bash
sudo bash /tmp/mesurer.sh 0.2
```

Mesure complète (24 heures, en arrière-plan) :

```bash
sudo nohup bash /tmp/mesurer.sh 24 > /var/tmp/mirador-resume.txt 2>&1 &
```

Le lendemain, recopier le résumé dans la session suivante :

```bash
cat /var/tmp/mirador-resume.txt
```

Objectifs (SPEC.md § 6.1) : agent < 30 Mio de RAM, < 1 % d'un cœur, pas
d'écriture disque en régime normal (seul le curseur du journal, ~70 octets toutes
les 5 minutes).

## 7. Retirer mirador-agent

```bash
sudo bash /tmp/mirador-agent-install.sh --desinstaller
```

Puis retirer sa ligne de la `map` nginx. Alloy n'est pas touché.

## 8. Plus tard : mode mirador-server (étape 3)

Quand mirador-server sera installé sur le VPS (partie 7), l'agent se branche
dessus au lieu de VictoriaMetrics. Plus de ligne dans la `map` de nginx : nginx
ne filtre que par IP, mirador vérifie les tokens (SPEC § 16.5).

1. Sur le VPS, créer un jeton d'enrôlement (usage unique, 24 h) :

   ```bash
   docker exec mirador-server /mirador-server -config /etc/mirador/server.conf jeton -site bureau -env vm
   ```

   Plus simple : interface › Machines › « + Ajouter une machine » (site, type,
   groupe, puis « Créer la commande d'installation » : elle s'affiche avec
   un bouton « Copier »).

2. Sur la machine, télécharger et installer avec ce jeton (site et type ne se
   donnent plus ici : ils viennent du jeton, et se changent dans la fiche de
   la machine). Rien à copier à la main :

   ```bash
   curl -fsSL https://mirador.example.org/api/agent/v1/install -o mirador-agent-install.sh
   # en root (sudo -i) ; l'espace en tête garde le jeton hors de l'historique
    MIRADOR_JETON=mirador_enrol_… bash mirador-agent-install.sh \
     --serveur https://mirador.example.org --nom srv-test-mirador
   ```

   Adresse : celle du **vhost d'ingestion** (agents), pas celle de
   l'interface. Le script est public ; il télécharge ensuite le binaire
   (amd64 ou arm64, détecté) avec le jeton et vérifie son empreinte SHA-256.
   Hors ligne : `--binaire FICHIER` reste possible. `--sans-logs` (métriques
   seulement) ou `--sans-metriques` (logs seulement) : modifiables ensuite
   dans l'interface (Configuration des agents).

   Ajouter `--auto-update` pour les mises à jour signées automatiques.

   **Signature vérifiée à l'installation** (revue du 28/11) : si une clé
   publique est déjà connue sur la machine (`--cle-publique`, ou
   `/etc/mirador-agent/cles-publiques.txt` posé par une installation
   précédente), le script vérifie, **avant de lancer le binaire**, son
   manifeste signé (envoyé par le serveur avec lui) avec `openssl` 3 :
   signature Ed25519, produit, architecture, empreinte, taille, puis la
   version annoncée par le binaire (lancé sous le compte `mirador-agent`,
   jamais en root). Refus :
   - signature fausse ou d'une autre clé ;
   - serveur qui n'a qu'un binaire « embarqué » non signé : publier une
     version signée (Machines › Mises à jour des agents), puis relancer ;
   - openssl absent ou plus ancien que 3.0 (Debian 11, Ubuntu 20.04) ;
   - version plus ancienne que celle en place (sauf retour arrière signé, ou
     `--accepter-version-ancienne`).

   `--sans-verification-signature` passe outre (déconseillé). À la toute
   première installation sans `--cle-publique`, la clé vient du serveur
   lui-même : rien n'est vérifié ; donner `--cle-publique <clé>` (copiée
   depuis l'interface ou le poste de signature) pour vérifier dès la
   première fois.

3. Sur le VPS, approuver la machine (le token est alors limité à l'adresse de
   la demande ; `approuver -sans-ip NOM` sinon) :

   ```bash
   docker exec mirador-server /mirador-server -config /etc/mirador/server.conf approuver srv-test-mirador
   ```

L'agent reçoit son token dans les 30 s et commence à envoyer. Voir
`docs/administration-serveur.md` pour les autres commandes.
