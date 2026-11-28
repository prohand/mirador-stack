// SPDX-License-Identifier: GPL-3.0-or-later

package checks

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"

	"mirador-stack/internal/adresses"
	"mirador-stack/internal/agent/config"
)

// --- http ----------------------------------------------------------------------------

// tailleLueHTTP : octets lus au plus (texte attendu).
const tailleLueHTTP = 64 << 10

// checkHTTP : GET, 3 redirections au plus, sans proxy, code 2xx ou 3xx ;
// texte attendu dans les 64 premiers Kio ; fin du certificat (même non
// vérifié si verifier_tls = non).
func checkHTTP(ctx context.Context, c config.Check) resultat {
	verifier := c.VerifierTLS == nil || *c.VerifierTLS
	tr := &http.Transport{Proxy: nil, DisableKeepAlives: true, TLSHandshakeTimeout: c.DelaiEffectif(),
		DialContext:     numeroteur().DialContext,                                                 // lien local refusé, redirections comprises
		TLSClientConfig: &tls.Config{InsecureSkipVerify: !verifier, MinVersion: tls.VersionTLS12}} // #nosec G402 -- choix de l'admin (site au certificat maison), la date de fin reste mesurée
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, CheckRedirect: func(_ *http.Request, via []*http.Request) error {
		if len(via) >= 3 {
			return errors.New("plus de 3 redirections")
		}
		return nil
	}}
	r := resultat{valeur: map[string]float64{}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Cible, nil)
	if err != nil {
		return r
	}
	req.Header.Set("User-Agent", "mirador-agent")
	debut := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		r.duree = time.Since(debut)
		return r
	}
	defer resp.Body.Close()
	corps, _ := io.ReadAll(io.LimitReader(resp.Body, tailleLueHTTP))
	r.duree = time.Since(debut)
	r.valeur["mirador_check_http_code"] = float64(resp.StatusCode)
	if resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
		r.valeur["mirador_check_certificat_fin_timestamp_seconds"] = float64(resp.TLS.PeerCertificates[0].NotAfter.Unix())
	}
	r.ok = resp.StatusCode >= 200 && resp.StatusCode < 400
	if r.ok && c.Contient != "" {
		r.ok = bytes.Contains(corps, []byte(c.Contient))
	}
	return r
}

// numeroteur : connexions des checks. Le lien local (169.254.0.0/16,
// fe80::/10 : métadonnées des clouds, qui donnent les secrets de la
// machine) est toujours refusé, vérifié sur l'adresse réellement jointe
// (après résolution, à chaque redirection) ; revue du 28/11, M4. Le LAN et
// la machine elle-même restent permis : c'est le but des checks.
func numeroteur() *net.Dialer {
	return &net.Dialer{Control: adresses.Controle(adresses.LienLocal, "refusée (lien local)")}
}

// --- tcp, dns ----------------------------------------------------------------------------

func checkTCP(ctx context.Context, c config.Check) resultat {
	d := numeroteur()
	debut := time.Now()
	conn, err := d.DialContext(ctx, "tcp", c.Cible)
	r := resultat{duree: time.Since(debut)}
	if err == nil {
		r.ok = true
		_ = conn.Close()
	}
	return r
}

func checkDNS(ctx context.Context, c config.Check) resultat {
	debut := time.Now()
	adresses, err := net.DefaultResolver.LookupHost(ctx, c.Cible)
	return resultat{ok: err == nil && len(adresses) > 0, duree: time.Since(debut),
		valeur: map[string]float64{"mirador_check_dns_adresses": float64(len(adresses))}}
}

// --- ping --------------------------------------------------------------------------------

// checkPing : un écho ICMP par socket « datagramme » (sans droits root :
// autorisé par net.ipv4.ping_group_range, ouvert par défaut sur Debian et
// Ubuntu récents). Refusé par le noyau : ok = 0, l'interface le dit.
func checkPing(ctx context.Context, c config.Check) resultat {
	r := resultat{}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, c.Cible)
	if err != nil || len(ips) == 0 {
		return r
	}
	ip := ips[0].IP
	if adresses.LienLocal(ip) {
		return r // lien local toujours refusé (voir numeroteur)
	}
	reseau, proto := "udp4", 1
	var typ icmp.Type = ipv4.ICMPTypeEcho
	if ip.To4() == nil {
		reseau, proto, typ = "udp6", 58, ipv6.ICMPTypeEchoRequest
	}
	conn, err := icmp.ListenPacket(reseau, "")
	if err != nil {
		return r
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	msg := icmp.Message{Type: typ, Body: &icmp.Echo{ID: os.Getpid() & 0xffff, Seq: 1, Data: []byte("mirador")}}
	brut, err := msg.Marshal(nil)
	if err != nil {
		return r
	}
	debut := time.Now()
	if _, err := conn.WriteTo(brut, &net.UDPAddr{IP: ip}); err != nil {
		return r
	}
	tampon := make([]byte, 1500)
	for {
		n, _, err := conn.ReadFrom(tampon)
		if err != nil {
			return r
		}
		rep, err := icmp.ParseMessage(proto, tampon[:n])
		if err != nil {
			continue
		}
		if rep.Type == ipv4.ICMPTypeEchoReply || rep.Type == ipv6.ICMPTypeEchoReply {
			r.ok, r.duree = true, time.Since(debut)
			return r
		}
	}
}

