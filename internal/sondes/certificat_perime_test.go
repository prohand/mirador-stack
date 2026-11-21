// SPDX-License-Identifier: GPL-3.0-or-later

package sondes

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Relecture du 21/11 : certificat périmé : son échéance est encore envoyée
// et le message d'erreur ne change pas d'un essai à l'autre.
func TestCertificatPerime(t *testing.T) {
	cle, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore: time.Now().Add(-48 * time.Hour), NotAfter: time.Now().Add(-time.Hour)}
	der, _ := x509.CreateCertificate(rand.Reader, tpl, tpl, &cle.PublicKey, cle)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: cle}}}
	srv.StartTLS()
	defer srv.Close()
	r := Essayer(context.Background(), Sonde{Type: HTTP, Cible: srv.URL, Certificat: 15}, Options{})
	if r.OK || r.FinCertificat.IsZero() || strings.ContainsAny(r.Erreur, "0123456789") {
		t.Errorf("OK=%v erreur=%q FinCertificat=%v", r.OK, r.Erreur, r.FinCertificat)
	}
	if _, ok := Valeurs(Sonde{Type: HTTP, Certificat: 15}, r)["mirador_sonde_certificat_fin_timestamp_seconds"]; !ok {
		t.Errorf("échéance du certificat non envoyée : %v", Valeurs(Sonde{Type: HTTP, Certificat: 15}, r))
	}
}
