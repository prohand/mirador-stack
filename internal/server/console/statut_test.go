// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"mirador-stack/internal/server/servicesmetier"
)

// Page de statut publique (SPEC § 84) : réglage, accès par le jeton
// seulement (même hors de la liste d'adresses), applications choisies
// seulement, aucun nom de machine, incidents, nouvelle adresse.
func TestPageStatut(t *testing.T) {
	b := nouveauBanc(t)
	ctx := context.Background()
	if err := servicesmetier.Enregistrer(ctx, b.c.db, []servicesmetier.Service{
		{Nom: "Nextcloud", Composants: []servicesmetier.Composant{{Sorte: servicesmetier.Machine, Nom: "nas-secret", Cle: true}}},
		{Nom: "Interne", Composants: []servicesmetier.Composant{{Sorte: servicesmetier.Machine, Nom: "pve1"}}},
	}, "test", "", b.h.maintenant()); err != nil {
		t.Fatal(err)
	}
	m := b.h.maintenant()
	for _, x := range []struct {
		regle    string
		deb, fin time.Duration
	}{{"ApplicationEnPanne", 50 * time.Hour, 49 * time.Hour}, {"ApplicationDegradee", 20 * time.Minute, 0}} {
		fin := int64(0)
		if x.fin > 0 {
			fin = m.Add(-x.fin).UnixMilli()
		}
		etat := "resolue"
		if fin == 0 {
			etat = "active"
		}
		if _, err := b.c.db.Exec(`INSERT INTO alertes(regle, etiquettes, gravite, etat, debut, active_le, fin) VALUES (?, ?, 'critical', ?, ?, ?, ?)`,
			x.regle, `{"service":"Nextcloud","instance":"nas-secret","alertname":"`+x.regle+`"}`, etat, m.Add(-x.deb).UnixMilli(), m.Add(-x.deb).UnixMilli(), fin); err != nil {
			t.Fatal(err)
		}
	}
	c, _ := b.premierAdmin("admin", mdpAdmin)
	csrf := b.csrf(c, "/reglages/interface")
	b.post(c, "/reglages/statut", url.Values{"csrf": {csrf}, "action": {"enregistrer"}, "actif": {"1"}, "titre": {"Services de la maison"},
		"applications": {"Nextcloud", "Inconnue"}})
	r := lireStatut(ctx, b.c.db)
	if !r.Actif || len(r.Jeton) < 20 || len(r.Applications) != 1 {
		t.Fatalf("réglage : %+v", r)
	}
	_, corps, _ := b.get(c, "/reglages/interface")
	if !strings.Contains(corps, "/statut/"+r.Jeton) {
		t.Error("adresse absente de Réglages › Interface")
	}
	// Liste d'adresses restreinte : la page passe quand même (jeton).
	sec := SecuriteDefaut()
	sec.IPs = "203.0.113.0/24"
	b.c.securite.Store(&sec)
	anonyme := b.client()
	code, page, ent := b.get(anonyme, "/statut/"+r.Jeton)
	if code != http.StatusOK {
		t.Fatalf("page : %d", code)
	}
	for _, attendu := range []string{"<title>Services de la maison</title>", "Nextcloud", "Résolu", "Nextcloud en panne", `href="/statut.css"`} {
		if !strings.Contains(page, attendu) {
			t.Errorf("page : %q absent", attendu)
		}
	}
	for _, interdit := range []string{"nas-secret", "Interne", "<script", "csrf", "Set-Cookie"} {
		if strings.Contains(page, interdit) {
			t.Errorf("page : %q ne doit pas paraître", interdit)
		}
	}
	if ent.Get("Set-Cookie") != "" || !strings.Contains(ent.Get("X-Robots-Tag"), "noindex") {
		t.Errorf("en-têtes : %v", ent)
	}
	if strings.Count(page, `<i class="`) != 90 {
		t.Errorf("90 jours attendus : %d", strings.Count(page, `<i class="`))
	}
	if code, css, _ := b.get(anonyme, "/statut.css"); code != http.StatusOK || !strings.Contains(css, ".jours") {
		t.Errorf("feuille de style : %d", code)
	}
	// Mauvais jeton, et rien d'autre n'est joignable.
	if code, _, _ := b.get(anonyme, "/statut/"+strings.Repeat("A", 24)); code != http.StatusNotFound {
		t.Errorf("mauvais jeton : %d", code)
	}
	if code, _, _ := b.get(anonyme, "/applications"); code != http.StatusForbidden {
		t.Errorf("reste de l'interface : %d", code)
	}
	// Nouvelle adresse : l'ancienne ne marche plus.
	b.c.securite.Store(ptr(SecuriteDefaut()))
	b.post(c, "/reglages/statut", url.Values{"csrf": {csrf}, "action": {"jeton"}})
	if code, _, _ := b.get(anonyme, "/statut/"+r.Jeton); code != http.StatusNotFound {
		t.Errorf("ancienne adresse : %d", code)
	}
	// Désactivée : 404.
	b.post(c, "/reglages/statut", url.Values{"csrf": {csrf}, "action": {"enregistrer"}, "applications": {"Nextcloud"}})
	if code, _, _ := b.get(anonyme, "/statut/"+lireStatut(ctx, b.c.db).Jeton); code != http.StatusNotFound {
		t.Errorf("désactivée : %d", code)
	}
}

func ptr[T any](v T) *T { return &v }
