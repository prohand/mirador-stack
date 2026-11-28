// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"bufio"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"mirador-stack/internal/protocole"
	"mirador-stack/internal/server/alertes"
	"mirador-stack/internal/server/auth"
	"mirador-stack/internal/server/direct"
	"mirador-stack/internal/server/notif"
	"mirador-stack/internal/server/stockage"
	"mirador-stack/internal/server/stockage/journaux"
	"mirador-stack/internal/server/webpush"
)

// Tests des corrections de la revue de sécurité du 28/11.

// sessionDe : la session en mémoire du compte (nil si aucune).
func (b *banc) sessionDe(nom string) *auth.Session {
	for _, s := range b.c.sessions.Lister() {
		if s.Nom == nom {
			return &s
		}
	}
	return nil
}

// B1 : après 12 h (durée maximale ordinaire), le nettoyage des 5 s ne
// retire plus la session d'un appareil de confiance : ses compteurs
// survivent, et 5 codes faux espacés ferment bien la session et oublient
// l'appareil.
func TestConfianceSurvitAuNettoyage(t *testing.T) {
	b := nouveauBanc(t)
	_, secret := b.premierAdmin("admin", mdpAdmin)
	c, _ := b.connecterConfiance("admin", mdpAdmin, secret)

	// 13 h d'usage : une requête par heure (pas d'inactivité), un tour de
	// nettoyage à chaque fois.
	for i := 0; i < 13; i++ {
		b.h.avancer(time.Hour)
		if code, _, _ := b.get(c, "/"); code != http.StatusOK {
			t.Fatalf("heure %d : %d", i+1, code)
		}
		b.c.Tour(context.Background())
	}
	s := b.sessionDe("admin")
	if s == nil || !s.Confiance {
		t.Fatal("session de confiance retirée par le nettoyage après 12 h")
	}
	// Mode administration ouvert : il doit survivre aux tours suivants.
	jusqua := b.h.maintenant().Add(time.Hour)
	b.c.sessions.Modifier(s.Empreinte, func(x *auth.Session) { x.AdminJusqua = jusqua })

	for i := 1; i <= 5; i++ {
		b.h.avancer(6 * time.Second) // plus d'un tour de boucle entre deux essais
		b.c.Tour(context.Background())
		code, corps, _ := b.post(c, "/mode-admin/ouvrir", url.Values{"csrf": {b.csrf(c, "/compte")}, "code": {"000000"}, "retour": {"/compte"}})
		if code != http.StatusSeeOther {
			t.Fatalf("essai %d : %d %s", i, code, corps)
		}
		if i < 5 {
			s := b.sessionDe("admin")
			if s == nil || s.EssaisSensibles != i || !s.AdminJusqua.Equal(jusqua) {
				t.Fatalf("essai %d : compteurs perdus (%+v)", i, s)
			}
		}
	}
	if b.sessionDe("admin") != nil {
		t.Fatal("session encore ouverte après 5 codes faux")
	}
	if b.nbAppareils() != 0 {
		t.Fatal("appareil de confiance pas oublié après 5 codes faux")
	}
	if code, _, _ := b.get(c, "/"); code != http.StatusSeeOther {
		t.Fatalf("session revenue depuis le disque : %d", code)
	}
}

// lireEvenement : prochain évènement d'un flux (« nom données »), "" si le
// flux est fermé ; échec après 5 s.
func lireEvenement(t *testing.T, lu *bufio.Reader) string {
	t.Helper()
	fini := make(chan string, 1)
	go func() {
		ev := ""
		for {
			l, err := lu.ReadString('\n')
			if err != nil {
				fini <- ""
				return
			}
			if strings.HasPrefix(l, "event: ") {
				ev = strings.TrimSpace(l[7:])
			}
			if strings.HasPrefix(l, "data: ") {
				fini <- ev + " " + strings.TrimSpace(l[6:])
				return
			}
		}
	}()
	select {
	case e := <-fini:
		return e
	case <-time.After(5 * time.Second):
		t.Fatal("flux muet")
		return ""
	}
}

// fluxFerme : le serveur a fermé le flux (fin du corps).
func fluxFerme(t *testing.T, lu *bufio.Reader) {
	t.Helper()
	fini := make(chan error, 1)
	go func() { _, err := io.Copy(io.Discard, lu); fini <- err }()
	select {
	case <-fini:
	case <-time.After(5 * time.Second):
		t.Fatal("flux toujours ouvert")
	}
}

