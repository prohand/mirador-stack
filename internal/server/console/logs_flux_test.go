// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"bufio"
	"context"
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
