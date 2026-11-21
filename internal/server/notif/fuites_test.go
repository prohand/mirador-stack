// SPDX-License-Identifier: GPL-3.0-or-later

package notif

import (
	"context"
	"fmt"
	"testing"
	"time"

	"mirador-stack/internal/server/alertes"
)

// Relecture du 21/11 : « escaladees » vidée quand l'alerte se termine.
func TestEscaladeesOubliees(t *testing.T) {
	b := nouveauBanc(t)
	ctx := context.Background()
	r, _ := LireReglages(ctx, b.db)
	ro := r.Routes[alertes.Critique]
	ro.Escalade = alertes.Duree(15 * time.Minute)
	r.Routes[alertes.Critique] = ro
	if err := EcrireReglages(ctx, b.db, r, cleTest, "test", b.horloge); err != nil {
		t.Fatal(err)
	}
	const n = 300
	for i := 0; i < n; i++ {
		a := alerte(int64(1000+i), "CpuEleve", alertes.Critique, fmt.Sprintf("pc%d", i), b.horloge)
		b.debut(a)
		b.avancer(time.Minute)
		b.avancer(16 * time.Minute) // escalade
		b.fin(a)
		b.avancer(time.Minute) // fin envoyée
	}
	b.g.mu.Lock()
	defer b.g.mu.Unlock()
	t.Logf("après %d alertes escaladées puis résolues : escaladees=%d groupes=%d aRappeler=%d deplacees=%d",
		n, len(b.g.escaladees), len(b.g.groupes), len(b.g.aRappeler), len(b.g.deplacees))
	if len(b.g.escaladees) > 1 { // la dernière attend encore l'envoi de sa fin
		t.Errorf("escaladees garde %d entrées pour des alertes terminées", len(b.g.escaladees))
	}
}

// Relecture du 21/11 : regroupement changé pendant qu'une alerte est en cours : sa fin tombe
// dans un autre groupe, le suivi d'origine restait à vie (et était rappelé).
func TestRegroupementChangeFinAnnoncee(t *testing.T) {
	b := nouveauBanc(t)
	ctx := context.Background()
	a := alerte(7, "CpuEleve", alertes.Critique, "pc1", b.horloge)
	b.debut(a)
	if m := b.avancer(time.Minute); len(m) != 1 {
		t.Fatalf("premier envoi : %d", len(m))
	}
	r, _ := LireReglages(ctx, b.db)
	r.GrouperPar = []string{"alertname"}
	if err := EcrireReglages(ctx, b.db, r, cleTest, "test", b.horloge); err != nil {
		t.Fatal(err)
	}
	b.avancer(time.Minute) // relecture des réglages
	b.fin(a)
	// Sur 48 h : un seul message, l'annonce de la fin ; aucun rappel.
	var msgs []*Message
	msgs = append(msgs, b.avancer(time.Minute)...)
	for i := 0; i < 48; i++ {
		msgs = append(msgs, b.avancer(time.Hour)...)
	}
	if len(msgs) != 1 || msgs[0].Type != TypeResolution || len(msgs[0].Resolues) != 1 {
		for _, x := range msgs {
			t.Logf("%s actives=%d resolues=%d", x.Type, len(x.Actives), len(x.Resolues))
		}
		t.Errorf("attendu : la seule annonce de la fin ; reçu %d message(s)", len(msgs))
	}
	b.g.mu.Lock()
	n := 0
	for _, g := range b.g.groupes {
		n += len(g.alertes)
	}
	b.g.mu.Unlock()
	if n > 0 {
		t.Errorf("%d suivi(s) d'alerte terminée gardé(s)", n)
	}
}
