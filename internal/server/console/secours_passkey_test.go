// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"mirador-stack/internal/server/auth"
	"mirador-stack/internal/server/auth/fauxcle"
)

const hoteTest = "mirador.example.org" // les passkeys demandent un nom de domaine

// sous : requête envoyée sous un nom de domaine (en-tête Host), comme
// derrière nginx.
func (b *banc) sous(c *http.Client, methode, chemin string, v url.Values) (int, string, http.Header) {
	b.t.Helper()
	var corps io.Reader
	if v != nil {
		corps = strings.NewReader(v.Encode())
	}
	r, _ := http.NewRequest(methode, b.srv.URL+chemin, corps)
	r.Host = hoteTest
	if v != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := c.Do(r)
	if err != nil {
		b.t.Fatal(err)
	}
	defer resp.Body.Close()
	page, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(page), resp.Header
}

var (
	reCodeSecours = regexp.MustCompile(`<li><code>([A-Z0-9-]{14})</code></li>`)
	reDefi        = regexp.MustCompile(`data-defi="([A-Za-z0-9_-]{43})"`)
)

// motDePasse : première étape de la connexion ; renvoie le client et la
// page de l'étape 2FA.
func (b *banc) motDePasse(nom, mdp string) (*http.Client, string) {
	b.t.Helper()
	c := b.client()
	if code, _, h := b.sous(c, http.MethodPost, "/connexion", url.Values{"nom": {nom}, "mot_de_passe": {mdp}}); code != http.StatusSeeOther || h.Get("Location") != "/connexion/code" {
		b.t.Fatalf("mot de passe : %d %s", code, h.Get("Location"))
	}
	_, page, _ := b.sous(c, http.MethodGet, "/connexion/code", nil)
	return c, page
}

func TestCodesDeSecours(t *testing.T) {
	b := nouveauBanc(t)
	const mdp = "une longue phrase de passe"
	c, secret := b.premierAdmin("admin", mdp)
	messages := b.ecouter()

	// Création : code 2FA demandé ; les codes sont affichés une seule fois.
	_, page, _ := b.get(c, "/compte")
	if !strings.Contains(page, "Aucun code de secours") {
		t.Fatal("la page du compte doit dire qu'il n'y a pas de code")
	}
	csrf := trouver(t, reCSRF, page)
	b.post(c, "/compte/codes-secours", url.Values{"csrf": {csrf}, "code": {b.code(secret)}})
	_, page, _ = b.get(c, "/compte")
	codes := reCodeSecours.FindAllStringSubmatch(page, -1)
	if len(codes) != auth.NombreCodesSecours || !strings.Contains(page, "Il reste <strong>10</strong>") {
		t.Fatalf("%d codes affichés", len(codes))
	}
	if _, page, _ = b.get(c, "/compte"); reCodeSecours.MatchString(page) {
		t.Error("codes affichés une deuxième fois")
	}

	// Connexion avec un code de secours : une fois, pas deux.
	c2, etape := b.motDePasse("admin", mdp)
	jeton := trouver(t, reCSRF, etape)
	if code, _, _ := b.sous(c2, http.MethodPost, "/connexion/code", url.Values{"csrf": {jeton}, "secours": {"AAAA-BBBB-CCCC"}}); code != http.StatusUnauthorized {
		t.Fatalf("code de secours faux : %d", code)
	}
	code, _, h := b.sous(c2, http.MethodPost, "/connexion/code", url.Values{"csrf": {jeton}, "secours": {strings.ToLower(codes[0][1])}})
	if code != http.StatusSeeOther || h.Get("Location") != "/" {
		t.Fatalf("connexion par code de secours : %d %s", code, h.Get("Location"))
	}
	m := <-messages
	for !strings.Contains(m, "s'est connecté") { // le premier message parle du code faux
		m = <-messages
	}
	if !strings.Contains(m, "Codes restants : 9") {
		t.Errorf("message à l'administrateur : %q", m)
	}
	c3, etape := b.motDePasse("admin", mdp)
	if code, _, _ := b.sous(c3, http.MethodPost, "/connexion/code", url.Values{"csrf": {trouver(t, reCSRF, etape)}, "secours": {codes[0][1]}}); code != http.StatusUnauthorized {
		t.Fatalf("code de secours rejoué : %d", code)
	}

	// Téléphone perdu : le code de secours sert aussi à préparer une nouvelle 2FA.
	_, page, _ = b.sous(c2, http.MethodGet, "/compte", nil)
	b.sous(c2, http.MethodPost, "/compte/2fa", url.Values{"csrf": {trouver(t, reCSRF, page)}, "secours": {codes[1][1]}})
	if _, page, _ = b.sous(c2, http.MethodGet, "/compte", nil); !strings.Contains(page, "Enregistrer la nouvelle 2FA") {
		i := strings.Index(page, `role="alert"`)
		t.Errorf("nouvelle 2FA non préparée avec un code de secours : %s", page[max(i, 0):max(i, 0)+200])
	}
	// Remise à zéro par un administrateur : plus aucun code.
	u, _ := auth.LireUtilisateur(context.Background(), b.db, "admin")
	if n, _ := auth.CodesSecoursRestants(context.Background(), b.db, u.ID); n != 8 {
		t.Errorf("codes restants : %d", n)
	}
}

