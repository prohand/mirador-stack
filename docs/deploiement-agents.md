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

Relevé en lecture seule (`inventaire-ancienne-stack.yml`), à me coller
(noms de services et de fichiers seulement, aucun secret) ; j'écris la
suppression exacte à partir de ce relevé (Alloy, SNMP, `smartctl_exporter`,
scripts `obs-*`, minuteries, cron), avec une garde : rien n'est supprimé sur
une machine dont l'agent mirador n'est pas approuvé et actif.

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

## Retour arrière (une machine)

```bash
sudo bash /root/mirador-agent-install.sh --desinstaller
```
