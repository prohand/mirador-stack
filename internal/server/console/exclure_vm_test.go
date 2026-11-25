// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"mirador-stack/internal/server/alertes"
)

// « Ne pas surveiller » une VM sans sauvegarde planifiée (retour du 25/11) :
// depuis la page de l'alerte et depuis Tâches planifiées, puis retour en
// arrière.
func TestExclureVM(t *testing.T) {
	b := nouveauBanc(t)
	vm := func(id, nom string) []string {
		return []string{"__name__", "mirador_pve_invite_sans_sauvegarde", "instance", "proxmox", "id", id, "type", "qemu", "nom", nom}
	}
	b.metriquesSimulees(t, []serieTest{{vm("200", "pbs"), constante(1)}, {vm("201", "test"), constante(1)}})
	b.machinesApprouvees(t, "proxmox")
	maintenant := time.Now().UnixMilli()
	res, err := b.c.db.Exec(`INSERT INTO alertes(regle, etiquettes, gravite, etat, debut, active_le, fin) VALUES ('VMSansSauvegarde', ?, 'warning', 'active', ?, ?, 0)`,
		`{"alertname":"VMSansSauvegarde","instance":"proxmox","id":"200","nom":"pbs"}`, maintenant, maintenant)
	if err != nil {
		t.Fatal(err)
	}
	idAlerte, _ := res.LastInsertId()
	page := "/alertes/" + strconv.FormatInt(idAlerte, 10)
	cl, _ := b.premierAdmin("admin", mdpAdmin)

	_, corps, _ := b.get(cl, "/taches")
	if strings.Count(corps, `value="exclure_vm"`) != 2 {
		t.Fatalf("boutons « Ne pas surveiller » sur Tâches : %d", strings.Count(corps, `value="exclure_vm"`))
	}
	_, corps, _ = b.get(cl, page)
	if !strings.Contains(corps, `id="exclure-vm"`) || !strings.Contains(corps, `name="retour" value="`+page+`"`) {
		t.Fatal("carte « Ne pas surveiller cette VM » absente de la page de l'alerte")
	}

	csrf := b.csrf(cl, page)
	_, _, h := b.post(cl, "/taches", url.Values{"csrf": {csrf}, "action": {"exclure_vm"}, "machine": {"proxmox"}, "vm": {"200"}, "nom": {"pbs"},
		"motif": {"c'est le PBS"}, "retour": {page}})
	if !strings.HasPrefix(h.Get("Location"), page) {
		t.Fatalf("retour : %q", h.Get("Location"))
	}
	l, _ := alertes.LireInvitesExclus(t.Context(), b.c.db)
	if len(l) != 1 || l[0].ID != "200" || l[0].Motif != "c'est le PBS" || l[0].Qui == "" {
		t.Fatalf("exclusion : %+v", l)
	}
	_, corps, _ = b.get(cl, page)
	if !strings.Contains(corps, "plus surveillée") {
		t.Error("message après l'exclusion absent")
	}
	_, corps, _ = b.get(cl, "/taches")
	if strings.Count(corps, `value="exclure_vm"`) != 1 || !strings.Contains(corps, "1 VM exclue · voir") || !strings.Contains(corps, "« c&#39;est le PBS »") {
		t.Error("Tâches : VM exclue encore proposée ou liste des exclues absente")
	}

	// Retour en arrière.
	_, _, h = b.post(cl, "/taches", url.Values{"csrf": {csrf}, "action": {"reintegrer_vm"}, "machine": {"proxmox"}, "vm": {"200"}})
	if h.Get("Location") != "/taches" && !strings.HasPrefix(h.Get("Location"), "/taches?") {
		t.Errorf("retour : %q", h.Get("Location"))
	}
	if l, _ := alertes.LireInvitesExclus(t.Context(), b.c.db); len(l) != 0 {
		t.Errorf("toujours exclue : %+v", l)
	}
	// Page de retour d'une autre origine : ignorée.
	_, _, h = b.post(cl, "/taches", url.Values{"csrf": {csrf}, "action": {"exclure_vm"}, "machine": {"proxmox"}, "vm": {"201"}, "retour": {"//exemple.org/x"}})
	if strings.Contains(h.Get("Location"), "exemple.org") {
		t.Errorf("retour vers une autre origine : %q", h.Get("Location"))
	}
	// Valeurs invalides refusées.
	b.post(cl, "/taches", url.Values{"csrf": {csrf}, "action": {"exclure_vm"}, "machine": {"proxmox"}, "vm": {"1;2"}})
	if l, _ := alertes.LireInvitesExclus(t.Context(), b.c.db); len(l) != 1 {
		t.Errorf("exclusions : %+v", l)
	}
}