// --- processus -----------------------------------------------------------------------------

// checkProcessus : processus dont le nom (/proc/PID/comm) est la cible ;
// nombre et mémoire résidente totale (/proc/PID/statm, lisibles sans droits).
func checkProcessus(proc string, c config.Check) resultat {
	entrees, err := os.ReadDir(proc)
	r := resultat{valeur: map[string]float64{}}
	if err != nil {
		return r
	}
	page := float64(os.Getpagesize())
	n, rss := 0, 0.0
	for _, e := range entrees {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		comm, err := os.ReadFile(filepath.Join(proc, e.Name(), "comm")) // #nosec G304 -- /proc/PID/comm, PID numérique
		if err != nil || string(bytes.TrimSpace(comm)) != c.Cible {
			continue
		}
		n++
		if statm, err := os.ReadFile(filepath.Join(proc, e.Name(), "statm")); err == nil { // #nosec G304 -- /proc/PID/statm, PID numérique
			champs := bytes.Fields(statm)
			if len(champs) > 1 {
				if v, err := strconv.ParseFloat(string(champs[1]), 64); err == nil {
					rss += v * page
				}
			}
		}
	}
	r.ok = n > 0
	r.valeur["mirador_check_processus_nombre"] = float64(n)
	r.valeur["mirador_check_processus_memoire_octets"] = rss
	return r
}

// --- fichier, dossier ------------------------------------------------------------------------

// maxCorrespondances : fichiers examinés au plus pour un motif « * ».
const maxCorrespondances = 10000

// checkFichier : le fichier, ou le plus récent de ceux qui correspondent au
// motif (sauvegardes datées) : existe, âge, taille. Rien n'est lu dedans.
func checkFichier(c config.Check, maintenant time.Time) resultat {
	r := resultat{valeur: map[string]float64{}}
	chemins := []string{c.Cible}
	if m, err := filepath.Glob(c.Cible); err == nil && len(m) > 0 {
		chemins = m
	}
	if len(chemins) > maxCorrespondances {
		chemins = chemins[:maxCorrespondances]
	}
	var plusRecent fs.FileInfo
	for _, ch := range chemins {
		info, err := os.Stat(ch)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if plusRecent == nil || info.ModTime().After(plusRecent.ModTime()) {
			plusRecent = info
		}
	}
	if plusRecent == nil {
		return r
	}
	r.ok = true
	r.valeur["mirador_check_fichier_age_secondes"] = maintenant.Sub(plusRecent.ModTime()).Seconds()
	r.valeur["mirador_check_fichier_taille_octets"] = float64(plusRecent.Size())
	return r
}

// maxEntreesDossier : entrées parcourues au plus (protection disque).
const maxEntreesDossier = 200000

// checkDossier : taille totale et nombre de fichiers (sans suivre les liens),
// borné en entrées et en temps (délai du check).
func checkDossier(ctx context.Context, c config.Check) resultat {
	r := resultat{valeur: map[string]float64{}}
	var taille, fichiers float64
	entrees := 0
	err := filepath.WalkDir(c.Cible, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // dossier illisible : ignoré
		}
		entrees++
		if entrees > maxEntreesDossier || ctx.Err() != nil {
			return errArret
		}
		if d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				taille += float64(info.Size())
				fichiers++
			}
		}
		return nil
	})
	if info, e := os.Stat(c.Cible); e != nil || !info.IsDir() {
		return r
	}
	r.ok = err == nil || errors.Is(err, errArret)
	r.valeur["mirador_check_dossier_taille_octets"] = taille
	r.valeur["mirador_check_dossier_fichiers"] = fichiers
	if errors.Is(err, errArret) {
		r.valeur["mirador_check_dossier_incomplet"] = 1
	}
	return r
}

var errArret = errors.New("arrêt du parcours")
