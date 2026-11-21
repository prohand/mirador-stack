// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"mirador-stack/internal/server/alertes"
)

// Page Conservation (retour du 21/11) : durées en clair dans une liste,
// plafonds en Go, cartes avec prévision ; une baisse demande toujours la
// confirmation (texte qui dit ce qui sera supprimé), une hausse non.
func TestConservationPage(t *testing.T) {
	b := nouveauBanc(t)
	c, secret := b.premierAdmin("admin", mdpAdmin)
	_, corps, _ := b.get(c, "/reglages/conservation")
	for _, attendu := range []string{`<option value="7d" data-sec="604800" selected>7 jours</option>`, `<option value="365d" data-sec="31536000" selected>1 an</option>`,
		`name="max_brut_go" value="5"`, `name="max_logs_go" value="4"`, "Place sur le disque", `data-cons-carte="agrege"`, "Prévu : pas encore mesuré"} {
		if !strings.Contains(corps, attendu) {
			t.Errorf("page : %q absent", attendu)
		}
	}
	envoyer := func(v url.Values) string {
		v.Set("csrf", b.csrf(c, "/reglages/conservation"))
		v.Set("action", "enregistrer")
		_, _, h := b.post(c, "/reglages/conservation", v)
		return h.Get("Location")
	}
	base := url.Values{"brut": {"7d"}, "agrege": {"30d"}, "logs": {"30d"}, "audit": {"365d"}, "historique": {"180d"},
		"max_brut_go": {"5"}, "max_agrege_go": {"1"}, "max_logs_go": {"4"}}
	// Hausse : enregistrée tout de suite (plafond en Go, virgule acceptée).
	h := url.Values{}
	for k, v := range base {
		h[k] = v
	}
	h.Set("logs", "60d")
	h.Set("max_logs_go", "6,5")
	envoyer(h)
	cons, _ := LireConservation(context.Background(), b.db)
	if cons.Logs != alertes.Duree(60*24*time.Hour) || cons.MaxLogs != int64(6.5*(1<<30)) {
		t.Fatalf("hausse non enregistrée : %+v", cons)
	}
	// Baisse sans confirmation : refusée, rien en attente.
	r := url.Values{}
	for k, v := range h {
		r[k] = v
	}
	r.Set("logs", "14d")
	envoyer(r)
	if a, _ := LireAttente(context.Background(), b.db); a != nil {
		t.Fatal("baisse acceptée sans confirmation")
	}
	// Avec confirmation et code : programmée ; la page dit quoi en clair.
	r.Set("confirmer", "1")
	r.Set("code", b.code(secret))
	envoyer(r)
	a, _ := LireAttente(context.Background(), b.db)
	if a == nil || a.Valeur.Logs != alertes.Duree(14*24*time.Hour) {
		t.Fatalf("baisse non programmée : %+v", a)
	}
	_, corps, _ = b.get(c, "/reglages/conservation")
	if !strings.Contains(corps, "logs 60 j → 14 j") {
		t.Errorf("réduction programmée pas en clair :\n%s", corps)
	}
}

func TestPrevisionConservation(t *testing.T) {
	j := alertes.Duree(24 * time.Hour)
	if p, txt := prevision(100<<20, 30*j, 1<<30); p != 1<<30 || !strings.Contains(txt, "limité par le plafond : ~10 j gardés") {
		t.Errorf("plafond : %d %q", p, txt)
	}
	if p, txt := prevision(100<<20, 7*j, 0); p != 7*100<<20 || !strings.Contains(txt, "7 j × 100,0 Mo") {
		t.Errorf("sans plafond : %d %q", p, txt)
	}
	if o := optionsDuree(joursLogs, 45*j); o[5].Texte != "45 j (réglage actuel)" || !o[5].Choisie {
		t.Errorf("valeur hors liste : %+v", o)
	}
}