// B2 : un flux de logs en direct est coupé quand la session est fermée, ou
// quand le compte est désactivé.
func TestFluxLogsCoupeSessionFermee(t *testing.T) {
	ancien := verifFlux
	verifFlux = 0 // vérification à chaque tour (test)
	t.Cleanup(func() { verifFlux = ancien })
	b := nouveauBanc(t)
	m := b.h.maintenant()
	jx, err := journaux.Ouvrir(journaux.Options{Dossier: t.TempDir(), Maintenant: b.h.maintenant})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = jx.Fermer() })
	b.c.opt.Logs = jx
	c, secret := b.premierAdmin("admin", mdpAdmin)
	depuis := strconv.FormatInt(m.Add(-10*time.Minute).UnixMilli(), 10)
	ecrire := func(msg string) {
		_ = jx.Ecrire(context.Background(), []stockage.Ligne{{T: b.h.maintenant().UnixMilli(), Msg: msg, Champs: map[string]string{"host": "web1"}}})
	}
	ouvrir := func(c *http.Client) (*bufio.Reader, func()) {
		resp, err := c.Get(b.srv.URL + "/logs/flux?host=web1&periode=1h&depuis=" + depuis)
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("flux : %v %v", err, resp)
		}
		return bufio.NewReader(resp.Body), func() { resp.Body.Close() }
	}

	// 1. Session fermée (comme « fermer la session » ou 5 codes faux).
	ecrire("premiere")
	lu, fermer := ouvrir(c)
	defer fermer()
	if e := lireEvenement(t, lu); !strings.Contains(e, "premiere") {
		t.Fatalf("premier envoi : %s", e)
	}
	b.c.sessions.FermerUtilisateur(1, "")
	ecrire("apres fermeture")
	if e := lireEvenement(t, lu); e != `fin {"raison":"session"}` {
		t.Fatalf("après fermeture de la session : %q", e)
	}
	fluxFerme(t, lu)

	// 2. Compte désactivé (par une commande : seule la base change).
	c = b.connecter("admin", mdpAdmin, secret)
	ecrire("deuxieme")
	lu, fermer2 := ouvrir(c)
	defer fermer2()
	if e := lireEvenement(t, lu); !strings.HasPrefix(e, "lignes ") {
		t.Fatalf("deuxième flux : %s", e)
	}
	if _, err := b.db.Exec(`UPDATE utilisateurs SET desactive = 1`); err != nil {
		t.Fatal(err)
	}
	b.c.oublierComptes()
	ecrire("apres desactivation")
	if e := lireEvenement(t, lu); e != `fin {"raison":"session"}` {
		t.Fatalf("après désactivation : %q", e)
	}
	fluxFerme(t, lu)
}

// B2 : relevés en direct coupés quand la génération du compte change (mot
// de passe changé ailleurs).
func TestFluxDirectCoupeGeneration(t *testing.T) {
	ancien := verifFlux
	verifFlux = 0
	t.Cleanup(func() { verifFlux = ancien })
	b := nouveauBanc(t)
	hub := direct.Nouveau()
	b.c.opt.Direct = hub
	if _, err := b.c.db.Exec(`INSERT INTO machines(nom, statut, type, cree_le, capacites, version_agent) VALUES
		('web1', 'approuvee', 'agent', 1, 'surveillance,direct', '0.6.13')`); err != nil {
		t.Fatal(err)
	}
	var id int64
	_ = b.c.db.QueryRow(`SELECT id FROM machines WHERE nom = 'web1'`).Scan(&id)
	c, _ := b.premierAdmin("admin", mdpAdmin)
	resp, err := c.Get(b.srv.URL + "/machines/web1/direct")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	lu := bufio.NewReader(resp.Body)
	if e := lireEvenement(t, lu); !strings.HasPrefix(e, "debut ") {
		t.Fatalf("premier évènement : %s", e)
	}
	hub.Recevoir(id, protocole.Direct{CPU: 10})
	if e := lireEvenement(t, lu); !strings.HasPrefix(e, "point ") {
		t.Fatalf("relevé : %s", e)
	}
	if _, err := b.db.Exec(`UPDATE utilisateurs SET generation = generation + 1`); err != nil {
		t.Fatal(err)
	}
	b.c.oublierComptes()
	// Le relevé déjà attendu peut encore partir ; le flux s'arrête au tour
	// suivant.
	hub.Recevoir(id, protocole.Direct{CPU: 20})
	e := lireEvenement(t, lu)
	if strings.HasPrefix(e, "point ") {
		hub.Recevoir(id, protocole.Direct{CPU: 30})
		e = lireEvenement(t, lu)
	}
	if e != `fin {"raison":"session"}` {
		t.Fatalf("après changement de génération : %q", e)
	}
	fluxFerme(t, lu)
}

