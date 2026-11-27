// SPDX-License-Identifier: GPL-3.0-or-later

package notif

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"mirador-stack/internal/server/alertes"
)

type canalLent struct {
	debut, suite chan struct{}
}

func (c *canalLent) Nom() string { return NomTelegram }
func (c *canalLent) Envoyer(context.Context, *Message, Mise) error {
	c.debut <- struct{}{}
	<-c.suite
	return errors.New("échec")
}
func (c *canalLent) Verifier(context.Context) error { return nil }

// Revue du 27/11 (corrigé) : le secours est retiré des réglages pendant un envoi : p.etats est
// remplacé, l'envoi relit p.etats[nom] (nil) et plante.
func TestRevueSecoursRetirePendantEnvoi(t *testing.T) {
	ctx := context.Background()
	db := baseTest(t)
	h := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	r := ReglagesDefaut()
	r.Mode = Actif
	r.Canaux.Telegram = &Telegram{Actif: true, Jeton: jetonTest, Chat: "-1001234"}
	r.Canaux.Ntfy = &Ntfy{Actif: true, URL: "https://ntfy.example.org/mirador"}
	r.Canaux.Secours = NomNtfy
	if err := EcrireReglages(ctx, db, r, cleTest, "test", h); err != nil {
		t.Fatal(err)
	}
	tg := &canalLent{debut: make(chan struct{}), suite: make(chan struct{})}
	nt := &canalSimule{nom: NomNtfy}
	p, err := NouveauPostier(ctx, OptionsPostier{DB: db, Cle: cleTest, Maintenant: func() time.Time { return h },
		DerniereEvaluationOK: func() time.Time { return h },
		Fabrique: func(c *Canaux, _ []byte) (map[string]Canal, error) {
			m := map[string]Canal{NomTelegram: tg}
			if c.Ntfy != nil && c.Ntfy.Actif {
				m[NomNtfy] = nt
			}
			return m, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	fin := make(chan string)
	go func() {
		defer func() {
			if x := recover(); x != nil {
				fin <- fmt.Sprint("PANIQUE : ", x)
				return
			}
			fin <- ""
		}()
		a := alertes.Alerte{ID: 1, Regle: "A", Gravite: alertes.Critique, Etat: alertes.Active, ActiveLe: h}
		p.envoyer(ctx, &Message{Le: h, Groupe: "A", Gravite: alertes.Critique, Type: TypeAlerte, Actives: []alertes.Alerte{a}, Nouvelles: 1}, true)
	}()
	<-tg.debut
	// Pendant l'envoi : l'admin retire ntfy (autre goroutine : Tour, Prevenir…).
	r.Canaux.Ntfy, r.Canaux.Secours = nil, ""
	if err := EcrireReglages(ctx, db, r, cleTest, "test", h); err != nil {
		t.Fatal(err)
	}
	if err := p.recharger(ctx); err != nil {
		t.Fatal(err)
	}
	close(tg.suite)
	if msg := <-fin; msg != "" {
		t.Fatal(msg)
	}
}
