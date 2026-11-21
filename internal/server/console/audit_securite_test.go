// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"bytes"
	"context"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"mirador-stack/internal/server/alertes"
	"mirador-stack/internal/server/auth"
	"mirador-stack/internal/server/notif"
)

// Tests des corrections de l'audit du 16/11 (SPEC § 34).

// quitterModeAdmin : ferme le mode administration ouvert à la connexion.
func (b *banc) quitterModeAdmin(c *http.Client) {
	b.t.Helper()
	if code, _, _ := b.post(c, "/mode-admin/quitter", url.Values{"csrf": {b.csrf(c, "/compte")}}); code != http.StatusSeeOther {
		b.t.Fatalf("quitter le mode administration : %d", code)
	}
}

// postFichier : formulaire avec un fichier (import).
func (b *banc) postFichier(c *http.Client, chemin string, champs url.Values, fichier string) (int, string) {
	b.t.Helper()
	var corps bytes.Buffer
	w := multipart.NewWriter(&corps)
	for k, vs := range champs {
		for _, v := range vs {
			_ = w.WriteField(k, v)
		}
	}
	f, _ := w.CreateFormFile("fichier", "export.yaml")
	_, _ = f.Write([]byte(fichier))
	_ = w.Close()
	r, _ := http.NewRequest(http.MethodPost, b.srv.URL+chemin, &corps)
	r.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := c.Do(r)
	if err != nil {
		b.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out bytes.Buffer
	_, _ = out.ReadFrom(resp.Body)
	return resp.StatusCode, out.String()
}

// L'import de la partie « securite » exige un NOUVEAU code 2FA, même en
// mode administration (comme la page Sécurité).
func TestImportSecuriteCodeFrais(t *testing.T) {
	b := nouveauBanc(t)
	c, secret := b.premierAdmin("admin", mdpAdmin)
	// Export sans secret, puis réglages de sécurité relâchés dans le fichier.
	_, yaml, _ := b.get(c, "/reglages/export.yaml")
	if !strings.Contains(yaml, "duree_max: 12h") {
		t.Fatalf("export inattendu :\n%s", yaml)
	}
	relache := strings.Replace(yaml, "duree_max: 12h", "duree_max: 7d", 1)
	champs := url.Values{"csrf": {b.csrf(c, "/reglages/export")}, "parties": {"securite"}}
	// Mode administration ouvert (connexion récente), sans code : refusé.
	b.postFichier(c, "/reglages/importer", champs, relache)
	if s, _ := LireSecurite(context.Background(), b.db); time.Duration(s.DureeMax) != 12*time.Hour {
		t.Fatalf("sécurité changée par import sans nouveau code : durée max %v", time.Duration(s.DureeMax))
	}
	// Avec un code : appliqué.
	champs.Set("code", b.code(secret))
	b.postFichier(c, "/reglages/importer", champs, relache)
	if s, _ := LireSecurite(context.Background(), b.db); time.Duration(s.DureeMax) != 7*24*time.Hour {
		t.Fatalf("import avec code non appliqué : durée max %v", time.Duration(s.DureeMax))
	}
}

// Actions qui demandent maintenant le code 2FA ou le mode administration.
func TestActionsProtegees(t *testing.T) {
	b := nouveauBanc(t)
	c, secret := b.premierAdmin("admin", mdpAdmin)
	ctx := context.Background()
	regle := alertes.Regle{Nom: "RegleTest", Groupe: "essai", Intervalle: alertes.Duree(time.Minute), Expr: "up == 0",
		Gravite: alertes.Critique, Active: true}
	if _, err := alertes.Enregistrer(ctx, b.db, []alertes.Regle{regle}, "test", b.h.maintenant()); err != nil {
		t.Fatal(err)
	}
	ingestion := func() string { return lireInterface(ctx, b.db).Ingestion }
	regleActive := func() bool {
		rs, _ := alertes.ListerRegles(ctx, b.db)
		for _, r := range rs {
			if r.Nom == "RegleTest" {
				return r.Active
			}
		}
		t.Fatal("règle de test absente")
		return false
	}
	silences := func() int {
		s, _ := notif.ListerSilences(ctx, b.db, true, b.h.maintenant())
		return len(s)
	}
	maintenances := func() int {
		m, _ := notif.ListerMaintenances(ctx, b.db)
		return len(m)
	}
	csrf := b.csrf(c, "/compte")

	// Mode administration fermé, aucun code : tout est refusé.
	b.quitterModeAdmin(c)
	csrf = b.csrf(c, "/compte")
	b.post(c, "/reglages/interface", url.Values{"csrf": {csrf}, "action": {"adresse"}, "ingestion": {"https://attaquant.example"}})
	if ingestion() != "" {
		t.Errorf("adresse d'ingestion changée sans code : %q", ingestion())
	}
	b.post(c, "/reglages/interface", url.Values{"csrf": {csrf}, "action": {"general"}, "fuseau": {"Europe/Paris"}, "ingestion": {"https://attaquant.example"}})
	if ingestion() != "" {
		t.Errorf("adresse d'ingestion changée sans code (bloc Général) : %q", ingestion())
	}
	b.post(c, "/regles/action", url.Values{"csrf": {csrf}, "action": {"desactiver"}, "nom": {"RegleTest"}})
	if !regleActive() {
		t.Error("règle désactivée sans code")
	}
	b.post(c, "/sourdine", url.Values{"csrf": {csrf}, "action": {"maintenance"}, "cible": {`{instance="pve1"}`}, "duree": {"2h"}})
	if maintenances() != 0 {
		t.Error("maintenance posée sans code")
	}
	b.post(c, "/sourdine", url.Values{"csrf": {csrf}, "action": {"silence"}, "selecteur": {`{instance="pve1"}`}, "duree": {"48h"}})
	if silences() != 0 {
		t.Error("silence de 48 h posé sans code")
	}
	b.post(c, "/reglages/notifications", url.Values{"csrf": {csrf}, "action": {"vmalert"}, "url": {"http://127.0.0.1:9"}})
	if v, _ := alertes.LireReglagesVmalert(ctx, b.db); v.URL != "" {
		t.Errorf("adresse vmalert changée sans code : %q", v.URL)
	}
	// Export avec les secrets : plus par un simple lien, ni sans code.
	if code, corps, _ := b.get(c, "/reglages/export.yaml?secrets=chiffres"); code != http.StatusSeeOther || strings.Contains(corps, "version:") {
		t.Errorf("export avec secrets par un lien : %d", code)
	}
	if code, corps, _ := b.post(c, "/reglages/export.yaml", url.Values{"csrf": {csrf}}); code != http.StatusSeeOther || strings.Contains(corps, "version:") {
		t.Errorf("export avec secrets sans code : %d", code)
	}
	// Le message dit quoi faire (rien n'est compté comme un essai manqué).
	if _, corps, _ := b.get(c, "/reglages/export"); !strings.Contains(corps, "code 2FA demandé") {
		t.Error("message « code 2FA demandé » absent")
	}

	// Un silence court reste possible sans code (geste courant).
	b.post(c, "/sourdine", url.Values{"csrf": {csrf}, "action": {"silence"}, "selecteur": {`{instance="pve1"}`}, "duree": {"2h"}})
	if silences() != 1 {
		t.Error("silence de 2 h refusé sans code")
	}

	// Avec un code : l'action passe et le mode administration s'ouvre.
	b.post(c, "/regles/action", url.Values{"csrf": {csrf}, "action": {"desactiver"}, "nom": {"RegleTest"}, "code": {b.code(secret)}})
	if regleActive() {
		t.Error("règle non désactivée avec le code")
	}
	// Mode ouvert : plus de code pour la suite.
	b.post(c, "/reglages/interface", url.Values{"csrf": {csrf}, "action": {"adresse"}, "ingestion": {"https://ingest.exemple.fr"}})
	if ingestion() != "https://ingest.exemple.fr" {
		t.Errorf("adresse d'ingestion en mode administration : %q", ingestion())
	}
	if code, corps, h := b.post(c, "/reglages/export.yaml", url.Values{"csrf": {csrf}}); code != http.StatusOK || !strings.Contains(corps, "version:") ||
		!strings.Contains(h.Get("Content-Disposition"), "mirador-configuration") {
		t.Errorf("export avec secrets en mode administration : %d", code)
	}
	// Réactiver une règle ne demande jamais de code (plus de surveillance).
	b.quitterModeAdmin(c)
	b.post(c, "/regles/action", url.Values{"csrf": {b.csrf(c, "/compte")}, "action": {"activer"}, "nom": {"RegleTest"}})
	if !regleActive() {
		t.Error("règle non réactivée")
	}
}

// Ouverture du mode administration depuis « Mon compte ».
func TestOuvrirModeAdmin(t *testing.T) {
	b := nouveauBanc(t)
	c, secret := b.premierAdmin("admin", mdpAdmin)
	b.quitterModeAdmin(c)
	_, corps, _ := b.get(c, "/compte")
	if !strings.Contains(corps, `action="/mode-admin/ouvrir"`) || strings.Contains(corps, "data-mode-admin") {
		t.Fatal("formulaire d'ouverture absent, ou mode encore ouvert")
	}
	csrf := trouver(t, reCSRF, corps)
	b.post(c, "/mode-admin/ouvrir", url.Values{"csrf": {csrf}, "code": {"000000"}, "retour": {"/compte"}})
	if _, corps, _ := b.get(c, "/compte"); strings.Contains(corps, "data-mode-admin") {
		t.Fatal("mode ouvert avec un code faux")
	}
	code, _, h := b.post(c, "/mode-admin/ouvrir", url.Values{"csrf": {csrf}, "code": {b.code(secret)}, "retour": {"https://ailleurs.example/"}})
	if code != http.StatusSeeOther || h.Get("Location") != "/" {
		t.Fatalf("retour hors du site accepté : %d %s", code, h.Get("Location"))
	}
	if _, corps, _ := b.get(c, "/compte"); !strings.Contains(corps, "data-mode-admin") {
		t.Fatal("mode non ouvert avec un code juste")
	}
}

// Un compte lecture est refusé AVANT la lecture du corps (pas de jeton CSRF
// à chercher dans 31 Mio).
func TestRoleAvantCorps(t *testing.T) {
	b := nouveauBanc(t)
	b.premierAdmin("admin", mdpAdmin)
	ctx := context.Background()
	if err := auth.CreerUtilisateur(ctx, b.db, "lea", auth.RoleLecture, "mot de passe de lea", "test", "", b.h.maintenant()); err != nil {
		t.Fatal(err)
	}
	cl := b.client()
	b.post(cl, "/connexion", url.Values{"nom": {"lea"}, "mot_de_passe": {"mot de passe de lea"}})
	_, corps, _ := b.get(cl, "/connexion/2fa-creer")
	sl := strings.ReplaceAll(trouver(t, reSecret, corps), " ", "")
	b.post(cl, "/connexion/2fa-creer", url.Values{"csrf": {trouver(t, reCSRF, corps)}, "code": {b.code(sl)}})
	// Sans jeton CSRF : la réponse est celle du rôle, pas celle du jeton.
	code, corps, _ := b.post(cl, "/mises-a-jour/publier", url.Values{"x": {strings.Repeat("a", 4096)}})
	if code != http.StatusForbidden || !strings.Contains(corps, "rôle admin") {
		t.Fatalf("compte lecture : %d %q (rôle attendu avant le jeton)", code, corps)
	}
	// Rôle lecture : le sujet ntfy n'est pas montré.
	r, _ := notif.LireReglages(ctx, b.db)
	r.Canaux.Ntfy = &notif.Ntfy{Actif: false, URL: "https://ntfy.exemple.fr/sujet-tres-secret"}
	if err := notif.EcrireReglages(ctx, b.db, r, cleTest, "test", b.h.maintenant()); err != nil {
		t.Fatal(err)
	}
	if _, corps, _ := b.get(cl, "/reglages/notifications"); strings.Contains(corps, "sujet-tres-secret") || !strings.Contains(corps, "https://ntfy.exemple.fr/***") {
		t.Error("sujet ntfy visible par un compte lecture")
	}
}

// Défis de la connexion par passkey seule : une adresse n'en garde que
// defisParAdresse et ne peut pas remplir la table à elle seule.
func TestDefisParAdresse(t *testing.T) {
	var d defisConnexion
	t0 := time.Unix(1_800_000_000, 0)
	var premier string
	for i := 0; i < defisConnexionMax+500; i++ {
		defi, err := d.nouveau("203.0.113.7", t0.Add(time.Duration(i)*time.Millisecond))
		if err != nil {
			t.Fatalf("demande %d refusée : %v", i, err)
		}
		if i == 0 {
			premier = defi
		}
	}
	if len(d.tout) != defisParAdresse {
		t.Fatalf("%d défis gardés pour une adresse, %d attendus", len(d.tout), defisParAdresse)
	}
	if d.prendre(premier, t0.Add(time.Minute)) {
		t.Error("le plus ancien défi de l'adresse aurait dû être remplacé")
	}
	// Une autre adresse (et tout un /64 compte pour une seule) obtient le sien.
	autre, err := d.nouveau("2001:db8::1", t0.Add(time.Minute))
	if err != nil || !d.prendre(autre, t0.Add(2*time.Minute)) {
		t.Fatalf("autre adresse bloquée : %v", err)
	}
	for i := 0; i < 50; i++ {
		_, _ = d.nouveau(fmt.Sprintf("2001:db8::%x", i+2), t0.Add(time.Minute))
	}
	if len(d.tout) != 2*defisParAdresse {
		t.Errorf("/64 : %d défis, %d attendus", len(d.tout), 2*defisParAdresse)
	}
	// Usage unique et durée de vie inchangés.
	if d.prendre(autre, t0.Add(2*time.Minute)) {
		t.Error("défi accepté deux fois")
	}
}

// Retour du 16/11 : les alertes qui ne sont que la suite d'une panne déjà
// affichée (machine muette, parent en panne) vont dans un bloc replié ;
// silences et maintenances restent dans la liste.
func TestSeparerMasquees(t *testing.T) {
	rs := []resumeAlerte{
		{Regle: "AgentMuet", Machine: "pve1"},
		{Regle: "ServiceDuModeleAbsent", Machine: "pve1", Sourdine: "inhibée par AgentMuet (pve1)"},
		{Regle: "CpuEleve", Machine: "vm1", Sourdine: "dépendance : pve1 en panne"},
		{Regle: "DisqueFaible", Machine: "nas", Sourdine: "silence 3"},
		{Regle: "CpuEleve", Machine: "nas", Sourdine: "maintenance 2 (nas)"},
	}
	visibles, masquees := separerMasquees(rs)
	if len(visibles) != 3 || len(masquees) != 2 || visibles[0].Regle != "AgentMuet" || masquees[0].Regle != "ServiceDuModeleAbsent" {
		t.Fatalf("visibles %+v, masquées %+v", visibles, masquees)
	}
	// Rendu : le bloc replié nomme la raison, les alertes n'y sont pas comptées.
	b := nouveauBanc(t)
	w := httptest.NewRecorder()
	b.c.pages.executer(w, http.StatusOK, "alertes-fragment", Page{D: donneesEnCours{Vue: "machine", Total: 1, Critiques: 1, Masquees: masquees}})
	corps := w.Body.String()
	for _, attendu := range []string{"Masquées par une panne</strong> (2)", "Inhibée par AgentMuet (pve1)", "Dépendance : pve1 en panne", `data-nb-alertes="1"`} {
		if !strings.Contains(corps, attendu) {
			t.Errorf("bloc des alertes masquées : %q absent", attendu)
		}
	}
}

// Lien d'une notification (SPEC § 34.5) : la page demandée est gardée
// pendant la connexion, et jamais vers un autre site.
func TestSuiteApresConnexion(t *testing.T) {
	b := nouveauBanc(t)
	c0, secret := b.premierAdmin("admin", mdpAdmin)
	_ = c0
	c := b.client()
	_, _, h := b.get(c, "/alertes/12?onglet=graphique")
	vers := h.Get("Location")
	if vers != "/connexion?suite="+url.QueryEscape("/alertes/12?onglet=graphique") {
		t.Fatalf("redirection : %s", vers)
	}
	_, corps, _ := b.get(c, vers)
	if !strings.Contains(corps, `name="suite" value="/alertes/12?onglet=graphique"`) {
		t.Fatal("page demandée absente du formulaire de connexion")
	}
	b.post(c, "/connexion", url.Values{"nom": {"admin"}, "mot_de_passe": {mdpAdmin}, "suite": {"/alertes/12?onglet=graphique"}})
	_, corps, _ = b.get(c, "/connexion/code")
	code, _, h := b.post(c, "/connexion/code", url.Values{"csrf": {trouver(t, reCSRF, corps)}, "code": {b.code(secret)}})
	if code != http.StatusSeeOther || h.Get("Location") != "/alertes/12?onglet=graphique" {
		t.Fatalf("après la connexion : %d %s", code, h.Get("Location"))
	}
	// Adresses refusées : autre site, pages de connexion, adresses des scripts.
	for _, mauvaise := range []string{"https://ailleurs.example/x", "//ailleurs.example/x", `/\ailleurs.example`, "/connexion/code", "/deconnexion",
		"/statique/mirador.js", "/donnees/courbe?x=1", "/fragments/alertes", "/", "", "/" + strings.Repeat("a", 600)} {
		if s := suiteLocale(mauvaise); s != "" {
			t.Errorf("suiteLocale(%q) = %q", mauvaise, s)
		}
	}
	c2 := b.client()
	b.post(c2, "/connexion", url.Values{"nom": {"admin"}, "mot_de_passe": {mdpAdmin}, "suite": {"https://ailleurs.example/x"}})
	_, corps, _ = b.get(c2, "/connexion/code")
	if _, _, h := b.post(c2, "/connexion/code", url.Values{"csrf": {trouver(t, reCSRF, corps)}, "code": {b.code(secret)}}); h.Get("Location") != "/" {
		t.Fatalf("suite vers un autre site suivie : %s", h.Get("Location"))
	}
}

// Lien ouvert depuis un autre site : page relais (le cookie Strict n'est pas
// envoyé au premier passage), seulement pour l'affichage d'une page.
func TestRelaisLienExterne(t *testing.T) {
	b := nouveauBanc(t)
	b.premierAdmin("admin", mdpAdmin)
	c := b.client()
	demande := func(methode, chemin string, entetes ...string) (int, string, http.Header) {
		r, _ := http.NewRequest(methode, b.srv.URL+chemin, nil)
		for i := 0; i+1 < len(entetes); i += 2 {
			r.Header.Set(entetes[i], entetes[i+1])
		}
		resp, err := c.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var corps bytes.Buffer
		_, _ = corps.ReadFrom(resp.Body)
		return resp.StatusCode, corps.String(), resp.Header
	}
	externe := []string{"Sec-Fetch-Site", "cross-site", "Sec-Fetch-Mode", "navigate", "Sec-Fetch-Dest", "document"}
	code, corps, _ := demande(http.MethodGet, "/alertes/7?a=1&b=2", externe...)
	if code != http.StatusOK || !strings.Contains(corps, `http-equiv="refresh" content="0;url=/alertes/7?a=1&amp;b=2"`) {
		t.Fatalf("relais : %d %s", code, corps)
	}
	// Rechargement « de l'intérieur » : plus de relais (pas de boucle).
	if code, _, h := demande(http.MethodGet, "/alertes/7", "Sec-Fetch-Site", "same-origin", "Sec-Fetch-Mode", "navigate", "Sec-Fetch-Dest", "document"); code != http.StatusSeeOther ||
		!strings.HasPrefix(h.Get("Location"), "/connexion") {
		t.Fatalf("après le relais : %d %s", code, h.Get("Location"))
	}
	// Jamais de relais vers un autre site, ni pour une image ou un cadre.
	// (Go ramène déjà « //hôte/x » à « /hôte/x », une page de ce site.)
	if _, corps, h := demande(http.MethodGet, "//ailleurs.example/x", externe...); strings.Contains(corps, "url=//") || strings.Contains(corps, `href="//`) ||
		strings.HasPrefix(h.Get("Location"), "//") {
		t.Fatalf("relais vers un autre site :\n%s", corps)
	}
	if code, _, _ := demande(http.MethodGet, "/alertes/7", "Sec-Fetch-Site", "cross-site", "Sec-Fetch-Mode", "no-cors", "Sec-Fetch-Dest", "image"); code != http.StatusSeeOther {
		t.Fatalf("image d'un autre site : %d", code)
	}
	// Une écriture venue d'un autre site reste refusée.
	if code, _, _ := demande(http.MethodPost, "/alertes/7", externe...); code != http.StatusForbidden {
		t.Fatalf("écriture d'un autre site : %d", code)
	}
}