// B3 : mode administration fermé, les actions de la page Tâches et de la
// page de statut demandent le code 2FA ; avec le code, elles passent.
func TestActionsProtegeesTachesStatut(t *testing.T) {
	b := nouveauBanc(t)
	ctx := context.Background()
	c, secret := b.premierAdmin("admin", mdpAdmin)
	csrf := b.csrf(c, "/taches")
	// Mode administration ouvert (à la connexion) : création sans code.
	if code, _, _ := b.post(c, "/taches", url.Values{"csrf": {csrf}, "action": {"creer"}, "nom": {"NAS"}, "periode": {"86400"}, "marge": {"1"}, "gravite": {"warning"}}); code != http.StatusOK {
		t.Fatalf("création en mode administration : %d", code)
	}
	var id, jeton string
	_ = b.db.QueryRow(`SELECT id, jeton FROM taches`).Scan(&id, &jeton)
	b.quitterModeAdmin(c)

	nbTaches := func() int {
		var n int
		_ = b.db.QueryRow(`SELECT count(*) FROM taches`).Scan(&n)
		return n
	}
	// Sans code : rien n'est fait.
	b.post(c, "/taches", url.Values{"csrf": {csrf}, "action": {"creer"}, "nom": {"Autre"}, "periode": {"86400"}, "marge": {"1"}, "gravite": {"warning"}})
	b.post(c, "/taches", url.Values{"csrf": {csrf}, "action": {"jeton"}, "id": {id}, "nom": {"NAS"}})
	b.post(c, "/taches", url.Values{"csrf": {csrf}, "action": {"retirer"}, "id": {id}, "nom": {"NAS"}})
	b.post(c, "/taches", url.Values{"csrf": {csrf}, "action": {"exclure_vm"}, "machine": {"proxmox"}, "vm": {"200"}})
	var j2 string
	_ = b.db.QueryRow(`SELECT jeton FROM taches WHERE id = ?`, id).Scan(&j2)
	if nbTaches() != 1 || j2 != jeton {
		t.Fatalf("action de la page Tâches faite sans code : %d tâche(s), jeton changé %v", nbTaches(), j2 != jeton)
	}
	if l, _ := alertes.LireInvitesExclus(ctx, b.db); len(l) != 0 {
		t.Fatal("VM exclue sans code")
	}
	// Avec le code : faites.
	if code, _, _ := b.post(c, "/taches", url.Values{"csrf": {csrf}, "action": {"jeton"}, "id": {id}, "nom": {"NAS"}, "code": {b.code(secret)}}); code != http.StatusOK {
		t.Fatalf("nouvelle adresse avec le code : %d", code)
	}
	b.post(c, "/taches", url.Values{"csrf": {csrf}, "action": {"retirer"}, "id": {id}, "nom": {"NAS"}}) // mode ouvert par le code
	if nbTaches() != 0 {
		t.Fatal("tâche pas retirée après le code")
	}

	// Page de statut : activer sans code refusé ; désactiver sans code accepté.
	b.quitterModeAdmin(c)
	b.post(c, "/reglages/statut", url.Values{"csrf": {csrf}, "action": {"enregistrer"}, "actif": {"1"}})
	if r := lireStatut(ctx, b.db); r.Actif || r.Jeton != "" {
		t.Fatalf("page de statut activée sans code : %+v", r)
	}
	b.post(c, "/reglages/statut", url.Values{"csrf": {csrf}, "action": {"enregistrer"}, "actif": {"1"}, "code": {b.code(secret)}})
	r := lireStatut(ctx, b.db)
	if !r.Actif || r.Jeton == "" {
		t.Fatalf("page de statut non activée avec le code : %+v", r)
	}
	b.quitterModeAdmin(c)
	b.post(c, "/reglages/statut", url.Values{"csrf": {csrf}, "action": {"jeton"}})
	if r2 := lireStatut(ctx, b.db); r2.Jeton != r.Jeton {
		t.Fatal("adresse de la page de statut changée sans code")
	}
	b.post(c, "/reglages/statut", url.Values{"csrf": {csrf}, "action": {"enregistrer"}})
	if r2 := lireStatut(ctx, b.db); r2.Actif {
		t.Fatal("désactivation refusée sans code")
	}
}

