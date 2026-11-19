// SPDX-License-Identifier: GPL-3.0-or-later

package collect

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"mirador-stack/internal/agent/metrics"
)

// Ecoutes : adresses IP de la machine et ports TCP ouverts (SPEC § 60),
// pour proposer des sondes dans l'interface (ping, HTTPS, HTTP, TCP) :
//
//		mirador_adresse_info{interface, adresse}       = 1
//		mirador_port_ecoute{port, adresse, protocole}  = 1
//
//	  - Ports : lus dans /proc/net/tcp et tcp6 (lisibles par tous, aucun
//	    programme lancé). Un port qui n'écoute que sur 127.0.0.1 / ::1 n'est
//	    pas envoyé (personne d'autre ne peut l'essayer). adresse = « * »
//	    (toutes les adresses) ou l'adresse précise.
//	  - Protocole : chaque port nouveau est essayé UNE fois, en local, par
//	    une tâche à part (la collecte n'attend pas) :
//	    1. connexion, 0,5 s d'attente : le service parle le premier
//	    (SSH, SMTP, FTP…) → « ssh » ou « tcp » ;
//	    2. poignée de main TLS, puis « HEAD / » → « https » (ou « tls ») ;
//	    3. « HEAD / » en clair → « http », sinon « tcp ».
//	    Lecture seule : aucune donnée envoyée hormis ces deux questions. Le
//	    service peut noter une ligne dans son journal (connexion fermée tôt) :
//	    une fois par port, au démarrage de l'agent ou à l'ouverture du port.
//	  - Relu toutes les 5 min (fichiers de quelques Kio) ; envoyé à chaque
//	    passage lent pour que les séries ne disparaissent pas.
type Ecoutes struct {
	proc       string
	maintenant func() time.Time
	delai      time.Duration               // délai d'un essai
	essayer    func(adresse string) string // détection (remplaçable en test)

	lu        time.Time
	adresses  []adresseIP
	ports     []portEcoute
	mu        sync.Mutex
	protocole map[string]string // cle() → protocole détecté
	enCours   bool
}

type adresseIP struct{ iface, ip string }

type portEcoute struct {
	port    uint16
	adresse string // « * » ou adresse précise
	essai   string // adresse:port à essayer
}

func (p portEcoute) cle() string { return p.essai }

// Bornes.
const (
	relectureEcoutes = 5 * time.Minute
	maxPortsEcoute   = 64
	maxAdressesIP    = 16
	attenteBanniere  = 500 * time.Millisecond
	delaiEssaiPort   = 2 * time.Second
)

// NewEcoutes crée le collecteur (proc : racine de /proc).
func NewEcoutes(proc string) *Ecoutes {
	e := &Ecoutes{proc: proc, maintenant: time.Now, delai: delaiEssaiPort, protocole: map[string]string{}}
	e.essayer = func(a string) string { return DetecterProtocole(a, e.delai) }
	return e
}

func (e *Ecoutes) Nom() string { return "ecoutes" }
func (e *Ecoutes) Lent() bool  { return true }

