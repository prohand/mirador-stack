// SPDX-License-Identifier: GPL-3.0-or-later

package unifi

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"mirador-stack/internal/agent/config"
	"mirador-stack/internal/agent/metrics"
)

// Bornes d'un relevé (protection mémoire et nombre de séries).
const (
	maxAppareils = 300
	maxClients   = 1000
	maxPorts     = 64
)

// propre : texte d'étiquette (UTF-8, sans caractère de contrôle, 64 au plus).
func propre(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == utf8.RuneError {
			return -1
		}
		return r
	}, strings.TrimSpace(s))
	for len(s) > 64 {
		_, n := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-n]
	}
	return s
}

// Ajouter : métriques d'un site, noms et étiquettes d'unpoller.
func Ajouter(b *metrics.Batch, site string, d Donnees) {
	L := metrics.L
	s := L("site_name", propre(site))
	nomsAP := map[string]string{}
	for i, a := range d.Appareils {
		if i >= maxAppareils {
			break
		}
		nom := propre(a.Nom)
		if nom == "" {
			nom = propre(a.Mac)
		}
		nomsAP[a.Mac] = nom
		n, t := L("name", nom), L("type", propre(a.Type))
		b.Add("unpoller_device_info", 1, n, t, s, L("model", propre(a.Modele)), L("version", propre(a.Version)), L("ip", propre(a.IP)), L("mac", propre(a.Mac)))
		if a.Etat != 1 { // déconnecté : seulement l'info (comme unpoller)
			continue
		}
		b.Add("unpoller_device_uptime_seconds", float64(a.Uptime), n, t, s)
		b.Add("unpoller_device_cpu_utilization_ratio", float64(a.SystemeStats.CPU)/100, n, t, s)
		b.Add("unpoller_device_memory_utilization_ratio", float64(a.SystemeStats.Mem)/100, n, t, s)
		b.Add("unpoller_device_receive_bytes_total", float64(a.RxOctets), n, t, s)
		b.Add("unpoller_device_transmit_bytes_total", float64(a.TxOctets), n, t, s)
		b.Add("unpoller_device_stations", float64(a.Stations), n, t, s, L("station_type", "user"))
		b.Add("unpoller_device_stations", float64(a.Invites), n, t, s, L("station_type", "guest"))
		up := 0.0
		if a.Upgradable {
			up = 1
		}
		b.Add("unpoller_device_upgradeable", up, n, t, s)
		for _, tp := range a.Temperatures {
			b.Add("unpoller_device_temperature_celsius", float64(tp.Valeur), n, t, s, L("temp_area", propre(tp.Nom)), L("temp_type", propre(tp.Type)))
		}
		if len(a.Temperatures) == 0 && a.TemperatureGenerale > 0 {
			b.Add("unpoller_device_temperature_celsius", float64(a.TemperatureGenerale), n, t, s, L("temp_area", "general"), L("temp_type", "board"))
		}
		for j, p := range a.Ports {
			if j >= maxPorts {
				break
			}
			num, pn := L("port_num", strconv.Itoa(int(p.Numero))), L("port_name", propre(p.Nom))
			b.Add("unpoller_device_port_receive_bytes_total", float64(p.Rx), n, t, s, num, pn)
			b.Add("unpoller_device_port_transmit_bytes_total", float64(p.Tx), n, t, s, num, pn)
			b.Add("unpoller_device_port_receive_errors_total", float64(p.RxErreurs), n, t, s, num, pn)
			b.Add("unpoller_device_port_transmit_errors_total", float64(p.TxErreurs), n, t, s, num, pn)
			b.Add("unpoller_device_port_receive_dropped_total", float64(p.RxRejets), n, t, s, num, pn)
			b.Add("unpoller_device_port_transmit_dropped_total", float64(p.TxRejets), n, t, s, num, pn)
			b.Add("unpoller_device_port_port_speed_bps", float64(p.Vitesse)*1e6, n, t, s, num, pn)
			if p.PoE > 0 {
				b.Add("unpoller_device_port_poe_watts", float64(p.PoE), n, t, s, num, pn)
			}
		}
		for _, r := range a.Radios {
			radio := r.Radio
			if radio == "" {
				radio = r.Nom
			}
			b.Add("unpoller_device_radio_channel_utilization_total_ratio", float64(r.CU)/100, n, t, s, L("radio", propre(radio)))
		}
	}
	for i, c := range d.Clients {
		if i >= maxClients {
			break
		}
		nom := propre(c.Nom)
		if nom == "" {
			nom = propre(c.NomHote)
		}
		if nom == "" {
			nom = propre(c.Mac)
		}
		filaire := "false"
		if c.Filaire {
			filaire = "true"
		}
		lbl := []metrics.Label{L("name", nom), L("mac", propre(c.Mac)), s, L("wired", filaire), L("essid", propre(c.ESSID)),
			L("ap_name", nomsAP[c.APMac]), L("radio", propre(c.Radio))}
		b.Add("unpoller_client_uptime_seconds", float64(c.Uptime), lbl...)
		b.Add("unpoller_client_receive_bytes_total", float64(c.Rx), lbl...)
		b.Add("unpoller_client_transmit_bytes_total", float64(c.Tx), lbl...)
		if !c.Filaire {
			b.Add("unpoller_client_radio_signal_db", float64(c.Signal), lbl...)
			b.Add("unpoller_client_satisfaction_ratio", float64(c.Satisfaction)/100, lbl...)
		}
	}
	for _, h := range d.Sante {
		b.Add("unpoller_site_disconnected", float64(h.Deconnectes), s, L("subsystem", propre(h.Sous)))
	}
}

