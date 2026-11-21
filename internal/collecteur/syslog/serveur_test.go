// SPDX-License-Identifier: GPL-3.0-or-later

package syslog

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/netip"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"mirador-stack/internal/agent/config"
)

// certificatTest : CA de test qui signe un certificat pour la clé du
// collecteur (rôle du serveur ACME dans la vraie vie).
func certificatTest(t *testing.T, cle *ecdsa.PrivateKey, nom string, fin time.Time) (chaine string, racines *x509.CertPool) {
	t.Helper()
	cleCA, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "CA de test"}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: fin.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	derCA, _ := x509.CreateCertificate(rand.Reader, ca, ca, &cleCA.PublicKey, cleCA)
	caCert, _ := x509.ParseCertificate(derCA)
	feuille := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: nom}, DNSNames: []string{nom},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: fin, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, feuille, caCert, &cle.PublicKey, cleCA)
	if err != nil {
		t.Fatal(err)
	}
	racines = x509.NewCertPool()
	racines.AddCert(caCert)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), racines
}

func TestCleEtCertificat(t *testing.T) {
	chemin := filepath.Join(t.TempDir(), "syslog.key")
	cle, err := ChargerOuCreerCle(chemin)
	if err != nil {
		t.Fatal(err)
	}
	cle2, err := ChargerOuCreerCle(chemin)
	if err != nil || !cle.Equal(cle2) {
		t.Fatal("clé non relue à l'identique")
	}
	csr, err := DemandeCertificat(cle, "syslog.lan.exemple.org")
	if err != nil {
		t.Fatal(err)
	}
	bloc, _ := pem.Decode([]byte(csr))
	req, err := x509.ParseCertificateRequest(bloc.Bytes)
	if err != nil || req.CheckSignature() != nil || req.DNSNames[0] != "syslog.lan.exemple.org" {
		t.Fatalf("CSR : %v", err)
	}
	var c Certificats
	chaine, _ := certificatTest(t, cle, "syslog.lan.exemple.org", time.Now().Add(90*24*time.Hour))
	if err := c.Definir(chaine, cle, "autre.exemple.org", time.Now()); err == nil {
		t.Error("certificat pour un autre nom accepté")
	}
	autreCle, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err := c.Definir(chaine, autreCle, "syslog.lan.exemple.org", time.Now()); err == nil {
		t.Error("certificat d'une autre clé accepté")
	}
	if err := c.Definir(chaine, cle, "syslog.lan.exemple.org", time.Now().Add(100*24*time.Hour)); err == nil {
		t.Error("certificat périmé accepté")
	}
	if _, err := c.Obtenir(nil); err == nil {
		t.Error("certificat servi avant d'être reçu")
	}
	if err := c.Definir(chaine, cle, "syslog.lan.exemple.org", time.Now()); err != nil || c.Fin().IsZero() {
		t.Fatal(err)
	}
}

func TestServeurSyslog(t *testing.T) {
	// 127.0.0.1 accepté dans ce test (en vrai : adresses privées seulement).
	adresseAutorisee = func(a netip.Addr) bool { return a.IsLoopback() || config.AdressePrivee(a) }
	lireEmetteur = func(v string) (netip.Prefix, error) { return netip.ParsePrefix(v) }
	defer func() { adresseAutorisee, lireEmetteur = config.AdressePrivee, config.LireEmetteur }()
	if _, err := Nouveau(Options{Ecoute: "0.0.0.0:6514"}); err == nil {
		t.Fatal("écoute sur toutes les adresses acceptée")
	}

	cle, _ := ChargerOuCreerCle(filepath.Join(t.TempDir(), "k"))
	chaine, racines := certificatTest(t, cle, "syslog.lan.exemple.org", time.Now().Add(24*time.Hour))
	var certs Certificats
	if err := certs.Definir(chaine, cle, "syslog.lan.exemple.org", time.Now()); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var recues []Ligne
	s, err := Nouveau(Options{Ecoute: "127.0.0.1:0", Emetteurs: []config.Emetteur{{Adresse: "127.0.0.1/32", Machine: "nas"}},
		Debit: 1, Rafale: 5, Certificat: certs.Obtenir,
		Sortie: func(l Ligne) bool { mu.Lock(); recues = append(recues, l); mu.Unlock(); return true }})
	if err != nil {
		t.Fatal(err)
	}
	ctx, fin := context.WithCancel(context.Background())
	defer fin()
	go func() { _ = s.Tourner(ctx) }()
	<-s.Pret()
	cl, err := tls.Dial("tcp", s.Adresse().String(), &tls.Config{RootCAs: racines, ServerName: "syslog.lan.exemple.org", MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	messages := []string{
		"<14>1 2026-11-14T10:00:00Z nas-pretendu prog - - - bonjour",
		"<134>Nov 14 10:00:00 opnsense filterlog[1]: bloqué",
		"pas un message syslog",
	}
	for _, m := range messages {
		fmt.Fprintf(cl, "%d %s", len(m), m)
	}
	for i := 0; i < 10; i++ { // au-delà de la rafale (5) : refusées
		fmt.Fprintf(cl, "<14>app: ligne %d\n", i)
	}
	cl.Close()
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	e := s.Etat()
	if len(recues) == 0 || recues[0].Hote != "nas" || recues[0].App != "prog" || recues[0].Texte != "bonjour" {
		t.Fatalf("lignes reçues : %+v", recues)
	}
	// rafale 5 : bonjour + filterlog (jeté) + format (refusé) + 2 lignes ; le reste : débit.
	if e.Refus[RefusFilterlog] != 1 || e.Refus[RefusFormat] != 1 || e.Refus[RefusDebit] != 8 || len(recues) != 3 || e.Lignes["nas"] != 3 {
		t.Errorf("compteurs : %+v, %d reçues", e, len(recues))
	}
	// Connexions ouvertes puis fermées : aucune goroutine ne reste
	// (relecture du 21/11 : une par connexion jusqu'au redémarrage).
	mu.Unlock()
	time.Sleep(100 * time.Millisecond)
	avant := runtime.NumGoroutine()
	for i := 0; i < 50; i++ {
		if c, err := tls.Dial("tcp", s.Adresse().String(), &tls.Config{RootCAs: racines, ServerName: "syslog.lan.exemple.org", MinVersion: tls.VersionTLS12}); err == nil {
			c.Close()
		}
	}
	time.Sleep(300 * time.Millisecond)
	if apres := runtime.NumGoroutine(); apres > avant+5 {
		t.Errorf("goroutines : %d avant, %d après 50 connexions fermées", avant, apres)
	}
	mu.Lock()
	// Émetteur inconnu : fermé avant le TLS.
	s.mu.Lock()
	s.emetteurs = nil
	s.mu.Unlock()
	if c2, err := net.Dial("tcp", s.Adresse().String()); err == nil {
		_ = c2.SetReadDeadline(time.Now().Add(time.Second))
		if _, err := c2.Read(make([]byte, 1)); err == nil {
			t.Error("émetteur inconnu servi")
		}
		c2.Close()
	}
	time.Sleep(50 * time.Millisecond)
	if s.Etat().Refus[RefusEmetteur] != 1 {
		t.Errorf("émetteur inconnu non compté : %+v", s.Etat().Refus)
	}
}
