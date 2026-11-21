// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"mirador-stack/internal/server/flotte"
	essais "mirador-stack/internal/sondes"
)

// Bouton « Vérifier maintenant » (SPEC § 108).
func TestVerifierSonde(t *testing.T) {
	b := nouveauBanc(t)
	ctx := context.Background()
	c, _ := b.premierAdmin("admin", mdpAdmin)
	if _, err := b.db.Exec(`INSERT INTO machines(nom, type, statut, cree_le, capacites) VALUES ('col-ancien', 'collecteur', 'approuvee', 1, 'collecteur,sondes')`); err != nil {
		t.Fatal(err)
	}
	if err := flotte.EnregistrerSondes(ctx, b.db, []essais.Sonde{
		{Nom: "Site public", Type: essais.HTTP, Cible: "https://exemple.fr/", Depuis: essais.ServeurLocal},
		{Nom: "Box (fibre)", Type: essais.TCP, Cible: "192.168.1.1:443", Depuis: "col-ancien"},
	}, "admin", b.h.maintenant()); err != nil {
		t.Fatal(err)
	}
	csrf := b.csrf(c, "/sondes")
	essai := func(nom string) (string, string) {
		_, _, ent := b.post(c, "/sondes/"+url.PathEscape(nom)+"/essai", url.Values{"csrf": {csrf}, "retour": {"/sondes"}})
		_, corps, _ := b.get(c, ent.Get("Location"))
		return ent.Get("Location"), corps
	}
	// Collecteur trop ancien : bouton grisé avec la raison, demande refusée.
	_, corps, _ := b.get(c, "/sondes")
	if !strings.Contains(corps, `disabled title="collecteur « col-ancien » à mettre à jour pour ce bouton"`) ||
		!strings.Contains(corps, `action="/sondes/Site%20public/essai"`) {
		t.Error("boutons « Vérifier maintenant » absents ou mal grisés")
	}
	if _, corps = essai("Box (fibre)"); !strings.Contains(corps, "à mettre à jour") {
		t.Error("demande acceptée pour un collecteur ancien")
	}
	// Sonde du serveur : demande transmise, page en mise à jour seule.
	loc, corps := essai("Site public")
	select {
	case nom := <-flotte.EssaisServeur():
		if nom != "Site public" {
			t.Fatal(nom)
		}
	case <-time.After(time.Second):
		t.Fatal("essai du serveur non demandé")
	}
	if !strings.HasPrefix(loc, "/sondes?essai=") || !strings.Contains(corps, `data-recharger="5"`) || !strings.Contains(corps, "résultat dans quelques secondes") {
		t.Errorf("retour après la demande : %q", loc)
	}
	// Deuxième demande dans les 10 s : refusée.
	if _, corps = essai("Site public"); !strings.Contains(corps, "moins de 10 s") {
		t.Error("limite de 10 s non appliquée")
	}
	// Collecteur à jour : demande gardée pour lui.
	if _, err := b.db.Exec(`UPDATE machines SET capacites = 'collecteur,sondes,sonder' WHERE nom = 'col-ancien'`); err != nil {
		t.Fatal(err)
	}
	if _, corps = essai("Box (fibre)"); !strings.Contains(corps, "résultat dans quelques secondes") {
		t.Error("demande refusée pour un collecteur à jour")
	}
	// Fiche de la sonde : bouton présent.
	if _, corps, _ = b.get(c, "/sondes/Box%20%28fibre%29"); !strings.Contains(corps, "Vérifier maintenant") {
		t.Error("bouton absent de la fiche")
	}
	// Plus tard : la page ne se recharge plus.
	b.h.avancer(time.Minute)
	if _, corps, _ = b.get(c, loc); strings.Contains(corps, `data-recharger`) {
		t.Error("rechargement après la fenêtre de 30 s")
	}
}