func (e *Ecoutes) Collecter(b *metrics.Batch) error {
	t := e.maintenant()
	var err error
	if e.lu.IsZero() || t.Sub(e.lu) >= relectureEcoutes {
		e.lu = t
		e.adresses = adressesLocales()
		e.ports, err = lirePortsEcoute(e.proc)
		e.lancerDetection()
	}
	for _, a := range e.adresses {
		b.Add("mirador_adresse_info", 1, metrics.L("interface", a.iface), metrics.L("adresse", a.ip))
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, p := range e.ports {
		if proto := e.protocole[p.cle()]; proto != "" {
			b.Add("mirador_port_ecoute", 1, metrics.L("port", strconv.Itoa(int(p.port))),
				metrics.L("adresse", p.adresse), metrics.L("protocole", proto))
		}
	}
	return err
}

// lancerDetection : essaie les ports pas encore connus, dans une tâche à
// part (une seule à la fois). Les ports fermés sont oubliés (essayés de
// nouveau s'ils rouvrent).
func (e *Ecoutes) lancerDetection() {
	e.mu.Lock()
	defer e.mu.Unlock()
	ouverts := map[string]bool{}
	var nouveaux []string
	for _, p := range e.ports {
		ouverts[p.cle()] = true
		if _, ok := e.protocole[p.cle()]; !ok {
			nouveaux = append(nouveaux, p.essai)
		}
	}
	for k := range e.protocole {
		if !ouverts[k] {
			delete(e.protocole, k)
		}
	}
	if e.enCours || len(nouveaux) == 0 {
		return
	}
	e.enCours = true
	go func() {
		for _, a := range nouveaux {
			proto := e.essayer(a)
			e.mu.Lock()
			if proto != "" { // injoignable : réessayé au prochain passage
				e.protocole[a] = proto
			}
			e.mu.Unlock()
		}
		e.mu.Lock()
		e.enCours = false
		e.mu.Unlock()
	}()
}

// --- Ports ouverts ------------------------------------------------------------

// lirePortsEcoute : ports TCP en écoute (état 0A), hors boucle locale,
// triés par port ; « * » l'emporte sur les adresses précises d'un port.
func lirePortsEcoute(proc string) ([]portEcoute, error) {
	type ecoute struct {
		ip   netip.Addr
		port uint16
	}
	var toutes []ecoute
	var dernErr error
	for _, f := range []string{"tcp", "tcp6"} {
		contenu, err := os.ReadFile(filepath.Join(proc, "net", f)) // #nosec G304 -- chemin fixe sous /proc
		if err != nil {
			dernErr = err
			continue
		}
		sc := bufio.NewScanner(bytes.NewReader(contenu))
		sc.Scan() // en-tête
		for sc.Scan() {
			// Tri rapide : seules les lignes en écoute (état « 0A ») sont
			// découpées ; les connexions établies (souvent des milliers)
			// sont sautées sans allocation.
			if !bytes.Contains(sc.Bytes(), etatEcoute) {
				continue
			}
			ip, port, ok := ligneEcoute(sc.Text())
			if ok {
				toutes = append(toutes, ecoute{ip, port})
			}
		}
	}
	parPort := map[uint16][]netip.Addr{}
	for _, x := range toutes {
		parPort[x.port] = append(parPort[x.port], x.ip)
	}
	var out []portEcoute
	for port, ips := range parPort {
		tous := false
		v4 := false
		for _, ip := range ips {
			if ip.IsUnspecified() {
				tous = true
				v4 = v4 || ip.Is4()
			}
		}
		if tous {
			essai := "[::1]"
			if v4 {
				essai = "127.0.0.1"
			}
			out = append(out, portEcoute{port: port, adresse: "*", essai: essai + ":" + strconv.Itoa(int(port))})
			continue
		}
		vu := map[netip.Addr]bool{}
		for _, ip := range ips {
			if ip.IsLoopback() || ip.IsLinkLocalUnicast() || vu[ip] {
				continue
			}
			vu[ip] = true
			out = append(out, portEcoute{port: port, adresse: ip.String(), essai: netip.AddrPortFrom(ip, port).String()})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].port != out[j].port {
			return out[i].port < out[j].port
		}
		return out[i].adresse < out[j].adresse
	})
	if len(out) > maxPortsEcoute {
		out = out[:maxPortsEcoute]
	}
	if len(out) > 0 {
		dernErr = nil
	}
	return out, dernErr
}

var etatEcoute = []byte(" 0A ")

// ligneEcoute : « 0: 0100007F:0016 00000000:0000 0A … » → adresse, port, vrai
// si l'état est LISTEN (0A). Les adresses sont écrites mot de 32 bits par
// mot, dans l'ordre de la machine.
func ligneEcoute(l string) (netip.Addr, uint16, bool) {
	ch := strings.Fields(l)
	if len(ch) < 4 || ch[3] != "0A" {
		return netip.Addr{}, 0, false
	}
	hote, portHex, ok := strings.Cut(ch[1], ":")
	if !ok {
		return netip.Addr{}, 0, false
	}
	p, err := strconv.ParseUint(portHex, 16, 16)
	if err != nil {
		return netip.Addr{}, 0, false
	}
	brut, err := hex.DecodeString(hote)
	if err != nil || (len(brut) != 4 && len(brut) != 16) {
		return netip.Addr{}, 0, false
	}
	octets := make([]byte, len(brut))
	for i := 0; i < len(brut); i += 4 {
		binary.BigEndian.PutUint32(octets[i:], binary.NativeEndian.Uint32(brut[i:]))
	}
	ip, _ := netip.AddrFromSlice(octets)
	return ip.Unmap(), uint16(p), true
}

