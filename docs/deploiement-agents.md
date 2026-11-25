# Installer mirador-agent sur plusieurs machines (avant l'étape 7)

But : mettre l'agent mirador sur chaque machine que voit encore l'ancienne
stack, **à côté d'Alloy** (rien n'est arrêté). Alloy ne s'arrête qu'en
phase B de `docs/bascule-finale.md`, sur validation.

Principe (avec **Semaphore**, un seul modèle de tâche) :

- pour chaque machine, le playbook crée sur le VPS un **jeton d'enrôlement à
  usage unique** (valable 1 h), le dépose dans `/root/.mirador-jeton` (0600),
  lance le script d'installation, puis efface le fichier, même en cas
  d'échec ;
- le jeton ne passe jamais sur une ligne de commande (ni dans `ps`) et
  n'apparaît pas dans les journaux de Semaphore (`no_log` sur les deux tâches
  qui le touchent ; le script d'installation ne l'affiche jamais, sa sortie
  reste donc visible pour comprendre un échec) ;
- une machine qui a déjà l'agent est sautée (aucun jeton créé) : le modèle
  peut être relancé sans risque ;
- chaque machine arrive « en attente » dans mirador : on l'approuve.

**Les jetons sont des secrets** : ne jamais les coller dans une session.

## 0. Inventaire (Semaphore › Inventaire)

Le nom mirador doit être **le même que dans l'ancienne stack** (étiquette
`instance`) : les règles s'appuient dessus. Ne pas mettre `obscollector`
(c'est le collecteur, `collecteur-lan` dans mirador) ni les machines qui ont
déjà l'agent (proxmoxbackup, dhcpdns02).

Un groupe `mirador_agents` ; `mirador_nom` seulement si le nom d'hôte
Ansible diffère du nom à garder. Le VPS doit aussi être dans l'inventaire
(hors du groupe), joignable en SSH avec `sudo` par la clé de Semaphore :

```ini
[mirador_agents]
dns1
dns2
192.168.35.20  mirador_nom=proxmox

[mirador_vps]
vps
```

## 1. Variables (Semaphore › Environnement, « Extra variables »)

```json
{"serveur": "https://VOTRE-VHOST-INGESTION", "vps": "vps"}
```

- `serveur` : adresse d'ingestion de mirador ;
- `vps` : nom du VPS **tel qu'écrit dans l'inventaire**.

## 2. Playbook (dans le dépôt Git que lit Semaphore)

`installer-mirador.yml` :

```yaml
# Installe mirador-agent à côté d'Alloy. Variables : serveur, vps.
- name: Installer mirador-agent
  hosts: mirador_agents
  become: true
  gather_facts: false
  vars:
    nom: "{{ mirador_nom | default(inventory_hostname) }}"
  tasks:
    - name: Agent déjà installé ?
      ansible.builtin.stat:
        path: /etc/mirador-agent/agent.conf
      register: deja

    - name: Installation
      when: not deja.stat.exists
      block:
        - name: Jeton d'enrôlement créé sur le VPS (un par machine)
          ansible.builtin.command:
            cmd: docker exec mirador-server /mirador-server -config /etc/mirador/server.conf jeton -duree 1h
          delegate_to: "{{ vps }}"
          throttle: 1          # un à la fois : la base du serveur n'aime pas les écritures simultanées
          register: sortie_jeton
          changed_when: true
          no_log: true

        - name: Jeton déposé sur la machine (root seul)
          ansible.builtin.copy:
            content: "{{ sortie_jeton.stdout | regex_search('mirador_enrol_[A-Za-z0-9_-]+') | default('', true) }}"
            dest: /root/.mirador-jeton
            mode: "0600"
          no_log: true

        - name: Script d'installation (depuis mirador-server)
          ansible.builtin.get_url:
            url: "{{ serveur }}/api/agent/v1/install"
            dest: /root/mirador-agent-install.sh
            mode: "0700"

        - name: Installer mirador-agent
          ansible.builtin.shell:
            cmd: >-
              MIRADOR_JETON="$(cat /root/.mirador-jeton)"
              bash /root/mirador-agent-install.sh
              --serveur {{ serveur | quote }} --nom {{ nom | quote }} --auto-update
            creates: /etc/mirador-agent/agent.conf
      always:
        - name: Jeton effacé
          ansible.builtin.file:
            path: /root/.mirador-jeton
            state: absent
```

Le site et le type sont repris d'Alloy (`/etc/default/alloy`) ; Docker
présent : le proxy de socket en lecture seule est installé ; SMART sur les
machines physiques.

## 2 bis. Modèle de tâche (Semaphore › Modèles)

- Playbook : `installer-mirador.yml`, inventaire et environnement ci-dessus,
  clé SSH + mot de passe `sudo` comme pour les autres modèles ;
- **d'abord une machine** : limite `dns1` (champ « Limit », ou arguments
  `["--limit", "dns1"]` si la version le demande ainsi) ;
