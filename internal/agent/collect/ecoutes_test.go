// SPDX-License-Identifier: GPL-3.0-or-later

package collect

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mirador-stack/internal/agent/metrics"
)

func TestLigneEcoute(t *testing.T) {
	cas := []struct {
		l, ip string
		port  uint16
		ok    bool
	}{
		{"   0: 00000000:0016 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1 1", "0.0.0.0", 22, true},
		{"   1: 0100007F:0CEA 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1 1", "127.0.0.1", 3306, true},
		{"   2: 0501A8C0:01BB 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1 1", "192.168.1.5", 443, true},
		{"   3: 0501A8C0:01BB 0A01A8C0:D431 01 00000000:00000000 00:00000000 00000000     0        0 1 1", "", 0, false}, // établie
		{"   0: 00000000000000000000000000000000:0050 00000000000000000000000000000000:0000 0A 0 0 0 0 0 1", "::", 80, true},
		{"   1: 0000000000000000FFFF00000501A8C0:1F90 00000000000000000000000000000000:0000 0A 0 0 0 0 0 1", "192.168.1.5", 8080, true},
	}
	for _, c := range cas {
		ip, port, ok := ligneEcoute(c.l)
		if ok != c.ok || (ok && (ip.String() != c.ip || port != c.port)) {
			t.Errorf("%q : %v %d %v, attendu %s %d %v", c.l, ip, port, ok, c.ip, c.port, c.ok)
		}
	}
}

func TestLirePortsEcoute(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "net"), 0o755)
	tcp := `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000:0016 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1 1
   1: 0100007F:0CEA 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1 1
   2: 0501A8C0:0035 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1 1
   3: 0501A8C0:0016 0A01A8C0:D431 01 00000000:00000000 00:00000000 00000000     0        0 1 1
`
	tcp6 := `  sl  local_address rem_address st
   0: 00000000000000000000000000000000:0016 00000000000000000000000000000000:0000 0A 0 0 0 0 0 1
   1: 00000000000000000000000001000000:1F40 00000000000000000000000000000000:0000 0A 0 0 0 0 0 1
`
	os.WriteFile(filepath.Join(dir, "net", "tcp"), []byte(tcp), 0o644)
	os.WriteFile(filepath.Join(dir, "net", "tcp6"), []byte(tcp6), 0o644)
	ps, err := lirePortsEcoute(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, p := range ps {
		got = append(got, p.adresse+" "+p.essai)
	}
	// 22 : toutes adresses (v4 → essai en 127.0.0.1) ; 3306 et 8000 (::1)
	// en boucle locale : écartés ; 53 sur une adresse précise.
	want := "* 127.0.0.1:22|192.168.1.5 192.168.1.5:53"
	if strings.Join(got, "|") != want {
		t.Errorf("ports %q, attendu %q", strings.Join(got, "|"), want)
	}
}

func TestDetecterProtocole(t *testing.T) {
	sTLS := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer sTLS.Close()
	sHTTP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer sHTTP.Close()
	banniere := ecouter(t, func(c net.Conn) { c.Write([]byte("SSH-2.0-OpenSSH_9.6\r\n")); time.Sleep(time.Second) })
	muet := ecouter(t, func(c net.Conn) {
		buf := make([]byte, 64)
		c.SetReadDeadline(time.Now().Add(3 * time.Second))
		c.Read(buf)
	})
	smtp := ecouter(t, func(c net.Conn) { c.Write([]byte("220 mail ESMTP\r\n")) })
	for adresse, want := range map[string]string{
		strings.TrimPrefix(sTLS.URL, "https://"): "https",
		strings.TrimPrefix(sHTTP.URL, "http://"): "http",
		banniere:                                 "ssh",
		smtp:                                     "tcp",
		muet:                                     "tcp",
	} {
		if got := DetecterProtocole(adresse, time.Second); got != want {
			t.Errorf("%s : %q, attendu %q", adresse, got, want)
		}
	}
	// Port fermé : rien (réessayé plus tard).
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	a := l.Addr().String()
	l.Close()
	if got := DetecterProtocole(a, time.Second); got != "" {
		t.Errorf("port fermé : %q", got)
	}
}

func ecouter(t *testing.T, servir func(net.Conn)) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() { servir(c); c.Close() }()
		}
	}()
	return l.Addr().String()
}

// Collecter : rien envoyé avant la détection, puis le port avec son
// protocole ; relu toutes les 5 min seulement.
func TestEcoutesCollecter(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "net"), 0o755)
	os.WriteFile(filepath.Join(dir, "net", "tcp"), []byte("en-tête\n   0: 00000000:01BB 00000000:0000 0A 0 0 0 0 0 1\n"), 0o644)
	e := NewEcoutes(dir)
	h := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	e.maintenant = func() time.Time { return h }
	essais := 0
	e.essayer = func(a string) string { essais++; return "https" }
	b := &metrics.Batch{}
	e.Collecter(b)
	for i := 0; i < 100 && func() bool { e.mu.Lock(); defer e.mu.Unlock(); return e.enCours }(); i++ {
		time.Sleep(10 * time.Millisecond)
	}
	b.Reset(0)
	e.Collecter(b)
	var trouve bool
	for _, s := range b.Samples {
		if s.Name == "mirador_port_ecoute" {
			lab := map[string]string{}
			for _, l := range s.Labels {
				lab[l.Name] = l.Value
			}
			trouve = lab["port"] == "443" && lab["adresse"] == "*" && lab["protocole"] == "https"
		}
	}
	if !trouve {
		t.Errorf("port 443 https absent : %+v", b.Samples)
	}
	h = h.Add(6 * time.Minute)
	e.Collecter(b)
	if essais != 1 {
		t.Errorf("%d essais, attendu 1 (déjà connu)", essais)
	}
}

