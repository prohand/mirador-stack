// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"mirador-stack/internal/server/auth"
)

// connecterConfiance : mot de passe (case « Rester connecté » cochée), code.
func (b *banc) connecterConfiance(nom, mdp, secret string) (*http.Client, string) {
	b.t.Helper()
	c := b.client()
	if code, _, _ := b.post(c, "/connexion", url.Values{"nom": {nom}, "mot_de_passe": {mdp}, "confiance": {"1"}}); code != http.StatusSeeOther {
		b.t.Fatalf("connexion : %d", code)
	}
	_, corps, _ := b.get(c, "/connexion/code")
	code, _, h := b.post(c, "/connexion/code", url.Values{"csrf": {trouver(b.t, reCSRF, corps)}, "code": {b.code(secret)}})
	if code != http.StatusSeeOther {
		b.t.Fatalf("code : %d", code)
	}
	return c, h.Get("Set-Cookie")
}

func (b *banc) nbAppareils() int {
	var n int
	_ = b.db.QueryRow(`SELECT count(*) FROM appareils_confiance`).Scan(&n)
	return n
}

// Appareil de confiance (SPEC § 78) : cookie daté, session reprise après la
// perte de la mémoire (redémarrage), durées, déconnexion, mot de passe.
func TestAppareilConfiance(t *testing.T) {
	b := nouveauBanc(t)
	_, secret := b.premierAdmin("admin", mdpAdmin)

	// Sans la case : cookie de session ordinaire, rien sur disque, perdu au redémarrage.
	ordinaire := b.connecter("admin", mdpAdmin, secret)
	if b.nbAppareils() != 0 {
		t.Fatal("appareil enregistré sans la case")
	}

	c, ck := b.connecterConfiance("admin", mdpAdmin, secret)
	if !strings.Contains(ck, "Max-Age=2592000") || !strings.Contains(ck, "HttpOnly") || !strings.Contains(ck, "SameSite=Strict") {
		t.Fatalf("cookie d'un appareil de confiance : %s", ck)
	}
	if b.nbAppareils() != 1 {
		t.Fatal("appareil de confiance absent de la base")
	}
	// Seule l'empreinte est gardée : la valeur du cookie n'est pas en base.
	var e string
	_ = b.db.QueryRow(`SELECT empreinte FROM appareils_confiance`).Scan(&e)
	valeur := strings.TrimPrefix(strings.SplitN(ck, ";", 2)[0], auth.NomCookie+"=")
	if e == valeur || e != auth.EmpreinteDe(valeur) {
		t.Fatal("la base doit garder l'empreinte, pas l'identifiant")
	}

	// « Redémarrage » : la mémoire des sessions est vidée.
	b.c.sessions = auth.NouveauMagasin()
	if code, _, _ := b.get(c, "/"); code != http.StatusOK {
		t.Fatalf("appareil de confiance après redémarrage : %d", code)
	}
	if code, _, h := b.get(ordinaire, "/"); code != http.StatusSeeOther || h.Get("Location") == "/" {
		t.Fatalf("session ordinaire après redémarrage : %d", code)
	}
	// Formulaires utilisables (nouveau jeton CSRF) et page Sécurité qui le liste.
	_, corps, _ := b.get(c, "/securite")
	if !strings.Contains(corps, "Appareils de confiance (1)") || !strings.Contains(corps, "Cet appareil") {
		t.Fatal("appareil absent de la page Sécurité")
	}

	// Inactivité de plus de 30 min : la session ordinaire mourrait ; ici non.
	b.h.avancer(3 * time.Hour)
	b.c.sessions = auth.NouveauMagasin()
	if code, _, _ := b.get(c, "/"); code != http.StatusOK {
		t.Fatalf("après 3 h : %d", code)
	}
	// 8 jours sans usage : oublié.
	b.h.avancer(8 * 24 * time.Hour)
	if code, _, _ := b.get(c, "/"); code != http.StatusSeeOther {
		t.Fatalf("après 8 jours sans usage : %d", code)
	}
	if b.nbAppareils() != 0 {
		t.Fatal("appareil expiré pas oublié")
	}

	// Déconnexion : l'appareil est oublié.
	c, _ = b.connecterConfiance("admin", mdpAdmin, secret)
	if code, _, _ := b.post(c, "/deconnexion", url.Values{"csrf": {b.csrf(c, "/")}}); code != http.StatusSeeOther || b.nbAppareils() != 0 {
		t.Fatalf("déconnexion : %d, %d appareil(s)", code, b.nbAppareils())
	}

	// Génération du compte changée (mot de passe changé ailleurs) : refusé.
	c, _ = b.connecterConfiance("admin", mdpAdmin, secret)
	if _, err := b.db.Exec(`UPDATE utilisateurs SET generation = generation + 1`); err != nil {
		t.Fatal(err)
	}
	b.c.oublierComptes()
	b.c.sessions = auth.NouveauMagasin()
	if code, _, _ := b.get(c, "/"); code != http.StatusSeeOther {
		t.Fatalf("génération changée : %d", code)
	}

	// Réglage « interdit » : la case est ignorée.
	sec := SecuriteDefaut()
	sec.SansConfiance = true
	b.c.securite.Store(&sec)
	if _, ck := b.connecterConfiance("admin", mdpAdmin, secret); strings.Contains(ck, "Max-Age") || b.nbAppareils() != 0 {
		t.Fatalf("appareil de confiance malgré le réglage : %s", ck)
	}
}

func TestAppareilDe(t *testing.T) {
	for ua, attendu := range map[string]string{
		"Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Mobile/15E148 Safari/604.1": "iPhone · Safari",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:132.0) Gecko/20100101 Firefox/132.0":                                                        "Windows · Firefox",
		"": "Appareil inconnu",
	} {
		if got := auth.AppareilDe(ua); got != attendu {
			t.Errorf("AppareilDe(%q) = %q, attendu %q", ua, got, attendu)
		}
	}
}
