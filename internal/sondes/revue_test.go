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
)

// Revue du 28/11 (M4) : rien de ce que le service répond n'est recopié
// dans l'erreur (étiquette remontée au serveur) : ni la valeur d'un champ
// JSON, ni une réponse mal formée.
func TestErreurSansContenuRecu(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"jeton":"SECRET-123"}`))
	}))
	defer srv.Close()
	r := Essayer(context.Background(), Sonde{Type: HTTP, Cible: srv.URL, ChampJSON: "jeton", ValeurJSON: "S"}, Options{})
	if r.OK || strings.Contains(r.Erreur, "SECRET") {
		t.Fatalf("valeur reçue recopiée : %+v", r)
	}
	// Service qui n'est pas du HTTP (bannière) : texte reçu jamais recopié.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			_, _ = c.Write([]byte("SSH-2.0-SECRET-banniere\r\n\r\n"))
			_ = c.Close()
		}
	}()
	r = Essayer(context.Background(), Sonde{Type: HTTP, Cible: "http://" + l.Addr().String()}, Options{})
	if r.OK || strings.Contains(r.Erreur, "SECRET") {
		t.Fatalf("bannière recopiée : %+v", r)
	}
	if got := court(fmt.Errorf(`lecture %q impossible`, "SECRET")); strings.Contains(got, "SECRET") {
		t.Errorf("citation recopiée : %s", got)
	}
}

// Une redirection vers une adresse refusée est bloquée au moment de la
// connexion (contrôle à chaque connexion, pas seulement sur la cible).
func TestRedirectionVersAdresseRefusee(t *testing.T) {
	li, err := net.Listen("tcp", "127.0.0.2:0")
	if err != nil {
		t.Skip("127.0.0.2 indisponible : " + err.Error())
	}
	interne := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	interne.Listener.Close()
	interne.Listener = li
	interne.Start()
	defer interne.Close()
	externe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, interne.URL+"/", http.StatusFound)
	}))
	defer externe.Close()
	o := Options{Refuser: func(ip net.IP) bool { return ip.Equal(net.IPv4(127, 0, 0, 2)) }}
	r := Essayer(context.Background(), Sonde{Type: HTTP, Cible: externe.URL}, o)
	if r.OK || !strings.Contains(r.Erreur, "refusée") {
		t.Fatalf("redirection vers une adresse refusée acceptée : %+v", r)
	}
	// Sans refus, la même redirection aboutit.
	if r := Essayer(context.Background(), Sonde{Type: HTTP, Cible: externe.URL}, Options{}); !r.OK {
		t.Fatalf("redirection : %+v", r)
	}
}