- puis sans limite : 5 machines à la fois (réglage par défaut d'Ansible).

Si la tâche « Jeton d'enrôlement » échoue, sa sortie est masquée : lancer
la même commande à la main sur le VPS pour voir l'erreur (puis ne pas garder
le jeton affiché).

Une machine hors du LAN (VPS, autre hébergeur) doit avoir son adresse
publique autorisée par nginx sur l'adresse d'ingestion de mirador, sinon le
téléchargement échoue (« adresse de cette machine pas autorisée »).

## 3. Approuver

Dans mirador : **Machines**, bandeau « en attente » : vérifier nom, site et
adresse, approuver. Ou sur le VPS, machine par machine :

```bash
docker exec mirador-server /mirador-server -config /etc/mirador/server.conf machines
docker exec mirador-server /mirador-server -config /etc/mirador/server.conf approuver NOM
```

## 4. Vérifier

Rien à nettoyer : chaque jeton est effacé de sa machine à la fin, et un
jeton non utilisé expire seul après 1 h.

📋 À me coller : la liste de la page Machines (noms, en ligne ou non).
Ensuite : phase A de `docs/bascule-finale.md` (pas de nouvelle
comparaison vmalert, choix du 24/11).

## 5. Supprimer Alloy et l'ancienne collecte (2e passage, après approbation)

Pas dans le même passage que l'installation :

- le script d'installation **lit `/etc/default/alloy`** (site, type) et
  `/etc/obs-systemd-ignore` : Alloy doit encore être là à l'installation ;
- une machine n'envoie rien tant qu'elle n'est pas approuvée : supprimer
  Alloy avant, c'est un trou dans la supervision ;
- 24 machines muettes d'un coup = autant d'alertes « muet » de l'ancien
  Alertmanager : faire d'abord la **phase A** de `docs/bascule-finale.md`.

Ordre : installer → approuver → vérifier dans mirador (machines vertes,
graphiques remplis) → phase A → relevé ci-dessous → suppression.

Relevé facultatif en lecture seule (`inventaire-ancienne-stack.yml`, plus
bas) ; la suppression fait son propre relevé et l'affiche avant d'agir.

### 5.1 Suppression (`supprimer-ancienne-stack.yml`, Semaphore)

Supprime, **seulement là où l'agent mirador est approuvé et actif** (sinon
la machine est sautée, avec la raison) :

- Alloy (paquet, `/etc/alloy`, `/etc/default/alloy`, dépôt apt de Grafana) ;
- SNMP : `snmpd` et `snmp_exporter` (paquet ou binaire + unité) ;
- `smartctl_exporter` (l'assistant root de mirador lance `smartctl`
  lui-même : **`smartmontools` est gardé**) ;
