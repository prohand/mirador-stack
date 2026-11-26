// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"mirador-stack/internal/server/flotte"
	"mirador-stack/internal/server/ingestion"
	"mirador-stack/internal/server/stockage"
	"mirador-stack/internal/server/web"
)

// Retour du 26/11 (dns2 refusé une minute en IPv4) : « Autoriser cette
// adresse » ajoute l'adresse refusée à la liste, avec le code 2FA ; une
// autre adresse est refusée.
func TestAutoriserAdresseRefusee(t *testing.T) {
	b := nouveauBanc(t)
	ctx := t.Context()
	b.machinesApprouvees(t, "dns2")
	mem := &stockage.Memoire{}
	ing := ingestion.Nouvelle(ingestion.Options{DB: b.db, Metriques: mem, Logs: stockage.LogsMemoire{Memoire: mem}, Maintenant: b.h.maintenant})
	b.c.opt.Ingestion = ing
	token, err := flotte.TokenManuel(ctx, b.db, "dns2", "admin", b.h.maintenant())
	if err != nil {
		t.Fatal(err)
	}
	if err := flotte.DefinirIPs(ctx, b.db, "dns2", "2001:db8::618", "admin", b.h.maintenant()); err != nil {
		t.Fatal(err)
	}
	// Envoi depuis une adresse IPv4 non autorisée : refusé (403).
	h := web.IPClient(nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, liberer, ok := ing.Accueillir(w, r); ok {
			liberer()
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	r := httptest.NewRequest(http.MethodPost, "/api/agent/v1/heartbeat", nil)
	r.RemoteAddr = "203.0.113.7:40000"
	w := httptest.NewRecorder()
	r.Header.Set("Authorization", "Bearer "+token)
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("envoi : %d, 403 attendu", w.Code)
	}

	cl, secret := b.premierAdmin("admin", mdpAdmin)
	_, corps, _ := b.get(cl, "/machines/dns2")
	if !strings.Contains(corps, `value="ips_ajouter"`) || !strings.Contains(corps, `name="adresse" value="203.0.113.7"`) {
		t.Fatal("bouton « Autoriser cette adresse » absent")
	}
	csrf := b.csrf(cl, "/machines/dns2")
	// Une autre adresse que celle du refus : refusée.
	b.post(cl, "/machines/dns2", url.Values{"csrf": {csrf}, "action": {"ips_ajouter"}, "adresse": {"198.51.100.9"}, "code": {b.code(secret)}})
	if m := machineNommee(t, b, "dns2"); m.IPsAutorisees != "2001:db8::618/128" {
		t.Fatalf("adresse quelconque ajoutée : %q", m.IPsAutorisees)
	}
	b.h.avancer(31 * time.Second) // code 2FA suivant (anti-rejeu)
	b.post(cl, "/machines/dns2", url.Values{"csrf": {csrf}, "action": {"ips_ajouter"}, "adresse": {"203.0.113.7"}, "code": {b.code(secret)}})
	if m := machineNommee(t, b, "dns2"); !adresseAutorisee("203.0.113.7", m.IPsAutorisees) || !adresseAutorisee("2001:db8::618", m.IPsAutorisees) {
		t.Fatalf("adresses après l'ajout : %q", m.IPsAutorisees)
	}
	if _, corps, _ = b.get(cl, "/machines/dns2"); strings.Contains(corps, `value="ips_ajouter"`) {
		t.Error("bouton encore proposé pour une adresse autorisée")
	}
}

func machineNommee(t *testing.T, b *banc, nom string) flotte.InfoMachine {
	t.Helper()
	ms, err := flotte.ListerMachines(t.Context(), b.db)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range ms {
		if m.Nom == nom {
			return m
		}
	}
	t.Fatalf("machine %s introuvable", nom)
	return flotte.InfoMachine{}
}

func TestAdresseAutorisee(t *testing.T) {
	for _, c := range []struct {
		adresse, liste string
		ok             bool
	}{
		{"203.0.113.7", "", true},
		{"203.0.113.7", "203.0.113.0/24", true},
		{"203.0.113.7", "2001:db8::/32", false},
		{"2001:db8::618", "185.0.0.1, 2001:db8::/32", true},
		{"pas une adresse", "203.0.113.0/24", false},
	} {
		if g := adresseAutorisee(c.adresse, c.liste); g != c.ok {
			t.Errorf("%q dans %q : %v", c.adresse, c.liste, g)
		}
	}
}
