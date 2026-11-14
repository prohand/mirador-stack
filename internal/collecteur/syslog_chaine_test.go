// SPDX-License-Identifier: GPL-3.0-or-later

package collecteur

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"path/filepath"
	"testing"
	"time"

	"mirador-stack/internal/agent/config"
	"mirador-stack/internal/agent/metrics"
)

// signer : la CA de test signe la demande du collecteur (rôle d'ACME).
func signer(t *testing.T, csrPEM string) string {
	t.Helper()
	bloc, _ := pem.Decode([]byte(csrPEM))
	req, err := x509.ParseCertificateRequest(bloc.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	cleCA, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "CA"}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(48 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	feuille := &x509.Certificate{SerialNumber: big.NewInt(2), DNSNames: req.DNSNames, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour)}
	der, _ := x509.CreateCertificate(rand.Reader, feuille, ca, req.PublicKey, cleCA)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// Syslog dans le collecteur : clé et demande créées au premier démarrage,
// écoute préparée seulement avec un certificat valable pour cette clé.
func TestNouveauSyslog(t *testing.T) {
	journal := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.DefautCollecteur()
	cfg.Nom, cfg.URLLogs = "collecteur-lan", "https://mirador.exemple.invalid/api/agent/v1/logs"
	cfg.FichierConfigDistante = filepath.Join(t.TempDir(), "config-distante.json")
	if s, err := NouveauSyslog(cfg, "token", journal); s != nil || err != nil {
		t.Fatal("syslog lancé sans réglage")
	}
	cfg.Collecteur = config.Collecteur{Equipements: []string{"nas"}, Syslog: &config.Syslog{Ecoute: "192.168.1.10:6514",
		NomCertificat: "syslog.lan.exemple.org", Emetteurs: []config.Emetteur{{Adresse: "192.168.1.20", Machine: "nas"}}}}
	s, err := NouveauSyslog(cfg, "token", journal)
	if err != nil || s == nil || s.CSR == "" || s.serveur != nil {
		t.Fatalf("sans certificat : %+v %v", s, err)
	}
	b := metrics.NewBatch(16, nil)
	b.Reset(0)
	_ = s.Collecter(b)
	if b.Len() != 1 || b.Samples[0].Name != "mirador_collecteur_syslog_ecoute" || b.Samples[0].Value != 0 {
		t.Errorf("mesures sans certificat : %+v", b.Samples[:b.Len()])
	}
	// Même clé au redémarrage ; certificat reçu pour elle : écoute prête.
	cfg.Collecteur.Syslog.Certificat = signer(t, s.CSR)
	s2, err := NouveauSyslog(cfg, "token", journal)
	if err != nil || s2.serveur == nil || s2.certs.Fin().IsZero() {
		t.Fatalf("avec certificat : %v", err)
	}
	b.Reset(0)
	_ = s2.Collecter(b)
	if b.Len() < 4 {
		t.Errorf("mesures avec certificat : %d", b.Len())
	}
	// Certificat d'une autre clé : écoute non préparée, nouvelle demande.
	autre := signer(t, s.CSR)
	cfg.Collecteur.Syslog.Certificat = autre[:len(autre)/2]
	if s3, err := NouveauSyslog(cfg, "token", journal); err != nil || s3.serveur != nil {
		t.Errorf("certificat abîmé accepté : %v", err)
	}
}
