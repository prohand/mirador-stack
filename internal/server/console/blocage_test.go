// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"mirador-stack/internal/server/base"
	"mirador-stack/internal/server/fluxbloques"
)

// Liste de blocage (demande du 27/11) : désactivée par défaut, code 2FA
// pour l'activer, adresse de lecture montrée une fois, adresses jamais
// bloquées écartées, lue sans session ni liste d'adresses autorisées.
func TestListeBlocage(t *testing.T) {
	b := nouveauBanc(t)
	ctx := context.Background()
	m := b.h.maintenant()
	db, err := base.Ouvrir(filepath.Join(t.TempDir(), "flux.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	e, err := fluxbloques.Nouveau(ctx, fluxbloques.Options{DB: db, Maintenant: b.h.maintenant})
	if err != nil {
		t.Fatal(err)
	}
	so, _ := fluxbloques.Nouveau(ctx, fluxbloques.Options{Maintenant: b.h.maintenant})
	b.c.opt.Flux = fluxbloques.NouveauFlux(e, so)
	h := m.Unix()/3600*3600 - 3600
	for _, x := range []struct {
		ip, pf string
		n      int
	}{
		{"203.0.113.5", "fw", 10}, {"203.0.113.5", "vps", 3}, // 2 pare-feu
		{"198.51.100.7", "fw", 900},  // très insistante
		{"198.51.100.8", "fw", 20},   // trop peu
		{"192.168.1.20", "fw", 5000}, // LAN
		{"198.51.100.9", "fw", 800},  // exclue par l'utilisateur
		{"198.51.100.23", "fw", 800}, // SSH « connue »
	} {
		if _, err := db.Exec(`INSERT INTO heures(heure, genre, a, b, n) VALUES (?, 'i', ?, ?, ?)`, h, x.ip, x.pf, x.n); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := b.c.db.Exec(`INSERT INTO ssh_connues(prefixe, machine, par, le) VALUES ('198.51.100.23/32', 'web1', 'admin', 0)`); err != nil {
		t.Fatal(err)
	}
	cl, secret := b.premierAdmin("admin", mdpAdmin)
	if _, corps, _ := b.get(cl, "/flux-bloques?vue=reseau"); !strings.Contains(corps, "<h2>Liste de blocage</h2>") || !strings.Contains(corps, "désactivée") {
		t.Fatal("carte « Liste de blocage » absente ou active par défaut")
	}
	reglages := func(code string) {
		v := url.Values{"csrf": {b.csrf(cl, "/flux-bloques?vue=reseau")}, "action": {"reglages"}, "actif": {"1"},
			"min_pare_feux": {"2"}, "min_blocages": {"500"}, "jours": {"7"}, "exclus": {"198.51.100.9"}}
		if code != "" {
			v.Set("code", code)
		}
		b.post(cl, "/flux-bloques/blocage", v)
	}
	reglages(b.code(secret)) // activer : code 2FA (ou mode administration ouvert)
	if r := b.c.lireBlocage(ctx); !r.Actif || r.Exclus != "198.51.100.9" {
		t.Fatalf("réglage : %+v", r)
	}
	_, corps, _ := b.post(cl, "/flux-bloques/blocage", url.Values{"csrf": {b.csrf(cl, "/flux-bloques?vue=reseau")}, "action": {"jeton"}})
	adresse := regexp.MustCompile(`https://[^<\s]+/blocage/[A-Za-z0-9_-]+\.txt`).FindString(corps)
	if adresse == "" {
		t.Fatalf("adresse de lecture non montrée :\n%s", corps)
	}
	if r := b.c.lireBlocage(ctx); strings.Contains(adresse, r.Jeton) || len(r.Jeton) != 64 {
		t.Error("jeton gardé en clair")
	}
	chemin := adresse[strings.Index(adresse, "/blocage/"):]
	// Sans session, et même avec une liste d'adresses autorisées.
	sec := SecuriteDefaut()
	sec.IPs = "192.0.2.0/24"
	b.c.securite.Store(&sec)
	anonyme := b.client()
	code, liste, _ := b.get(anonyme, chemin)
	if code != http.StatusOK || liste != "198.51.100.7\n203.0.113.5\n" {
		t.Fatalf("liste : %d %q", code, liste)
	}
	if code, _, _ := b.get(anonyme, "/blocage/mauvais.txt"); code != http.StatusNotFound {
		t.Errorf("mauvais jeton : %d", code)
	}
	resp, err := anonyme.Get(b.srv.URL + "/blocage/installer.sh")
	if err != nil {
		t.Fatal(err)
	}
	script, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(script), "table inet $TABLE") {
		t.Errorf("script : %d", resp.StatusCode)
	}
	// Désactivée : plus rien.
	b.c.securite.Store(func() *Securite { s := SecuriteDefaut(); return &s }())
	b.post(cl, "/flux-bloques/blocage", url.Values{"csrf": {b.csrf(cl, "/flux-bloques?vue=reseau")}, "action": {"reglages"},
		"min_pare_feux": {"2"}, "min_blocages": {"500"}, "jours": {"7"}})
	if code, _, _ := b.get(anonyme, chemin); code != http.StatusNotFound {
		t.Errorf("liste désactivée encore lisible : %d", code)
	}
}
