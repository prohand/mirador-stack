#!/bin/sh
# SPDX-License-Identifier: GPL-3.0-or-later
#
# mirador-blocage : liste de blocage de mirador appliquée sur ce serveur
# (nftables). À installer soi-même, une fois par serveur ; mirador ne
# touche à rien : c'est ce serveur qui vient lire la liste.
#
#   Installer :    sudo sh mirador-blocage.sh https://mirador…/blocage/JETON.txt
#   Relire :       sudo systemctl start mirador-blocage.service
#   Voir :         sudo nft list set inet mirador_blocage v4
#   Désinstaller : sudo sh mirador-blocage.sh --retirer
#
# Ce que fait le script :
#   - installe une copie de lui-même dans /usr/local/sbin/mirador-blocage,
#     l'adresse de la liste dans /etc/mirador-blocage.url (lisible par root) ;
#   - une minuterie systemd relit la liste toutes les 15 min ;
#   - chaque lecture remplace la table nftables « inet mirador_blocage » et
#     elle seule : vos propres règles ne sont jamais modifiées ;
#   - la liste ne contient que des adresses : toute autre ligne est refusée
#     et l'ancienne liste reste en place ; liste illisible ou injoignable :
#     rien ne change.
set -eu

TABLE=mirador_blocage
FICHIER_URL=/etc/mirador-blocage.url
BINAIRE=/usr/local/sbin/mirador-blocage
UNITE=/etc/systemd/system/mirador-blocage
MAX=5000

erreur() { echo "mirador-blocage : $*" >&2; exit 1; }

[ "$(id -u)" = 0 ] || erreur "à lancer en root (sudo)"
command -v nft >/dev/null 2>&1 || erreur "nft absent (paquet nftables)"
command -v curl >/dev/null 2>&1 || erreur "curl absent"

retirer() {
	systemctl disable --now mirador-blocage.timer 2>/dev/null || true
	rm -f "$UNITE.timer" "$UNITE.service" "$BINAIRE" "$FICHIER_URL"
	systemctl daemon-reload 2>/dev/null || true
	nft delete table inet "$TABLE" 2>/dev/null || true
	echo "mirador-blocage retiré (table $TABLE supprimée)"
}

# appliquer : lit la liste et remplace la table (une seule opération nft).
appliquer() {
	[ -r "$FICHIER_URL" ] || erreur "$FICHIER_URL absent : installer d'abord"
	url=$(head -n 1 "$FICHIER_URL")
	tmp=$(mktemp)
	trap 'rm -f "$tmp" "$tmp.nft"' EXIT
	curl -fsS --proto =https --max-time 30 --max-filesize 1000000 -o "$tmp" "$url" || erreur "liste injoignable : rien ne change"
	# Que des adresses IPv4 ou IPv6, une par ligne.
	if grep -Evq '^([0-9]{1,3}(\.[0-9]{1,3}){3}|[0-9A-Fa-f]{0,4}(:[0-9A-Fa-f]{0,4}){2,7})$' "$tmp"; then
		erreur "ligne inattendue dans la liste : rien ne change"
	fi
	n=$(wc -l <"$tmp")
	[ "$n" -le "$MAX" ] || erreur "liste trop longue ($n) : rien ne change"
	v4=$(grep -E '^[0-9.]+$' "$tmp" | paste -sd, - || true)
	v6=$(grep -E ':' "$tmp" | paste -sd, - || true)
	e4="" e6=""
	[ -n "$v4" ] && e4=" elements = { $v4 };"
	[ -n "$v6" ] && e6=" elements = { $v6 };"
	{
		echo "table inet $TABLE"
		echo "delete table inet $TABLE"
		echo "table inet $TABLE {"
		echo "  set v4 { type ipv4_addr; flags interval; auto-merge;$e4 }"
		echo "  set v6 { type ipv6_addr; flags interval; auto-merge;$e6 }"
		echo "  chain entree {"
		echo "    type filter hook input priority -10; policy accept;"
		echo "    ip saddr @v4 drop"
		echo "    ip6 saddr @v6 drop"
		echo "  }"
		echo "}"
	} >"$tmp.nft"
	nft -f "$tmp.nft" || erreur "nft a refusé la liste : rien ne change"
	echo "mirador-blocage : $n adresse(s) bloquée(s)"
}

installer() {
	case "$1" in
	https://*/blocage/*.txt) ;;
	*) erreur "adresse attendue : https://…/blocage/JETON.txt" ;;
	esac
	umask 077
	printf '%s\n' "$1" >"$FICHIER_URL"
	install -m 0755 "$0" "$BINAIRE"
	cat >"$UNITE.service" <<EOF
[Unit]
Description=mirador : liste de blocage (nftables)
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
ExecStart=$BINAIRE --appliquer
NoNewPrivileges=yes
PrivateTmp=yes
ProtectHome=yes
EOF
	cat >"$UNITE.timer" <<EOF
[Unit]
Description=mirador : relire la liste de blocage toutes les 15 min

[Timer]
OnBootSec=2min
OnUnitActiveSec=15min
RandomizedDelaySec=60

[Install]
WantedBy=timers.target
EOF
	systemctl daemon-reload
	systemctl enable --now mirador-blocage.timer
	appliquer
	echo "installé : relu toutes les 15 min (systemctl list-timers mirador-blocage.timer)"
}

case "${1:-}" in
--retirer) retirer ;;
--appliquer) appliquer ;;
https://*) installer "$1" ;;
*) erreur "usage : $0 https://…/blocage/JETON.txt | --appliquer | --retirer" ;;
esac
