#!/usr/bin/env bash
# SPDX-License-Identifier: GPL-3.0-or-later
#
# mesurer.sh — mesure la consommation de mirador-agent (et d'Alloy pour comparer)
#
# Lecture seule : ce script ne modifie rien. Il relève toutes les minutes la
# mémoire résidente, le temps CPU et les écritures disque de :
#   - mirador-agent (processus principal) ;
#   - son journalctl (processus enfant) ;
#   - l'assistant root (temps cumulé de ses passages, via systemd) ;
#   - le proxy Docker mirador-docker-proxy (s'il existe) ;
#   - Alloy (pour comparer).
#
# Usage (en root, pour lire /proc/<pid>/io) :
#   sudo ./mesurer.sh 24        mesure pendant 24 heures (défaut : 1 heure)
#   sudo ./mesurer.sh 0.2       mesure pendant 12 minutes (essai rapide)
#
# Résultat : /var/tmp/mirador-mesures.csv (détail) et un résumé à l'écran,
# à recopier dans la session suivante.

set -euo pipefail
export LC_ALL=C

HEURES="${1:-1}"
FICHIER=/var/tmp/mirador-mesures.csv
CLK=$(getconf CLK_TCK)
PAGE=$(getconf PAGESIZE)
FIN=$(( $(date +%s) + $(awk -v h="$HEURES" 'BEGIN{printf "%d", h*3600}') ))

[ "$(id -u)" -eq 0 ] || { echo "à lancer en root (lecture de /proc/<pid>/io)"; exit 1; }

pid_unite() { systemctl show -p MainPID --value "$1" 2>/dev/null || echo 0; }

# releve PID NOM : une ligne CSV. rss = mémoire résidente totale (Ko) ;
# anon = mémoire propre au processus (RssAnon, Ko), sans le cache des fichiers
# projetés en mémoire (cas de journalctl) ; cpu en ticks ; écritures en octets.
releve() {
  local pid=$1 nom=$2
  [ "$pid" -gt 0 ] 2>/dev/null && [ -r "/proc/$pid/stat" ] || return 0
  local rss anon cpu ecrit
  rss=$(awk -v p="$PAGE" '{print $2*p/1024}' "/proc/$pid/statm")
  anon=$(awk '/^RssAnon:/{print $2}' "/proc/$pid/status" 2>/dev/null || echo 0)
  cpu=$(awk '{print $14+$15}' "/proc/$pid/stat")
  ecrit=$(awk '/^write_bytes/{print $2}' "/proc/$pid/io" 2>/dev/null || echo 0)
  echo "$(date +%s),$nom,$pid,$rss,$cpu,$ecrit,${anon:-0}" >> "$FICHIER"
}

echo "horodatage,processus,pid,rss_ko,cpu_ticks,ecrit_octets,anon_ko" > "$FICHIER"
echo "Mesure pendant $HEURES h, une ligne par minute dans $FICHIER (Ctrl-C pour arrêter)."
while [ "$(date +%s)" -lt "$FIN" ]; do
  AGENT=$(pid_unite mirador-agent.service)
  releve "$AGENT" mirador-agent
  for enfant in $(pgrep -P "$AGENT" 2>/dev/null || true); do
    releve "$enfant" journalctl
  done
  releve "$(pid_unite alloy.service)" alloy
  if command -v docker >/dev/null 2>&1 && docker inspect mirador-docker-proxy >/dev/null 2>&1; then
    releve "$(docker inspect -f '{{.State.Pid}}' mirador-docker-proxy)" proxy-docker
  fi
  sleep 60
done

echo
echo "===== Résumé (à recopier) ====="
echo "Machine : $(hostname -s) — $(nproc) cœurs — mesure sur $HEURES h"
awk -F, -v clk="$CLK" 'NR>1 {
  n[$2]++; s[$2]+=$4; sa[$2]+=$7; if ($4>mx[$2]) mx[$2]=$4; if (!($2 in mn) || $4<mn[$2]) mn[$2]=$4
  if (!($2 in c0)) {c0[$2]=$5; t0[$2]=$1; w0[$2]=$6}
  c1[$2]=$5; t1[$2]=$1; w1[$2]=$6
} END {
  printf "%-14s %10s %10s %10s %11s %12s %14s\n", "processus", "RSS min", "RSS moy", "RSS max", "propre moy", "CPU moyen", "écrit disque"
  for (p in n) {
    d = t1[p]-t0[p]; cpu = (d>0) ? (c1[p]-c0[p])/clk/d*100 : 0
    printf "%-14s %8.1fMo %8.1fMo %8.1fMo %9.1fMo %10.3f %% %12.0f Ko\n", p, mn[p]/1024, s[p]/n[p]/1024, mx[p]/1024, sa[p]/n[p]/1024, cpu, (w1[p]-w0[p])/1024
  }
}' "$FICHIER"
echo
echo "Assistant root (cumul depuis le démarrage de l'unité) :"
systemctl show mirador-agent-root.service -p CPUUsageNSec -p MemoryPeak -p NRestarts 2>/dev/null || true
echo "Avertissements de l'agent sur les 5 dernières minutes (vide = aucun problème) :"
journalctl -u mirador-agent --since '-5min' -p warning --no-pager -q 2>/dev/null | tail -5 || true
echo "Mémoire de journalctl (RssAnon = propre au processus, RssFile = cache des fichiers du journal) :"
for p in $(pgrep -P "$(pid_unite mirador-agent.service)" 2>/dev/null || true); do
  grep -E 'Rss(Anon|File|Shmem)' "/proc/$p/status" 2>/dev/null || true
done
