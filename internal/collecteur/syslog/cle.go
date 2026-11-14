// SPDX-License-Identifier: GPL-3.0-or-later

package syslog

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

// Clé et certificat du syslog (SPEC § 23.8) : la clé privée est créée sur
// le collecteur et n'en sort jamais ; il envoie seulement une demande de
// certificat (CSR) au serveur, qui fait la démarche ACME et renvoie le
// certificat dans la configuration.

// ChargerOuCreerCle : clé ECDSA P-256 du fichier (0600), créée si absente.
func ChargerOuCreerCle(chemin string) (*ecdsa.PrivateKey, error) {
	if brut, err := os.ReadFile(chemin); err == nil { // #nosec G304 -- chemin de la configuration locale
		bloc, _ := pem.Decode(brut)
		if bloc == nil || bloc.Type != "EC PRIVATE KEY" {
			return nil, errors.New("clé du syslog illisible : " + chemin)
		}
		return x509.ParseECPrivateKey(bloc.Bytes)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	cle, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalECPrivateKey(cle)
	if err != nil {
		return nil, err
	}
	tmp := chemin + ".tmp"
	if err := os.WriteFile(tmp, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		return nil, err
	}
	return cle, os.Rename(tmp, chemin)
}

// DemandeCertificat : CSR (PEM) pour ce nom DNS.
func DemandeCertificat(cle *ecdsa.PrivateKey, nom string) (string, error) {
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: nom}, DNSNames: []string{nom}}, cle)
	if err != nil {
		return "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})), nil
}

// Certificats : certificat courant, remplacé sans couper les connexions.
type Certificats struct {
	mu   sync.RWMutex
	cert *tls.Certificate
	fin  time.Time
}

// Definir : chaîne PEM reçue du serveur, vérifiée avec la clé (une chaîne
// qui ne correspond pas à la clé, ou périmée, est refusée).
func (c *Certificats) Definir(chainePEM string, cle *ecdsa.PrivateKey, nom string, maintenant time.Time) error {
	der, err := x509.MarshalECPrivateKey(cle)
	if err != nil {
		return err
	}
	cert, err := tls.X509KeyPair([]byte(chainePEM), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}))
	if err != nil {
		return fmt.Errorf("certificat du syslog : %w", err)
	}
	feuille, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return err
	}
	if err := feuille.VerifyHostname(nom); err != nil {
		return fmt.Errorf("certificat du syslog : %w", err)
	}
	if maintenant.After(feuille.NotAfter) {
		return errors.New("certificat du syslog périmé")
	}
	cert.Leaf = feuille
	c.mu.Lock()
	c.cert, c.fin = &cert, feuille.NotAfter
	c.mu.Unlock()
	return nil
}

// Obtenir : pour tls.Config.GetCertificate.
func (c *Certificats) Obtenir(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.cert == nil {
		return nil, errors.New("certificat du syslog pas encore reçu")
	}
	return c.cert, nil
}

// Fin : échéance du certificat courant (zéro si aucun).
func (c *Certificats) Fin() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.fin
}
