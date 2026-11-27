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

// Sens d'un flux bloqué.
const (
	Entrant = "entrant" // venu d'Internet (IP = source)
	Sortant = "sortant" // vers Internet (IP = destination, Source = adresse locale)
)

// FluxBloque : un flux bloqué (ou N flux identiques). IP est toujours
// l'adresse publique (source d'un entrant, destination d'un sortant) ; Port
// est le port visé.
type FluxBloque struct {
	Sens                    string
	Regle, Interface, Proto string
	IP                      netip.Addr
	Source                  netip.Addr // sortant : adresse locale qui essayait de sortir
	Port                    int
	N                       int
}

var (
	reIN    = regexp.MustCompile(`\bIN=(\S*)`)
	reOUT   = regexp.MustCompile(`\bOUT=(\S*)`)
	reSRC   = regexp.MustCompile(`\bSRC=(\S+)`)
	reDST   = regexp.MustCompile(`\bDST=(\S+)`)
	reSENS  = regexp.MustCompile(`\bSENS=(entrant|sortant)\b`)
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

// LireFluxBloque lit une ligne au format du noyau. Gardée seulement si un
// côté est public :
//   - IN rempli, OUT vide (paquet pour la machine) : entrant si la source
//     est publique ;
//   - IN vide, OUT rempli (paquet émis par la machine) : sortant si la
//     destination est publique ;
//   - les deux remplis (paquet qui traverse) : entrant si la source est
//     publique, sinon sortant si la destination l'est.
//
// avecN : ligne de comptage du collecteur (« N= » et « SENS= » lus) ;
// sinon N vaut 1.
func LireFluxBloque(msg string, avecN bool) (FluxBloque, bool) {
	var f FluxBloque
	if !strings.Contains(msg, "SRC=") {
		return f, false
	}
	in, out, src := reIN.FindStringSubmatch(msg), reOUT.FindStringSubmatch(msg), reSRC.FindStringSubmatch(msg)
	if in == nil || out == nil || src == nil || (in[1] == "" && out[1] == "") {
		return f, false
	}
	ipSrc, err := netip.ParseAddr(src[1])
	if err != nil {
		return f, false
	}
	ipSrc = ipSrc.Unmap()
	var ipDst netip.Addr
	if d := reDST.FindStringSubmatch(msg); d != nil {
		if a, err := netip.ParseAddr(d[1]); err == nil {
			ipDst = a.Unmap()
		}
	}
	sens := ""
	if avecN {
		if p := reSENS.FindStringSubmatch(msg); p != nil {
			sens = p[1]
		}
	}
	switch {
	case sens == Sortant || (sens == "" && in[1] == "" && out[1] != ""):
		sens = Sortant
	case sens == Entrant || (sens == "" && out[1] == ""):
		sens = Entrant
	case AdressePublique(ipSrc): // traversant
		sens = Entrant
	default:
		sens = Sortant
	}
	f.Sens, f.N = sens, 1
	if sens == Entrant {
		if !AdressePublique(ipSrc) {
			return FluxBloque{}, false
		}
		f.IP, f.Interface = ipSrc, borner(in[1], 32)
	} else {
		if !AdressePublique(ipDst) {
			return FluxBloque{}, false
		}
		f.IP, f.Source, f.Interface = ipDst, ipSrc, borner(out[1], 32)
		if f.Interface == "" {
			f.Interface = borner(in[1], 32)
		}
	}
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

// PrefixeLigne renvoie le préfixe d'une ligne du noyau (ce qui précède « IN= »),
// ou "" (filtre « seulement ces préfixes »).
func PrefixeLigne(msg string) string {
	i := strings.Index(msg, "IN=")
	if i < 0 {
		return ""
	}
	r := strings.TrimSpace(msg[:i])
	if j := strings.LastIndex(r, "] "); j >= 0 && strings.HasPrefix(r, "[") {
		r = strings.TrimSpace(r[j+2:])
	}
	return r
}

// LigneFlux écrit un comptage au format du noyau (lu par LireFluxBloque).
func LigneFlux(f FluxBloque) string {
	var b strings.Builder
	if f.Regle != "" {
		b.WriteString(f.Regle)
		b.WriteByte(' ')
	}
	if f.Sens == Sortant {
		b.WriteString("IN= OUT=" + f.Interface + " SRC=" + f.Source.String() + " DST=" + f.IP.String())
	} else {
		b.WriteString("IN=" + f.Interface + " OUT= SRC=" + f.IP.String())
	}
	if f.Proto != "" {
		b.WriteString(" PROTO=" + strings.ToUpper(f.Proto))
	}
	if f.Port > 0 {
		b.WriteString(" DPT=" + strconv.Itoa(f.Port))
	}
	b.WriteString(" N=" + strconv.Itoa(f.N))
	if f.Sens == Sortant {
		b.WriteString(" SENS=" + Sortant)
	}
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
