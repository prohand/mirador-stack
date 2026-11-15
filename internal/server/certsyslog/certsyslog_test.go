// SPDX-License-Identifier: GPL-3.0-or-later

package certsyslog

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"mirador-stack/internal/server/base"
)

var cleServeur = []byte("0123456789abcdef0123456789abcdef")

func baseTest(t *testing.T) *sql.DB {
	t.Helper()
	db, err := base.Ouvrir(filepath.Join(t.TempDir(), "mirador.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := base.Migrer(context.Background(), db, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO machines(id, nom, statut, type, cree_le) VALUES (7, 'collecteur-lan', 'approuvee', 'collecteur', 1)`); err != nil {
		t.Fatal(err)
	}
	return db
}

func csrTest(t *testing.T, nom string) (string, *ecdsa.PrivateKey) {
	t.Helper()
	cle, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: nom}, DNSNames: []string{nom}}, cle)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})), cle
}

// --- Faux serveur ACME (RFC 8555, le strict nécessaire, signatures non vérifiées) ----

type fauxACME struct {
	srv     *httptest.Server
	mu      sync.Mutex
	nom     string
	valide  bool
	certDER []byte
	caDER   []byte
	caCle   *ecdsa.PrivateKey
	txt     func() string // valeur posée chez le faux deSEC
	refus   bool          // le défi échoue
}

func nouveauFauxACME(t *testing.T, txt func() string) *fauxACME {
	f := &fauxACME{txt: txt}
	f.caCle, _ = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Faux ACME"}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(365 * 24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	f.caDER, _ = x509.CreateCertificate(rand.Reader, ca, ca, &f.caCle.PublicKey, f.caCle)
	f.srv = httptest.NewTLSServer(http.HandlerFunc(f.servir))
	t.Cleanup(f.srv.Close)
	return f
}

func charge(r *http.Request) []byte {
	var jws struct{ Payload string }
	b, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(b, &jws)
	p, _ := base64.RawURLEncoding.DecodeString(jws.Payload)
	return p
}

func (f *fauxACME) servir(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b := f.srv.URL
	w.Header().Set("Replay-Nonce", base64.RawURLEncoding.EncodeToString([]byte(time.Now().String())))
	json := func(code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = jsonEncode(w, v)
	}
	commande := func() map[string]any {
		statut := "pending"
		if f.valide {
			statut = "ready"
		}
		o := map[string]any{"status": statut, "identifiers": []any{map[string]string{"type": "dns", "value": f.nom}},
			"authorizations": []string{b + "/authz/1"}, "finalize": b + "/finalize/1"}
		if f.certDER != nil {
			o["status"], o["certificate"] = "valid", b+"/cert/1"
		}
		return o
	}
	switch r.URL.Path {
	case "/dir":
		json(200, map[string]string{"newNonce": b + "/nonce", "newAccount": b + "/acct", "newOrder": b + "/order", "revokeCert": b + "/revoke", "keyChange": b + "/kc"})
	case "/nonce":
		w.WriteHeader(200)
	case "/acct":
		w.Header().Set("Location", b+"/acct/1")
		json(201, map[string]string{"status": "valid"})
	case "/order":
		var p struct{ Identifiers []struct{ Value string } }
		_ = jsonDecode(charge(r), &p)
		f.nom, f.valide, f.certDER = p.Identifiers[0].Value, false, nil
		w.Header().Set("Location", b+"/order/1")
		json(201, commande())
	case "/authz/1":
		statut := "pending"
		if f.valide {
			statut = "valid"
		}
		json(200, map[string]any{"status": statut, "identifier": map[string]string{"type": "dns", "value": f.nom},
			"challenges": []any{map[string]string{"type": "dns-01", "url": b + "/chal/1", "token": "jeton-du-defi", "status": statut}}})
	case "/chal/1":
		if f.txt() == "" || f.refus {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(403)
			_, _ = io.WriteString(w, `{"type":"urn:ietf:params:acme:error:unauthorized","detail":"TXT absent"}`)
			return
		}
		f.valide = true
		json(200, map[string]string{"type": "dns-01", "url": b + "/chal/1", "token": "jeton-du-defi", "status": "valid"})
	case "/order/1":
		json(200, commande())
	case "/finalize/1":
		var p struct{ CSR string }
		_ = jsonDecode(charge(r), &p)
		der, _ := base64.RawURLEncoding.DecodeString(p.CSR)
		req, err := x509.ParseCertificateRequest(der)
		if err != nil {
			w.WriteHeader(400)
			return
		}
		ca, _ := x509.ParseCertificate(f.caDER)
		feuille := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: req.DNSNames[0]}, DNSNames: req.DNSNames,
			NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(90 * 24 * time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
		f.certDER, _ = x509.CreateCertificate(rand.Reader, feuille, ca, req.PublicKey, f.caCle)
		json(200, commande())
	case "/cert/1":
		w.Header().Set("Content-Type", "application/pem-certificate-chain")
		_ = pem.Encode(w, &pem.Block{Type: "CERTIFICATE", Bytes: f.certDER})
		_ = pem.Encode(w, &pem.Block{Type: "CERTIFICATE", Bytes: f.caDER})
	default:
		w.WriteHeader(404)
	}
}