// compteLea : compte « lea » avec une 2FA, une passkey et un code de
// secours (valeurs factices : seule leur présence compte ici).
func (b *banc) compteLea() int64 {
	b.t.Helper()
	ctx := context.Background()
	if err := auth.CreerUtilisateur(ctx, b.db, "lea", auth.RoleLecture, "ancien mot de passe de lea", "test", "", b.h.maintenant()); err != nil {
		b.t.Fatal(err)
	}
	var id int64
	_ = b.db.QueryRow(`SELECT id FROM utilisateurs WHERE nom = 'lea'`).Scan(&id)
	for _, r := range []string{
		`UPDATE utilisateurs SET totp = 'factice' WHERE id = ?`,
		`INSERT INTO passkeys(utilisateur_id, identifiant, cle_publique, algorithme, domaine, nom, cree_le) VALUES (?, x'01', x'02', -7, 'exemple.org', 'tel', 1)`,
		`INSERT INTO codes_secours(utilisateur_id, empreinte, cree_le) VALUES (?, x'03', 1)`,
	} {
		if _, err := b.db.Exec(r, id); err != nil {
			b.t.Fatal(err)
		}
	}
	return id
}

// secoursDe : passkeys + codes de secours d'un compte.
func (b *banc) secoursDe(id int64) int {
	var p, s int
	_ = b.db.QueryRow(`SELECT count(*) FROM passkeys WHERE utilisateur_id = ?`, id).Scan(&p)
	_ = b.db.QueryRow(`SELECT count(*) FROM codes_secours WHERE utilisateur_id = ?`, id).Scan(&s)
	return p + s
}

// B4 : « Effacer la 2FA » par un admin impose un nouveau mot de passe.
func TestReinitialiser2FAImposeMotDePasse(t *testing.T) {
	b := nouveauBanc(t)
	ctx := context.Background()
	c, secret := b.premierAdmin("admin", mdpAdmin)
	id := b.compteLea()
	csrf := b.csrf(c, "/utilisateurs")
	// Sans mot de passe : refusé, rien n'est effacé.
	b.post(c, "/utilisateurs", url.Values{"csrf": {csrf}, "nom": {"lea"}, "action": {"reinitialiser_2fa"}, "code": {b.code(secret)}})
	u, _ := auth.LireUtilisateur(ctx, b.db, "lea")
	if u.TOTP == "" || b.secoursDe(id) != 2 {
		t.Fatal("2FA effacée sans nouveau mot de passe")
	}
	if _, corps, _ := b.get(c, "/utilisateurs"); !strings.Contains(corps, "aisir aussi un nouveau mot de passe") {
		t.Error("message « nouveau mot de passe » absent")
	}
	// Avec : mot de passe changé, 2FA, passkeys et codes de secours effacés.
	b.post(c, "/utilisateurs", url.Values{"csrf": {csrf}, "nom": {"lea"}, "action": {"reinitialiser_2fa"}, "mot_de_passe": {"provisoire pour lea 2026"}, "code": {b.code(secret)}})
	u, _ = auth.LireUtilisateur(ctx, b.db, "lea")
	if u.TOTP != "" || b.secoursDe(id) != 0 {
		t.Fatalf("2FA ou secours non effacés : %q, %d", u.TOTP, b.secoursDe(id))
	}
	if auth.Comparer(u.MotDePasse, "ancien mot de passe de lea") || !auth.Comparer(u.MotDePasse, "provisoire pour lea 2026") {
		t.Fatal("mot de passe non remplacé")
	}
}

// B5 : changement de mot de passe par un admin : passkeys et codes de
// secours effacés si la case est cochée (cochée d'office), gardés sinon.
// Le nombre de passkeys est affiché.
func TestMotDePasseAdminEffacePasskeys(t *testing.T) {
	b := nouveauBanc(t)
	c, secret := b.premierAdmin("admin", mdpAdmin)
	id := b.compteLea()
	_, corps, _ := b.get(c, "/utilisateurs")
	if !strings.Contains(corps, `name="effacer_secours" value="1" checked`) || !strings.Contains(corps, "1 passkey") {
		t.Fatal("case « effacer aussi les passkeys » ou nombre de passkeys absent")
	}
	csrf := trouver(t, reCSRF, corps)
	b.post(c, "/utilisateurs", url.Values{"csrf": {csrf}, "nom": {"lea"}, "action": {"mot_de_passe"}, "mot_de_passe": {"autre phrase pour lea 1"}, "code": {b.code(secret)}})
	if b.secoursDe(id) != 2 {
		t.Fatal("case décochée : passkeys effacées quand même")
	}
	b.post(c, "/utilisateurs", url.Values{"csrf": {csrf}, "nom": {"lea"}, "action": {"mot_de_passe"}, "mot_de_passe": {"autre phrase pour lea 2"}, "effacer_secours": {"1"}, "code": {b.code(secret)}})
	if b.secoursDe(id) != 0 {
		t.Fatal("case cochée : passkeys ou codes de secours gardés")
	}
}

