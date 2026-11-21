// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"mirador-stack/internal/server/auth"
	"mirador-stack/internal/server/flotte"
	"mirador-stack/internal/server/notif"
)

// Tests de la relecture de sécurité du 21/11 (SPEC § 113).
//
// R1 : déconnexion alors que la session n'est plus en mémoire (inactivité
// > 30 min, ou redémarrage) : l'appareil de confiance reste valable.
func TestRelectureDeconnexionHorsMemoire(t *testing.T) {
	b := nouveauBanc(t)
	_, secret := b.premierAdmin("admin", mdpAdmin)
	c, ck := b.connecterConfiance("admin", mdpAdmin, secret)
	valeur := strings.TrimPrefix(strings.SplitN(ck, ";", 2)[0], auth.NomCookie+"=")
	csrf := b.csrf(c, "/")
	// 40 min sans usage : Tour retire la session de la mémoire (durées ordinaires).
	b.h.avancer(40 * time.Minute)
	b.c.Tour(context.Background())
	code, _, _ := b.post(c, "/deconnexion", url.Values{"csrf": {csrf}})
	t.Logf("déconnexion : %d ; appareils restants : %d", code, b.nbAppareils())
	// Cookie copié avant la déconnexion (vol) : rejoué dans un autre navigateur.
	vol := b.client()
	u, _ := url.Parse(b.srv.URL)
	vol.Jar.SetCookies(u, []*http.Cookie{{Name: auth.NomCookie, Value: valeur, Path: "/", Secure: true}})
	code, _, _ = b.get(vol, "/")
	t.Logf("cookie rejoué après déconnexion : %d", code)
	if code == http.StatusOK {
		t.Error("cookie encore valable après la déconnexion")
	}
}

// R2 : 5 codes faux sur une session d'appareil de confiance : la session
// « fermée » revient aussitôt depuis le disque, compteur remis à zéro.
func TestRelectureEssaisSensiblesConfiance(t *testing.T) {
	b := nouveauBanc(t)
	_, secret := b.premierAdmin("admin", mdpAdmin)
	c, _ := b.connecterConfiance("admin", mdpAdmin, secret)
	var corps string
	for i := 0; i < 5; i++ {
		_, _, _ = b.post(c, "/compte/codes-secours", url.Values{"csrf": {b.csrf(c, "/compte")}, "code": {"000000"}})
		// La sécurité de l'adresse n'est pas le sujet ici : on lève les verrous
		// d'adresse (comme une rotation d'adresses IPv6 /64 différentes).
		b.c.verrous.Lever()
	}
	code, corps, _ := b.get(c, "/compte")
	t.Logf("après 5 codes faux : GET /compte → %d", code)
	if code == http.StatusOK && strings.Contains(corps, "Mon compte") {
		t.Error("session « fermée » après 5 essais reprise depuis le disque (compteur remis à zéro)")
	}
}

// R3 : import « agents » : contrôleur UniFi déplacé vers une autre adresse
// avec « *** » : l'ancien mot de passe chiffré est gardé (le formulaire le
// refuse).
func TestRelectureImportUniFiAdresse(t *testing.T) {
	b := nouveauBanc(t)
	c, secret := b.premierAdmin("admin", mdpAdmin)
	ctx := context.Background()
	csrf := b.csrf(c, "/machines")
	if _, err := b.c.db.Exec(`INSERT INTO machines(nom, statut, type, cree_le) VALUES ('collecteur-lan', 'approuvee', 'collecteur', 1), ('unifi', 'approuvee', 'agent', 1)`); err != nil {
		t.Fatal(err)
	}
	idC, _ := flotte.IDMachine(ctx, b.c.db, "collecteur-lan")
	idU, _ := flotte.IDMachine(ctx, b.c.db, "unifi")
	_ = flotte.RattacherEquipement(ctx, b.c.db, idC, idU, "admin", time.Now())
	emp := strings.Repeat("AB:", 31) + "AB"
	f := url.Values{"csrf": {csrf}, "action": {"unifi"}, "machine": {"unifi"}, "adresse": {"https://192.168.1.5/"}, "mode": {"compte"},
		"utilisateur": {"lecture"}, "secret": {"mot-de-passe-unifi"}, "empreinte": {emp}, "sites": {"default"}, "code": {b.code(secret)}}
	b.post(c, "/machines/collecteur-lan", f)
	doc, _, _ := flotte.ConfigMachine(ctx, b.c.db, idC)
	t.Logf("avant : %s", doc)
	// Formulaire : adresse changée sans secret → refusé.
	f.Set("secret", "")
	f.Set("adresse", "https://192.168.1.66")
	f.Set("empreinte", "")
	f.Del("code")
	b.post(c, "/machines/collecteur-lan", f)
	if d, _, _ := flotte.ConfigMachine(ctx, b.c.db, idC); strings.Contains(string(d), "192.168.1.66") {
		t.Fatal("le formulaire accepte déjà : test sans objet")
	}
	// Export sans secret puis import avec l'adresse changée et l'empreinte retirée.
	_, yaml, _ := b.get(c, "/reglages/export.yaml")
	modif := strings.Replace(yaml, "https://192.168.1.5", cibleImport, 1)
	modif = strings.Replace(modif, strings.ToLower(strings.ReplaceAll(emp, ":", "")), "", 1)
	modif = strings.Replace(modif, "empreinte: \"\"", "", 1)
	_, rap := b.postFichier(c, "/reglages/importer", url.Values{"csrf": {b.csrf(c, "/reglages/export")}, "parties": {"agents"}}, modif)
	d, _, _ := flotte.ConfigMachine(ctx, b.c.db, idC)
	t.Logf("après import : %s", d)
	if strings.Contains(string(d), cibleImport) && strings.Contains(string(d), "chiffre:v1:") {
		t.Error("mot de passe UniFi gardé pour une autre adresse par l'import")
	} else {
		t.Logf("rapport : %s", rap[max(0, strings.Index(rap, "configuration")):min(len(rap), strings.Index(rap, "configuration")+400)])
	}
}