- les scripts `obs-*` (fichiers, minuteries et services `obs-*`) ; les
  lignes cron qui les appellent sont **commentées** (root et `/etc/cron.d`),
  les autres lignes ne bougent pas ; `/etc/obs-systemd-ignore` (son contenu
  est déjà dans la configuration de l'agent).
- **gardés** : tout ce qui contient `obs-geo` (sur le VPS, `obs-geo-fr` et
  `obs-geoip-update` tiennent à jour la liste d'adresses françaises de
  nginx, dont mirador se sert).

Avant : archive `/root/ancienne-stack.tgz` (configuration, scripts, unités,
crontab de root d'avant). Relançable sans risque. Testé en local (faux
paquets, faux systemctl et crontab) : essai sans effet, suppression,
archive, garde, 2e passage vide.

1. **Essai** (rien n'est changé) : modèle avec l'argument `--check`, limite
   `dns1` ; lire « Ce qui va être supprimé ». 📋 à me coller.
2. Pour de vrai sur `dns1`, vérifier dans mirador (machine verte), puis
   toutes les machines.

```yaml
# 2e passage : supprime Alloy, SNMP (snmpd, snmp_exporter) et l'ancienne
# collecte (smartctl_exporter, scripts obs-*), seulement sur les machines dont
# l'agent mirador est approuvé et actif. Garde smartmontools : l'assistant
# root de mirador s'en sert.
# Essai d'abord (rien n'est changé) : lancer avec --check.
- name: Supprimer l'ancienne collecte
  hosts: mirador_agents
  become: true
  gather_facts: false
  vars:
    paquets_anciens:
      - alloy
      - snmpd
      - prometheus-snmp-exporter
      - prometheus-smartctl-exporter
    # Unités systemd ; les motifs (obs-*) sont compris par systemctl.
    unites_anciennes: >-
      alloy.service snmpd.service
      snmp_exporter.service snmp-exporter.service prometheus-snmp-exporter.service
      smartctl_exporter.service smartctl-exporter.service prometheus-smartctl-exporter.service
      'obs-*'
    # Fichiers et dossiers : archivés puis effacés.
    chemins_anciens: >-
      /etc/alloy /etc/default/alloy /etc/obs-systemd-ignore
      /etc/apt/sources.list.d/grafana.list /etc/apt/keyrings/grafana.gpg
      /usr/local/bin/obs-* /usr/local/sbin/obs-*
      /usr/local/bin/snmp_exporter /usr/local/bin/smartctl_exporter
      /etc/systemd/system/obs-* /etc/systemd/system/snmp_exporter.service
      /etc/systemd/system/smartctl_exporter.service
    archive: /root/ancienne-stack.tgz
    # Gardés : sur le VPS, obs-geo-fr et obs-geoip-update tiennent à jour la
    # liste d'adresses françaises de nginx, dont mirador se sert.
    garder: 'obs-geo'
  tasks:
    # --- Garde : rien sur une machine que mirador ne surveille pas encore.
    - name: Agent mirador approuvé ?
      ansible.builtin.stat:
        path: /var/lib/mirador-agent/token
      register: token

    - name: Agent mirador actif ?
      ansible.builtin.command: systemctl is-active mirador-agent
      register: actif
      changed_when: false
      failed_when: false
      check_mode: false

    - name: Machine sautée (agent mirador pas approuvé ou arrêté)
      ansible.builtin.debug:
        msg: "SAUTÉE : approuvé={{ token.stat.exists }}, mirador-agent={{ actif.stdout }}"
      when: not token.stat.exists or actif.stdout != 'active'

    - name: Fin pour cette machine
      ansible.builtin.meta: end_host
      when: not token.stat.exists or actif.stdout != 'active'

    # --- Relevé (lecture seule, aussi en essai).
    - name: Unités présentes
      ansible.builtin.shell: >-
        systemctl list-unit-files --no-legend --plain {{ unites_anciennes }}
        | awk '{print $1}' | grep -v '^obs-.*@\.' | grep -v -E '{{ garder }}' || true
      register: unites
      changed_when: false
      check_mode: false

    - name: Fichiers présents
      ansible.builtin.shell: >-
        ls -d {{ chemins_anciens }} 2>/dev/null | grep -v -E '{{ garder }}'; true
      register: fichiers
      changed_when: false
      check_mode: false

    - name: Fichiers de /etc/cron.d qui appellent obs-*
      ansible.builtin.shell: grep -lE '^[^#].*(^|[^[:alnum:]_])obs-' /etc/cron.d/* 2>/dev/null | grep -v -E '{{ garder }}' || true
      register: cron_d
      changed_when: false
      check_mode: false

    - name: Paquets présents
      ansible.builtin.shell: >-
        dpkg-query -W -f='${db:Status-Abbrev} ${Package}\n' {{ paquets_anciens | join(' ') }} 2>/dev/null
        | awk '$1 ~ /^[ih]i/ {print $2}' || true
      register: paquets
      changed_when: false
      check_mode: false

    - name: Lignes cron de root qui appellent obs-*
      ansible.builtin.shell: crontab -l 2>/dev/null | grep -v '^#' | grep -E '(^|[^[:alnum:]_])obs-' | grep -v -E '{{ garder }}' || true
      register: cron_root
      changed_when: false
      check_mode: false

    - name: Ce qui va être supprimé
      ansible.builtin.debug:
        msg:
          - "unités : {{ unites.stdout_lines | join(', ') or 'aucune' }}"
          - "paquets : {{ paquets.stdout_lines | join(', ') or 'aucun' }}"
          - "fichiers : {{ fichiers.stdout_lines | join(', ') or 'aucun' }}"
          - "cron : {{ cron_root.stdout_lines | length }} ligne(s) de root commentée(s) ; lignes commentées dans {{ cron_d.stdout_lines | join(', ') or 'aucun fichier de /etc/cron.d' }}"

    # --- Archive (une seule fois) : permet le retour arrière.
    - name: Archive de la configuration de l'ancienne stack
      ansible.builtin.shell: >-
        crontab -l > /root/crontab-avant-mirador 2>/dev/null;
        tar czf {{ archive }} --ignore-failed-read
        {{ (fichiers.stdout_lines + cron_d.stdout_lines) | map('quote') | join(' ') }} /root/crontab-avant-mirador
      args:
        creates: "{{ archive }}"
      when: (fichiers.stdout_lines + cron_d.stdout_lines + cron_root.stdout_lines) | length > 0

    # --- Suppression.
    - name: Arrêter et désactiver les unités
      ansible.builtin.command: systemctl disable --now {{ item }}
      loop: "{{ unites.stdout_lines }}"

    - name: Commenter les lignes cron de root qui appellent obs-*
      ansible.builtin.shell: >-
        t=$(mktemp) && crontab -l > "$t" &&
        sed -E '/^[^#]/{/{{ garder }}/!{/(^|[^[:alnum:]_])obs-/s/^/# (retiré pour mirador) /}}' "$t" | crontab - ;
        r=$?; rm -f "$t"; exit $r
      when: cron_root.stdout_lines | length > 0

    - name: Commenter les lignes de /etc/cron.d qui appellent obs-*
      ansible.builtin.command: >-
        sed -i -E '/^[^#]/{/{{ garder }}/!{/(^|[^[:alnum:]_])obs-/s/^/# (retiré pour mirador) /}}' {{ item }}
      loop: "{{ cron_d.stdout_lines }}"

    - name: Désinstaller les paquets (configuration comprise)
      ansible.builtin.command: apt-get purge -y {{ paquets.stdout_lines | join(' ') }}
      environment:
        DEBIAN_FRONTEND: noninteractive
      when: paquets.stdout_lines | length > 0

    - name: Effacer les fichiers restants
      ansible.builtin.file:
        path: "{{ item }}"
        state: absent
      loop: "{{ fichiers.stdout_lines }}"

    - name: Recharger systemd
      ansible.builtin.command: systemctl daemon-reload
      when: unites.stdout_lines | length > 0
```

Retour arrière d'une machine : réinstaller les paquets, puis
`sudo tar xzf /root/ancienne-stack.tgz -C /` et
`sudo crontab /root/crontab-avant-mirador`, `sudo systemctl daemon-reload`,
réactiver les unités.

### 5.2 Hors Semaphore

- Nœud Proxmox : Datacenter › Metric Server › supprimer l'entrée qui
  envoie vers l'ancienne stack.
- obscollector (collecteur, pas d'agent : à la main) : Alloy et unpoller
  déjà arrêtés le 24/11 ; à supprimer de la même façon (relevé d'abord).
- VPS : un `snmp_exporter` éventuel de l'ancienne stack part avec elle
  (phase C puis E de `docs/bascule-finale.md`).

### 5.3 Relevé seul (lecture seule)

```yaml
# Lecture seule : ce qui reste de l'ancienne stack sur chaque machine.
- name: Ancienne stack, ce qui reste
  hosts: mirador_agents
  become: true
  gather_facts: false
  tasks:
    - name: Services, minuteries, cron, paquets, scripts
      ansible.builtin.shell: |
        systemctl list-unit-files --no-pager --no-legend | grep -i -E 'alloy|exporter|obs-|snmp' | awk '{print "unité : "$1" ("$2")"}'
        grep -l -E 'obs-|alloy' /etc/crontab /etc/cron.d/* /var/spool/cron/crontabs/* 2>/dev/null | sed 's/^/cron : /'
        dpkg -l 2>/dev/null | awk '/^ii/ && $2 ~ /alloy|snmp|exporter/ {print "paquet : "$2}'
        ls -d /usr/local/bin/obs-* /usr/local/sbin/obs-* /etc/obs-* /etc/alloy /etc/default/alloy /etc/snmp 2>/dev/null | sed 's/^/fichier : /'
        test -s /var/lib/mirador-agent/token && echo "mirador : agent approuvé" || echo "mirador : agent PAS encore approuvé"
        systemctl is-active mirador-agent | sed 's/^/mirador-agent : /'
        true
      register: reste
      changed_when: false
    - name: Résultat
      ansible.builtin.debug:
        var: reste.stdout_lines
```

### 5.4 Effacer les « en échec » laissés par la suppression (`effacer-echecs-ancienne-stack.yml`)

Un service supprimé alors qu'il était en échec (Alloy le plus souvent) reste
noté « failed » par systemd ; l'agent le voit et le signale (retour du
25/11 : `alloy.service` sur une quinzaine de machines). Ce playbook fait un
`systemctl reset-failed` des seuls services de l'ancienne stack en échec :
rien n'est relancé, arrêté ni supprimé. Sans risque ; essai possible en
mode `--check` (il liste sans rien effacer).

```yaml
# Efface la trace « en échec » laissée par les services de l'ancienne stack
# supprimés (alloy, snmpd, obs-*…) : systemctl reset-failed. Rien n'est
# relancé, arrêté ni supprimé ; l'agent mirador cesse de les voir en échec.
- name: Effacer les échecs des services de l'ancienne stack
  hosts: mirador_agents
  become: true
  gather_facts: false
  vars:
    motifs:
      - alloy.service
      - snmpd.service
      - snmp_exporter.service
      - smartctl_exporter.service
      - obs-*
  tasks:
    - name: Services de l'ancienne stack en échec
      ansible.builtin.command:
        argv: "{{ ['systemctl', 'list-units', '--state=failed', '--plain', '--no-legend', '--no-pager'] + motifs }}"
      register: echecs
      changed_when: false
      check_mode: false

    - name: Effacer leur échec
      ansible.builtin.command:
        argv: "{{ ['systemctl', 'reset-failed'] + (echecs.stdout_lines | map('split') | map('first') | list) }}"
      when: echecs.stdout_lines | length > 0

    - name: Résultat
      ansible.builtin.debug:
        msg: "{{ 'effacé : ' + (echecs.stdout_lines | map('split') | map('first') | join(', ')) if echecs.stdout_lines | length > 0 else 'rien en échec' }}"
```

### 5.5 Limiter le journal local (`limiter-journal.yml`)

Retour du 26/11 : 2,9 Go de journal système sur sup (paquets bloqués par
le pare-feu, journalisés), alors que les logs partent déjà dans mirador
(30 jours gardés). Ce playbook limite le journal de chaque machine à
500 Mo (fichier `/etc/systemd/journald.conf.d/50-taille.conf`, sans
toucher `journald.conf`), relance journald et réduit le journal tout de
suite ; il affiche la place avant et après. Essai possible en `--check`.
Retour en arrière : supprimer le fichier, relancer `systemd-journald`.

```yaml
# Limite le journal local (journald) à 500 Mo sur chaque machine : les logs
# partent déjà dans mirador (30 jours gardés), la copie locale n'a pas
# besoin de plusieurs Go (retour du 26/11 : 2,9 Go sur sup). Réglage dans
# un fichier à part (journald.conf n'est pas touché) ; retirer le fichier
# et relancer systemd-journald pour revenir en arrière.
- name: Limiter le journal local à 500 Mo
  hosts: mirador_agents
  become: true
  gather_facts: false
  vars:
    taille: 500M
    dossier: /etc/systemd/journald.conf.d
  tasks:
    - name: Place prise avant
      ansible.builtin.command: journalctl --disk-usage
      register: avant
      changed_when: false
      check_mode: false

    - name: Dossier des réglages de journald
      ansible.builtin.file:
        path: "{{ dossier }}"
        state: directory
        mode: "0755"

    - name: Limite de taille du journal
      ansible.builtin.copy:
        dest: "{{ dossier }}/50-taille.conf"
        content: |
          # Posé par Semaphore (limiter-journal.yml) : les logs sont dans mirador.
          [Journal]
          SystemMaxUse={{ taille }}
        mode: "0644"
      register: reglage

    - name: Relancer journald (nouvelle limite)
      ansible.builtin.command: systemctl restart systemd-journald
      when: reglage.changed

    # Toujours (sans effet si déjà sous la limite) : un passage interrompu
    # est rattrapé au suivant.
    - name: Réduire le journal tout de suite
      ansible.builtin.command: journalctl --vacuum-size={{ taille }}
      changed_when: false

    - name: Place prise après
      ansible.builtin.command: journalctl --disk-usage
      register: apres
      changed_when: false
      check_mode: false

    - name: Résultat
      ansible.builtin.debug:
        msg: "avant : {{ avant.stdout | regex_search('take up (\\S+)', '\\1') | first | default('?') }} · après : {{ apres.stdout | regex_search('take up (\\S+)', '\\1') | first | default('?') }}"
```

## 6. Relais Docker à adresse fixe (après la mise à jour du serveur, SPEC § 126)

Machines avec Docker : le relais `mirador-docker-proxy` passe sur son propre
réseau (`mirador-docker`, pont `br-mirador`, relais en `172.31.254.2`).

1. **Pare-feu** des machines dont la sortie est en DROP (sup, mail, proxy) :
   une seule règle, la même partout (à mettre dans le rôle du pare-feu) :

   ```bash
   iptables -I OUTPUT 1 -o br-mirador -s 172.31.254.1 -d 172.31.254.2 -p tcp --dport 2375 -j ACCEPT
   ```

   L'ancienne règle vers `docker0` / `172.19.0.0/24` (port 2375) peut
   ensuite être retirée.
2. **Serveur à jour** (le script est embarqué dans le serveur).
3. **Semaphore** : `relais-docker-mirador.yml`, d'abord limite `sup`, puis
   sans limite. Il relance le script sur les seules machines avec Docker et
   un agent enrôlé (pas de jeton ; nom, token, liste des services ignorés et
   mises à jour automatiques gardés) et dit si le relais répond.

```yaml
# Relance le script d'installation sur les machines qui ont Docker et l'agent :
# le relais Docker passe sur son réseau à adresse fixe (172.31.254.2).
# Agent déjà enrôlé : pas de jeton ; nom, token et réglages gardés.
- name: Relais Docker de mirador à adresse fixe
  hosts: mirador_agents
  become: true
  gather_facts: false
  tasks:
    - name: Docker présent ?
      ansible.builtin.stat:
        path: /var/run/docker.sock
      register: docker

    - name: Agent mirador enrôlé ?
      ansible.builtin.stat:
        path: /var/lib/mirador-agent/token
      register: token

    - name: Fin pour cette machine (pas de Docker, ou pas d'agent)
      ansible.builtin.meta: end_host
      when: not docker.stat.exists or not token.stat.exists

    - name: Script d'installation à jour (depuis mirador-server)
      ansible.builtin.get_url:
        url: "{{ serveur }}/api/agent/v1/install"
        dest: /root/mirador-agent-install.sh
        mode: "0700"
        force: true

    - name: Relancer le script (relais recréé sur son réseau)
      ansible.builtin.command: >-
        bash /root/mirador-agent-install.sh --mirador --auto-update
        --serveur {{ serveur | quote }}
      register: sortie
      changed_when: "'Relais Docker en lecture seule' in sortie.stdout"

    - name: Relais joignable ?
      ansible.builtin.debug:
        msg: "{{ 'relais OK' if 'ATTENTION : le relais Docker' not in sortie.stdout else 'relais BLOQUÉ par le pare-feu : ajouter iptables -I OUTPUT 1 -o br-mirador -s 172.31.254.1 -d 172.31.254.2 -p tcp --dport 2375 -j ACCEPT' }}"
```

## Retour arrière (une machine)

```bash
sudo bash /root/mirador-agent-install.sh --desinstaller
```
