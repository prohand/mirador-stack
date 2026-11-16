// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"mirador-stack/internal/server/notif"
)

// Notifications push (SPEC § 35) : service worker public et sans cache,
// abonnement d'un appareil depuis « Mon compte », essai, retrait.

func TestServiceWorker(t *testing.T) {
	b := nouveauBanc(t)
	b.premierAdmin("admin", mdpAdmin)
	code, corps, h := b.get(b.client(), "/sw.js") // sans session
	if code != http.StatusOK || !strings.HasPrefix(h.Get("Content-Type"), "text/javascript") || h.Get("Cache-Control") != "no-cache" ||
		h.Get("Service-Worker-Allowed") != "/" {
		t.Fatalf("/sw.js : %d %v", code, h)
	}
	// Notifications seulement : il n'intercepte aucune requête et ne garde
	// rien en cache (aucune page de l'interface ne reste sur l'appareil).
	for _, interdit := range []string{`addEventListener("fetch"`, "caches.", "importScripts"} {
		if strings.Contains(corps, interdit) {
			t.Errorf("sw.js contient %q", interdit)
		}
	}
	for _, attendu := range []string{`addEventListener("push"`, `addEventListener("notificationclick"`, "setAppBadge"} {
		if !strings.Contains(corps, attendu) {
			t.Errorf("sw.js : %q absent", attendu)
		}
	}
	if code, _, _ := b.post(b.client(), "/sw.js", url.Values{}); code == http.StatusOK {
		t.Error("POST /sw.js accepté")
	}
}

// fauxPush : service de push simulé pour tout le test.
type fauxPush struct{ appels atomic.Int64 }

func (f *fauxPush) RoundTrip(r *http.Request) (*http.Response, error) {
	f.appels.Add(1)
	_, _ = io.Copy(io.Discard, r.Body)
	return &http.Response{StatusCode: http.StatusCreated, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}}, nil
}

