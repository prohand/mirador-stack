// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"mirador-stack/internal/protocole"
	"mirador-stack/internal/server/auth"
	"mirador-stack/internal/server/direct"
	"mirador-stack/internal/server/stockage"
	"mirador-stack/internal/server/stockage/journaux"
)

// Tests des corrections de la revue de sécurité du 28/11.

// sessionDe : la session en mémoire du compte (nil si aucune).
func (b *banc) sessionDe(nom string) *auth.Session {
	for _, s := range b.c.sessions.Lister() {
		if s.Nom == nom {
			return &s
		}
	}
	return nil
}

// B1 : après 12 h (durée maximale ordinaire), le nettoyage des 5 s ne
// retire plus la session d'un appareil de confiance : ses compteurs
// survivent, et 5 codes faux espacés ferment bien la session et oublient
// l'appareil.
func TestConfianceSurvitAuNettoyage(t *testing.T) {
	b := nouveauBanc(t)
	_, secret := b.premierAdmin("admin", mdpAdmin)
	c, _ := b.connecterConfiance("admin", mdpAdmin, secret)

	// 13 h d'usage : une requête par heure (pas d'inactivité), un tour de
	// nettoyage à chaque fois.
	for i := 0; i < 13; i++ {
		b.h.avancer(time.Hour)
		if code, _, _ := b.get(c, "/"); code != http.StatusOK {
			t.Fatalf("heure %d : %d", i+1, code)
		}
		b.c.Tour(context.Background())
	}
	s := b.sessionDe("admin")
	if s == nil || !s.Confiance {
		t.Fatal("session de confiance retirée par le nettoyage après 12 h")
	}
	// Mode administration ouvert : il doit survivre aux tours suivants.
	jusqua := b.h.maintenant().Add(time.Hour)
	b.c.sessions.Modifier(s.Empreinte, func(x *auth.Session) { x.AdminJusqua = jusqua })

	for i := 1; i <= 5; i++ {
		b.h.avancer(6 * time.Second) // plus d'un tour de boucle entre deux essais
		b.c.Tour(context.Background())
		code, corps, _ := b.post(c, "/mode-admin/ouvrir", url.Values{"csrf": {b.csrf(c, "/compte")}, "code": {"000000"}, "retour": {"/compte"}})
		if code != http.StatusSeeOther {
			t.Fatalf("essai %d : %d %s", i, code, corps)
		}
		if i < 5 {
			s := b.sessionDe("admin")
			if s == nil || s.EssaisSensibles != i || !s.AdminJusqua.Equal(jusqua) {
				t.Fatalf("essai %d : compteurs perdus (%+v)", i, s)
			}
		}
	}
	if b.sessionDe("admin") != nil {
		t.Fatal("session encore ouverte après 5 codes faux")
	}
	if b.nbAppareils() != 0 {
		t.Fatal("appareil de confiance pas oublié après 5 codes faux")
	}
	if code, _, _ := b.get(c, "/"); code != http.StatusSeeOther {
		t.Fatalf("session revenue depuis le disque : %d", code)
	}
}

// lireEvenement : prochain évènement d'un flux (« nom données »), "" si le
// flux est fermé ; échec après 5 s.
func lireEvenement(t *testing.T, lu *bufio.Reader) string {
	t.Helper()
	fini := make(chan string, 1)
	go func() {
		ev := ""
		for {
			l, err := lu.ReadString('\n')
			if err != nil {
				fini <- ""
				return
			}
			if strings.HasPrefix(l, "event: ") {
				ev = strings.TrimSpace(l[7:])
			}
			if strings.HasPrefix(l, "data: ") {
				fini <- ev + " " + strings.TrimSpace(l[6:])
				return
			}
		}
	}()
	select {
	case e := <-fini:
		return e
	case <-time.After(5 * time.Second):
		t.Fatal("flux muet")
		return ""
	}
}

// fluxFerme : le serveur a fermé le flux (fin du corps).
func fluxFerme(t *testing.T, lu *bufio.Reader) {
	t.Helper()
	fini := make(chan error, 1)
	go func() { _, err := io.Copy(io.Discard, lu); fini <- err }()
	select {
	case <-fini:
	case <-time.After(5 * time.Second):
		t.Fatal("flux toujours ouvert")
	}
}

