// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"mirador-stack/internal/server/notif"
)

// Page « Modèles des messages » (SPEC § 82) : modèle de départ, aperçu sans
// enregistrer, erreur signalée, enregistrement par canal, retour au défaut.
func TestPageModelesMessages(t *testing.T) {
	b := nouveauBanc(t)
	c, _ := b.premierAdmin("admin", mdpAdmin)
	ctx := context.Background()
	_, corps, _ := b.get(c, "/reglages/notifications/modeles?type=alerte")
	for _, attendu := range []string{"Modèle par défaut de mirador", `name="corps"`, "Aperçu (alertes d", `data-onglet="ntfy"`, "<b>Disque presque plein</b>", "Résolution"} {
		if !strings.Contains(corps, attendu) {
			t.Errorf("page : %q absent", attendu)
		}
	}
	csrf := b.csrf(c, "/reglages/notifications/modeles")
	// Aperçu d'un modèle saisi : rendu, rien d'enregistré.
	_, corps, _ = b.post(c, "/reglages/notifications/modeles", url.Values{"csrf": {csrf}, "action": {"apercu"}, "type": {"alerte"},
		"titre": {"{{.Nb}} pépins"}, "corps": {"{{range .Alertes}}* {{.Titre}} <{{.Machine}}>\n{{end}}"}})
	if !strings.Contains(corps, "<b>2 pépins</b>") || !strings.Contains(corps, "* Disque presque plein &lt;nas&gt;") {
		i := strings.Index(corps, "apercu-telegram")
		t.Errorf("aperçu :\n%s", corps[max(i, 0):min(len(corps), max(i, 0)+600)])
	}
	if r, _ := notif.LireReglages(ctx, b.db); len(r.Modeles) != 0 {
		t.Fatal("aperçu enregistré")
	}
	// Erreur : signalée, pas enregistrée.
	_, corps, _ = b.post(c, "/reglages/notifications/modeles", url.Values{"csrf": {csrf}, "action": {"enregistrer"}, "type": {"alerte"},
		"corps": {"{{range .Alertes}}"}})
	if !strings.Contains(corps, "Modèle non enregistré") {
		t.Error("erreur non signalée")
	}
	// Enregistrement pour Telegram seulement.
	b.post(c, "/reglages/notifications/modeles", url.Values{"csrf": {csrf}, "action": {"enregistrer"}, "type": {"rappel"}, "canal": {"telegram"},
		"titre": {""}, "corps": {"Toujours là : {{.Nb}}"}})
	r, _ := notif.LireReglages(ctx, b.db)
	if r.Modeles["rappel:telegram"].Corps != "Toujours là : {{.Nb}}" {
		t.Fatalf("modèles : %+v", r.Modeles)
	}
	if _, corps, _ = b.get(c, "/reglages/notifications/modeles?type=rappel&canal=telegram"); !strings.Contains(corps, "Modèle personnalisé") {
		t.Error("modèle personnalisé non montré")
	}
	if _, corps, _ = b.get(c, "/reglages/notifications"); !strings.Contains(corps, "1 modèle(s) personnalisé(s)") {
		t.Error("lien des modèles absent de Notifications")
	}
	// Retour au défaut.
	b.post(c, "/reglages/notifications/modeles", url.Values{"csrf": {csrf}, "action": {"defaut"}, "type": {"rappel"}, "canal": {"telegram"}})
	if r, _ = notif.LireReglages(ctx, b.db); len(r.Modeles) != 0 {
		t.Errorf("modèle non retiré : %+v", r.Modeles)
	}
}
