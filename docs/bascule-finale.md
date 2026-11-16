# Bascule finale vers mirador (étape 7)

But : arrêter l'ancienne stack (Alloy, VictoriaMetrics, VictoriaLogs,
vmalert, Alertmanager, Grafana) **par petites phases**, chacune réversible,
et **seulement après ta validation** (brief : « aucune coupure de
l'ancienne stack sans ma validation explicite »). Rien n'est fait à
distance : tu lances les commandes, tu me colles les sorties marquées 📋.

Ordre : alertes (A) → machines (B) → VPS (C) → réseau Docker (D) →
nettoyage, au moins 30 jours plus tard (E). Une phase par jour au plus ;
on passe à la suivante quand la précédente n'a rien donné d'anormal.

Rien n'est effacé avant la phase E : chaque retour arrière est une
commande.

## 0. Avant de commencer (liste de contrôle)

| Point | Où le vérifier | Fait ? |
|---|---|---|
| Étape 6 validée (collecteur du LAN en place, Alloy et unpoller arrêtés sur le LAN) | `docs/bascule-collecteur.md` | |
| Chaque machine de l'ancienne stack a son agent mirador, approuvé, **avec son site** | Machines (et commande 1 ci-dessous) | |
| Comparaison vmalert : 7 jours sans écart inexpliqué | Alertes › Comparaison vmalert, export CSV | |
| Règles importées sans source dans mirador désactivées (`ProxmoxMuet` : mirador ne reçoit pas le metric server de Proxmox ; la panne du nœud est couverte par `AgentMuet`) | Règles | |
| Mode **actif** (fait le 13/11) ; canal principal et secours testés | Réglages › Notifications › Tester | |
| Heartbeat HetrixTools réglé dans mirador (il remplace le `Watchdog` d'Alertmanager) | Réglages › Notifications › Heartbeat « OK » | |
| Sauvegarde mirador réussie ce jour, clé des secrets copiée hors du VPS | Réglages › Sauvegardes | |
| Mesure du serveur sur 24 h faite | `docs/installation-serveur.md` § 5 | |

**Commande 1** — machines vues par l'ancienne stack (sur le VPS, lecture
seule, image `curl` officielle lancée une fois sur le réseau `obs`) :

```bash
docker run --rm --network obs curlimages/curl:8.11.1 -s 'http://obs-victoriametrics:8428/api/v1/query?query=count%20by%20(instance)%20(node_uname_info)' | tr ',' '\n' | grep '"instance"'
```

📋 Colle la sortie : je la compare à la liste des machines de mirador.
Si le nom du conteneur n'est pas `obs-victoriametrics`, `docker ps --format '{{.Names}}'`
le donne.

## A. Un seul système envoie les alertes : arrêt d'Alertmanager

Aujourd'hui, Alertmanager et mirador envoient tous les deux (alertes en
double depuis le 13/11).

1. Vérifier que mirador pousse le heartbeat : HetrixTools montre un
   battement récent (même adresse que le `Watchdog`, ou un nouveau moniteur).
