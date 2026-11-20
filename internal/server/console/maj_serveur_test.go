// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"mirador-stack/internal/majserveur"
	"mirador-stack/internal/server/flotte"
	"mirador-stack/internal/version"
)

// Réglages › Mise à jour du serveur (SPEC § 102) : service absent expliqué,
// dernière version lue sur GitHub, demande déposée seulement avec un code
// 2FA frais, notée dans l'audit, état du service du VPS affiché.
func TestMajServeur(t *testing.T) {
	b := nouveauBanc(t)
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/prohand/mirador-stack/releases/latest" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Location", "https://github.com/prohand/mirador-stack/releases/tag/v99.1.0")
		w.WriteHeader(http.StatusFound)
	}))
	defer gh.Close()
	b.c.sourceGitHub = flotte.SourceGitHub{Base: gh.URL}
	racine := t.TempDir()
	b.c.opt.MajDemande, b.c.opt.MajEtat = filepath.Join(racine, "maj"), filepath.Join(racine, "maj-etat")
	c, secret := b.premierAdmin("admin", mdpAdmin)

	// Service pas installé : expliqué, pas de bouton.
	_, corps, _ := b.get(c, "/reglages/serveur")
	if !strings.Contains(corps, "pas encore installé") || !strings.Contains(corps, "<b>99.1.0</b>") || strings.Contains(corps, `name="version"`) {
		t.Fatalf("service absent :\n%s", corps)
	}
	_ = os.MkdirAll(b.c.opt.MajDemande, 0o755)
	_ = os.MkdirAll(b.c.opt.MajEtat, 0o755)
	_, corps, _ = b.get(c, "/reglages/serveur")
	if !strings.Contains(corps, `name="version" value="99.1.0"`) {
		t.Fatalf("bouton absent :\n%s", corps)
	}
	demande := filepath.Join(b.c.opt.MajDemande, majserveur.NomDemande)
	// Code faux : rien n'est déposé.
	b.post(c, "/reglages/serveur", url.Values{"csrf": {b.csrf(c, "/reglages/serveur")}, "version": {"99.1.0"}, "code": {"000000"}})
	if _, err := os.Stat(demande); err == nil {
		t.Fatal("demande déposée avec un code faux")
	}
	// Version mal formée : refusée.
	b.post(c, "/reglages/serveur", url.Values{"csrf": {b.csrf(c, "/reglages/serveur")}, "version": {"99.1.0; rm -rf /"}, "code": {b.code(secret)}})
	if _, err := os.Stat(demande); err == nil {
		t.Fatal("version mal formée acceptée")
	}
	b.post(c, "/reglages/serveur", url.Values{"csrf": {b.csrf(c, "/reglages/serveur")}, "version": {"99.1.0"}, "code": {b.code(secret)}})
	var d majserveur.Demande
	if err := majserveur.LireJSON(demande, &d); err != nil || d.Version != "99.1.0" || d.Par != "admin" {
		t.Fatalf("demande : %+v, %v", d, err)
	}
	var n int
	_ = b.db.QueryRow(`SELECT COUNT(*) FROM audit WHERE action = 'maj_serveur_demandee' AND apres = '99.1.0'`).Scan(&n)
	if n != 1 {
		t.Error("demande absente de l'audit")
	}
	_, corps, _ = b.get(c, "/reglages/serveur")
	if !strings.Contains(corps, "en attente du service du VPS") || !strings.Contains(corps, `data-recharger="5"`) || strings.Contains(corps, `name="version"`) {
		t.Errorf("demande en attente mal affichée :\n%s", corps)
	}
	// Le service du VPS a traité : état affiché avec son déroulé.
	_ = os.Remove(demande)
	e := majserveur.Etat{Etat: majserveur.EtatEchec, Version: "99.1.0", Avant: "0.5.0", Message: "la version 99.1.0 n'a pas démarré : retour à 0.5.0 fait",
		Le: b.h.maintenant().Unix(), Etapes: []majserveur.Etape{{Le: b.h.maintenant().Unix(), Texte: "signature vérifiée"}}}
	if err := majserveur.EcrireJSON(b.c.opt.MajEtat, majserveur.NomEtat, e, 0o644); err != nil {
		t.Fatal(err)
	}
	// Réglages du service du VPS : affichés, sans formulaire pour les changer.
	if err := majserveur.EcrireJSON(b.c.opt.MajEtat, majserveur.NomService, majserveur.InfoService{Version: "0.6.0", DossierCompose: "/opt/mirador",
		Service: "mirador-server", Image: "ghcr.io/prohand/mirador-server", Depot: "prohand/mirador-stack", Cles: []string{"0123456789abcdef"},
		Delai: "3m0s", Problemes: []string{"volume de l'état absent"}, Le: b.h.maintenant().Unix()}, 0o644); err != nil {
		t.Fatal(err)
	}
	_, corps, _ = b.get(c, "/reglages/serveur")
	for _, attendu := range []string{"Dernière mise à jour en échec", "0.5.0 → 99.1.0", "signature vérifiée", "retour à 0.5.0 fait",
		"<code>/opt/mirador</code>", "<code>0123456789abcdef</code>", "1 point(s) à corriger", "volume de l&#39;état absent"} {
		if !strings.Contains(corps, attendu) {
			t.Errorf("état : %q absent", attendu)
		}
	}
}

