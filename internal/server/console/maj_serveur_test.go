// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mirador-stack/internal/majserveur"
	"mirador-stack/internal/server/flotte"
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
	_, corps, _ = b.get(c, "/reglages/serveur")
	for _, attendu := range []string{"Dernière mise à jour en échec", "0.5.0 → 99.1.0", "signature vérifiée", "retour à 0.5.0 fait"} {
		if !strings.Contains(corps, attendu) {
			t.Errorf("état : %q absent", attendu)
		}
	}
}
