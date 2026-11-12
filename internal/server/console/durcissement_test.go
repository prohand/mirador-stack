// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"mirador-stack/internal/server/auth"
	"net/http"
	"net/url"
	"testing"
	"time"
)

// Trois sessions « avant le code » au plus par compte : la plus ancienne
// est fermée.
func TestSessionsPartiellesLimitees(t *testing.T) {
	b := nouveauBanc(t)
	b.premierAdmin("admin", mdpAdmin)
	b.h.avancer(16 * time.Minute)
	var clients []*http.Client
	for i := 0; i < 5; i++ {
		c := b.client()
		if code, _, _ := b.post(c, "/connexion", url.Values{"nom": {"admin"}, "mot_de_passe": {mdpAdmin}}); code != http.StatusSeeOther {
			t.Fatalf("mot de passe %d : %d", i, code)
		}
		clients = append(clients, c)
		b.h.avancer(time.Second)
	}
	for i, c := range clients {
		code, _, _ := b.get(c, "/connexion/code")
		if ouverte := code == http.StatusOK; ouverte != (i >= 2) {
			t.Errorf("session partielle %d : ouverte = %v", i, ouverte)
		}
	}
	// La session complète de l'admin n'est pas touchée.
	n := 0
	for _, s := range b.c.sessions.Lister() {
		if s.Nom == "admin" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d session(s) complète(s), 1 attendue", n)
	}
}

// Mot de passe trop simple : refusé à la création d'un compte et au
// changement de mot de passe (règles testées dans le paquet auth).
func TestMotDePasseSimpleBranche(t *testing.T) {
	b := nouveauBanc(t)
	if err := auth.CreerUtilisateur(t.Context(), b.db, "lea", auth.RoleLecture, "aaaaaaaaaaaa", "test", "", b.h.maintenant()); err == nil {
		t.Error("création avec un mot de passe trop simple")
	}
	if err := auth.CreerUtilisateur(t.Context(), b.db, "lea", auth.RoleAdmin, mdpAdmin, "test", "", b.h.maintenant()); err != nil {
		t.Fatal(err)
	}
	if err := auth.DefinirMotDePasse(t.Context(), b.db, "lea", "lea-12345678", "test", "", b.h.maintenant()); err == nil {
		t.Error("changement vers un mot de passe trop simple")
	}
}
