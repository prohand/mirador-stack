// SPDX-License-Identifier: GPL-3.0-or-later

// Package sondes : certificats TLS des services du LAN, lus par le
// collecteur (SPEC § 23.15) : échéance, sujet et émetteur, avec les noms de
// métriques de la sonde actuelle (job="certs") : règles et tableau
// « Certificats » inchangés. Le certificat est lu sans être validé (seule
// son échéance compte, il peut être fait maison).
package sondes

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"time"

	"mirador-stack/internal/agent/config"
	"mirador-stack/internal/agent/metrics"
)

// Rythme et délai.
const (
	Intervalle = 5 * time.Minute
	Delai      = 10 * time.Second
)

// controleLAN : contrôle de l'adresse jointe (remplacé dans les tests, qui
// sondent la boucle locale).
var controleLAN = config.ControleLAN

// Resultat : lecture d'un certificat.
type Resultat struct {
	Fin             time.Time
	Sujet, Emetteur string
	Duree           time.Duration
}

// Sonder : poignée de main TLS, certificat du serveur lu (pas validé).
func Sonder(ctx context.Context, s config.Sonde) (Resultat, error) {
	debut := time.Now()
	hote, _, err := net.SplitHostPort(s.Adresse)
	if err != nil {
		return Resultat{}, err
	}
	nom := s.Nom
	if nom == "" && net.ParseIP(hote) == nil {
		nom = hote
	}
	// Adresse réellement jointe : du LAN seulement (revue du 28/11).
	d := tls.Dialer{NetDialer: &net.Dialer{Timeout: Delai, Control: controleLAN},
		// #nosec G402 -- lecture de l'échéance seulement : un certificat fait
		// maison ou périmé doit justement être lu ; rien n'est envoyé.
		Config: &tls.Config{InsecureSkipVerify: true, ServerName: nom, MinVersion: tls.VersionTLS10}}
	ctxD, fin := context.WithTimeout(ctx, Delai)
	defer fin()
	conn, err := d.DialContext(ctxD, "tcp", s.Adresse)
	if err != nil {
		return Resultat{Duree: time.Since(debut)}, err
	}
	defer conn.Close()
	etat := conn.(*tls.Conn).ConnectionState()
	if len(etat.PeerCertificates) == 0 {
		return Resultat{Duree: time.Since(debut)}, errors.New("aucun certificat présenté")
	}
	c := etat.PeerCertificates[0]
	return Resultat{Fin: c.NotAfter, Sujet: c.Subject.String(), Emetteur: c.Issuer.String(), Duree: time.Since(debut)}, nil
}

// Ajouter : métriques d'une sonde (job="certs", path = adresse sondée).
func Ajouter(b *metrics.Batch, s config.Sonde, r Resultat, err error) {
	chemin := metrics.L("path", s.Adresse)
	b.Add("probe_duration_seconds", r.Duree.Seconds(), chemin)
	if err != nil {
		b.Add("probe_success", 0, chemin)
		return
	}
	b.Add("probe_success", 1, chemin)
	b.Add("ssl_cert_not_after_timestamp_seconds", float64(r.Fin.Unix()), chemin)
	b.Add("probe_ssl_last_chain_info", 1, chemin, metrics.L("subject", r.Sujet), metrics.L("issuer", r.Emetteur))
}

// Tourner : toutes les sondes toutes les 5 min ; un lot par sonde
// (instance = équipement), passé à envoyer.
func Tourner(ctx context.Context, sondes []config.Sonde, envoyer func(*metrics.Batch), journal *slog.Logger) {
	t := time.NewTicker(Intervalle)
	defer t.Stop()
	echecs := map[string]string{}
	for {
		for _, s := range sondes {
			r, err := Sonder(ctx, s)
			// Arrêt pendant l'essai (redémarrage du collecteur) : rien
			// envoyé, sinon un faux échec (relecture du 21/11).
			if err != nil && ctx.Err() != nil {
				return
			}
			b := metrics.NewBatch(8, []metrics.Label{metrics.L("instance", s.Machine), metrics.L("job", "certs")})
			b.Reset(time.Now().UnixMilli())
			Ajouter(b, s, r, err)
			envoyer(b)
			msg := ""
			if err != nil {
				msg = err.Error()
			}
			if msg != echecs[s.Adresse] && msg != "" && journal != nil {
				journal.Warn("sonde de certificat en échec", "adresse", s.Adresse, "erreur", msg)
			}
			echecs[s.Adresse] = msg
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