// Supprimer un compte retire aussi son changement de rôle en attente.
func TestSupprimerCompteRetireRoleEnAttente(t *testing.T) {
	b := nouveauBanc(t)
	ctx := context.Background()
	_, _ = b.premierAdmin("admin", mdpAdmin)
	b.compteLea()
	if _, err := b.db.Exec(`INSERT INTO a_confirmer(quoi, avant, par, cree_le, expire_le) VALUES ('role:lea', 'lecture', 'web:lea', 1, 9999999999)`); err != nil {
		t.Fatal(err)
	}
	if err := auth.SupprimerUtilisateur(ctx, b.db, "lea", "test", "", b.h.maintenant()); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = b.db.QueryRow(`SELECT count(*) FROM a_confirmer WHERE quoi = 'role:lea'`).Scan(&n)
	if n != 0 {
		t.Fatal("demande « role:lea » gardée après la suppression du compte")
	}
}

// Serveur occupé pendant le changement de son mot de passe : pas compté
// comme un essai manqué.
func TestMotDePasseOccupePasCompte(t *testing.T) {
	b := nouveauBanc(t)
	c, _ := b.premierAdmin("admin", mdpAdmin)
	ancien := comparerMotDePasse
	comparerMotDePasse = func(string, string) (bool, bool) { return false, true }
	t.Cleanup(func() { comparerMotDePasse = ancien })
	csrf := b.csrf(c, "/compte")
	for i := 0; i < essaisSensiblesMax+1; i++ {
		b.post(c, "/compte/mot-de-passe", url.Values{"csrf": {csrf}, "actuel": {"x"}, "nouveau": {"y"}, "nouveau2": {"y"}})
	}
	s := b.sessionDe("admin")
	if s == nil || s.EssaisSensibles != 0 {
		t.Fatalf("serveur occupé compté comme un échec : %+v", s)
	}
	if _, corps, _ := b.get(c, "/compte"); !strings.Contains(corps, "erveur occupé") {
		t.Error("message « serveur occupé » absent")
	}
}

// Notifications push : l'appareil abonné par un compte ne change pas de
// compte si l'on ne présente pas ses clés (adresse seule connue) ; le même
// navigateur (mêmes clés) peut passer à un autre compte.
func TestPushPasDeRepriseSansCles(t *testing.T) {
	b := nouveauBanc(t)
	ctx := context.Background()
	_, _ = b.premierAdmin("admin", mdpAdmin)
	lea := b.compteLea()
	appareil := func() webpush.Abonnement {
		k, _ := ecdh.P256().GenerateKey(rand.Reader)
		a := make([]byte, 16)
		_, _ = rand.Read(a)
		return webpush.Abonnement{Adresse: "https://fcm.googleapis.com/fcm/send/admin", P256dh: k.PublicKey().Bytes(), Auth: a}
	}
	vrai := appareil()
	if err := notif.AbonnerPush(ctx, b.db, 1, vrai, "https://mirador.exemple.fr", "tel admin", b.h.maintenant()); err != nil {
		t.Fatal(err)
	}
	// Même adresse, autres clés, autre compte : refusé, l'admin garde son appareil.
	if err := notif.AbonnerPush(ctx, b.db, lea, appareil(), "https://mirador.exemple.fr", "x", b.h.maintenant()); !errors.Is(err, notif.ErrAppareilAutreCompte) {
		t.Fatalf("reprise sans les clés : %v", err)
	}
	if l, _ := notif.ListerAppareilsPush(ctx, b.db, 1); len(l) != 1 || l[0].Appareil != "tel admin" {
		t.Fatal("appareil de l'admin détourné")
	}
	// Même navigateur (mêmes clés) : passe au compte qui s'y connecte.
	if err := notif.AbonnerPush(ctx, b.db, lea, vrai, "https://mirador.exemple.fr", "tel partagé", b.h.maintenant()); err != nil {
		t.Fatalf("navigateur partagé : %v", err)
	}
	// Le compte qui le tient peut renouveler ses clés.
	if err := notif.AbonnerPush(ctx, b.db, lea, appareil(), "https://mirador.exemple.fr", "tel partagé", b.h.maintenant()); err != nil {
		t.Fatalf("renouvellement par le même compte : %v", err)
	}
}