// --- Adresses -----------------------------------------------------------------

// interfacesIgnorees : réseaux internes (conteneurs, ponts virtuels).
var interfacesIgnorees = []string{"docker", "br-", "veth", "virbr", "cni", "flannel", "cali", "vnet", "tap", "fwbr", "fwpr", "fwln"}

// adressesLocales : adresses des interfaces actives (hors boucle locale,
// lien local et réseaux de conteneurs), 16 au plus.
func adressesLocales() []adresseIP {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []adresseIP
	for _, i := range ifs {
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagLoopback != 0 || ignoree(i.Name) {
			continue
		}
		as, err := i.Addrs()
		if err != nil {
			continue
		}
		for _, a := range as {
			n, ok := a.(*net.IPNet)
			if !ok || n.IP.IsLoopback() || n.IP.IsLinkLocalUnicast() {
				continue
			}
			out = append(out, adresseIP{iface: i.Name, ip: n.IP.String()})
			if len(out) == maxAdressesIP {
				return out
			}
		}
	}
	return out
}

func ignoree(nom string) bool {
	for _, p := range interfacesIgnorees {
		if strings.HasPrefix(nom, p) {
			return true
		}
	}
	return false
}

// --- Détection du protocole ---------------------------------------------------

// DetecterProtocole : « ssh », « tcp », « https », « tls », « http », ou ""
// si le port ne répond pas (voir Ecoutes).
func DetecterProtocole(adresse string, delai time.Duration) string {
	// 1. Le service parle-t-il le premier ?
	c, err := net.DialTimeout("tcp", adresse, delai)
	if err != nil {
		return ""
	}
	_ = c.SetReadDeadline(time.Now().Add(attenteBanniere))
	buf := make([]byte, 8)
	n, _ := c.Read(buf)
	_ = c.Close()
	if n > 0 {
		if bytes.HasPrefix(buf[:n], []byte("SSH-")) {
			return "ssh"
		}
		return "tcp"
	}
	// 2. TLS.
	conf := &tls.Config{
		InsecureSkipVerify: true, // #nosec G402 -- détection seulement : le certificat n'est pas en jeu, rien n'est envoyé
		MinVersion:         tls.VersionTLS12,
		NextProtos:         []string{"h2", "http/1.1"},
	}
	if tc, err := tls.DialWithDialer(&net.Dialer{Timeout: delai}, "tcp", adresse, conf); err == nil {
		alpn := tc.ConnectionState().NegotiatedProtocol
		web := alpn == "h2" || alpn == "http/1.1" || repondHTTP(tc, delai)
		_ = tc.Close()
		if web {
			return "https"
		}
		return "tls"
	}
	// 3. HTTP en clair.
	if c, err := net.DialTimeout("tcp", adresse, delai); err == nil {
		web := repondHTTP(c, delai)
		_ = c.Close()
		if web {
			return "http"
		}
	}
	return "tcp"
}

// repondHTTP : « HEAD / » ; vrai si la réponse commence par « HTTP/ ».
func repondHTTP(c net.Conn, delai time.Duration) bool {
	_ = c.SetDeadline(time.Now().Add(delai))
	if _, err := c.Write([]byte("HEAD / HTTP/1.0\r\nHost: localhost\r\nUser-Agent: mirador-agent\r\n\r\n")); err != nil {
		return false
	}
	buf := make([]byte, 5)
	n := 0
	for n < len(buf) {
		k, err := c.Read(buf[n:])
		n += k
		if err != nil {
			break
		}
	}
	return string(buf[:n]) == "HTTP/"
}
