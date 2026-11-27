// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"
)

// Résumé de la semaine (demande du 27/11) : lignes utiles seulement, un
// envoi par semaine au jour et à l'heure réglés, désactivable.
func TestResumeSemaine(t *testing.T) {
	b := nouveauBanc(t)
	b.metriquesSimulees(t, []serieTest{
		{[]string{"__name__", "apt_upgrades_security_pending", "instance", "web1"}, constante(3)},
		{[]string{"__name__", "node_reboot_required", "instance", "web1"}, constante(1)},
	})
	b.machinesApprouvees(t, "web1")
	m := b.h.maintenant()
	inserer := func(regle, gravite, etq string, debut, fin time.Time) {
		f := int64(0)
		if !fin.IsZero() {
			f = fin.UnixMilli()
		}
		if _, err := b.c.db.Exec(`INSERT INTO alertes(regle, etiquettes, gravite, etat, debut, active_le, fin) VALUES (?, ?, ?, 'resolue', ?, ?, ?)`,
			regle, etq, gravite, debut.UnixMilli(), debut.UnixMilli(), f); err != nil {
			t.Fatal(err)
		}
	}
	inserer("DisquePresquePlein", "critical", `{"alertname":"DisquePresquePlein","instance":"nas"}`, m.Add(-50*time.Hour), m.Add(-47*time.Hour))
	inserer("TacheEnEchec", "warning", `{"alertname":"TacheEnEchec","instance":"pve1","nom":"vzdump"}`, m.Add(-20*time.Hour), m.Add(-19*time.Hour))
	inserer("CpuEleve", "warning", `{"alertname":"CpuEleve","instance":"vieux"}`, m.Add(-9*24*time.Hour), m.Add(-9*24*time.Hour+time.Hour)) // hors semaine

	txt := b.c.texteResume(context.Background(), m)
	t.Log("\n" + txt)
	for _, attendu := range []string{"📋 mirador · semaine du", "🔔 2 alertes (1 critique) · la plus longue : nas · ",
		"🗄️ Tâches et sauvegardes en échec ou en retard : 1 (pve1 · vzdump)",
		"🔒 Mises à jour de sécurité : 1 machine · redémarrage requis : 1"} {
		if !strings.Contains(txt, attendu) {
			t.Errorf("résumé : %q absent\n%s", attendu, txt)
		}
	}
	if strings.Contains(txt, "vieux") || strings.Contains(txt, "💾") {
		t.Errorf("ligne en trop :\n%s", txt)
	}

	// Envoi : une fois le jour réglé, après l'heure réglée.
	var envois []string
	b.c.informer = func(_ context.Context, _, texte string) bool { envois = append(envois, texte); return true }
	l := m.In(fuseau.Load())
	jour := int(l.Weekday())
	if jour == 0 {
		jour = 7
	}
	if err := b.c.ecrireResume(context.Background(), ReglageResume{Actif: true, Jour: jour, Heure: "00:00"}, "test", "", m); err != nil {
		t.Fatal(err)
	}
	b.c.tourResume(context.Background(), m)
	b.c.tourResume(context.Background(), m.Add(time.Minute))
	if len(envois) != 1 {
		t.Fatalf("envois le jour réglé : %d", len(envois))
	}
	// Autre jour : rien.
	autre := jour%7 + 1
	_ = b.c.ecrireResume(context.Background(), ReglageResume{Actif: true, Jour: autre, Heure: "00:00"}, "test", "", m)
	_, _ = b.c.db.Exec(`DELETE FROM reglages WHERE cle = ?`, cleResumeDernier)
	b.c.tourResume(context.Background(), m)
	// Désactivé : rien.
	_ = b.c.ecrireResume(context.Background(), ReglageResume{Actif: false, Jour: jour, Heure: "00:00"}, "test", "", m)
	b.c.tourResume(context.Background(), m)
	if len(envois) != 1 {
		t.Fatalf("envoi un autre jour ou désactivé : %d", len(envois))
	}
	if err := b.c.ecrireResume(context.Background(), ReglageResume{Actif: true, Jour: 8, Heure: "08:00"}, "test", "", m); err == nil {
		t.Error("jour 8 accepté")
	}

	// Page Notifications : réglage et bouton d'essai.
	c, _ := b.premierAdmin("admin", mdpAdmin)
	if _, corps, _ := b.get(c, "/reglages/notifications"); !strings.Contains(corps, "Résumé de la semaine") || !strings.Contains(corps, `value="resume-essai"`) {
		t.Error("carte « Résumé de la semaine » absente")
	}
	b.post(c, "/reglages/notifications", url.Values{"csrf": {b.csrf(c, "/reglages/notifications")}, "action": {"resume"}, "actif": {"1"}, "jour": {"3"}, "heure": {"09:30"}})
	if r := b.c.lireResume(context.Background()); !r.Actif || r.Jour != 3 || r.Heure != "09:30" {
		t.Errorf("réglage enregistré : %+v", r)
	}
	b.post(c, "/reglages/notifications", url.Values{"csrf": {b.csrf(c, "/reglages/notifications")}, "action": {"resume-essai"}})
	if len(envois) != 2 {
		t.Errorf("« Envoyer maintenant » : %d envois", len(envois))
	}
}
