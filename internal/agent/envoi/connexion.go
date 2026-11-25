// SPDX-License-Identifier: GPL-3.0-or-later

package envoi

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"time"
)

// Connexion bloquée (retour du 25/11, ansible) : après une coupure du
// serveur, l'agent gardait une connexion HTTP/2 vers le VPS qui ne
// répondait plus ; chaque envoi attendait 30 s puis abandonnait, pendant
// plusieurs minutes, alors qu'une connexion neuve (curl) répondait en
// 0,07 s. Deux protections :
//   - TransportServeur : HTTP/2 vérifie la connexion par un « ping » quand
//     rien n'est reçu pendant 20 s, et la ferme sans réponse en 10 s ;
//   - Renouveleur : après 2 délais dépassés d'affilée, les connexions au
//     repos sont fermées ; l'envoi suivant en ouvre une neuve.
// Coût : un ping (quelques dizaines d'octets) par connexion silencieuse
// toutes les 20 s, rien d'autre.

const (
	pingApres       = 20 * time.Second
	pingSansReponse = 10 * time.Second
	ecritureBloquee = 15 * time.Second
	// delaisAvantRenouvellement : délais dépassés d'affilée avant de
	// fermer les connexions.
	delaisAvantRenouvellement = 2
)

// TransportServeur : transport HTTPS des échanges avec mirador-server
// (TLS 1.2 minimum, autorités du système, proxy de l'environnement,
// vérification des connexions HTTP/2).
func TransportServeur() *http.Transport {
	return &http.Transport{
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
		MaxIdleConns:        2,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
		ForceAttemptHTTP2:   true,
		Proxy:               http.ProxyFromEnvironment,
		HTTP2: &http.HTTP2Config{
			SendPingTimeout:  pingApres,
			PingTimeout:      pingSansReponse,
			WriteByteTimeout: ecritureBloquee,
		},
	}
}

// DelaiDepasse : l'erreur est une absence de réponse (pas un refus).
func DelaiDepasse(err error) bool {
	if err == nil {
		return false
	}
	var ne net.Error
	return errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout())
}

// Renouveleur compte les délais dépassés d'affilée d'un client et ferme ses
// connexions au repos quand il y en a trop. Non partagé entre goroutines.
type Renouveleur struct {
	delais int
}

// Noter : à appeler après chaque requête. Renvoie vrai si les connexions
// viennent d'être fermées (pour le journal).
func (r *Renouveleur) Noter(c *http.Client, err error) bool {
	if !DelaiDepasse(err) {
		r.delais = 0
		return false
	}
	if r.delais++; r.delais < delaisAvantRenouvellement || c == nil {
		return false
	}
	r.delais = 0
	c.CloseIdleConnections()
	return true
}
