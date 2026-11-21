// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"bufio"
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"mirador-stack/internal/server/stockage"
	"mirador-stack/internal/server/stockage/journaux"
)

// Logs en direct poussés (SPEC § 99) : lignes déjà là envoyées à
// l'ouverture, nouvelles lignes poussées dès leur écriture, filtres de la
// page respectés.
func TestLogsFlux(t *testing.T) {
	b := nouveauBanc(t)
	m := b.h.maintenant()
	jx, err := journaux.Ouvrir(journaux.Options{Dossier: t.TempDir(), Maintenant: b.h.maintenant})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = jx.Fermer() })
	b.c.opt.Logs = jx
	_ = jx.Ecrire(context.Background(), []stockage.Ligne{
		{T: m.Add(-2 * time.Minute).UnixMilli(), Msg: "ancienne ligne", Champs: map[string]string{"host": "web1", "unit": "nginx.service"}},
		{T: m.Add(-2 * time.Minute).UnixMilli(), Msg: "autre machine", Champs: map[string]string{"host": "web2", "unit": "nginx.service"}},
	})
	c, _ := b.premierAdmin("admin", mdpAdmin)
	depuis := strconv.FormatInt(m.Add(-10*time.Minute).UnixMilli(), 10)
	resp, err := c.Get(b.srv.URL + "/logs/flux?host=web1&periode=1h&depuis=" + depuis)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("Content-Type") != "text/event-stream; charset=utf-8" {
		t.Fatalf("type : %q", resp.Header.Get("Content-Type"))
	}
	lu := bufio.NewReader(resp.Body)
	lire := func() string {
		fini := make(chan string, 1)
		go func() {
			for {
				l, err := lu.ReadString('\n')
				if err != nil {
					fini <- ""
					return
				}
				if strings.HasPrefix(l, "data: ") {
					fini <- l
					return
				}
			}
		}()
		select {
		case l := <-fini:
			return l
		case <-time.After(5 * time.Second):
			t.Fatal("aucune ligne poussée")
			return ""
		}
	}
	if l := lire(); !strings.Contains(l, "ancienne ligne") || strings.Contains(l, "autre machine") {
		t.Fatalf("premier envoi : %s", l)
	}
	_ = jx.Ecrire(context.Background(), []stockage.Ligne{{T: m.UnixMilli(), Msg: "toute nouvelle", Champs: map[string]string{"host": "web1", "unit": "nginx.service"}}})
	if l := lire(); !strings.Contains(l, "toute nouvelle") || strings.Contains(l, "ancienne ligne") {
		t.Fatalf("nouvelle ligne : %s", l)
	}
}

// Relecture du 21/11 : reconnexion du navigateur avec « Last-Event-ID » :
// les lignes déjà reçues ne sont pas renvoyées.
func TestLogsFluxReprise(t *testing.T) {
	b := nouveauBanc(t)
	m := b.h.maintenant()
	jx, err := journaux.Ouvrir(journaux.Options{Dossier: t.TempDir(), Maintenant: b.h.maintenant})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = jx.Fermer() })
	b.c.opt.Logs = jx
	t1 := m.Add(-2 * time.Minute).UnixMilli()
	_ = jx.Ecrire(context.Background(), []stockage.Ligne{
		{T: t1, Msg: "deja recue", Champs: map[string]string{"host": "web1"}},
		{T: t1 + 1000, Msg: "pas encore recue", Champs: map[string]string{"host": "web1"}},
	})
	c, _ := b.premierAdmin("admin", mdpAdmin)
	depuis := strconv.FormatInt(m.Add(-10*time.Minute).UnixMilli(), 10)
	req, _ := http.NewRequest("GET", b.srv.URL+"/logs/flux?host=web1&periode=1h&depuis="+depuis, nil)
	req.Header.Set("Last-Event-ID", strconv.FormatInt(t1, 10))
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	lu := bufio.NewReader(resp.Body)
	var id, data string
	for data == "" {
		l, err := lu.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(l, "id: ") {
			id = strings.TrimSpace(l[4:])
		}
		if strings.HasPrefix(l, "data: ") {
			data = l
		}
	}
	if strings.Contains(data, "deja recue") || !strings.Contains(data, "pas encore recue") {
		t.Fatalf("reprise : %s", data)
	}
	if id != strconv.FormatInt(t1+1000, 10) {
		t.Errorf("id : %q", id)
	}
}

// Relecture du 21/11 : « voir autour » sur une machine bavarde garde les
// lignes d'avant (la lecture de ±1 h limitée aux plus récentes les perdait).
func TestLogsAutourMachineBavarde(t *testing.T) {
	b := nouveauBanc(t)
	m := b.h.maintenant()
	jx, err := journaux.Ouvrir(journaux.Options{Dossier: t.TempDir(), Maintenant: b.h.maintenant})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = jx.Fermer() })
	b.c.opt.Logs = jx
	cible := m.Add(-50 * time.Minute).UnixMilli()
	var ls []stockage.Ligne
	for i := 1; i <= 5; i++ {
		ls = append(ls, stockage.Ligne{T: cible - int64(i)*1000, Msg: "ligne-avant-" + strconv.Itoa(i), Champs: map[string]string{"host": "web1"}})
	}
	ls = append(ls, stockage.Ligne{T: cible, Msg: "la-ligne-cible", Champs: map[string]string{"host": "web1"}})
	for i := 1; i <= 2500; i++ {
		ls = append(ls, stockage.Ligne{T: cible + int64(i)*700, Msg: "bavarde", Champs: map[string]string{"host": "web1"}})
	}
	if err := jx.Ecrire(context.Background(), ls); err != nil {
		t.Fatal(err)
	}
	c, _ := b.premierAdmin("admin", mdpAdmin)
	_, corps, _ := b.get(c, "/logs?host=web1&autour="+strconv.FormatInt(cible, 10))
	if !strings.Contains(corps, "la-ligne-cible") || !strings.Contains(corps, "ligne-avant-1") || !strings.Contains(corps, "ligne-avant-5") {
		t.Errorf("lignes autour manquantes (cible : %v, avant : %v)", strings.Contains(corps, "la-ligne-cible"), strings.Contains(corps, "ligne-avant-1"))
	}
	if n := strings.Count(corps, "bavarde"); n < 20 {
		t.Errorf("lignes après : %d", n)
	}
}