func TestAbonnementPushDepuisLeCompte(t *testing.T) {
	b := nouveauBanc(t)
	c, secret := b.premierAdmin("admin", mdpAdmin)
	ctx := context.Background()
	faux := &fauxPush{}
	avant := notif.ClientPush
	notif.ClientPush = func() *http.Client { return &http.Client{Transport: faux} }
	t.Cleanup(func() { notif.ClientPush = avant })

	// La page donne la clé publique du serveur et prévient que le canal est coupé.
	_, corps, _ := b.get(c, "/compte")
	cle := trouver(t, reClePush, corps)
	if brut, err := base64.RawURLEncoding.DecodeString(cle); err != nil || len(brut) != 65 {
		t.Fatalf("clé publique des notifications : %q", cle)
	}
	if !strings.Contains(corps, "Le canal « Application » n'est pas actif") || !strings.Contains(corps, `data-nb-alertes="0"`) {
		t.Error("avertissement « canal non actif » ou attribut data-nb-alertes absent")
	}
	appareil, _ := ecdh.P256().GenerateKey(rand.Reader)
	auth := make([]byte, 16)
	_, _ = rand.Read(auth)
	abonnement := func(adresse string) url.Values {
		return url.Values{"csrf": {b.csrf(c, "/compte")}, "adresse": {adresse},
			"p256dh": {base64.RawURLEncoding.EncodeToString(appareil.PublicKey().Bytes())}, "auth": {base64.RawURLEncoding.EncodeToString(auth)}}
	}
	nombre := func() int {
		l, _ := notif.ListerAppareilsPush(ctx, b.db, 1)
		return len(l)
	}
	entetes := []string{"User-Agent", "Mozilla/5.0 (Linux; Android 14) AppleWebKit/537.36 Chrome/130.0 Mobile Safari/537.36"}

	// Mode administration fermé, sans code : refusé (une session volée ne
	// suffit pas à faire partir les alertes vers un autre téléphone).
	b.quitterModeAdmin(c)
	b.post(c, "/compte/push/abonner", abonnement("https://fcm.googleapis.com/fcm/send/abc"), entetes...)
	if nombre() != 0 {
		t.Fatal("appareil abonné sans code 2FA")
	}
	// Avec le code : abonné ; adresse d'un service inconnu : refusée.
	v := abonnement("https://fcm.googleapis.com/fcm/send/abc")
	v.Set("code", b.code(secret))
	b.post(c, "/compte/push/abonner", v, entetes...)
	if nombre() != 1 {
		t.Fatal("appareil non abonné avec le code")
	}
	b.post(c, "/compte/push/abonner", abonnement("https://127.0.0.1:9441/x"), entetes...)
	b.post(c, "/compte/push/abonner", abonnement("https://attaquant.example/x"), entetes...)
	if nombre() != 1 {
		t.Fatal("adresse hors des services de push acceptée")
	}
	_, corps, _ = b.get(c, "/compte")
	if !strings.Contains(corps, "Chrome sur Android") || !strings.Contains(corps, "fcm.googleapis.com") || strings.Contains(corps, "/fcm/send/abc") {
		t.Error("liste des appareils : nom ou service absent, ou adresse de l'abonnement affichée")
	}
	// Essai : une notification part ; pas deux coup sur coup.
	csrf := b.csrf(c, "/compte")
	b.post(c, "/compte/push/tester", url.Values{"csrf": {csrf}})
	b.post(c, "/compte/push/tester", url.Values{"csrf": {csrf}})
	if n := faux.appels.Load(); n != 1 {
		t.Fatalf("%d notification(s) d'essai parties, 1 attendue", n)
	}
	// Retrait.
	l, _ := notif.ListerAppareilsPush(ctx, b.db, 1)
	b.post(c, "/compte/push/retirer", url.Values{"csrf": {csrf}, "id": {itoa(l[0].ID)}})
	if nombre() != 0 {
		t.Fatal("appareil non retiré")
	}
	// Audit : abonnement et retrait notés, sans l'adresse de l'abonnement.
	var n int
	_ = b.db.QueryRow(`SELECT count(*) FROM audit WHERE action IN ('push_abonner', 'push_retirer') AND apres NOT LIKE '%fcm/send%' AND avant NOT LIKE '%fcm/send%'`).Scan(&n)
	if n != 2 {
		t.Errorf("%d ligne(s) d'audit, 2 attendues", n)
	}
}

func TestNomAppareil(t *testing.T) {
	for ua, voulu := range map[string]string{
		"Mozilla/5.0 (iPhone; CPU iPhone OS 18_1 like Mac OS X) AppleWebKit/605.1.15 Mobile/15E148 Safari/604.1": "iPhone",
		"Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 Chrome/130.0.0.0 Mobile Safari/537.36":      "Chrome sur Android",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:132.0) Gecko/20100101 Firefox/132.0":                       "Firefox sur Windows",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 Version/18.1 Safari/605.1.15":      "Safari sur Mac",
		"Mozilla/5.0 (Windows NT 10.0) AppleWebKit/537.36 Chrome/130.0.0.0 Safari/537.36 Edg/130.0.0.0":          "Edge sur Windows",
		"": "Navigateur",
	} {
		if g := nomAppareil(ua); g != voulu {
			t.Errorf("nomAppareil(%q) = %q, attendu %q", ua, g, voulu)
		}
	}
}

// Régression (16/11) : <body data-alertes> était pris pour une liste
// d'alertes par mirador.js, qui plantait sur le premier tableau de la page
// (plus de cartes sur téléphone, plus de bouton « Copier »). Seules les
// listes d'alertes portent data-alertes.
func TestBodySansDataAlertes(t *testing.T) {
	b := nouveauBanc(t)
	c, _ := b.premierAdmin("admin", mdpAdmin)
	_, corps, _ := b.get(c, "/machines")
	if i := strings.Index(corps, "<body"); i < 0 || strings.Contains(corps[i:i+strings.Index(corps[i:], ">")], "data-alertes=") {
		t.Error("<body> porte encore data-alertes")
	}
}
