// SPDX-License-Identifier: GPL-3.0-or-later

package certsyslog

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
)

// Attente de la publication du défi DNS (SPEC § 32) : au lieu d'attendre un
// délai fixe, le serveur interroge lui-même les serveurs DNS qui font
// autorité pour la zone, et demande la vérification à Let's Encrypt dès
// que l'enregistrement y est visible partout. Le délai réglé devient un
// maximum.

// VerifDNS : nombre de serveurs DNS de la zone qui montrent la valeur.
type VerifDNS func(ctx context.Context, zone, nom, valeur string) (vus, total int, err error)

// Délais de publication.
const (
	pasVerification   = 10 * time.Second // entre deux vérifications
	attenteMaxDefaut  = 3 * time.Minute  // maximum par défaut (deSEC, Cloudflare…)
	attenteMaxLente   = 20 * time.Minute // fournisseur lent (CSC)
	attenteFixeDefaut = time.Minute      // sans vérification possible (DNS sortant bloqué)
	serveursDNSMax    = 6
	AttenteMaxMin     = 10 * time.Second // bornes du réglage
	AttenteMaxPlafond = 30 * time.Minute
)

// ChoixAttente : valeurs proposées dans l'interface (secondes ; 0 = selon
// le fournisseur).
var ChoixAttente = []struct {
	Secondes int
	Titre    string
}{
	{0, "selon le fournisseur (3 min, CSC : 20 min)"},
	{60, "1 min"}, {180, "3 min"}, {300, "5 min"}, {600, "10 min"}, {1200, "20 min"}, {1800, "30 min"},
}

// attentes : maximum (vérification active) et délai fixe (sans vérification).
func attentes(r Reglages, dns FournisseurDNS) (max, fixe time.Duration) {
	max, fixe = attenteMaxDefaut, attenteFixeDefaut
	if p, ok := dns.(interface{ AttentePropagation() time.Duration }); ok {
		max, fixe = attenteMaxLente, p.AttentePropagation()
	}
	if r.AttenteMax > 0 {
		max = time.Duration(r.AttenteMax) * time.Second
		if fixe > max {
			fixe = max
		}
	}
	return max, fixe
}

// attendrePublication : jusqu'à ce que tous les serveurs DNS de la zone
// montrent la valeur, ou jusqu'au maximum. Sans vérification possible :
// délai fixe. Chaque essai est noté dans l'avancement.
func attendrePublication(ctx context.Context, verif VerifDNS, pas time.Duration, zone, nom, valeur string, max, fixe time.Duration) error {
	debut := time.Now()
	fin := debut.Add(max)
	attendre := func(d time.Duration) error {
		select {
		case <-time.After(d):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if verif == nil {
		suivre(ctx, EtapePropagation, "", debut.Add(fixe))
		return attendre(fixe)
	}
	for essai := 0; ; essai++ {
		vus, total, err := verif(ctx, zone, nom, valeur)
		if err != nil && essai == 0 {
			// Serveurs DNS injoignables d'ici : délai fixe, comme avant.
			suivre(ctx, EtapePropagation, "vérification impossible depuis le serveur ("+raccourcir(err.Error(), 120)+") : attente fixe", debut.Add(fixe))
			return attendre(fixe)
		}
		if err == nil && total > 0 && vus == total {
			suivre(ctx, EtapePropagation, fmt.Sprintf("visible sur %d/%d serveurs DNS", vus, total), time.Time{})
			return nil
		}
		if !time.Now().Add(pas).Before(fin) {
			// Maximum atteint : Let's Encrypt essaie quand même.
			return nil
		}
		info := fmt.Sprintf("visible sur %d/%d serveurs DNS", vus, total)
		if err != nil {
			info = "vérification en échec, nouvel essai"
		}
		suivre(ctx, EtapePropagation, info, fin)
		if err := attendre(pas); err != nil {
			return err
		}
	}
}

func raccourcir(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// verifDNSReel : interroge directement chaque serveur DNS qui fait
// autorité pour la zone (sans cache intermédiaire).
func verifDNSReel(ctx context.Context, zone, nom, valeur string) (vus, total int, err error) {
	ctxNS, fin := context.WithTimeout(ctx, 10*time.Second)
	defer fin()
	nss, err := net.DefaultResolver.LookupNS(ctxNS, zone)
	if err != nil {
		return 0, 0, err
	}
	if len(nss) == 0 {
		return 0, 0, errors.New("aucun serveur DNS pour la zone")
	}
	if len(nss) > serveursDNSMax {
		nss = nss[:serveursDNSMax]
	}
	for _, ns := range nss {
		total++
		ips, err := net.DefaultResolver.LookupHost(ctxNS, strings.TrimSuffix(ns.Host, "."))
		if err != nil || len(ips) == 0 {
			continue
		}
		ip := ips[0]
		r := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, reseau, _ string) (net.Conn, error) {
			d := net.Dialer{Timeout: 3 * time.Second}
			return d.DialContext(ctx, reseau, net.JoinHostPort(ip, "53"))
		}}
		ctxQ, finQ := context.WithTimeout(ctx, 5*time.Second)
		txts, err := r.LookupTXT(ctxQ, "_acme-challenge."+nom)
		finQ()
		if err != nil {
			continue
		}
		for _, t := range txts {
			if t == valeur {
				vus++
				break
			}
		}
	}
	return vus, total, nil
}
