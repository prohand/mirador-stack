// SPDX-License-Identifier: GPL-3.0-or-later

package console

// Actions sur un groupe d'alertes (SPEC § 141, maquette validée le 27/11) :
// « Acquitter les N » (4 h) et « Silence 1 h » sur chacune des alertes en
// cours du groupe (machine, règle ou site). Même effet que la même action
// faite ligne par ligne ; les silences d'une heure ne demandent pas de code.

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"mirador-stack/internal/server/alertes"
	"mirador-stack/internal/server/notif"
)

// maxGroupe : alertes traitées au plus par une action de groupe.
const maxGroupe = 5000

func (c *Console) postAlertesGroupe(q *req) {
	ctx := q.r.Context()
	vue, cle := vueAlertesValide(q.r.PostFormValue("vue")), q.r.PostFormValue("cle")
	retour := retourLocal(q.r.PostFormValue("retour"))
	if !strings.HasPrefix(retour, "/alertes") {
		retour = "/alertes"
	}
	rs, _ := separerAttente(c.alertesEnCours(""))
	rs, _ = separerPrises(rs)
	rs, _ = separerMasquees(rs)
	sites := c.sitesMachines(ctx)
	var ids []int64
	for _, r := range rs {
		if k, _, _ := cleGroupeAlerte(r, vue, sites); k == cle {
			ids = append(ids, r.ID)
		}
	}
	if len(ids) == 0 || len(ids) > maxGroupe {
		c.retour(q, retour, errors.New("aucune alerte en cours dans ce groupe"), "")
		return
	}
	var errs []error
	n := 0
	ok := ""
	switch q.r.PostFormValue("action") {
	case "acquitter":
		jusqua := q.t.Add(notif.DureeAcquittement).In(fuseau.Load())
		for _, id := range ids {
			if err := notif.Acquitter(ctx, c.db, id, q.qui(), "action de groupe", jusqua, q.t); err != nil {
				errs = append(errs, err)
				continue
			}
			n++
		}
		ok = fmt.Sprintf("%d %s jusqu'à %s : ensuite, rappel si elles durent", n, accorder(n, "alerte(s) prise(s) en charge"), jusqua.Format("15:04"))
	case "silence":
		for _, id := range ids {
			a, err := alertes.LireAlerte(ctx, c.db, id)
			if err == nil {
				_, err = notif.AjouterSilence(ctx, c.db, selecteurSilence(&a), q.t, time.Hour, q.qui(), "action de groupe", q.t)
			}
			if err != nil {
				errs = append(errs, err)
				continue
			}
			n++
		}
		ok = fmt.Sprintf("silence d'une heure posé sur %d %s", n, accorder(n, "alerte(s)"))
	default:
		errs = append(errs, errors.New("action inconnue"))
	}
	c.retour(q, retour, errors.Join(errs...), ok)
}
