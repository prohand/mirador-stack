# Installer mirador-agent sur plusieurs machines (avant l'étape 7)

But : mettre l'agent mirador sur chaque machine que voit encore l'ancienne
stack, **à côté d'Alloy** (rien n'est arrêté). Alloy ne s'arrête qu'en
phase B de `docs/bascule-finale.md`, sur validation.

Principe :

- un **jeton d'enrôlement à usage unique** par machine, créé sur le VPS
  (valable 24 h) ;
- l'installation se fait depuis la machine `ansible` (playbook) ; le jeton
  passe par une variable d'environnement : jamais sur une ligne de commande
  (invisible dans `ps`), jamais dans les journaux d'Ansible (`no_log`) ;
- chaque machine arrive « en attente » dans mirador : on l'approuve.

**Les jetons sont des secrets** : ne jamais les coller dans une session ni
les garder après l'installation.

## 0. Liste des machines

Le nom mirador doit être **le même que dans l'ancienne stack** (étiquette
`instance`) : la comparaison vmalert et les règles s'appuient dessus. Ne pas
mettre `obscollector` (c'est le collecteur, `collecteur-lan` dans mirador) ni
les machines qui ont déjà l'agent.

Inventaire Ansible (`mirador.ini`), un groupe `mirador_agents` ;
`mirador_nom` seulement si le nom d'hôte Ansible diffère du nom à garder :

```ini
[mirador_agents]
dns1
dns2
proxmox  mirador_nom=proxmox
```

## 1. Créer les jetons (depuis `ansible`, sortie gardée en local)

`VPS` : nom SSH du VPS. Les jetons arrivent dans `~/mirador-jetons/<nom>`
(dossier 0700), sans passer par une ligne de commande :

```bash
install -d -m 700 ~/mirador-jetons
for n in $(ansible -i mirador.ini mirador_agents --list-hosts | tail -n +2); do
  ssh VPS "sudo docker exec mirador-server /mirador-server -config /etc/mirador/server.conf jeton -duree 24h" \
    | grep -o 'mirador_enrol_[A-Za-z0-9_-]*' > ~/mirador-jetons/"$n" && chmod 600 ~/mirador-jetons/"$n"
done
wc -c ~/mirador-jetons/*
```

Chaque fichier doit faire une cinquantaine d'octets ; un fichier vide = jeton
pas créé (relancer pour cette machine). Si `sudo` demande un mot de passe sur
le VPS, ajouter `-t` à `ssh`.

## 2. Installer (playbook)

`installer-mirador.yml` :

```yaml
- hosts: mirador_agents
  become: true
  vars:
    serveur: https://mirador-ingest.srv-home.fr
    nom: "{{ mirador_nom | default(inventory_hostname) }}"
  tasks:
    - name: Script d'installation (depuis mirador-server)
      ansible.builtin.get_url:
        url: "{{ serveur }}/api/agent/v1/install"
        dest: /root/mirador-agent-install.sh
        mode: "0700"
    - name: Installer mirador-agent (une seule fois)
      ansible.builtin.command:
        cmd: bash /root/mirador-agent-install.sh --serveur {{ serveur }} --nom {{ nom }} --auto-update
        creates: /etc/mirador-agent/agent.conf
      environment:
        MIRADOR_JETON: "{{ lookup('ansible.builtin.file', '~/mirador-jetons/' + inventory_hostname) }}"
      no_log: true
```

D'abord une machine, pour voir :

```bash
ansible-playbook -i mirador.ini installer-mirador.yml -K --limit dns1
```

Puis les autres (5 à la fois) :

```bash
ansible-playbook -i mirador.ini installer-mirador.yml -K --forks 5
```

Le site et le type sont repris d'Alloy (`/etc/default/alloy`) ; Docker
présent : le proxy de socket en lecture seule est installé ; SMART sur les
machines physiques.

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

## 4. Nettoyer et vérifier

```bash
rm -rf ~/mirador-jetons
```

Jetons non utilisés : ils expirent seuls après 24 h.

📋 À me coller : la liste de la page Machines (noms, en ligne ou non).
Ensuite : phase A de `docs/bascule-finale.md` (pas de nouvelle
comparaison vmalert, choix du 24/11).

## Retour arrière (une machine)

```bash
sudo bash /root/mirador-agent-install.sh --desinstaller
```