// Mise à jour la nuit (réglage, désactivé par défaut) : demande déposée
// seulement à l'heure choisie, une seule fois par version.
func TestMajServeurNuit(t *testing.T) {
	avant := version.Version
	version.Version = "0.5.0"
	defer func() { version.Version = avant }()
	b := nouveauBanc(t)
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "https://github.com/prohand/mirador-stack/releases/tag/v0.6.0")
		w.WriteHeader(http.StatusFound)
	}))
	defer gh.Close()
	b.c.sourceGitHub = flotte.SourceGitHub{Base: gh.URL}
	racine := t.TempDir()
	b.c.opt.MajDemande, b.c.opt.MajEtat = filepath.Join(racine, "maj"), filepath.Join(racine, "maj-etat")
	_ = os.MkdirAll(b.c.opt.MajDemande, 0o755)
	c, secret := b.premierAdmin("admin", mdpAdmin)
	demande := filepath.Join(b.c.opt.MajDemande, majserveur.NomDemande)
	ctx := t.Context()

	// Désactivée par défaut : rien.
	b.c.majNuit(ctx, b.h.maintenant())
	if _, err := os.Stat(demande); err == nil {
		t.Fatal("demande sans réglage")
	}
	// Activée pour une autre heure : rien.
	h := b.h.maintenant().In(fuseau.Load()).Hour()
	autre := (h + 5) % 24
	b.post(c, "/reglages/serveur", url.Values{"csrf": {b.csrf(c, "/reglages/serveur")}, "action": {"nuit"}, "actif": {"1"}, "heure": {strconv.Itoa(autre)}, "code": {b.code(secret)}})
	if lireMajNuit(ctx, b.db) != autre {
		t.Fatal("réglage non enregistré")
	}
	b.c.dernierMajNuit = time.Time{}
	b.c.majNuit(ctx, b.h.maintenant())
	if _, err := os.Stat(demande); err == nil {
		t.Fatal("demande hors de l'heure choisie")
	}
	// À la bonne heure : demandée, une seule fois.
	b.post(c, "/reglages/serveur", url.Values{"csrf": {b.csrf(c, "/reglages/serveur")}, "action": {"nuit"}, "actif": {"1"}, "heure": {strconv.Itoa(h)}, "code": {b.code(secret)}})
	b.c.dernierMajNuit = time.Time{}
	b.c.majNuit(ctx, b.h.maintenant())
	var d majserveur.Demande
	if err := majserveur.LireJSON(demande, &d); err != nil || d.Version != "0.6.0" || d.Par != "nuit (automatique)" {
		t.Fatalf("demande de nuit : %+v, %v", d, err)
	}
	_ = os.Remove(demande) // traitée (en échec, par exemple)
	b.c.dernierMajNuit = time.Time{}
	b.c.majNuit(ctx, b.h.maintenant())
	if _, err := os.Stat(demande); err == nil {
		t.Error("même version redemandée la nuit")
	}
	var n int
	_ = b.db.QueryRow(`SELECT COUNT(*) FROM audit WHERE action = 'maj_serveur_nuit'`).Scan(&n)
	if n != 2 {
		t.Errorf("réglage de nuit dans l'audit : %d lignes", n)
	}
	// Désactivation.
	b.post(c, "/reglages/serveur", url.Values{"csrf": {b.csrf(c, "/reglages/serveur")}, "action": {"nuit"}, "code": {b.code(secret)}})
	if lireMajNuit(ctx, b.db) != -1 {
		t.Error("désactivation non enregistrée")
	}
}
