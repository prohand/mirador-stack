// SPDX-License-Identifier: GPL-3.0-or-later

package sondes

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

func TestValider(t *testing.T) {
	bonnes := []Sonde{
		{Nom: "Nextcloud", Type: HTTP, Cible: "https://cloud.exemple.fr/status.php", Depuis: "serveur", Code: 200, Contenu: "installed"},
		{Nom: "SSH du VPS", Type: TCP, Cible: "vps.exemple.fr:22", Depuis: "lan"},
		{Nom: "Box", Type: Ping, Cible: "192.168.1.254", Depuis: "lan", Intervalle: "2m"},
		{Nom: "DNS interne", Type: DNS, Cible: "192.168.1.2", Question: "nas.lan", Depuis: "lan"},
		{Nom: "HA", Type: HTTP, Cible: "https://ha.lan:8123/api/", Depuis: "lan", ChampJSON: "checks.0.status", ValeurJSON: "ok", Certificat: 14},
	}
	for _, s := range bonnes {
		if err := s.Valider(); err != nil {
			t.Errorf("%s : %v", s.Nom, err)
		}
	}
	mauvaises := []Sonde{
		{Nom: "", Type: HTTP, Cible: "https://a", Depuis: "x"},
		{Nom: "a", Type: HTTP, Cible: "ftp://a", Depuis: "x"},
		{Nom: "a", Type: TCP, Cible: "hote-sans-port", Depuis: "x"},
		{Nom: "a", Type: DNS, Cible: "1.1.1.1", Depuis: "x"},
		{Nom: "a", Type: "smtp", Cible: "a:25", Depuis: "x"},
		{Nom: "a", Type: TCP, Cible: "a:1", Depuis: "x", Intervalle: "5s"},
		{Nom: "a", Type: TCP, Cible: "a:1", Depuis: ""},
		{Nom: "<script>", Type: TCP, Cible: "a:1", Depuis: "x"},
		{Nom: "a", Type: TCP, Cible: "a:1", Depuis: "x", ChampJSON: "status"},
		{Nom: "a", Type: HTTP, Cible: "https://a", Depuis: "x", ValeurJSON: "ok"},
		{Nom: "a", Type: HTTP, Cible: "https://a", Depuis: "x", ChampJSON: "a..b"},
		{Nom: "a", Type: HTTP, Cible: "http://a", Depuis: "x", Certificat: 14},
		{Nom: "a", Type: HTTP, Cible: "https://a", Depuis: "x", Certificat: 400},
	}
	for i, s := range mauvaises {
		if err := s.Valider(); err == nil {
			t.Errorf("mauvaise %d acceptée : %+v", i, s)
		}
	}
}

func TestHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			_, _ = w.Write([]byte(`{"installed":true}`))
		case "/lent":
			time.Sleep(60 * time.Millisecond)
			_, _ = w.Write([]byte("ok"))
		default:
			w.WriteHeader(http.StatusBadGateway)
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	s := Sonde{Nom: "a", Type: HTTP, Cible: srv.URL + "/ok", Contenu: `"installed":true`}
	if r := Essayer(ctx, s, Options{}); !r.OK || r.Code != 200 {
		t.Fatalf("ok : %+v", r)
	}
	s.Contenu = "absent"
	if r := Essayer(ctx, s, Options{}); r.OK || r.Erreur != "contenu attendu absent" {
		t.Fatalf("contenu : %+v", r)
	}
	s.Cible, s.Contenu = srv.URL+"/panne", ""
	if r := Essayer(ctx, s, Options{}); r.OK || r.Code != 502 || r.Texte() != "Échec : code 502" {
		t.Fatalf("502 : %+v", r)
	}
	s.Cible, s.Lente = srv.URL+"/lent", "20ms"
	r := Essayer(ctx, s, Options{})
	if v := Valeurs(s, r); v["mirador_sonde_succes"] != 1 || v["mirador_sonde_lente"] != 1 || v["mirador_sonde_code_http"] != 200 {
		t.Fatalf("lente : %+v %+v", r, v)
	}
	// Depuis le serveur : adresses locales refusées.
	refus := Options{Refuser: func(ip net.IP) bool { return ip.IsLoopback() }}
	if r := Essayer(ctx, Sonde{Type: HTTP, Cible: srv.URL + "/ok"}, refus); r.OK || !strings.Contains(r.Erreur, "refusée") {
		t.Fatalf("refus : %+v", r)
	}
}

