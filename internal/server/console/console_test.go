// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"mirador-stack/internal/server/alertes"
	"mirador-stack/internal/server/base"
	"mirador-stack/internal/server/web"
)

// horloge : heure réglable (les codes TOTP ne servent qu'une fois : il faut
// avancer de 30 s entre deux codes).
type horloge struct {
	mu sync.Mutex
	t  time.Time
}

func (h *horloge) maintenant() time.Time   { h.mu.Lock(); defer h.mu.Unlock(); return h.t }
func (h *horloge) avancer(d time.Duration) { h.mu.Lock(); h.t = h.t.Add(d); h.mu.Unlock() }

// journalPartage : tampon protégé (le serveur de test écrit depuis ses
// propres goroutines).
type journalPartage struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (j *journalPartage) Write(p []byte) (int, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.b.Write(p)
}
func (j *journalPartage) String() string { j.mu.Lock(); defer j.mu.Unlock(); return j.b.String() }

type banc struct {
	t       *testing.T
	db      *sql.DB
	c       *Console
	srv     *httptest.Server
	h       *horloge
	journal *journalPartage
}

var cleTest = bytes.Repeat([]byte{7}, 32)

func nouveauBanc(t *testing.T) *banc {
	t.Helper()
	db, err := base.Ouvrir(filepath.Join(t.TempDir(), "mirador.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := base.Migrer(context.Background(), db, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	b := &banc{t: t, db: db, h: &horloge{t: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)}, journal: &journalPartage{}}
	b.c, err = Nouvelle(context.Background(), Options{DB: db, Cle: cleTest, Version: "test",
		Journal: slog.New(slog.NewJSONHandler(b.journal, nil)), Maintenant: b.h.maintenant})
	if err != nil {
		t.Fatal(err)
	}
	b.srv = httptest.NewTLSServer(web.IPClient(nil)(web.EntetesSecurite(b.c)))
	t.Cleanup(b.srv.Close)
	return b
}

// client : navigateur avec ses cookies ; ne suit pas les redirections
// (pour les vérifier).
func (b *banc) client() *http.Client {
	jar, _ := cookiejar.New(nil)
	c := *b.srv.Client() // copie : chaque « navigateur » a ses propres cookies
	c.Jar = jar
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &c
}

func (b *banc) get(c *http.Client, chemin string) (int, string, http.Header) {
	b.t.Helper()
	resp, err := c.Get(b.srv.URL + chemin)
	if err != nil {
		b.t.Fatal(err)
	}
	defer resp.Body.Close()
	corps, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(corps), resp.Header
}

func (b *banc) post(c *http.Client, chemin string, v url.Values, entetes ...string) (int, string, http.Header) {
	b.t.Helper()
	r, _ := http.NewRequest(http.MethodPost, b.srv.URL+chemin, strings.NewReader(v.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for i := 0; i+1 < len(entetes); i += 2 {
		r.Header.Set(entetes[i], entetes[i+1])
	}
	resp, err := c.Do(r)
	if err != nil {
		b.t.Fatal(err)
	}
	defer resp.Body.Close()
	corps, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(corps), resp.Header
}

var (
	reCSRF   = regexp.MustCompile(`name="csrf" value="([^"]+)"`)
	reSecret = regexp.MustCompile(`<code class="secret">([^<]+)</code>`)
	reJeton  = regexp.MustCompile(`"jeton":"(mirador_premier_[^"]+)"`)
)

func trouver(t *testing.T, re *regexp.Regexp, s string) string {
	t.Helper()
	m := re.FindStringSubmatch(s)
	if m == nil {
		t.Fatalf("motif %s absent de :\n%s", re, s)
	}
	return m[1]
}

func (b *banc) code(secret string) string {
	b.h.avancer(31 * time.Second)
	c, err := codeTOTP(secret, b.h.maintenant().Unix()/pasTOTP)
	if err != nil {
		b.t.Fatal(err)
	}
	return c
}

// premierAdmin : parcours complet de création et de première connexion ;
// renvoie le client connecté et le secret 2FA.
func (b *banc) premierAdmin(nom, mdp string) (*http.Client, string) {
	t := b.t
	c := b.client()
	code, _, h := b.get(c, "/connexion")
	if code != http.StatusSeeOther || h.Get("Location") != "/premier-admin" {
		t.Fatalf("sans compte, /connexion → %d %s", code, h.Get("Location"))
	}
	jeton := trouver(t, reJeton, b.journal.String())
	if code, corps, _ := b.post(c, "/premier-admin", url.Values{"jeton": {"faux"}, "nom": {nom}, "mot_de_passe": {mdp}, "mot_de_passe2": {mdp}}); code != http.StatusUnauthorized {
		t.Fatalf("jeton faux accepté : %d %s", code, corps)
	}
	if code, corps, _ := b.post(c, "/premier-admin", url.Values{"jeton": {jeton}, "nom": {nom}, "mot_de_passe": {mdp}, "mot_de_passe2": {mdp}}); code != http.StatusSeeOther {
		t.Fatalf("création du premier admin : %d %s", code, corps)
	}
	// Jeton à usage unique.
	if code, _, h := b.get(c, "/premier-admin"); code != http.StatusSeeOther || h.Get("Location") != "/connexion" {
		t.Fatalf("/premier-admin encore ouvert : %d", code)
	}
	code, _, h = b.post(c, "/connexion", url.Values{"nom": {nom}, "mot_de_passe": {mdp}})
	if code != http.StatusSeeOther || h.Get("Location") != "/connexion/2fa-creer" {
		t.Fatalf("connexion sans 2FA → %d %s", code, h.Get("Location"))
	}
	// Avant la 2FA, aucune page n'est accessible.
	if code, _, h := b.get(c, "/"); code != http.StatusSeeOther || h.Get("Location") != "/connexion/2fa-creer" {
		t.Fatalf("page avant la 2FA : %d %s", code, h.Get("Location"))
	}
	_, corps, _ := b.get(c, "/connexion/2fa-creer")
	secret := strings.ReplaceAll(trouver(t, reSecret, corps), " ", "")
	if !strings.Contains(corps, "data:image/png;base64,") {
		t.Fatal("QR code absent")
	}
	csrf := trouver(t, reCSRF, corps)
	if code, _, _ := b.post(c, "/connexion/2fa-creer", url.Values{"csrf": {csrf}, "code": {"000000"}}); code != http.StatusUnauthorized {
		t.Fatalf("code faux accepté : %d", code)
	}
	code, _, h = b.post(c, "/connexion/2fa-creer", url.Values{"csrf": {csrf}, "code": {b.code(secret)}})
	if code != http.StatusSeeOther || h.Get("Location") != "/" {
		t.Fatalf("création 2FA : %d %s", code, h.Get("Location"))
	}
	if code, corps, _ := b.get(c, "/"); code != http.StatusOK || !strings.Contains(corps, "Vue d'ensemble") {
		t.Fatalf("accueil après connexion : %d", code)
	}
	return c, secret
}

// connecter : mot de passe puis code.
func (b *banc) connecter(nom, mdp, secret string) *http.Client {
	b.t.Helper()
	c := b.client()
	code, _, h := b.post(c, "/connexion", url.Values{"nom": {nom}, "mot_de_passe": {mdp}})
	if code != http.StatusSeeOther {
		b.t.Fatalf("connexion %s : %d", nom, code)
	}
	if h.Get("Location") == "/connexion/2fa-creer" {
		return c
	}
	_, corps, _ := b.get(c, "/connexion/code")
	code, _, h = b.post(c, "/connexion/code", url.Values{"csrf": {trouver(b.t, reCSRF, corps)}, "code": {b.code(secret)}})
	if code != http.StatusSeeOther || h.Get("Location") != "/" {
		b.t.Fatalf("code 2FA %s : %d %s", nom, code, h.Get("Location"))
	}
	return c
}

func (b *banc) csrf(c *http.Client, chemin string) string {
	_, corps, _ := b.get(c, chemin)
	return trouver(b.t, reCSRF, corps)
}

const mdpAdmin = "une longue phrase de passe"

func TestParcoursConnexion(t *testing.T) {
	b := nouveauBanc(t)
	c, secret := b.premierAdmin("admin", mdpAdmin)

	// Cookie : __Host-, Secure, HttpOnly, SameSite=Strict.
	resp, _ := b.client().Post(b.srv.URL+"/connexion", "application/x-www-form-urlencoded",
		strings.NewReader(url.Values{"nom": {"admin"}, "mot_de_passe": {mdpAdmin}}.Encode()))
	resp.Body.Close()
	ck := resp.Header.Get("Set-Cookie")
	for _, attendu := range []string{"__Host-mirador=", "Path=/", "HttpOnly", "Secure", "SameSite=Strict"} {
		if !strings.Contains(ck, attendu) {
			t.Errorf("cookie sans %s : %s", attendu, ck)
		}
	}

	// En-têtes de sécurité sur une page.
	_, _, h := b.get(c, "/")
	if !strings.Contains(h.Get("Content-Security-Policy"), "script-src 'self'") || h.Get("X-Frame-Options") != "DENY" {
		t.Errorf("en-têtes de sécurité absents : %v", h)
	}

	// Reconnexion : un code déjà utilisé est refusé (anti-rejeu).
	c2 := b.client()
	b.post(c2, "/connexion", url.Values{"nom": {"admin"}, "mot_de_passe": {mdpAdmin}})
	_, corps, _ := b.get(c2, "/connexion/code")
	jeton := trouver(t, reCSRF, corps)
	ok := b.code(secret)
	if code, _, _ := b.post(c2, "/connexion/code", url.Values{"csrf": {jeton}, "code": {ok}}); code != http.StatusSeeOther {
		t.Fatalf("bon code refusé : %d", code)
	}
	c3 := b.client()
	b.post(c3, "/connexion", url.Values{"nom": {"admin"}, "mot_de_passe": {mdpAdmin}})
	_, corps, _ = b.get(c3, "/connexion/code")
	if code, _, _ := b.post(c3, "/connexion/code", url.Values{"csrf": {trouver(t, reCSRF, corps)}, "code": {ok}}); code != http.StatusUnauthorized {
		t.Fatalf("code rejoué accepté : %d", code)
	}

	// Déconnexion.
	if code, _, _ := b.post(c, "/deconnexion", url.Values{"csrf": {b.csrf(c, "/compte")}}); code != http.StatusSeeOther {
		t.Fatalf("déconnexion : %d", code)
	}
	if code, _, h := b.get(c, "/"); code != http.StatusSeeOther || h.Get("Location") != "/connexion" {
		t.Fatalf("après déconnexion : %d", code)
	}
	// Journal lisible par CrowdSec.
	if !strings.Contains(b.journal.String(), `"msg":"console : échec de connexion"`) {
		t.Error("échecs absents du journal")
	}
}

func TestAccesSansSession(t *testing.T) {
	b := nouveauBanc(t)
	b.premierAdmin("admin", mdpAdmin)
	c := b.client()
	for _, p := range []string{"/", "/compte", "/utilisateurs", "/securite", "/n-importe-quoi", "/machines"} {
		if code, _, h := b.get(c, p); code != http.StatusSeeOther || h.Get("Location") != "/connexion" {
			t.Errorf("GET %s sans session : %d %s", p, code, h.Get("Location"))
		}
	}
	for _, p := range []string{"/utilisateurs", "/securite", "/compte/mot-de-passe"} {
		if code, _, _ := b.post(c, p, url.Values{"action": {"creer"}}); code != http.StatusUnauthorized {
			t.Errorf("POST %s sans session : %d", p, code)
		}
	}
	// Faux cookie.
	r, _ := http.NewRequest(http.MethodGet, b.srv.URL+"/", nil)
	r.AddCookie(&http.Cookie{Name: NomCookie, Value: "invente"})
	resp, err := c.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("faux cookie : %d", resp.StatusCode)
	}
	// Fichiers statiques : publics, compressés, gardés en cache.
	r, _ = http.NewRequest(http.MethodGet, b.srv.URL+adresseStatique("mirador.css"), nil)
	r.Header.Set("Accept-Encoding", "gzip")
	resp, err = b.srv.Client().Transport.RoundTrip(r)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Encoding") != "gzip" || !strings.Contains(resp.Header.Get("Cache-Control"), "immutable") {
		t.Errorf("statique : %d %v", resp.StatusCode, resp.Header)
	}
}

func TestCSRF(t *testing.T) {
	b := nouveauBanc(t)
	c, secret := b.premierAdmin("admin", mdpAdmin)
	v := url.Values{"action": {"creer"}, "nom": {"bob"}, "role": {"lecture"}, "mot_de_passe": {"mot de passe de bob"}}
	// Sans jeton.
	if code, _, _ := b.post(c, "/utilisateurs", v); code != http.StatusForbidden {
		t.Errorf("sans jeton CSRF : %d", code)
	}
	// Mauvais jeton.
	v.Set("csrf", "faux")
	if code, _, _ := b.post(c, "/utilisateurs", v); code != http.StatusForbidden {
		t.Errorf("faux jeton CSRF : %d", code)
	}
	// Bon jeton mais requête venue d'un autre site.
	v.Set("csrf", b.csrf(c, "/utilisateurs"))
	v.Set("code", b.code(secret))
	if code, _, _ := b.post(c, "/utilisateurs", v, "Sec-Fetch-Site", "cross-site"); code != http.StatusForbidden {
		t.Errorf("autre site : %d", code)
	}
	if code, _, _ := b.post(c, "/utilisateurs", v, "Origin", "https://pirate.example"); code != http.StatusForbidden {
		t.Errorf("autre origine : %d", code)
	}
	// Tout bon : accepté.
	if code, _, _ := b.post(c, "/utilisateurs", v); code != http.StatusSeeOther {
		t.Errorf("création correcte refusée : %d", code)
	}
	if _, err := LireUtilisateur(context.Background(), b.db, "bob"); err != nil {
		t.Error("bob non créé")
	}
}

func TestRolesEtCodeSensible(t *testing.T) {
	b := nouveauBanc(t)
	c, secret := b.premierAdmin("admin", mdpAdmin)
	// Sans code 2FA : refusé (action sensible).
	v := url.Values{"csrf": {b.csrf(c, "/utilisateurs")}, "action": {"creer"}, "nom": {"lea"}, "role": {"lecture"}, "mot_de_passe": {"mot de passe de lea"}}
	b.post(c, "/utilisateurs", v)
	if _, err := LireUtilisateur(context.Background(), b.db, "lea"); err == nil {
		t.Fatal("compte créé sans code 2FA")
	}
	v.Set("code", b.code(secret))
	b.post(c, "/utilisateurs", v)
	if _, err := LireUtilisateur(context.Background(), b.db, "lea"); err != nil {
		t.Fatal("compte non créé avec le code")
	}
	// lea se connecte (2FA à créer), puis n'a que la lecture.
	cl := b.client()
	b.post(cl, "/connexion", url.Values{"nom": {"lea"}, "mot_de_passe": {"mot de passe de lea"}})
	_, corps, _ := b.get(cl, "/connexion/2fa-creer")
	sl := strings.ReplaceAll(trouver(t, reSecret, corps), " ", "")
	b.post(cl, "/connexion/2fa-creer", url.Values{"csrf": {trouver(t, reCSRF, corps)}, "code": {b.code(sl)}})
	if code, _, _ := b.get(cl, "/compte"); code != http.StatusOK {
		t.Fatalf("lecture : /compte → %d", code)
	}
	for _, p := range []string{"/utilisateurs", "/securite"} {
		if code, _, _ := b.get(cl, p); code != http.StatusForbidden {
			t.Errorf("lecture : GET %s → %d", p, code)
		}
	}
	if code, _, _ := b.post(cl, "/utilisateurs", url.Values{"csrf": {b.csrf(cl, "/compte")}, "action": {"supprimer"}, "nom": {"admin"}}); code != http.StatusForbidden {
		t.Errorf("lecture : POST /utilisateurs → %d", code)
	}
	// Le dernier admin ne peut pas être rétrogradé ni supprimé.
	err := DefinirRole(context.Background(), b.db, "admin", RoleLecture, "test", "", b.h.maintenant())
	if err == nil {
		t.Error("dernier admin rétrogradé")
	}
	// Désactiver lea ferme sa session.
	b.post(c, "/utilisateurs", url.Values{"csrf": {b.csrf(c, "/utilisateurs")}, "action": {"desactiver"}, "nom": {"lea"}, "code": {b.code(secret)}})
	if code, _, _ := b.get(cl, "/compte"); code != http.StatusSeeOther {
		t.Errorf("compte désactivé encore connecté : %d", code)
	}
}

func TestVerrouillage(t *testing.T) {
	b := nouveauBanc(t)
	b.premierAdmin("admin", mdpAdmin)
	// Les échecs voulus du parcours ci-dessus comptent encore pour l'adresse
	// (une réussite n'efface plus le compteur de l'adresse) : on les laisse
	// passer la fenêtre de 15 min.
	b.h.avancer(16 * time.Minute)
	c := b.client()
	for i := 0; i < 5; i++ {
		if code, _, _ := b.post(c, "/connexion", url.Values{"nom": {"admin"}, "mot_de_passe": {"faux faux faux"}}); code != http.StatusUnauthorized {
			t.Fatalf("essai %d : %d", i, code)
		}
	}
	// Adresse verrouillée : même le bon mot de passe est refusé.
	if code, _, _ := b.post(c, "/connexion", url.Values{"nom": {"admin"}, "mot_de_passe": {mdpAdmin}}); code != http.StatusTooManyRequests {
		t.Fatalf("après 5 échecs : %d", code)
	}
	b.h.avancer(16 * time.Minute)
	if code, _, _ := b.post(c, "/connexion", url.Values{"nom": {"admin"}, "mot_de_passe": {mdpAdmin}}); code != http.StatusSeeOther {
		t.Fatalf("après le verrouillage : %d", code)
	}
	// Compte inconnu : même message que mot de passe faux.
	_, corps, _ := b.post(b.client(), "/connexion", url.Values{"nom": {"personne"}, "mot_de_passe": {"faux faux faux"}})
	if !strings.Contains(corps, messageEchec) {
		t.Error("message différent pour un compte inconnu")
	}
	if !strings.Contains(b.journal.String(), "adresse verrouillée") {
		t.Error("verrouillage absent du journal")
	}
}

func TestExpirationSession(t *testing.T) {
	b := nouveauBanc(t)
	c, _ := b.premierAdmin("admin", mdpAdmin)
	b.h.avancer(29 * time.Minute)
	if code, _, _ := b.get(c, "/"); code != http.StatusOK {
		t.Fatalf("session active : %d", code)
	}
	b.h.avancer(31 * time.Minute)
	if code, _, _ := b.get(c, "/"); code != http.StatusSeeOther {
		t.Fatalf("session inactive 31 min encore ouverte : %d", code)
	}
}

func TestListeIPAConfirmer(t *testing.T) {
	b := nouveauBanc(t)
	c, secret := b.premierAdmin("admin", mdpAdmin)
	form := func(ips string) url.Values {
		return url.Values{"csrf": {b.csrf(c, "/securite")}, "inactivite": {"30m"}, "duree_max": {"12h"}, "echecs": {"5"},
			"verrou": {"15m"}, "ips": {ips}, "code": {b.code(secret)}}
	}
	// Une liste qui m'exclut est refusée tout de suite.
	b.post(c, "/securite", form("203.0.113.0/24"))
	if s, _ := LireSecurite(context.Background(), b.db); s.IPs != "" {
		t.Fatalf("liste qui m'enferme acceptée : %q", s.IPs)
	}
	// Liste qui m'inclut : appliquée, à confirmer.
	b.post(c, "/securite", form("127.0.0.1, 203.0.113.0/24"))
	if s, _ := LireSecurite(context.Background(), b.db); s.IPs == "" {
		t.Fatal("liste non enregistrée")
	}
	ac, _ := ListerAConfirmer(context.Background(), b.db)
	if len(ac) != 1 {
		t.Fatalf("à confirmer : %v", ac)
	}
	// Sans confirmation : l'ancienne liste revient après 2 min.
	b.h.avancer(3 * time.Minute)
	b.c.Tour(context.Background())
	if s, _ := LireSecurite(context.Background(), b.db); s.IPs != "" {
		t.Fatalf("liste non annulée : %q", s.IPs)
	}
	// Avec confirmation : gardée.
	b.post(c, "/securite", form("127.0.0.1"))
	ac, _ = ListerAConfirmer(context.Background(), b.db)
	_, corps, _ := b.get(c, "/")
	if !strings.Contains(corps, "à confirmer avant") {
		t.Error("bandeau de confirmation absent")
	}
	b.post(c, "/confirmer", url.Values{"csrf": {b.csrf(c, "/")}, "id": {itoa(ac[0].ID)}})
	b.h.avancer(3 * time.Minute)
	b.c.Tour(context.Background())
	if s, _ := LireSecurite(context.Background(), b.db); s.IPs != "127.0.0.1" {
		t.Fatalf("liste confirmée perdue : %q", s.IPs)
	}
	// Une adresse hors liste est refusée partout (même la connexion).
	s, _ := LireSecurite(context.Background(), b.db)
	s.IPs = "203.0.113.9"
	b.c.securite.Store(&s)
	if code, _, _ := b.get(b.client(), "/connexion"); code != http.StatusForbidden {
		t.Errorf("adresse hors liste : %d", code)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestMotDePasse(t *testing.T) {
	e, err := Empreinte("douze caractères")
	if err != nil {
		t.Fatal(err)
	}
	if !Comparer(e, "douze caractères") || Comparer(e, "douze caracteres") {
		t.Error("comparaison fausse")
	}
	if _, err := Empreinte("court"); err == nil {
		t.Error("mot de passe court accepté")
	}
	// Empreinte abîmée : refus sans calcul démesuré.
	if Comparer("$argon2id$v=19$m=99999999,t=1,p=1$AAAA$AAAA", "x") {
		t.Error("empreinte abîmée acceptée")
	}
}

func TestTOTP(t *testing.T) {
	// Vecteur de la RFC 6238 (SHA-1, T = 59 s → 94287082, 6 derniers chiffres).
	secret := b32.EncodeToString([]byte("12345678901234567890"))
	if c, _ := codeTOTP(secret, 59/30); c != "287082" {
		t.Fatalf("RFC 6238 : %s", c)
	}
	t0 := time.Unix(59, 0)
	pas, ok := VerifierTOTP(secret, "287 082", t0, 0)
	if !ok || pas != 1 {
		t.Fatal("code juste refusé")
	}
	if _, ok := VerifierTOTP(secret, "287082", t0, pas); ok {
		t.Error("code rejoué accepté")
	}
	if _, ok := VerifierTOTP(secret, "287082", t0.Add(5*time.Minute), 0); ok {
		t.Error("code trop vieux accepté")
	}
	if !strings.HasPrefix(URITOTP("admin", secret), "otpauth://totp/mirador:admin?") {
		t.Error("URI otpauth")
	}
}

func TestRefererLocal(t *testing.T) {
	for _, c := range []struct{ ref, attendu string }{
		{"https://console.example/machines?x=1", "/machines?x=1"},
		{"https://pirate.example//pirate.example", "/"},
		{"", "/"},
		{"javascript:alert(1)", "/"},
	} {
		r := httptest.NewRequest(http.MethodPost, "/", nil)
		r.Header.Set("Referer", c.ref)
		if got := refererLocal(r); got != c.attendu {
			t.Errorf("%q → %q, attendu %q", c.ref, got, c.attendu)
		}
	}
}

// TestPasDeBlocageBase : la base n'a qu'une connexion ; une lecture faite
// pendant une transaction ouverte bloquerait toute l'interface (défaut
// trouvé le 10/11 sur « Réglages › Interface » et « nom déjà pris »).
func TestPasDeBlocageBase(t *testing.T) {
	b := nouveauBanc(t)
	c, secret := b.premierAdmin("admin", mdpAdmin)
	c.Timeout = 10 * time.Second
	code, _, _ := b.post(c, "/reglages/interface", url.Values{"csrf": {b.csrf(c, "/compte")}, "fuseau": {"Europe/Paris"},
		"theme": {"sombre"}, "menu": {"haut"}, "menu_replie": {"1"}})
	if code != http.StatusSeeOther {
		t.Fatalf("réglages de l'interface : %d", code)
	}
	if r := lireInterface(context.Background(), b.db); r.Menu != "haut" || !r.MenuReplie || r.Theme != "sombre" {
		t.Errorf("réglages non enregistrés : %+v", r)
	}
	_, corps, _ := b.get(c, "/")
	if !strings.Contains(corps, `data-theme="sombre"`) || !strings.Contains(corps, "menu-haut replie") {
		t.Error("thème ou disposition du menu non appliqués à la page")
	}
	// Création d'un compte dont le nom existe déjà : refus, sans blocage.
	b.post(c, "/utilisateurs", url.Values{"csrf": {b.csrf(c, "/utilisateurs")}, "action": {"creer"}, "nom": {"admin"},
		"role": {"lecture"}, "mot_de_passe": {"un autre mot de passe"}, "code": {b.code(secret)}})
	if code, _, _ := b.get(c, "/utilisateurs"); code != http.StatusOK {
		t.Fatalf("interface bloquée après un nom en double : %d", code)
	}
}

// Adresse d'ingestion : https://hôte[:port] seul, rien d'autre.
func TestNormaliserIngestion(t *testing.T) {
	bons := map[string]string{
		"":                                "",
		"mirador-ingest.exemple.fr":       "https://mirador-ingest.exemple.fr",
		"https://ingest.exemple.fr/":      "https://ingest.exemple.fr",
		" https://ingest.exemple.fr:8443": "https://ingest.exemple.fr:8443",
	}
	for e, v := range bons {
		if n, err := normaliserIngestion(e); err != nil || n != v {
			t.Errorf("%q : %q, %v (attendu %q)", e, n, err, v)
		}
	}
	for _, e := range []string{"http://ingest.exemple.fr", "https://a@ingest.exemple.fr", "https://ingest.exemple.fr/chemin", "https://ingest.exemple.fr/?x=1", "https://"} {
		if _, err := normaliserIngestion(e); err == nil {
			t.Errorf("%q accepté", e)
		}
	}
	if majusculeDebut("éteint") != "Éteint" || majusculeDebut("") != "" || tailleLisible(21_000_000_000) != "19,6 Go" {
		t.Error("majuscule ou taille lisible fausse")
	}
}

// Conservation : ancien réglage sans audit / historique → défauts ; bornes ;
// réduction de l'audit = réduction (attente + confirmation) ; purge.
func TestConservationAudit(t *testing.T) {
	b := nouveauBanc(t)
	ctx := context.Background()
	_, _ = b.db.Exec(`INSERT INTO reglages(cle, valeur, modifie_le, modifie_par) VALUES ('conservation', '{"brut":"7d","agrege":"30d","logs":"30d","max_brut":0,"max_agrege":0,"max_logs":0}', 1, 'x')`)
	c, err := LireConservation(ctx, b.db)
	if err != nil || c.Audit != auditDefaut || c.Historique != historiqueDefaut {
		t.Fatalf("ancien réglage : %+v %v", c, err)
	}
	n := c
	n.Audit = alertes.Duree(30 * 24 * time.Hour)
	if n.Valider() == nil {
		t.Error("audit de 30 j accepté (90 j au moins)")
	}
	n.Audit = alertes.Duree(100 * 24 * time.Hour)
	if !c.reduit(n) {
		t.Error("réduction de l'audit non vue comme une réduction")
	}
	now := time.Now()
	_ = Auditer(ctx, b.db, "x", "vieux", "", "", "", "", now.Add(-400*24*time.Hour))
	_ = Auditer(ctx, b.db, "x", "recent", "", "", "", "", now)
	if n, err := PurgerAudit(ctx, b.db, now.Add(-time.Duration(c.Audit))); err != nil || n != 1 {
		t.Fatalf("purge : %d %v", n, err)
	}
}
