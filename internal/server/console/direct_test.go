// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"bufio"
	"net/http"
	"strings"
	"testing"
	"time"

	"mirador-stack/internal/protocole"
	"mirador-stack/internal/server/direct"
)

// Direct (SPEC § 97), côté page : bloc sur la page d'une machine, flux
// réservé aux personnes connectées, relevés reçus, page fermée = plus
// d'abonné ; agent trop ancien : refus et message.
func TestFluxDirect(t *testing.T) {
	b := nouveauBanc(t)
	hub := direct.Nouveau()
	b.c.opt.Direct = hub
	if _, err := b.c.db.Exec(`INSERT INTO machines(nom, statut, type, cree_le, capacites) VALUES
		('web1', 'approuvee', 'agent', 1, 'surveillance,direct'), ('vieux', 'approuvee', 'agent', 1, 'surveillance')`); err != nil {
		t.Fatal(err)
	}
	var id int64
	_ = b.c.db.QueryRow(`SELECT id FROM machines WHERE nom = 'web1'`).Scan(&id)
	// Sans session : refusé.
	if code, _, _ := b.get(b.client(), "/machines/web1/direct"); code == http.StatusOK {
		t.Fatal("flux sans connexion")
	}
	c, _ := b.premierAdmin("admin", "une longue phrase de passe")
	if _, corps, _ := b.get(c, "/machines/web1"); !strings.Contains(corps, `data-direct="/machines/web1/direct"`) {
		t.Error("bloc « En direct » absent")
	}
	if _, corps, _ := b.get(c, "/machines/vieux"); !strings.Contains(corps, "agent trop ancien pour le direct") {
		t.Error("agent trop ancien : message absent")
	}
	if code, _, _ := b.get(c, "/machines/vieux/direct"); code != http.StatusConflict {
		t.Errorf("flux d'un agent trop ancien : %d", code)
	}
	resp, err := c.Get(b.srv.URL + "/machines/web1/direct")
	if err != nil {
		t.Fatal(err)
	}
	if resp.Header.Get("Content-Type") != "text/event-stream; charset=utf-8" || resp.Header.Get("X-Accel-Buffering") != "no" {
		t.Errorf("en-têtes : %v", resp.Header)
	}
	lu := bufio.NewReader(resp.Body)
	lire := func() string {
		var ev string
		for {
			l, err := lu.ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(l, "event: ") {
				ev = strings.TrimSpace(l[7:])
			}
			if strings.HasPrefix(l, "data: ") {
				return ev + " " + strings.TrimSpace(l[6:])
			}
		}
	}
	if e := lire(); !strings.HasPrefix(e, "debut ") {
		t.Fatalf("premier évènement : %s", e)
	}
	if hub.Reste(id) == 0 || hub.NbAbonnes() != 1 {
		t.Fatal("direct non demandé à l'agent")
	}
	hub.Recevoir(id, protocole.Direct{CPU: 33, Processus: []protocole.ProcessusDirect{{PID: 9, Nom: "<script>"}}})
	if e := lire(); !strings.HasPrefix(e, "point ") || !strings.Contains(e, `"cpu":33`) || strings.Contains(e, "<script>") {
		t.Errorf("relevé : %s", e) // JSON : « < » échappé
	}
	resp.Body.Close()
	for i := 0; i < 50 && hub.NbAbonnes() != 0; i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if hub.NbAbonnes() != 0 {
		t.Error("page fermée : toujours abonnée")
	}
}

// Collecteur (retour du 22/11 : « En ce moment n'est pas en direct sur le
// collecteur ») : direct dès qu'il annonce la capacité, sinon les moyennes,
// sans message « agent trop ancien ».
func TestDirectCollecteur(t *testing.T) {
	b := nouveauBanc(t)
	b.c.opt.Direct = direct.Nouveau()
	if _, err := b.c.db.Exec(`INSERT INTO machines(nom, statut, type, cree_le, version_agent, capacites) VALUES
		('col-neuf', 'approuvee', 'collecteur', 1, '0.6.13', 'collecteur,sondes,sonder,direct'), ('col-vieux', 'approuvee', 'collecteur', 1, '0.6.12', 'collecteur,sondes,sonder')`); err != nil {
		t.Fatal(err)
	}
	c, _ := b.premierAdmin("admin", "une longue phrase de passe")
	if _, corps, _ := b.get(c, "/machines/col-neuf"); !strings.Contains(corps, `data-direct="/machines/col-neuf/direct"`) {
		t.Error("collecteur récent : bloc « En direct » absent")
	}
	if _, corps, _ := b.get(c, "/machines/col-vieux"); strings.Contains(corps, "data-direct=") || strings.Contains(corps, "trop ancien pour le direct") {
		t.Error("collecteur ancien : direct proposé ou message d'agent")
	}
}
