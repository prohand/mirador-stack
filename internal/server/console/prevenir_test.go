// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// ecouter remplace l'envoi des messages de sécurité par un canal de test.
func (b *banc) ecouter() chan string {
	recus := make(chan string, 100)
	b.c.prevenir = func(_ context.Context, _, texte string) { recus <- texte }
	return recus
}

// attendre : prochain message (l'envoi se fait à part), "" s'il n'y en a pas.
func attendre(recus chan string) string {
	select {
	case m := <-recus:
		return m
	case <-time.After(300 * time.Millisecond):
		return ""
	}
}

func TestMessagesSecurite(t *testing.T) {
	b := nouveauBanc(t)
	recus := b.ecouter()

	// Première connexion : adresse nouvelle pour ce compte.
	_, secret := b.premierAdmin("admin", mdpAdmin)
	if m := attendre(recus); !strings.Contains(m, "nouvelle adresse") || !strings.Contains(m, "admin") {
		t.Fatalf("connexion depuis une nouvelle adresse : message %q", m)
	}
	// Même adresse ensuite : rien.
	b.connecter("admin", mdpAdmin, secret)
	if m := attendre(recus); m != "" {
		t.Fatalf("adresse déjà connue : message inattendu %q", m)
	}

	// Bon mot de passe puis code faux : un message, pas deux (1 par heure).
	b.h.avancer(16 * time.Minute)
	c := b.client()
	b.post(c, "/connexion", url.Values{"nom": {"admin"}, "mot_de_passe": {mdpAdmin}})
	csrf := b.csrf(c, "/connexion/code")
	for i := 0; i < 2; i++ {
		if code, _, _ := b.post(c, "/connexion/code", url.Values{"csrf": {csrf}, "code": {"000000"}}); code != http.StatusUnauthorized {
			t.Fatalf("code faux : %d", code)
		}
	}
	m := attendre(recus)
	if !strings.Contains(m, "code 2FA faux") || strings.Contains(m, "000000") || strings.Contains(m, mdpAdmin) {
		t.Fatalf("code 2FA faux : message %q", m)
	}
	if m := attendre(recus); m != "" {
		t.Fatalf("deuxième message dans l'heure : %q", m)
	}
	// Une heure plus tard, un nouveau message est permis.
	if b.c.messages.permis("2fa|admin", b.h.maintenant()) || !b.c.messages.permis("2fa|admin", b.h.maintenant().Add(61*time.Minute)) {
		t.Error("limite d'un message par heure mal appliquée")
	}
}

func TestMessageCompteVerrouille(t *testing.T) {
	b := nouveauBanc(t)
	b.premierAdmin("admin", mdpAdmin)
	recus := b.ecouter()
	t0 := b.h.maintenant()
	// 15 échecs venus de 15 adresses : le compte est verrouillé.
	for i := 0; i < 15; i++ {
		b.c.echecConnexion(fmt.Sprintf("203.0.113.%d", i+1), "admin", "mot de passe faux", t0)
	}
	if m := attendre(recus); !strings.Contains(m, "verrouillé") || !strings.Contains(m, "admin") {
		t.Fatalf("compte verrouillé : message %q", m)
	}
	// Compte qui n'existe pas, ou texte qui n'est pas un nom : aucun message.
	for _, nom := range []string{"personne", "Un Mot De Passe Tapé Ici"} {
		for i := 0; i < 15; i++ {
			b.c.echecConnexion(fmt.Sprintf("198.51.100.%d", i+1), nom, "compte inconnu", t0)
		}
	}
	if m := attendre(recus); m != "" {
		t.Fatalf("compte inconnu : message inattendu %q", m)
	}
}

// Un mot de passe tapé dans le champ « Nom » ne va pas dans le journal.
func TestNomJournal(t *testing.T) {
	b := nouveauBanc(t)
	b.premierAdmin("admin", mdpAdmin)
	b.h.avancer(16 * time.Minute)
	const secret = "Mon-Mot-De-Passe-Secret!"
	b.post(b.client(), "/connexion", url.Values{"nom": {secret}, "mot_de_passe": {"faux faux faux"}})
	b.post(b.client(), "/connexion", url.Values{"nom": {"admin"}, "mot_de_passe": {"faux faux faux"}})
	j := b.journal.String()
	if strings.Contains(j, secret) {
		t.Error("texte saisi dans le champ Nom écrit dans le journal")
	}
	if !strings.Contains(j, `"utilisateur":"(invalide)"`) || !strings.Contains(j, `"utilisateur":"admin","raison":"mot de passe faux"`) {
		t.Errorf("forme du journal changée :\n%s", j)
	}
}
