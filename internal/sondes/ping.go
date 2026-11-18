// SPDX-License-Identifier: GPL-3.0-or-later

package sondes

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

// essaiPing : un écho ICMP, par le socket « ping » sans droits de Linux
// (SOCK_DGRAM, autorisé par net.ipv4.ping_group_range, ouvert à tous par
// défaut sur Debian 12 et Ubuntu). Rien d'autre n'est envoyé.
func essaiPing(ctx context.Context, cible string, o Options) error {
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, cible)
	if err != nil {
		return err
	}
	if len(ips) == 0 {
		return errors.New("nom sans adresse")
	}
	ip := ips[0].IP
	if o.Refuser != nil && o.Refuser(ip) {
		return fmt.Errorf("adresse %s refusée depuis le serveur", ip)
	}
	reseau, ecoute, proto := "udp4", "0.0.0.0", 1
	var typ icmp.Type = ipv4.ICMPTypeEcho
	if ip.To4() == nil {
		reseau, ecoute, proto, typ = "udp6", "::", 58, ipv6.ICMPTypeEchoRequest
	}
	c, err := icmp.ListenPacket(reseau, ecoute)
	if err != nil {
		return fmt.Errorf("ping impossible sans droits (réglage net.ipv4.ping_group_range) : %w", err)
	}
	defer c.Close()
	fin, ok := ctx.Deadline()
	if !ok {
		fin = time.Now().Add(Delai)
	}
	_ = c.SetDeadline(fin)
	msg := icmp.Message{Type: typ, Body: &icmp.Echo{ID: os.Getpid() & 0xffff, Seq: 1, Data: []byte("mirador")}}
	brut, err := msg.Marshal(nil)
	if err != nil {
		return err
	}
	if _, err := c.WriteTo(brut, &net.UDPAddr{IP: ip}); err != nil {
		return err
	}
	tampon := make([]byte, 1500)
	for {
		n, _, err := c.ReadFrom(tampon)
		if err != nil {
			return err
		}
		rep, err := icmp.ParseMessage(proto, tampon[:n])
		if err != nil {
			continue
		}
		if rep.Type == ipv4.ICMPTypeEchoReply || rep.Type == ipv6.ICMPTypeEchoReply {
			return nil
		}
	}
}