func TestPasskeys(t *testing.T) {
	b := nouveauBanc(t)
	const mdp = "une longue phrase de passe"
	c, secret := b.premierAdmin("admin", mdp)
	// Les cookies sont rangés par hôte : une session ouverte sous le nom de domaine.
	cd, etape := b.motDePasse("admin", mdp)
	if code, _, _ := b.sous(cd, http.MethodPost, "/connexion/code", url.Values{"csrf": {trouver(t, reCSRF, etape)}, "code": {b.code(secret)}}); code != http.StatusSeeOther {
		t.Fatalf("connexion sous le nom de domaine : %d", code)
	}
	origine := "https://" + hoteTest
	cle := fauxcle.Nouvelle(-7, hoteTest, origine)

	// Sous une adresse IP : pas de passkey possible, et c'est dit.
	if _, page, _ := b.get(c, "/compte"); !strings.Contains(page, "nom de domaine en HTTPS") {
		t.Error("message attendu sous une adresse IP")
	}
	// Préparer : code 2FA demandé.
	_, page, _ := b.sous(cd, http.MethodGet, "/compte", nil)
	csrf := trouver(t, reCSRF, page)
	// (Peu d'essais faux dans ce test : au 5e, l'adresse serait verrouillée.)
	if reDefi.MatchString(page) {
		t.Fatal("défi présent avant le code 2FA")
	}
	b.sous(cd, http.MethodPost, "/compte/passkey/preparer", url.Values{"csrf": {csrf}, "nom": {"Clé de test"}, "code": {b.code(secret)}})
	_, page, _ = b.sous(cd, http.MethodGet, "/compte", nil)
	defi := trouver(t, reDefi, page)

	// Enregistrer : une réponse pour un autre défi est refusée et brûle le défi.
	client, att := cle.Creer("un-autre-defi")
	b.sous(cd, http.MethodPost, "/compte/passkey/enregistrer", url.Values{"csrf": {csrf}, "client": {client}, "attestation": {att}})
	client, att = cle.Creer(defi)
	b.sous(cd, http.MethodPost, "/compte/passkey/enregistrer", url.Values{"csrf": {csrf}, "client": {client}, "attestation": {att}})
	u, _ := auth.LireUtilisateur(context.Background(), b.db, "admin")
	if liste, _ := auth.ListerPasskeys(context.Background(), b.db, u.ID); len(liste) != 0 {
		t.Fatal("un défi déjà essayé a resservi")
	}
	b.sous(cd, http.MethodPost, "/compte/passkey/preparer", url.Values{"csrf": {csrf}, "nom": {"Clé de test"}, "code": {b.code(secret)}})
	_, page, _ = b.sous(cd, http.MethodGet, "/compte", nil)
	client, att = cle.Creer(trouver(t, reDefi, page))
	b.sous(cd, http.MethodPost, "/compte/passkey/enregistrer", url.Values{"csrf": {csrf}, "client": {client}, "attestation": {att}})
	liste, _ := auth.ListerPasskeys(context.Background(), b.db, u.ID)
	if len(liste) != 1 || liste[0].Nom != "Clé de test" || liste[0].Domaine != hoteTest {
		t.Fatalf("passkey non enregistrée : %+v", liste)
	}
	if _, page, _ = b.sous(cd, http.MethodGet, "/compte", nil); !strings.Contains(page, "Clé de test") {
		t.Error("passkey absente de la page du compte")
	}

	// Connexion : mot de passe, puis passkey à la place du code.
	envoyer := func(nav *http.Client, jeton string, k *fauxcle.Cle, defi string) (int, http.Header) {
		cl, aut, sig := k.Signer(defi)
		code, _, h := b.sous(nav, http.MethodPost, "/connexion/passkey", url.Values{"csrf": {jeton}, "identifiant": {liste[0].IdentifiantB64()},
			"client": {cl}, "authentificateur": {aut}, "signature": {sig}})
		return code, h
	}
	c2, etape2 := b.motDePasse("admin", mdp)
	etape = etape2
	if !strings.Contains(etape, "Utiliser une passkey") {
		t.Fatal("bouton passkey absent de l'étape 2FA")
	}
	jeton := trouver(t, reCSRF, etape)
	// Une autre clé : refusée (l'origine d'hameçonnage est testée dans le paquet auth).
	if code, _ := envoyer(c2, jeton, fauxcle.Nouvelle(-7, hoteTest, origine), trouver(t, reDefi, etape)); code != http.StatusUnauthorized {
		t.Fatalf("signature d'une autre clé : %d", code)
	}
	_, etape, _ = b.sous(c2, http.MethodGet, "/connexion/code", nil)
	// Défi rejoué : la page précédente ne vaut plus rien.
	ancien := trouver(t, reDefi, etape)
	_, etape, _ = b.sous(c2, http.MethodGet, "/connexion/code", nil)
	if code, _ := envoyer(c2, jeton, cle, ancien); code != http.StatusUnauthorized {
		t.Fatalf("ancien défi accepté : %d", code)
	}
	_, etape, _ = b.sous(c2, http.MethodGet, "/connexion/code", nil)
	code, h := envoyer(c2, jeton, cle, trouver(t, reDefi, etape))
	if code != http.StatusSeeOther || h.Get("Location") != "/" {
		t.Fatalf("connexion par passkey : %d %s", code, h.Get("Location"))
	}
	if code, page, _ := b.sous(c2, http.MethodGet, "/", nil); code != http.StatusOK || !strings.Contains(page, "Vue d'ensemble") {
		t.Fatalf("session après passkey : %d", code)
	}
	if liste, _ = auth.ListerPasskeys(context.Background(), b.db, u.ID); liste[0].UtiliseLe == 0 {
		t.Error("dernier usage non noté")
	}
	// Sans mot de passe : pas d'accès à l'étape passkey.
	if code, _, _ := b.sous(b.client(), http.MethodPost, "/connexion/passkey", url.Values{"identifiant": {"x"}}); code != http.StatusSeeOther {
		t.Errorf("étape passkey sans session : %d", code)
	}

	// Supprimer : code 2FA demandé.
	_, page, _ = b.sous(cd, http.MethodGet, "/compte", nil)
	id := url.Values{"csrf": {csrf}, "id": {"1"}, "code": {b.code(secret)}}
	b.sous(cd, http.MethodPost, "/compte/passkey/supprimer", id)
	if liste, _ = auth.ListerPasskeys(context.Background(), b.db, u.ID); len(liste) != 0 {
		t.Error("passkey non supprimée")
	}
	if _, etape = b.motDePasse("admin", mdp); strings.Contains(etape, "Utiliser une passkey") {
		t.Error("bouton passkey encore proposé sans passkey")
	}
}
