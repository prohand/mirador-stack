// SPDX-License-Identifier: GPL-3.0-or-later

package syslog

import (
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"mirador-stack/internal/protocole"
)

// Blocages d'OPNsense (SPEC § 135, § 136) : chaque ligne filterlog est lue,
// seules les lignes « block » entrantes venues d'une adresse publique ou
// sortantes vers une adresse publique sont comptées, par 10 s. Aucune ligne n'est gardée : le collecteur envoie
// des comptages (protocole.LigneFlux), quelques dizaines de lignes par lot.
//
// Format filterlog (CSV, documentation OPNsense) :
//
//	0 règle, 1 sous-règle, 2 ancre, 3 étiquette, 4 interface, 5 raison,
//	6 action, 7 sens, 8 version IP,
//	IPv4 : 9 tos, 10 ecn, 11 ttl, 12 id, 13 décalage, 14 drapeaux,
//	       15 n° de protocole, 16 protocole, 17 longueur, 18 source,
//	       19 destination, 20 port source, 21 port destination…
//	IPv6 : 9 classe, 10 étiquette de flux, 11 limite de sauts, 12 protocole,
//	       13 n° de protocole, 14 longueur, 15 source, 16 destination,
//	       17 port source, 18 port destination…

// LireFilterlog : un blocage d'un flux entrant venu d'Internet ou d'un flux
// sortant vers Internet, sinon false.
//   - sens « out » (paquet qui sort par une interface) : sortant si la
//     destination est publique ;
//   - sens « in » : entrant si la source est publique, sinon sortant si la
//     destination l'est (machine du LAN bloquée en allant vers Internet).
func LireFilterlog(texte string) (protocole.FluxBloque, bool) {
	var f protocole.FluxBloque
	// Découpage dans un tableau sur la pile (relecture perf du 27/11 :
	// strings.Split allouait ~500 octets par ligne, et filterlog est
	// l'essentiel du débit d'un pare-feu). Les champs utiles sont avant
	// le 22e ; au-delà de 32, le reste est ignoré.
	var champs [32]string
	c := decouper(champs[:0], strings.TrimSpace(texte))
	if len(c) < 17 || c[6] != "block" || (c[7] != "in" && c[7] != "out") {
		return f, false
	}
	var iProto, iSrc int
	switch c[8] {
	case "4":
		iProto, iSrc = 16, 18
	case "6":
		iProto, iSrc = 12, 15
	default:
		return f, false
	}
	if len(c) <= iSrc+1 {
		return f, false
	}
	src, err1 := netip.ParseAddr(c[iSrc])
	dst, err2 := netip.ParseAddr(c[iSrc+1])
	if err1 != nil || err2 != nil {
		return f, false
	}
	src, dst = src.Unmap(), dst.Unmap()
	switch {
	case c[7] == "in" && protocole.AdressePublique(src):
		f.Sens, f.IP = protocole.Entrant, src
	case protocole.AdressePublique(dst):
		f.Sens, f.IP, f.Source = protocole.Sortant, dst, src
	default:
		return f, false
	}
	f.N = 1
	f.Interface = nomCourt(c[4], 32)
	f.Proto = strings.ToLower(nomCourt(c[iProto], 12))
	if (f.Proto == "tcp" || f.Proto == "udp") && len(c) > iSrc+3 {
		if p, err := strconv.Atoi(c[iSrc+3]); err == nil && p > 0 && p <= 65535 {
			f.Port = p
		}
	}
	if r := nomCourt(c[0], 12); r != "" {
		f.Regle = "règle " + r
	}
	return f, true
}

// decouper : champs séparés par des virgules, ajoutés à c sans dépasser
// sa capacité (pas d'allocation).
func decouper(c []string, s string) []string {
	for len(c) < cap(c)-1 {
		i := strings.IndexByte(s, ',')
		if i < 0 {
			break
		}
		c = append(c, s[:i])
		s = s[i+1:]
	}
	return append(c, s)
}

// nomCourt : n octets au plus, lettres, chiffres, « . _ - » seulement.
func nomCourt(s string, n int) string {
	if len(s) > n {
		s = s[:n]
	}
	for i := 0; i < len(s); i++ {
		b := s[i]
		if !(b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '.' || b == '_' || b == '-') {
			return ""
		}
	}
	return s
}

// Comptage des blocages, par équipement, sur la fenêtre en cours.

// MaxClesFlux : clés différentes gardées par fenêtre de 10 s (au-delà, les
// blocages sont comptés à part dans « perdus » : mémoire bornée même
// pendant un balayage massif).
const MaxClesFlux = 5000

type cleFlux struct {
	hote string
	f    protocole.FluxBloque // N à 0 dans la clé
}

// CompteurFlux : blocages comptés depuis le dernier Vider.
type CompteurFlux struct {
	mu     sync.Mutex
	n      map[cleFlux]int
	perdus uint64
	total  uint64
}

// Ajouter compte un blocage vu sur l'équipement hote.
func (c *CompteurFlux) Ajouter(hote string, f protocole.FluxBloque) {
	n := f.N
	f.N = 0
	k := cleFlux{hote, f}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.total += uint64(n) // #nosec G115 -- n ≥ 1
	if c.n == nil {
		c.n = map[cleFlux]int{}
	}
	if _, ok := c.n[k]; !ok && len(c.n) >= MaxClesFlux {
		c.perdus += uint64(n) // #nosec G115 -- n ≥ 1
		return
	}
	c.n[k] += n
}

// LigneComptee : une ligne de comptage prête à envoyer.
type LigneComptee struct {
	Hote, Texte string
	Heure       time.Time
}

// Vider renvoie les comptages de la fenêtre et en ouvre une nouvelle.
func (c *CompteurFlux) Vider(heure time.Time) []LigneComptee {
	c.mu.Lock()
	n := c.n
	c.n = nil
	c.mu.Unlock()
	out := make([]LigneComptee, 0, len(n))
	for k, v := range n {
		k.f.N = min(v, protocole.MaxNFlux)
		out = append(out, LigneComptee{Hote: k.hote, Texte: protocole.LigneFlux(k.f), Heure: heure})
	}
	return out
}

// Totaux : blocages comptés depuis le démarrage, et ceux perdus (trop de
// clés dans une fenêtre).
func (c *CompteurFlux) Totaux() (total, perdus uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.total, c.perdus
}
