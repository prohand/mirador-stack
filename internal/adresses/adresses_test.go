// SPDX-License-Identifier: GPL-3.0-or-later

package adresses

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

func TestInterditeServeur(t *testing.T) {
	cas := []struct {
		ip       string
		interdit bool
	}{
		{"8.8.8.8", false},
		{"1.1.1.1", false},
		{"2606:4700:4700::1111", false},
		{"127.0.0.1", true},
		{"127.255.0.9", true},
		{"10.1.2.3", true},
		{"172.17.0.2", true},
		{"192.168.1.1", true},
		{"169.254.169.254", true},
		{"100.64.0.1", true},
		{"100.127.255.254", true},
		{"100.128.0.1", false},
		{"0.0.0.0", true},
		{"0.1.2.3", true},
		{"198.18.0.1", true},
		{"198.19.255.255", true},
		{"198.20.0.1", false},
		{"240.0.0.1", true},
		{"255.255.255.255", true},
		{"224.0.0.1", true},
		{"239.255.255.250", true},
		{"192.0.0.8", true},
		{"::", true},
		{"::1", true},
		{"fe80::1", true},
		{"fd00::1", true},
		{"ff02::1", true},
		// IPv4 cachée dans IPv6 : convertie d'abord.
		{"::ffff:127.0.0.1", true},
		{"::ffff:169.254.169.254", true},
		{"::ffff:8.8.8.8", false},
		{"64:ff9b::7f00:1", true},       // NAT64 de 127.0.0.1
		{"64:ff9b::a9fe:a9fe", true},    // NAT64 de 169.254.169.254
		{"64:ff9b::808:808", false},     // NAT64 de 8.8.8.8
		{"2002:7f00:1::1", true},        // 6to4 de 127.0.0.1
		{"2002:c0a8:101::", true},       // 6to4 de 192.168.1.1
		{"2002:808:808::1", false},      // 6to4 de 8.8.8.8
		{"64:ff9b:1::1", true},          // NAT64 local
		{"2001:0:4136:e378::1", true},   // Teredo
		{"2001:4860:4860::8888", false}, // Google (pas Teredo)
	}
	for _, c := range cas {
		ip := net.ParseIP(c.ip)
		if ip == nil {
			t.Fatalf("%s illisible", c.ip)
		}
		if got := InterditeServeur(ip); got != c.interdit {
			t.Errorf("InterditeServeur(%s) = %v, attendu %v", c.ip, got, c.interdit)
		}
	}
	if !InterditeServeur(nil) || !InterditeServeur(net.IP{1, 2}) {
		t.Error("adresse illisible : doit être refusée")
	}
}

func TestLienLocalEtBoucle(t *testing.T) {
	cas := []struct {
		ip                string
		lien, localOuLien bool
		privee            bool
	}{
		{"169.254.169.254", true, true, false},
		{"::ffff:169.254.169.254", true, true, false},
		{"64:ff9b::a9fe:a9fe", true, true, false},
		{"fe80::1", true, true, false},
		{"127.0.0.1", false, true, false},
		{"::ffff:127.0.0.1", false, true, false},
		{"0.0.0.0", false, true, false},
		{"::", false, true, false},
		{"::1", false, true, false},
		{"192.168.1.10", false, false, true},
		{"::ffff:10.0.0.3", false, false, true},
		{"fd12::3", false, false, true},
		{"8.8.8.8", false, false, false},
	}
	for _, c := range cas {
		ip := net.ParseIP(c.ip)
		if got := LienLocal(ip); got != c.lien {
			t.Errorf("LienLocal(%s) = %v", c.ip, got)
		}
		if got := LocaleOuLienLocal(ip); got != c.localOuLien {
			t.Errorf("LocaleOuLienLocal(%s) = %v", c.ip, got)
		}
		if got := Privee(ip); got != c.privee {
			t.Errorf("Privee(%s) = %v", c.ip, got)
		}
	}
}

// BenchmarkInterditeServeur : coût du contrôle fait à chaque connexion.
//
//	go test ./internal/adresses/ -bench . -benchmem -run '^$'
func BenchmarkInterditeServeur(b *testing.B) {
	ips := []net.IP{net.ParseIP("8.8.8.8"), net.ParseIP("2606:4700:4700::1111"), net.ParseIP("::ffff:10.0.0.1")}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = InterditeServeur(ips[i%len(ips)])
	}
}

// TestControle : refus au moment de la connexion (adresse réellement jointe).
func TestControle(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("pas de réseau local")
	}
	defer l.Close()
	d := &net.Dialer{Timeout: 2 * time.Second, Control: Controle(InterditeServeur, "refusée depuis le serveur")}
	ctx, fin := context.WithTimeout(context.Background(), 2*time.Second)
	defer fin()
	_, err = d.DialContext(ctx, "tcp", l.Addr().String())
	if err == nil || !strings.Contains(err.Error(), "refusée depuis le serveur") {
		t.Fatalf("boucle locale acceptée : %v", err)
	}
	// Nom qui se résout en boucle locale : refusé aussi (après résolution).
	_, err = d.DialContext(ctx, "tcp", strings.Replace(l.Addr().String(), "127.0.0.1", "localhost", 1))
	if err == nil {
		t.Fatal("localhost accepté")
	}
	// Sans refus : la connexion passe.
	d.Control = Controle(func(net.IP) bool { return false }, "")
	c, err := d.DialContext(ctx, "tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
}