func TestTCP(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	adr := l.Addr().String()
	ctx := context.Background()
	if r := Essayer(ctx, Sonde{Type: TCP, Cible: adr}, Options{}); !r.OK {
		t.Fatalf("ouvert : %+v", r)
	}
	l.Close()
	if r := Essayer(ctx, Sonde{Type: TCP, Cible: adr}, Options{}); r.OK || r.Erreur != "connexion refusée (port fermé)" {
		t.Fatalf("fermé : %+v", r)
	}
	// Nom inconnu : message court (pas tout le texte de Go).
	if r := Essayer(ctx, Sonde{Type: HTTP, Cible: "http://nom-inconnu.invalid/"}, Options{}); r.OK || len(r.Erreur) > 60 {
		t.Fatalf("nom inconnu : %+v", r)
	}
}

// serveurDNS : répond nas.lan → 192.168.1.10, NXDOMAIN sinon.
func serveurDNS(t *testing.T) string {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		b := make([]byte, 1500)
		for {
			n, adr, err := pc.ReadFrom(b)
			if err != nil {
				return
			}
			var m dnsmessage.Message
			if m.Unpack(b[:n]) != nil || len(m.Questions) == 0 {
				continue
			}
			q := m.Questions[0]
			m.Header.Response, m.Header.Authoritative = true, true
			if q.Name.String() == "nas.lan." && q.Type == dnsmessage.TypeA {
				m.Answers = []dnsmessage.Resource{{Header: dnsmessage.ResourceHeader{Name: q.Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 60},
					Body: &dnsmessage.AResource{A: [4]byte{192, 168, 1, 10}}}}
			} else if q.Name.String() != "nas.lan." {
				m.Header.RCode = dnsmessage.RCodeNameError
			}
			if rep, err := m.Pack(); err == nil {
				_, _ = pc.WriteTo(rep, adr)
			}
		}
	}()
	return pc.LocalAddr().String()
}

func TestDNS(t *testing.T) {
	adr := serveurDNS(t)
	ctx := context.Background()
	s := Sonde{Type: DNS, Cible: adr, Question: "nas.lan"}
	if r := Essayer(ctx, s, Options{}); !r.OK {
		t.Fatalf("ok : %+v", r)
	}
	s.Contenu = "192.168.1.10"
	if r := Essayer(ctx, s, Options{}); !r.OK {
		t.Fatalf("adresse attendue : %+v", r)
	}
	s.Contenu = "10.0.0.1"
	if r := Essayer(ctx, s, Options{}); r.OK {
		t.Fatalf("mauvaise adresse : %+v", r)
	}
	s.Question, s.Contenu = "inconnu.lan", ""
	if r := Essayer(ctx, s, Options{}); r.OK || r.Erreur != "nom inconnu du serveur DNS" {
		t.Fatalf("inconnu : %+v", r)
	}
}

func TestPing(t *testing.T) {
	r := Essayer(context.Background(), Sonde{Type: Ping, Cible: "127.0.0.1"}, Options{})
	if !r.OK && strings.Contains(r.Erreur, "ping impossible") {
		t.Skip("socket ping interdit sur cette machine : " + r.Erreur)
	}
	if !r.OK {
		t.Fatalf("ping : %+v", r)
	}
	if r := Essayer(context.Background(), Sonde{Type: Ping, Cible: "127.0.0.1"}, Options{Refuser: func(ip net.IP) bool { return ip.IsLoopback() }}); r.OK {
		t.Fatal("ping local accepté depuis le serveur")
	}
}

// Champ JSON (SPEC § 81) : chemin, nombres, booléens, listes, erreurs lisibles.
func TestVerifierJSON(t *testing.T) {
	corps := []byte(`{"status":"ok","checks":{"db":{"status":"degraded"}},"items":[{"etat":1},{"etat":2.5}],"actif":true,"vide":null}`)
	for _, c := range []struct{ champ, attendu, erreur string }{
		{"status", "ok", ""},
		{"status", "", ""},
		{"checks.db.status", "ok", "champ JSON checks.db.status = « degraded », attendu « ok »"},
		{"items.1.etat", "2.5", ""},
		{"items.0.etat", "1", ""},
		{"items.0.etat", "3", "champ JSON items.0.etat différent de « 3 »"}, // nombre reçu non recopié
		{"items.2.etat", "", "champ JSON items.2.etat absent"},
		{"actif", "true", ""},
		{"vide", "null", ""},
		{"checks", "x", "champ JSON checks : objet ou liste, valeur simple attendue"},
		{"status.x", "", "champ JSON status.x absent"},
	} {
		err := verifierJSON(corps, c.champ, c.attendu)
		if got := fmt.Sprint(err); c.erreur == "" && err != nil || c.erreur != "" && got != c.erreur {
			t.Errorf("%s=%s : %v", c.champ, c.attendu, err)
		}
	}
	if err := verifierJSON([]byte("<html>"), "status", ""); err == nil || err.Error() != "réponse non JSON" {
		t.Errorf("non JSON : %v", err)
	}
}