// R4 : changement de son propre rôle en attente ; un autre admin fixe ce
// rôle entre-temps ; l'annulation automatique écrase sa décision.
func TestRelectureRoleAnnuleEcrase(t *testing.T) {
	b := nouveauBanc(t)
	_, _ = b.premierAdmin("admin", mdpAdmin)
	ctx := context.Background()
	t0 := b.h.maintenant()
	if err := auth.CreerUtilisateur(ctx, b.db, "alice", auth.RoleAdmin, "une autre longue phrase", "test", "", t0); err != nil {
		t.Fatal(err)
	}
	// alice se rétrograde elle-même (en attente de confirmation).
	if err := auth.DefinirRole(ctx, b.db, "alice", auth.RoleLecture, "web:alice", "", t0); err != nil {
		t.Fatal(err)
	}
	if _, err := DemanderConfirmationRole(ctx, b.db, "alice", auth.RoleAdmin, "web:alice", t0); err != nil {
		t.Fatal(err)
	}
	// admin (autre compte) décide qu'alice reste en lecture (rôle remis à lecture, ou désactive puis...).
	if err := auth.DefinirRole(ctx, b.db, "alice", auth.RoleLecture, "web:admin", "", t0); err != nil {
		t.Fatal(err)
	}
	if _, err := AnnulerExpires(ctx, b.db, t0.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	u, _ := auth.LireUtilisateur(ctx, b.db, "alice")
	if u.Role != auth.RoleLecture {
		t.Errorf("rôle d'alice après l'annulation automatique : %s (la décision de l'autre admin est écrasée)", u.Role)
	}
}

var cibleImport = "https://192.168.1.66"

func TestRelectureImportUniFiPublic(t *testing.T) {
	cibleImport = "https://203.0.113.5"
	defer func() { cibleImport = "https://192.168.1.66" }()
	TestRelectureImportUniFiAdresse(t)
}

// R5 : sujet ntfy (secret si pas de jeton) caché sur la page Notifications
// pour un compte lecture, mais lisible dans le journal d'audit (lecture).
func TestRelectureNtfyAudit(t *testing.T) {
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
	r, _ := notif.LireReglages(ctx, b.db)
	r.Canaux.Ntfy = &notif.Ntfy{Actif: false, URL: "https://ntfy.exemple.fr/sujet-tres-secret"}
	if err := notif.EcrireReglages(ctx, b.db, r, cleTest, "web:admin", b.h.maintenant()); err != nil {
		t.Fatal(err)
	}
	_, p, _ := b.get(cl, "/reglages/notifications")
	_, a, _ := b.get(cl, "/audit")
	_, csv, _ := b.get(cl, "/audit.csv")
	t.Logf("page notif : %v ; /audit : %v ; /audit.csv : %v", strings.Contains(p, "sujet-tres-secret"), strings.Contains(a, "sujet-tres-secret"), strings.Contains(csv, "sujet-tres-secret"))
	if strings.Contains(a, "sujet-tres-secret") || strings.Contains(csv, "sujet-tres-secret") {
		t.Error("sujet ntfy lisible par un compte lecture dans l'audit")
	}
}

// R6 : lien des messages (adresse de l'interface) changé sans code 2FA par
// Réglages › Interface, alors que Réglages › Notifications le demande.
func TestRelectureLienSansCode(t *testing.T) {
	b := nouveauBanc(t)
	c, _ := b.premierAdmin("admin", mdpAdmin)
	b.quitterModeAdmin(c)
	ctx := context.Background()
	// Route « notifications » : refusée sans code.
	b.post(c, "/reglages/notifications", url.Values{"csrf": {b.csrf(c, "/compte")}, "action": {"lien"}, "lien": {"https://piege.exemple.net"}})
	r, _ := notif.LireReglages(ctx, b.db)
	t.Logf("après /reglages/notifications sans code : lien = %q", r.Lien)
	// Route « interface » : même réglage, sans code.
	b.post(c, "/reglages/interface", url.Values{"csrf": {b.csrf(c, "/compte")}, "action": {"general"}, "fuseau": {"Europe/Paris"}, "lien": {"https://piege.exemple.net"}})
	r, _ = notif.LireReglages(ctx, b.db)
	t.Logf("après /reglages/interface sans code : lien = %q", r.Lien)
	if r.Lien == "https://piege.exemple.net" {
		t.Error("lien des notifications changé sans code 2FA (mode administration fermé)")
	}
}
