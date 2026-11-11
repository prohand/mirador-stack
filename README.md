# mirador-stack

Supervision légère pour un petit parc (une trentaine de machines), écrite en Go.
Un seul serveur remplace VictoriaMetrics, VictoriaLogs, Grafana, vmalert et
Alertmanager : **objectif < 256 Mio de RAM** pour le serveur, < 30 Mio par agent.

> État : en développement (étape 5 sur 7 : interface web). Utilisé en
> parallèle d'une stack existante pour comparaison. Voir `SPEC.md`
> (décisions) et `PROGRES.md` (avancement et mesures).

## Principe

```
machines (mirador-agent) ──HTTPS, push──▶ nginx ──▶ mirador-server ──▶ interface web
                                                    (métriques, logs,     (2FA)
                                                     alertes, notifications)
```

- **mirador-agent** : envoie métriques et logs (journald, Docker). **N'ouvre
  aucun port**, ne reçoit jamais de commande : sa configuration à distance est
  typée et vérifiée (pas de shell, pas de script).
- **mirador-server** : stockage des métriques (TSDB Prometheus) et des logs
  (format compact maison), règles d'alerte en PromQL, notifications
  (Telegram, Olvid, ntfy, e-mail, webhook…), interface web, sauvegardes
  chiffrées.
- **mirador-release** : signe les versions de l'agent (Ed25519) sur le poste
  de l'administrateur ; la clé privée ne va jamais sur le serveur.
- **mirador-collector** (à venir) : collecteur sur le réseau local pour ce qui
  ne sait pas pousser seul (SNMP, syslog…).

## Fonctions

- Enrôlement d'une machine par jeton à usage unique, approuvé dans l'interface.
- Tableaux de bord modifiables, aide à l'écriture des requêtes PromQL.
- Recherche dans les logs, flux bloqués par le pare-feu (carte par pays).
- Alertes : silences, maintenances, dépendances, regroupement, mode « ombre »
  (tout est calculé, rien n'est envoyé) pour comparer avec une autre stack.
- Import des règles vmalert / Prometheus.
- Mises à jour signées des agents, avec retour arrière.

## Sécurité

- Interface : mots de passe Argon2id, **2FA obligatoire**, CSRF, CSP stricte
  (aucun script en ligne), verrouillage des tentatives, rôles lecture / admin,
  journal d'audit.
- Agents : token par machine (haché côté serveur, révocable, limitable à une
  adresse), quotas de réception.
- Secrets chiffrés au repos ; sauvegardes chiffrées.
- CI : tests (avec détection des accès concurrents), fuzzing, gosec,
  govulncheck, vérification des licences.

Signaler une faille : voir [SECURITY.md](SECURITY.md).

## Documentation

| Sujet | Fichier |
|---|---|
| Installer le serveur | [docs/installation-serveur.md](docs/installation-serveur.md) |
| Installer un agent | [docs/installation-agent.md](docs/installation-agent.md) |
| Interface web | [docs/interface-web.md](docs/interface-web.md) |
| Administration | [docs/administration-serveur.md](docs/administration-serveur.md) |
| Sauvegarde et restauration | [docs/sauvegarde-restauration.md](docs/sauvegarde-restauration.md) |
| Notifications Olvid | [docs/olvid.md](docs/olvid.md) |

Agents : Debian / Ubuntu, amd64 et arm64. Serveur : image Docker
(`FROM scratch`, utilisateur non root, système de fichiers en lecture seule).

## Licence

mirador-stack est un logiciel libre : vous pouvez le redistribuer et le modifier
selon les termes de la GNU General Public License publiée par la Free Software
Foundation, version 3 de la licence ou (à votre choix) toute version ultérieure.
Il est distribué sans aucune garantie. Voir [LICENSE](LICENSE).

Le client gRPC Olvid généré (`internal/server/notif/olvid/pb/`) reste sous
AGPL-3.0. Composants tiers et licences : [THIRD-PARTY.md](THIRD-PARTY.md).
