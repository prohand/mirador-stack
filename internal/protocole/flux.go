// SPDX-License-Identifier: GPL-3.0-or-later

package protocole

import (
	"net/netip"
	"regexp"
	"strconv"
	"strings"
)

// Flux entrants bloqués par les pare-feu (SPEC § 135).
//
// Deux sources, un seul format de ligne (celui du noyau Linux) :
//   - iptables des machines : lignes du noyau lues dans le journal par
//     l'agent (« préfixe IN=eth0 OUT= SRC=… DPT=22 PROTO=TCP ») ;
//   - OPNsense : le collecteur lit filterlog et envoie, toutes les 10 s, des
//     lignes de comptage au même format avec « N=nombre », sous l'identifiant
//     IdentFlux. Le serveur les compte sans les ranger dans les logs.

// IdentFlux : identifiant (champ « ident ») des lignes de comptage du collecteur.
const IdentFlux = "mirador-flux"

// MaxNFlux : nombre le plus grand accepté dans une ligne de comptage (un
// collecteur ne peut pas annoncer des milliards de flux d'un coup).
const MaxNFlux = 1_000_000

// FluxBloque : un flux entrant bloqué (ou N flux identiques).
type FluxBloque struct {
	Regle, Interface, Proto string
	IP                      netip.Addr
	Port                    int
	N                       int
}

var (
	reIN    = regexp.MustCompile(`\bIN=(\S*)`)
	reOUT   = regexp.MustCompile(`\bOUT=(\S*)`)
	reSRC   = regexp.MustCompile(`\bSRC=(\S+)`)
	reDPT   = regexp.MustCompile(`\bDPT=(\d{1,5})\b`)
	rePROTO = regexp.MustCompile(`\bPROTO=([A-Za-z0-9]{1,12})\b`)
	reN     = regexp.MustCompile(`\bN=(\d{1,7})\b`)
)

// AdressePublique : adresse joignable depuis Internet (ni privée, ni
// locale, ni partagée 100.64/10, ni réservée à la documentation).
func AdressePublique(a netip.Addr) bool {
	a = a.Unmap()
	if !a.IsValid() || !a.IsGlobalUnicast() || a.IsPrivate() || a.IsLoopback() {
		return false
	}
	for _, p := range nonPubliques {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

var nonPubliques = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"),   // adresses partagées (CGNAT)
	netip.MustParsePrefix("192.0.0.0/24"),    // protocoles IETF
	netip.MustParsePrefix("192.0.2.0/24"),    // documentation
	netip.MustParsePrefix("198.51.100.0/24"), // documentation
	netip.MustParsePrefix("203.0.113.0/24"),  // documentation
	netip.MustParsePrefix("198.18.0.0/15"),   // essais de performance
	netip.MustParsePrefix("2001:db8::/32"),   // documentation
	netip.MustParsePrefix("64:ff9b:1::/48"),  // traduction locale
	netip.MustParsePrefix("240.0.0.0/4"),     // réservé
	netip.MustParsePrefix("255.255.255.255/32"),
}

// LireFluxBloque lit une ligne au format du noyau. Gardée seulement si
// c'est un flux entrant (IN= rempli, OUT= vide) venu d'une adresse
// publique. avecN : la ligne peut porter « N= » (ligne de comptage du
// collecteur) ; sinon N vaut 1.
func LireFluxBloque(msg string, avecN bool) (FluxBloque, bool) {
	var f FluxBloque
	if !strings.Contains(msg, "SRC=") {
		return f, false
	}
	in, out, src := reIN.FindStringSubmatch(msg), reOUT.FindStringSubmatch(msg), reSRC.FindStringSubmatch(msg)
	if in == nil || out == nil || src == nil || in[1] == "" || out[1] != "" {
		return f, false
	}
	ip, err := netip.ParseAddr(src[1])
	if err != nil || !AdressePublique(ip) {
		return f, false
	}
	f.IP, f.Interface, f.N = ip.Unmap(), borner(in[1], 32), 1
	if p := reDPT.FindStringSubmatch(msg); p != nil {
		if n, err := strconv.Atoi(p[1]); err == nil && n <= 65535 {
			f.Port = n
		}
	}
	if p := rePROTO.FindStringSubmatch(msg); p != nil {
		f.Proto = strings.ToLower(p[1])
	}
	if avecN {
		if p := reN.FindStringSubmatch(msg); p != nil {
			if n, err := strconv.Atoi(p[1]); err == nil && n >= 1 && n <= MaxNFlux {
				f.N = n
			}
		}
	}
	// Règle = ce qui précède « IN= » (préfixe du journal du pare-feu), sans
	// l'horodatage du noyau entre crochets.
	r := strings.TrimSpace(msg[:strings.Index(msg, "IN=")])
	if i := strings.LastIndex(r, "] "); i >= 0 && strings.HasPrefix(r, "[") {
		r = strings.TrimSpace(r[i+2:])
	}
	f.Regle = borner(r, 60)
	return f, true
}

// LigneFlux écrit un comptage au format du noyau (lu par LireFluxBloque).
func LigneFlux(f FluxBloque) string {
	var b strings.Builder
	if f.Regle != "" {
		b.WriteString(f.Regle)
		b.WriteByte(' ')
	}
	b.WriteString("IN=" + f.Interface + " OUT= SRC=" + f.IP.String())
	if f.Proto != "" {
		b.WriteString(" PROTO=" + strings.ToUpper(f.Proto))
	}
	if f.Port > 0 {
		b.WriteString(" DPT=" + strconv.Itoa(f.Port))
	}
	b.WriteString(" N=" + strconv.Itoa(f.N))
	return b.String()
}

// borner : au plus n octets, sans caractère de contrôle (nom
// d'interface, règle).
func borner(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	if len(s) > n {
		s = strings.ToValidUTF8(s[:n], "")
	}
	return s
}
