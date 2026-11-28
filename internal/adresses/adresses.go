// SPDX-License-Identifier: GPL-3.0-or-later

// Package adresses : listes d'adresses IP refusées pour les connexions
// sortantes (protection SSRF), communes au serveur, à l'agent et au
// collecteur (revue de sécurité du 28/11, B7).
//
// Le contrôle se fait au moment de la connexion (net.Dialer.Control) : c'est
// l'adresse réellement jointe, après résolution DNS et à chaque redirection,
// qui est vérifiée. Un nom qui change d'adresse entre deux résolutions (« DNS
// rebinding ») ne passe donc pas.
//
// Les formes IPv6 qui contiennent une adresse IPv4 (IPv4 dans IPv6
// « ::ffff:a.b.c.d », NAT64 « 64:ff9b::a.b.c.d », 6to4 « 2002:aabb:ccdd:: »)
// sont d'abord converties : « ::ffff:127.0.0.1 » est la boucle locale.
//
// Coût : quelques comparaisons de préfixes par connexion, rien en mémoire.
package adresses

import (
	"fmt"
	"net"
	"net/netip"
	"syscall"
)

// prefixes : lit une liste de réseaux écrits dans le code (erreur = bogue).
func prefixes(l ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(l))
	for i, s := range l {
		out[i] = netip.MustParsePrefix(s)
	}
	return out
}

// interditsServeur : réseaux jamais joints depuis le serveur.
var interditsServeur = prefixes(
	// IPv4
	"0.0.0.0/8",      // « ce réseau » : 0.0.0.0 joint la machine elle-même
	"10.0.0.0/8",     // privé
	"100.64.0.0/10",  // CGNAT (adresses partagées des opérateurs, Tailscale)
	"127.0.0.0/8",    // boucle locale
	"169.254.0.0/16", // lien local (métadonnées des clouds : 169.254.169.254)
	"172.16.0.0/12",  // privé (dont les réseaux Docker)
	"192.0.0.0/24",   // protocoles de l'IETF
	"192.168.0.0/16", // privé
	"198.18.0.0/15",  // bancs d'essai
	"224.0.0.0/4",    // multicast
	"240.0.0.0/4",    // réservé (et 255.255.255.255)
	// IPv6
	"::/128",         // non spécifiée
	"::1/128",        // boucle locale
	"64:ff9b:1::/48", // NAT64 à usage local
	"2001::/32",      // Teredo (adresse IPv4 cachée dedans)
	"fc00::/7",       // privé (ULA)
	"fe80::/10",      // lien local
	"fec0::/10",      // ancien « site local »
	"ff00::/8",       // multicast
)

// lienLocal : lien local (métadonnées des clouds, voisins directs).
var lienLocal = prefixes("169.254.0.0/16", "fe80::/10")

// boucleLocale : la machine elle-même (boucle locale, adresse non spécifiée :
// se connecter à 0.0.0.0 ou à « :: » joint la machine locale sous Linux).
var boucleLocale = prefixes("127.0.0.0/8", "0.0.0.0/8", "::1/128", "::/128")

var (
	nat64 = netip.MustParsePrefix("64:ff9b::/96")
	six4  = netip.MustParsePrefix("2002::/16")
)

// Normaliser : l'adresse IPv4 contenue dans une adresse IPv6 (IPv4 dans
// IPv6, NAT64, 6to4) ; sinon l'adresse telle quelle (zone retirée).
func Normaliser(a netip.Addr) netip.Addr {
	a = a.WithZone("").Unmap()
	if !a.Is6() {
		return a
	}
	b := a.As16()
	switch {
	case nat64.Contains(a):
		return netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]})
	case six4.Contains(a):
		return netip.AddrFrom4([4]byte{b[2], b[3], b[4], b[5]})
	}
	return a
}

func dans(a netip.Addr, l []netip.Prefix) bool {
	for _, p := range l {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// depuisIP : net.IP → netip.Addr (invalide si l'adresse est illisible).
func depuisIP(ip net.IP) netip.Addr {
	a, _ := netip.AddrFromSlice(ip)
	return a
}

// InterditeServeur : adresse refusée pour une requête faite depuis le
// serveur (notifications, sondes « depuis le serveur », RDAP, push) :
// boucle locale, réseaux privés, lien local, CGNAT, multicast, réservés.
// Une adresse illisible est refusée.
func InterditeServeur(ip net.IP) bool { return InterditeServeurAddr(depuisIP(ip)) }

// InterditeServeurAddr : comme InterditeServeur, pour une netip.Addr.
func InterditeServeurAddr(a netip.Addr) bool {
	if !a.IsValid() {
		return true
	}
	return dans(Normaliser(a), interditsServeur)
}

// LienLocal : 169.254.0.0/16 ou fe80::/10 (formes IPv4 dans IPv6 comprises).
// Toujours refusé aux checks de l'agent et aux sondes du collecteur : c'est
// là que les clouds publient les secrets de la machine (métadonnées).
// Une adresse illisible est refusée.
func LienLocal(ip net.IP) bool {
	a := depuisIP(ip)
	return !a.IsValid() || dans(Normaliser(a), lienLocal)
}

// LocaleOuLienLocal : boucle locale, adresse non spécifiée ou lien local
// (refusées par défaut aux sondes du collecteur ; le LAN reste permis).
// Une adresse illisible est refusée.
func LocaleOuLienLocal(ip net.IP) bool {
	a := depuisIP(ip)
	if !a.IsValid() {
		return true
	}
	a = Normaliser(a)
	return dans(a, lienLocal) || dans(a, boucleLocale)
}

// Privee : adresse du LAN (RFC 1918, RFC 4193), après conversion d'une
// forme IPv4 dans IPv6 ; jamais la boucle locale, le lien local ni une
// adresse publique.
func Privee(ip net.IP) bool {
	a := depuisIP(ip)
	if !a.IsValid() {
		return false
	}
	a = Normaliser(a)
	return a.IsPrivate() && !a.IsUnspecified()
}

// Controle : fonction pour net.Dialer.Control qui refuse la connexion si
// refuser(adresse réellement jointe) est vrai. quoi : fin du message
// d'erreur (ex. « refusée depuis le serveur »). Le contrôle a lieu à chaque
// connexion, redirections comprises.
func Controle(refuser func(net.IP) bool, quoi string) func(network, address string, c syscall.RawConn) error {
	return func(_, adresse string, _ syscall.RawConn) error {
		hote, _, err := net.SplitHostPort(adresse)
		if err != nil {
			return err
		}
		if ip := net.ParseIP(hote); ip == nil || refuser(ip) {
			return fmt.Errorf("adresse %s %s", hote, quoi)
		}
		return nil
	}
}