// Adresses lues dans /proc (SPEC § 62) : nœud Proxmox avec pont vmbr0
// (route par défaut), second réseau sur vmbr1, docker0 ignoré, IPv6
// globale gardée, temporaire et lien local écartées.
func TestAdressesLocales(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "net"), 0o755)
	route := "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n" +
		"vmbr0\t00000000\t0101A8C0\t0003\t0\t0\t0\t00000000\t0\t0\t0\n" +
		"vmbr0\t0001A8C0\t00000000\t0001\t0\t0\t0\t00FFFFFF\t0\t0\t0\n" +
		"vmbr1\t00000A0A\t00000000\t0001\t0\t0\t0\t0000FFFF\t0\t0\t0\n" +
		"docker0\t000011AC\t00000000\t0001\t0\t0\t0\t0000FFFF\t0\t0\t0\n"
	fib := `Main:
  +-- 0.0.0.0/0 3 0 5
     |-- 0.0.0.0
        /0 universe UNICAST
     +-- 10.10.0.0/16 2 0 2
        |-- 10.10.0.1
           /32 host LOCAL
     +-- 127.0.0.0/8 2 0 2
        +-- 127.0.0.0/31 1 0 0
           |-- 127.0.0.0
              /8 host LOCAL
           |-- 127.0.0.1
              /32 host LOCAL
     +-- 172.17.0.0/16 2 0 2
        |-- 172.17.0.1
           /32 host LOCAL
     +-- 192.168.1.0/24 2 0 2
        |-- 192.168.1.0
           /24 link UNICAST
        |-- 192.168.1.10
           /32 host LOCAL
        |-- 192.168.1.255
           /32 link BROADCAST
Local:
     +-- 192.168.1.0/24 2 0 2
        |-- 192.168.1.10
           /32 host LOCAL
`
	inet6 := "00000000000000000000000000000001 01 80 10 80       lo\n" +
		"fe80000000000000021122fffe334455 03 40 20 80    vmbr0\n" +
		"20010db8000000000211 03 40 00 80    vmbr0\n" + // ligne abîmée : ignorée
		"20010db8000000000211223344556677 03 40 00 80    vmbr0\n" +
		"20010db80000000099aabbccddeeff00 03 40 00 01    vmbr0\n" // temporaire
	os.WriteFile(filepath.Join(dir, "net", "route"), []byte(route), 0o644)
	os.WriteFile(filepath.Join(dir, "net", "fib_trie"), []byte(fib), 0o644)
	os.WriteFile(filepath.Join(dir, "net", "if_inet6"), []byte(inet6), 0o644)
	var got []string
	for _, a := range adressesLocales(dir) {
		got = append(got, fmt.Sprintf("%s %s %v", a.iface, a.ip, a.principale))
	}
	want := "vmbr0 192.168.1.10 true|vmbr0 2001:db8::211:2233:4455:6677 true|vmbr1 10.10.0.1 false"
	if strings.Join(got, "|") != want {
		t.Errorf("adresses :\n %q\nattendu\n %q", strings.Join(got, "|"), want)
	}
}

// Relecture du 21/11 : VPS en /32 (aucune route de réseau ne contient
// l'adresse, passerelle « on-link ») : l'adresse publique est gardée,
// sur l'interface de la route par défaut.
func TestAdresseVPS32(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "net"), 0o755)
	route := "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n" +
		"eth0\t00000000\t0101FEAC\t0003\t0\t0\t0\t00000000\t0\t0\t0\n" + // défaut via 172.254.1.1
		"eth0\t0101FEAC\t00000000\t0005\t0\t0\t0\tFFFFFFFF\t0\t0\t0\n" // la passerelle seule (/32)
	fib := `Main:
  +-- 0.0.0.0/0 3 0 5
     |-- 0.0.0.0
        /0 universe UNICAST
     |-- 203.0.113.50
        /32 host LOCAL
`
	os.WriteFile(filepath.Join(dir, "net", "route"), []byte(route), 0o644)
	os.WriteFile(filepath.Join(dir, "net", "fib_trie"), []byte(fib), 0o644)
	a := adressesLocales(dir)
	if len(a) != 1 || a[0].ip != "203.0.113.50" || a[0].iface != "eth0" || !a[0].principale {
		t.Errorf("adresse du VPS : %+v", a)
	}
}
