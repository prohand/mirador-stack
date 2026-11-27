// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"strconv"
	"strings"
	"testing"

	"mirador-stack/internal/server/alertes"
)

// « Que faire ? » d'une règle (demande du 27/11) : replié sur la page de
// l'alerte, texte échappé, absent quand la règle n'en a pas.
func TestProcedureAlerte(t *testing.T) {
	b := nouveauBanc(t)
	m := b.h.maintenant()
	r := alertes.Regle{Nom: "DisqueTest", Groupe: "test", Expr: `node_filesystem_avail_bytes < 1`, Gravite: "warning", Active: true,
		Procedure: "1. journalctl --vacuum-size=500M\n2. <b>vider</b> /var/cache/apt"}
	if _, err := alertes.Enregistrer(t.Context(), b.c.db, []alertes.Regle{r}, "test", m); err != nil {
		t.Fatal(err)
	}
	inserer := func(regle string) string {
		res, err := b.c.db.Exec(`INSERT INTO alertes(regle, etiquettes, gravite, etat, debut, active_le) VALUES (?, ?, 'warning', 'active', ?, ?)`,
			regle, `{"alertname":"`+regle+`","instance":"sup"}`, m.UnixMilli(), m.UnixMilli())
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		return "/alertes/" + strconv.FormatInt(id, 10)
	}
	c, _ := b.premierAdmin("admin", mdpAdmin)
	_, corps, _ := b.get(c, inserer("DisqueTest"))
	for _, attendu := range []string{`<details class="que-faire"><summary>📝 Que faire ?</summary>`, "journalctl --vacuum-size=500M", "&lt;b&gt;vider&lt;/b&gt;"} {
		if !strings.Contains(corps, attendu) {
			t.Errorf("page de l'alerte : %q absent", attendu)
		}
	}
	if strings.Contains(corps, "<details class=\"que-faire\" open") {
		t.Error("« Que faire » doit être replié")
	}
	if _, corps, _ = b.get(c, inserer("AutreRegle")); strings.Contains(corps, "que-faire") {
		t.Error("« Que faire » affiché sans procédure")
	}
	// Trop long : refusé.
	r.Procedure = strings.Repeat("x", alertes.TailleMaxProcedure+1)
	if _, err := alertes.Enregistrer(t.Context(), b.c.db, []alertes.Regle{r}, "test", m); err == nil {
		t.Error("procédure trop longue acceptée")
	}
}