func jsonEncode(w io.Writer, v any) error { return json.NewEncoder(w).Encode(v) }
func jsonDecode(b []byte, v any) error    { return json.Unmarshal(b, v) }

// Démarche complète : réglages, demande du collecteur, défi DNS chez
// (faux) deSEC, certificat obtenu, vérifié et rangé ; renouvellement.
func TestDemarcheACME(t *testing.T) {
	ctx := context.Background()
	db := baseTest(t)
	var mu sync.Mutex
	txt := ""
	desec := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch || r.URL.Path != "/domains/exemple.org/rrsets/" || r.Header.Get("Authorization") != "Token jeton-desec-secret" {
			w.WriteHeader(403)
			return
		}
		var rr []struct {
			Subname string
			Records []string
		}
		_ = json.NewDecoder(r.Body).Decode(&rr)
		mu.Lock()
		defer mu.Unlock()
		if len(rr) == 1 && rr[0].Subname == "_acme-challenge.syslog.lan" {
			txt = strings.Join(rr[0].Records, "")
		}
		w.WriteHeader(200)
	}))
	defer desec.Close()
	faux := nouveauFauxACME(t, func() string { mu.Lock(); defer mu.Unlock(); return txt })

	if err := DefinirReglages(ctx, db, cleServeur, Saisie{Fournisseur: "desec", Domaine: "pas une zone", Identifiants: map[string]string{"jeton": "x"}}, "admin", time.Now()); err == nil {
		t.Error("zone invalide acceptée")
	}
	if err := DefinirReglages(ctx, db, cleServeur, Saisie{Fournisseur: "desec", Domaine: "exemple.org", Annuaire: faux.srv.URL + "/dir",
		Identifiants: map[string]string{"jeton": "jeton-desec-secret"}}, "admin", time.Now()); err != nil {
		t.Fatal(err)
	}
	r, _ := LireReglages(ctx, db)
	if strings.Contains(r.IdentifiantsChiffre, "jeton-desec-secret") || r.IdentifiantsChiffre == "" {
		t.Fatal("jeton deSEC non chiffré")
	}
	var audit string
	_ = db.QueryRow(`SELECT apres FROM audit WHERE action = 'certsyslog_reglages'`).Scan(&audit)
	if strings.Contains(audit, "jeton-desec-secret") {
		t.Fatal("jeton deSEC dans l'audit")
	}
	csr, cle := csrTest(t, "syslog.lan.exemple.org")
	if err := NoterCSR(ctx, db, 7, "pas une CSR", time.Now()); err == nil {
		t.Error("CSR illisible acceptée")
	}
	if err := NoterCSR(ctx, db, 7, csr, time.Now()); err != nil {
		t.Fatal(err)
	}
	nouveaux := 0
	g := &Gestionnaire{DB: db, Emetteur: &ACME{DB: db, Cle: cleServeur, BasesDNS: map[string]string{"desec": desec.URL}, ClientDNS: desec.Client(), Client: faux.srv.Client(), Attente: time.Millisecond},
		NomDe: func(context.Context, int64) (string, error) { return "syslog.lan.exemple.org", nil }, Apres: func() { nouveaux++ }}
	g.Passer(ctx)
	e, _ := LireEtat(ctx, db, 7)
	if e.Erreur != "" || e.Certificat == "" || e.Nom != "syslog.lan.exemple.org" || nouveaux != 1 || e.Etape != "" {
		t.Fatalf("après la démarche : %+v", e)
	}
	// Avancement (SPEC § 32) : les six étapes, dans l'ordre, avec la fin
	// prévue de l'attente DNS.
	var etapes []string
	var finAttente time.Time
	req, _ := LireCSR(csr)
	acmeSeul := &ACME{DB: db, Cle: cleServeur, BasesDNS: map[string]string{"desec": desec.URL}, ClientDNS: desec.Client(), Client: faux.srv.Client(), Attente: time.Millisecond}
	if _, err := acmeSeul.Obtenir(AvecSuivi(ctx, func(etape string, jusqua time.Time) {
		etapes = append(etapes, etape)
		if etape == EtapePropagation {
			finAttente = jusqua
		}
	}), "syslog.lan.exemple.org", req.Raw); err != nil {
		t.Fatal(err)
	}
	if strings.Join(etapes, ",") != "compte,commande,dns,propagation,validation,emission" || finAttente.IsZero() {
		t.Errorf("étapes : %v (fin d'attente %v)", etapes, finAttente)
	}
	bloc, _ := pem.Decode([]byte(e.Certificat))
	cert, _ := x509.ParseCertificate(bloc.Bytes)
	if !cert.PublicKey.(*ecdsa.PublicKey).Equal(&cle.PublicKey) || cert.VerifyHostname("syslog.lan.exemple.org") != nil {
		t.Fatal("certificat pour une autre clé ou un autre nom")
	}
	mu.Lock()
	if txt != "" {
		t.Error("enregistrement TXT du défi pas nettoyé")
	}
	mu.Unlock()
	// Rien à refaire tant que l'échéance est loin…
	g.Passer(ctx)
	if nouveaux != 1 {
		t.Error("certificat refait sans raison")
	}
	// … sauf « Renouveler maintenant », ou nouvelle clé (nouvelle CSR).
	if err := Demander(ctx, db, 7, "admin", time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	g.Maintenant = func() time.Time { return time.Now().Add(2 * time.Second) }
	g.Passer(ctx)
	if nouveaux != 2 {
		t.Errorf("« Renouveler maintenant » sans effet : %d", nouveaux)
	}
	// Échec du défi : erreur gardée, pas de nouvel essai avant 6 h.
	csr2, _ := csrTest(t, "syslog.lan.exemple.org")
	_ = NoterCSR(ctx, db, 7, csr2, time.Now())
	faux.mu.Lock()
	faux.refus = true
	faux.mu.Unlock()
	g.Maintenant = func() time.Time { return time.Now().Add(3 * time.Second) }
	g.Passer(ctx)
	if e, _ = LireEtat(ctx, db, 7); e.Erreur == "" || e.Etape != EtapeValidation || e.EtapeLe == 0 {
		t.Fatalf("échec non enregistré, ou sans son étape : %+v", e)
	}
	essai := e.EssaiLe
	g.Maintenant = func() time.Time { return time.Now().Add(time.Hour) }
	g.Passer(ctx)
	if e, _ = LireEtat(ctx, db, 7); e.EssaiLe != essai {
		t.Error("nouvel essai avant 6 h")
	}
}

