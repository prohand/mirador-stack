# Bascule vers mirador-collector (étape 6, partie 7)

But : faire tourner `mirador-collector` **à côté** d'Alloy et d'unpoller
quelques jours, comparer, puis arrêter Alloy et unpoller **seulement après
ta validation** (SPEC.md § 23.9). Rien n'est coupé par mirador lui-même.

## 1. Installer et régler le collecteur

1. Mettre à jour le serveur (tag) : il embarque le binaire du collecteur.
2. Machines › Ajouter une machine › Machine : **Collecteur du LAN** ;
   coller la commande sur la machine du LAN (un LXC suffit), approuver.
3. Créer les équipements (Machines › Équipement sans agent › *Relevé par*
   ce collecteur) : NAS, contrôleur UniFi, etc. Les noms doivent être
   **ceux d'aujourd'hui** (étiquette `instance` des tableaux et des règles).
4. Page du collecteur : SNMP (NAS), contrôleur UniFi, sondes de certificats,
   puis syslog TLS (réglages deSEC d'abord).

## 2. Comparer (quelques jours)

- Tableaux « Synology », « UniFi », « Certificats TLS » : les courbes du
  collecteur (étiquette `collecteur="…"`) arrivent à côté des anciennes.
- Alertes › Comparaison vmalert : pas d'écart inattendu.
- Logs : `job="syslog"` arrive pour chaque équipement.

## 3. Mesurer (à coller dans une nouvelle session)

Sur la machine du collecteur, après 24 h :

```bash
systemctl show mirador-collector -p MemoryCurrent -p CPUUsageNSec -p ActiveEnterTimestamp
ps -o rss=,etime=,cmd= -C mirador-collector
systemctl show alloy -p MemoryCurrent -p CPUUsageNSec
ps -o rss=,etime=,cmd= -C alloy -C unpoller -C unifi-poller
```

## 4. Arrêter Alloy et unpoller (seulement après validation)

```bash
sudo systemctl disable --now unpoller   # ou unifi-poller
sudo systemctl disable --now alloy
```

Puis, dans mirador : désactiver les règles importées qui lisaient
l'ancienne chaîne et n'ont plus de source (`SyslogMuet` alimentée par
vmalert-logs, `ProxmoxMuet` si le metric server est coupé).

Retour arrière : `sudo systemctl enable --now alloy unpoller` (rien n'a
été effacé).