// Essai HTTPS : champ JSON lu et fin du certificat remontée en métrique.
func TestHTTPJSONCertificat(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"degraded"}`))
	}))
	defer srv.Close()
	s := Sonde{Nom: "api", Type: HTTP, Cible: srv.URL, Depuis: "lan", NonVerifie: true, ChampJSON: "status", ValeurJSON: "ok", Certificat: 14}
	r := Essayer(context.Background(), s, Options{})
	if r.OK || r.Erreur != "champ JSON status = « degraded », attendu « ok »" || r.FinCertificat.IsZero() {
		t.Fatalf("essai : %+v", r)
	}
	v := Valeurs(s, r)
	if v["mirador_sonde_certificat_alerte_jours"] != 14 || v["mirador_sonde_certificat_fin_timestamp_seconds"] != float64(r.FinCertificat.Unix()) {
		t.Errorf("valeurs : %v", v)
	}
	s.ValeurJSON, s.Certificat = "degraded", 0
	if r := Essayer(context.Background(), s, Options{}); !r.OK {
		t.Errorf("attendu OK : %+v", r)
	} else if v := Valeurs(s, r); v["mirador_sonde_certificat_fin_timestamp_seconds"] == 0 {
		t.Error("fin du certificat non remontée (jours restants affichés)")
	} else if _, ok := v["mirador_sonde_certificat_alerte_jours"]; ok {
		t.Error("seuil d'alerte remonté sans alerte demandée")
	}
}

// « Vérifier maintenant » (SPEC § 108) : l'essai part sur le signal, sans
// attendre le rythme (1 h ici), puis le rythme reprend.
func TestTournerMaintenant(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	s := Sonde{Nom: "zz", Type: TCP, Cible: l.Addr().String(), Intervalle: "1h"}
	ctx, fin := context.WithCancel(context.Background())
	defer fin()
	signal := make(chan struct{}, 1)
	faits := make(chan Resultat, 4)
	go Tourner(ctx, s, Options{Maintenant: signal}, func(_ Sonde, r Resultat) { faits <- r })
	// Premier essai au démarrage (étalé sur 0 à 9 s selon le nom) : on
	// déclenche tout de suite pour ne pas l'attendre.
	signal <- struct{}{}
	select {
	case r := <-faits:
		if !r.OK {
			t.Fatalf("essai : %+v", r)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("essai non fait sur le signal")
	}
	signal <- struct{}{}
	select {
	case <-faits:
	case <-time.After(3 * time.Second):
		t.Fatal("deuxième signal sans essai")
	}
}

// Relecture du 21/11 : la sonde DNS interroge vraiment le serveur visé
// (pas /etc/hosts du collecteur) ; un code attendu 3xx n'est pas suivi.
func TestDNSEtRedirection(t *testing.T) {
	if err := essaiDNS(context.Background(), Sonde{Cible: "127.0.0.1:1", Question: "localhost"}, Options{}); err == nil {
		t.Error("DNS : « localhost » répondu sans serveur DNS (fichier hosts lu)")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/accueil", http.StatusMovedPermanently)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	if r, err := essaiHTTP(context.Background(), Sonde{Type: HTTP, Cible: srv.URL + "/", Code: 301}, Options{}); err != nil || r.Code != 301 {
		t.Errorf("code 301 attendu : %d %v", r.Code, err)
	}
	if r, err := essaiHTTP(context.Background(), Sonde{Type: HTTP, Cible: srv.URL + "/"}, Options{}); err != nil || r.Code != 200 {
		t.Errorf("sans code attendu, la redirection est suivie : %d %v", r.Code, err)
	}
}

// Relecture du 21/11 : un essai coupé par l'arrêt (redémarrage du
// collecteur) n'est pas envoyé comme un échec.
func TestTournerArretSansFauxEchec(t *testing.T) {
	lent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
	}))
	defer lent.Close()
	ctx, fin := context.WithCancel(context.Background())
	signal := make(chan struct{}, 1)
	signal <- struct{}{}
	rendus := make(chan Resultat, 2)
	go func() {
		time.Sleep(200 * time.Millisecond)
		fin()
	}()
	Tourner(ctx, Sonde{Nom: "lent", Type: HTTP, Cible: lent.URL, Intervalle: "1h"}, Options{Maintenant: signal},
		func(_ Sonde, r Resultat) { rendus <- r })
	if len(rendus) != 0 {
		t.Errorf("faux échec envoyé à l'arrêt : %+v", <-rendus)
	}
}