// Demande refusée : autre nom que celui réglé ; nom hors de la zone.
func TestRefus(t *testing.T) {
	ctx := context.Background()
	db := baseTest(t)
	csr, _ := csrTest(t, "autre.exemple.org")
	_ = NoterCSR(ctx, db, 7, csr, time.Now())
	appele := false
	g := &Gestionnaire{DB: db, Emetteur: emetteurFn(func(context.Context, string, []byte) (string, error) { appele = true; return "", errors.New("x") }),
		NomDe: func(context.Context, int64) (string, error) { return "syslog.lan.exemple.org", nil }}
	g.Passer(ctx)
	if e, _ := LireEtat(ctx, db, 7); appele || !strings.Contains(e.Erreur, "autre.exemple.org") {
		t.Errorf("demande pour un autre nom : appel ACME %v, %+v", appele, e)
	}
	if _, err := nomDansDomaine("syslog.ailleurs.net", "exemple.org"); err == nil {
		t.Error("nom hors de la zone accepté")
	}
	if s, _ := nomDansDomaine("syslog.lan.exemple.org", "exemple.org"); s != "_acme-challenge.syslog.lan" {
		t.Errorf("sous-nom : %q", s)
	}
	if err := Demander(ctx, db, 99, "admin", time.Now()); err == nil {
		t.Error("renouvellement demandé sans CSR")
	}
}

type emetteurFn func(context.Context, string, []byte) (string, error)

func (f emetteurFn) Obtenir(ctx context.Context, nom string, csr []byte) (string, error) {
	return f(ctx, nom, csr)
}
