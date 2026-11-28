// SPDX-License-Identifier: GPL-3.0-or-later

package flotte

import (
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	"mirador-stack/internal/protocole"
)

// Revue de sécurité du 28/11.

// B8 : « mirador-server » (instance des mesures internes du serveur) est
// refusé comme nom de machine, quelle que soit la casse.
func TestNomMiradorServerReserve(t *testing.T) {
	for _, n := range []string{"mirador-server", "Mirador-Server", "MIRADOR-SERVER"} {
		if !NomReserve(n) {
			t.Errorf("%s non réservé", n)
		}
	}
	b := nouveauBanc(t)
	if _, err := TokenManuel(ctx, b.db, "Mirador-Server", "admin", t0); err == nil {
		t.Error("machine « Mirador-Server » créée")
	}
	if _, err := TokenManuel(ctx, b.db, "mirador-server-2", "admin", t0); err != nil {
		t.Errorf("nom voisin refusé : %v", err)
	}
}

// B9 : machine révoquée pendant une demande de configuration gardée
// ouverte : pas de configuration à la fin de l'attente (même avec le token
// encore valable dans le cache).
func TestConfigRevoqueePendantAttente(t *testing.T) {
	b := nouveauBanc(t)
	token, _ := TokenManuel(ctx, b.db, "pc", "admin", t0)
	id, _ := IDMachine(ctx, b.db, "pc")
	w := b.requete(http.MethodGet, protocole.CheminConfig, token, nil, "")
	if w.Code != http.StatusOK {
		t.Fatal(w.Code)
	}
	etag := w.Header().Get("ETag")
	go func() {
		time.Sleep(200 * time.Millisecond)
		// Révocation en base sans vider le cache (le pire cas), puis un
		// changement de configuration qui réveille l'attente.
		_, _ = b.db.Exec(`UPDATE tokens SET revoque_le = 1 WHERE machine_id = ?`, id)
		_ = DefinirConfig(ctx, b.db, "machine:"+strconv.FormatInt(id, 10), []byte(`{"intervalle":"60s"}`), "admin", t0)
	}()
	w = b.requete(http.MethodGet, protocole.CheminConfig, token, nil, "", "If-None-Match", etag, protocole.EnteteAttendre, "3")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("après révocation pendant l'attente : %d %s", w.Code, w.Body)
	}
}

// B10 : une machine réveille le moteur d'alertes au plus une fois par
// minute ; la première fois tout de suite, les autres machines aussi.
func TestReveilsParMachine(t *testing.T) {
	var l limitePannes
	if !l.reveilPermis(1, t0) {
		t.Fatal("premier réveil refusé")
	}
	if l.reveilPermis(1, t0.Add(10*time.Second)) {
		t.Error("deuxième réveil dans la minute permis")
	}
	if !l.reveilPermis(2, t0.Add(10*time.Second)) {
		t.Error("autre machine bloquée")
	}
	if !l.reveilPermis(1, t0.Add(61*time.Second)) {
		t.Error("réveil refusé après une minute")
	}
	// Mémoire bornée : les vieilles entrées partent.
	for i := int64(10); i < 1200; i++ {
		l.reveilPermis(i, t0.Add(time.Duration(i)*time.Minute))
	}
	if len(l.reveils) > 1001 {
		t.Errorf("%d entrées gardées", len(l.reveils))
	}
}

// M6 : une machine qui signale des centaines d'adresses nouvelles n'évince
// plus l'alerte d'une autre machine, et garde 20 alertes au plus.
func TestSSHNouvellesEquitable(t *testing.T) {
	b := nouveauBanc(t)
	ev := func(ip string) protocole.EvenementAgent {
		return protocole.EvenementAgent{Type: protocole.EvenementSSH, Objet: ip, Texte: "root · password"}
	}
	// Machines au nom « avant » web1 dans l'ordre alphabétique : celles que
	// l'ancien LIMIT sans ordre favorisait.
	for _, pirate := range []string{"aaa", "abc"} {
		for i := 0; i < 300; i++ {
			if _, err := NoterConnexionSSH(ctx, b.db, 1, pirate, ev(fmt.Sprintf("203.0.%d.%d", i/250, i%250+1)), t0); err != nil {
				t.Fatal(err)
			}
		}
	}
	if n, err := NoterConnexionSSH(ctx, b.db, 2, "web1", ev("198.51.100.23"), t0.Add(time.Minute)); err != nil || !n {
		t.Fatalf("adresse nouvelle de web1 : %v %v", n, err)
	}
	l, err := SSHNouvelles(ctx, b.db, t0.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	par := map[string]int{}
	for _, c := range l {
		par[c.Machine]++
	}
	if par["web1"] != 1 {
		t.Errorf("alerte de web1 évincée : %v", par)
	}
	if par["aaa"] != maxNouvellesParMachine || par["abc"] != maxNouvellesParMachine {
		t.Errorf("plafond par machine : %v", par)
	}
	// Les plus anciennes sont gardées : la liste ne change pas d'une minute
	// à l'autre (pas d'alerte qui se ferme et se rouvre).
	l2, _ := SSHNouvelles(ctx, b.db, t0.Add(3*time.Minute))
	if fmt.Sprint(l) != fmt.Sprint(l2) {
		t.Error("sélection instable d'une minute à l'autre")
	}
}