// Tourner : relevé du contrôleur à son rythme (un lot par relevé, instance
// = équipement rattaché, job="unifi"), jusqu'à l'arrêt.
func Tourner(ctx context.Context, cfg config.UniFi, envoyer func(*metrics.Batch), journal *slog.Logger) {
	intervalle := time.Duration(cfg.Intervalle)
	if intervalle == 0 {
		intervalle = time.Minute
	}
	t := time.NewTicker(intervalle)
	defer t.Stop()
	// Un seul client (une session) pour tous les relevés ; session fermée
	// à l'arrêt.
	c := Nouveau(cfg)
	defer func() {
		fin, annuler := context.WithTimeout(context.Background(), 5*time.Second)
		defer annuler()
		c.Deconnecter(fin)
	}()
	dernier := ""
	for {
		debut := time.Now()
		b := metrics.NewBatch(1024, []metrics.Label{metrics.L("instance", cfg.Machine), metrics.L("job", "unifi")})
		b.Reset(debut.UnixMilli())
		err := c.Releve(ctx, b)
		if err != nil && ctx.Err() != nil {
			return // relevé coupé par l'arrêt : pas de faux « up=0 » (relecture du 21/11)
		}
		up := 1.0
		if err != nil {
			up = 0
			if err.Error() != dernier && journal != nil {
				journal.Warn("UniFi : relevé en échec", "controleur", cfg.Adresse, "erreur", err)
			}
			dernier = err.Error()
		} else {
			dernier = ""
		}
		b.Add("up", up)
		b.Add("unpoller_scrape_duration_seconds", time.Since(debut).Seconds())
		envoyer(b)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Releve : un relevé avec un client neuf (session ouverte puis fermée).
func Releve(ctx context.Context, cfg config.UniFi, b *metrics.Batch) error {
	c := Nouveau(cfg)
	defer c.Deconnecter(ctx)
	return c.Releve(ctx, b)
}

// Releve : sites, puis chaque site (ceux de la liste, sinon tous), avec la
// session déjà ouverte ; ouverte au besoin, rouverte une fois si le
// contrôleur la refuse (expirée).
func (c *Client) Releve(ctx context.Context, b *metrics.Batch) error {
	if strings.HasPrefix(c.cfg.MotDePasse, config.PrefixeSecretChiffre) || strings.HasPrefix(c.cfg.CleAPI, config.PrefixeSecretChiffre) {
		return errSecret
	}
	ctxR, fin := context.WithTimeout(ctx, 50*time.Second)
	defer fin()
	for essai := 0; ; essai++ {
		if !c.connecte {
			if err := c.Connecter(ctxR); err != nil {
				return err
			}
			c.connecte = true
		}
		lus, err := c.lireSites(ctxR)
		if errors.Is(err, errSession) && essai == 0 {
			c.connecte = false
			continue
		}
		if err != nil {
			return err
		}
		for _, l := range lus {
			Ajouter(b, l.site, l.d)
		}
		return nil
	}
}

type siteLu struct {
	site string
	d    Donnees
}

// lireSites : tout est lu avant d'écrire dans le lot (un essai refait
// après une reconnexion n'ajoute rien en double).
func (c *Client) lireSites(ctx context.Context) ([]siteLu, error) {
	sites := c.cfg.Sites
	if len(sites) == 0 {
		tous, err := c.Sites(ctx)
		if err != nil {
			return nil, err
		}
		for _, s := range tous {
			sites = append(sites, s.Nom)
		}
	}
	var out []siteLu
	for _, s := range sites {
		d, err := c.LireSite(ctx, s)
		if err != nil {
			return nil, err
		}
		out = append(out, siteLu{s, d})
	}
	return out, nil
}

type erreurTexte string

func (e erreurTexte) Error() string { return string(e) }

const errSecret = erreurTexte("mots de passe UniFi non déchiffrés par le serveur")