// B2 : un flux de logs en direct est coupé quand la session est fermée, ou
// quand le compte est désactivé.
func TestFluxLogsCoupeSessionFermee(t *testing.T) {
	ancien := verifFlux
	verifFlux = 0 // vérification à chaque tour (test)
	t.Cleanup(func() { verifFlux = ancien })
	b := nouveauBanc(t)
	m := b.h.maintenant()
	jx, err := journaux.Ouvrir(journaux.Options{Dossier: t.TempDir(), Maintenant: b.h.maintenant})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = jx.Fermer() })
	b.c.opt.Logs = jx
	c, secret := b.premierAdmin("admin", mdpAdmin)
	depuis := strconv.FormatInt(m.Add(-10*time.Minute).UnixMilli(), 10)
	ecrire := func(msg string) {
		_ = jx.Ecrire(context.Background(), []stockage.Ligne{{T: b.h.maintenant().UnixMilli(), Msg: msg, Champs: map[string]string{"host": "web1"}}})
	}
	ouvrir := func(c *http.Client) (*bufio.Reader, func()) {
		resp, err := c.Get(b.srv.URL + "/logs/flux?host=web1&periode=1h&depuis=" + depuis)
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("flux : %v %v", err, resp)
		}
		return bufio.NewReader(resp.Body), func() { resp.Body.Close() }
	}

	// 1. Session fermée (comme « fermer la session » ou 5 codes faux).
	ecrire("premiere")
	lu, fermer := ouvrir(c)
	defer fermer()
	if e := lireEvenement(t, lu); !strings.Contains(e, "premiere") {
		t.Fatalf("premier envoi : %s", e)
	}
	b.c.sessions.FermerUtilisateur(1, "")
	ecrire("apres fermeture")
	if e := lireEvenement(t, lu); e != `fin {"raison":"session"}` {
		t.Fatalf("après fermeture de la session : %q", e)
	}
	fluxFerme(t, lu)

	// 2. Compte désactivé (par une commande : seule la base change).
	c = b.connecter("admin", mdpAdmin, secret)
	ecrire("deuxieme")
	lu, fermer2 := ouvrir(c)
	defer fermer2()
	if e := lireEvenement(t, lu); !strings.HasPrefix(e, "lignes ") {
		t.Fatalf("deuxième flux : %s", e)
	}
	if _, err := b.db.Exec(`UPDATE utilisateurs SET desactive = 1`); err != nil {
		t.Fatal(err)
	}
	b.c.oublierComptes()
	ecrire("apres desactivation")
	if e := lireEvenement(t, lu); e != `fin {"raison":"session"}` {
		t.Fatalf("après désactivation : %q", e)
	}
	fluxFerme(t, lu)
}

// B2 : relevés en direct coupés quand la génération du compte change (mot
// de passe changé ailleurs).
func TestFluxDirectCoupeGeneration(t *testing.T) {
	ancien := verifFlux
	verifFlux = 0
	t.Cleanup(func() { verifFlux = ancien })
	b := nouveauBanc(t)
	hub := direct.Nouveau()
	b.c.opt.Direct = hub
	if _, err := b.c.db.Exec(`INSERT INTO machines(nom, statut, type, cree_le, capacites, version_agent) VALUES
		('web1', 'approuvee', 'agent', 1, 'surveillance,direct', '0.6.13')`); err != nil {
		t.Fatal(err)
	}
	var id int64
	_ = b.c.db.QueryRow(`SELECT id FROM machines WHERE nom = 'web1'`).Scan(&id)
	c, _ := b.premierAdmin("admin", mdpAdmin)
	resp, err := c.Get(b.srv.URL + "/machines/web1/direct")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	lu := bufio.NewReader(resp.Body)
	if e := lireEvenement(t, lu); !strings.HasPrefix(e, "debut ") {
		t.Fatalf("premier évènement : %s", e)
	}
	hub.Recevoir(id, protocole.Direct{CPU: 10})
	if e := lireEvenement(t, lu); !strings.HasPrefix(e, "point ") {
		t.Fatalf("relevé : %s", e)
	}
	if _, err := b.db.Exec(`UPDATE utilisateurs SET generation = generation + 1`); err != nil {
		t.Fatal(err)
	}
	b.c.oublierComptes()
	// Le relevé déjà attendu peut encore partir ; le flux s'arrête au tour
	// suivant.
	hub.Recevoir(id, protocole.Direct{CPU: 20})
	e := lireEvenement(t, lu)
	if strings.HasPrefix(e, "point ") {
		hub.Recevoir(id, protocole.Direct{CPU: 30})
		e = lireEvenement(t, lu)
	}
	if e != `fin {"raison":"session"}` {
		t.Fatalf("après changement de génération : %q", e)
	}
	fluxFerme(t, lu)
}