2. Arrêter Alertmanager (le conteneur reste, rien n'est effacé) :

   ```bash
   docker update --restart=no obs-alertmanager && docker stop obs-alertmanager
   ```

3. Le lendemain : Réglages › Notifications › Tester (Telegram reçu),
   HetrixTools toujours vert, plus de doublons.

vmalert continue d'évaluer (la comparaison reste utile) mais n'envoie plus
rien : ses erreurs « Alertmanager injoignable » dans ses logs sont normales.

**Retour arrière** :

```bash
docker update --restart=unless-stopped obs-alertmanager && docker start obs-alertmanager
```

## B. Machines : arrêt d'Alloy et des anciens scripts

Une machine à la fois, en commençant par `srv-test`. Sur chaque machine :

1. Trouver ce qui tourne encore pour l'ancienne stack (lecture seule) :

   ```bash
   systemctl list-units --type=service --all --no-pager | grep -i -E 'alloy|exporter|obs-' ; systemctl list-timers --all --no-pager | grep -i obs ; grep -rl 'obs-' /etc/cron* 2>/dev/null
   ```

   📋 Colle la sortie de la première machine : je te dis quoi arrêter
   (Alloy, `smartctl_exporter` sur Proxmox, minuteries ou cron des scripts
   `obs-systemd.sh`, `obs-certs.sh`, `obs-firewall.sh`, `obs-docker.py`).

2. Arrêter (sans désinstaller) :

   ```bash
   sudo systemctl disable --now alloy
   ```

   Puis, selon la sortie de l'étape 1, les minuteries et services
   trouvés (`sudo systemctl disable --now NOM`) ; pour une ligne cron :
   la commenter (`#`).

3. Vérifier dans mirador (5 min après) : la machine reste verte, ses
   tableaux continuent, aucune alerte nouvelle.

Sur le nœud Proxmox : Datacenter › Metric Server › désactiver l'entrée qui
envoie vers l'ancienne stack (la case « Disable », sans la supprimer).

Après la dernière machine, la page Comparaison vmalert n'a plus de sens
(vmalert ne reçoit plus rien) : vider l'adresse de vmalert dans
Réglages › Notifications.

**Retour arrière** : `sudo systemctl enable --now alloy` (et les
minuteries ou lignes cron remises).

## C. VPS : arrêt de l'ancienne stack

À faire quand toutes les machines sont passées (phase B).

1. Avant (📋 à coller, pour mesurer le gain) :

   ```bash
   free -m && docker stats --no-stream --format '{{.Name}} {{.MemUsage}} {{.CPUPerc}}' && df -h / && du -sh /var/lib/docker/volumes/*obs* 2>/dev/null
   ```

2. Arrêter **sans rien effacer** (conteneurs et volumes gardés). Les noms
   viennent de la mesure du 07/11 ; `docker-socket-proxy` n'est **pas**
   dans la liste : l'agent mirador du VPS s'en sert.

   ```bash
   docker update --restart=no obs-vmalert obs-vmalert-logs obs-grafana obs-victoriametrics obs-victorialogs && docker stop obs-vmalert obs-vmalert-logs obs-grafana obs-victoriametrics obs-victorialogs
   ```

3. nginx : garder une copie, puis repérer les blocs de l'ancienne stack
   (écriture vers VictoriaMetrics et VictoriaLogs, Grafana, table des
   tokens) :

   ```bash
   sudo cp -a /etc/nginx /root/nginx-avant-etape7 && grep -rn -E '8428|9428|3000|obs-|map ' /etc/nginx/sites-enabled/ /etc/nginx/conf.d/ 2>/dev/null
   ```

   📋 Colle la sortie : je te dis précisément quelles lignes commenter.
   `obs-allow.conf` (liste d'adresses des machines) **reste** : mirador
   s'en sert. Après modification : `sudo nginx -t && sudo systemctl reload nginx`.

4. Le lendemain : refaire la commande 1 de cette phase (📋) ; mirador vert,
   Telegram et heartbeat OK.

**Retour arrière** :

```bash
docker update --restart=unless-stopped obs-vmalert obs-vmalert-logs obs-grafana obs-victoriametrics obs-victorialogs && docker start obs-victoriametrics obs-victorialogs obs-vmalert obs-vmalert-logs obs-grafana
```

```bash
sudo cp -a /root/nginx-avant-etape7/. /etc/nginx/ && sudo nginx -t && sudo systemctl reload nginx
```

## D. Réseau Docker dédié à mirador (SPEC § 18.27)

Aujourd'hui, mirador est sur le réseau `obs`, partagé avec l'ancienne
stack. Une fois celle-ci arrêtée, mirador passe sur son propre réseau :
seul nginx (sur l'hôte) et le daemon Olvid le joignent.

1. Choisir une plage libre (📋 colle la sortie) :

   ```bash
   docker network ls && docker network inspect obs -f '{{json .IPAM.Config}}' && ip -4 route
   ```

   Exemple ci-dessous : `172.31.0.0/24` (à remplacer si elle est prise).

2. Copies de sécurité, puis création du réseau :

   ```bash
   cd /opt/mirador && cp docker-compose.yml docker-compose.yml.avant-etape7 && cp config/server.conf /root/server.conf.avant-etape7 && docker network create --driver bridge --subnet 172.31.0.0/24 mirador
   ```

3. Dans `/opt/mirador/docker-compose.yml`, remplacer `obs` par `mirador` :

   ```yaml
   services:
     mirador-server:
       networks:
         mirador:
           ipv4_address: 172.31.0.20
     mirador-olvid:
       networks: [mirador]

   networks:
     mirador:
       external: true
   ```

4. Dans `config/server.conf` : `MIRADOR_PROXY=172.31.0.1` (passerelle du
   nouveau réseau = nginx vu depuis le conteneur). Dans nginx : remplacer
   l'adresse `172.30.0.20` de mirador par `172.31.0.20` (ingestion et
   interface).

5. Appliquer :

   ```bash
   cd /opt/mirador && docker compose up -d && sleep 30 && docker compose ps && sudo nginx -t && sudo systemctl reload nginx
   ```

6. Vérifier : interface joignable, toutes les machines vertes dans la
   minute (les agents renvoient ce qu'ils ont gardé pendant la bascule),
   Telegram (Tester), Olvid si utilisé.

Si `MIRADOR_PROXY` est faux, le serveur prend nginx pour un client : les
machines sont refusées (adresse non autorisée) ou vues avec la mauvaise
adresse. Rien n'est perdu (les agents gardent leurs données ~19 h) :
corriger la ligne et relancer.

**Retour arrière** :

```bash
cd /opt/mirador && cp docker-compose.yml.avant-etape7 docker-compose.yml && sudo cp /root/server.conf.avant-etape7 config/server.conf && docker compose up -d
```

(et l'adresse de mirador remise dans nginx).

## E. Nettoyage (au moins 30 jours plus tard, sur validation)

Définitif : à faire seulement quand tu es sûr de ne plus revenir en
arrière. mirador garde ses propres données depuis son installation sur
le VPS : les 30 jours d'historique de VictoriaMetrics ne servent plus
après 30 jours.

1. Archiver si tu veux garder l'ancien historique (facultatif) :
   `docker run --rm -v NOM_DU_VOLUME:/d -v /root:/sortie alpine:3.20 tar czf /sortie/NOM_DU_VOLUME.tgz -C /d .`
2. Supprimer les conteneurs de l'ancienne stack (`cd /opt/obs && docker compose down`,
   sans `-v` : les volumes restent), puis les volumes, un par un
   (`docker volume ls | grep obs`, puis `docker volume rm NOM`).
3. Réseau `obs` : vérifier qu'il est vide, puis le supprimer :

   ```bash
   docker network inspect obs -f '{{range .Containers}}{{.Name}} {{end}}'
   ```

   Sortie vide (ou seulement `docker-socket-proxy`, à déplacer d'abord) →
   `docker network rm obs`.
4. Machines : `sudo apt purge alloy` (ou le paquet installé), supprimer les
   scripts `obs-*` et `/etc/obs-systemd-ignore` (son contenu est dans
   `MIRADOR_SYSTEMD_IGNORER` de l'agent : vérifier avant).
5. nginx : supprimer les lignes commentées en phase C ; supprimer la table
   des tokens d'Alloy.
6. mirador : supprimer les règles importées désactivées qui n'ont plus de
   source (`ProxmoxMuet`, `SyslogMuet`…) ; Réglages › Notifications : retirer
   l'adresse de vmalert si ce n'est pas déjà fait.

## Mesures à me coller

Après la phase C, puis après une semaine :

```bash
free -m && docker stats --no-stream --format '{{.Name}} {{.MemUsage}} {{.CPUPerc}}' && df -h /
```

Attendu (SPEC § 2.1) : environ **770 Mio** de RAM rendus par l'ancienne
stack ; mirador sous 256 Mio.

## Grafana (facultatif)

Le brief prévoit, en option et désactivée par défaut, une API de lecture
compatible Prometheus pour garder Grafana. Elle n'est **pas** faite : sans
elle, Grafana s'arrête avec l'ancienne stack (phase C). Si tu veux la
garder, dis-le avant la phase C : j'étudie et je mesure son coût en RAM
avant de te la proposer.
