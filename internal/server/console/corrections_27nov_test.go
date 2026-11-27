// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"mirador-stack/internal/server/domaines"
	"mirador-stack/internal/server/flotte"
	"mirador-stack/internal/server/stockage"
	"mirador-stack/internal/server/stockage/journaux"
)

// Corrections de la revue du 27/11 : un test par bug trouvé.

// « /\autre.site » est lu « //autre.site » par les navigateurs : refusé.
func TestRetourAntislashRefuse(t *testing.T) {
	for _, v := range []string{`/\evil.example`, `/alertes/../\evil.example`} {
		if r := retourLocal(v); r != "/" {
			t.Errorf("retourLocal(%q) = %q, attendu /", v, r)
		}
	}
}

// Domaine expiré depuis 12 h : « expiré », pas « dans 0 j ».
func TestDomaineExpireDepuisMoinsDUnJour(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	ls := lignesDomaines([]domaines.Domaine{{Nom: "exemple.fr", Expire: now.Add(-12 * time.Hour)}}, now)
	if ls[0].Expire != "expiré" || ls[0].Classe != "erreur" {
		t.Errorf("domaine expiré il y a 12 h : %q (%s)", ls[0].Expire, ls[0].Classe)
	}
}

// Recherche « mode_admin » dans l'audit : le « _ » est échappé, pas supprimé.
func TestAuditRechercheSouligne(t *testing.T) {
	b := nouveauBanc(t)
	ctx := context.Background()
	_ = Auditer(ctx, b.db, "web:admin", "mode_admin_ouvert", "", "", "", "127.0.0.1", b.h.maintenant())
	_ = Auditer(ctx, b.db, "web:admin", "modeXadmin", "", "", "", "127.0.0.1", b.h.maintenant())
	f := lireFiltreAudit(url.Values{"q": {"mode_admin"}, "periode": {"tout"}})
	cond, args := f.condition(b.h.maintenant(), false)
	var n int
	if err := b.db.QueryRow(`SELECT COUNT(*) FROM audit WHERE `+cond, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("recherche « mode_admin » : %d ligne(s), attendu 1", n)
	}
}

// Recherche coupée à 100 octets sans couper une lettre accentuée.
func TestCouperUTF8(t *testing.T) {
	s := strings.Repeat("a", 99) + "é" // « é » : octets 100 et 101
	if c := couperUTF8(s, 100); len(c) != 99 {
		t.Errorf("coupe : %d octets, attendu 99", len(c))
	}
}

// « demain » : veille du passage à l'heure d'été, et date d'une autre année.
func TestHeureDemain(t *testing.T) {
	loc, _ := time.LoadLocation("Europe/Paris")
	n := time.Date(2026, 3, 28, 23, 30, 0, 0, loc)
	d := n.AddDate(0, 0, 1)
	l := time.Date(2026, 3, 29, 3, 0, 0, 0, loc)
	if !(l.Year() == d.Year() && l.YearDay() == d.YearDay()) {
		t.Error("29/03 03:00 vu le 28/03 23:30 : devrait être demain")
	}
}

// Les boutons de la page Machines comptent le site choisi.
func TestCompteursMachinesSite(t *testing.T) {
	ms := []vueMachine{
		{InfoMachine: flotte.InfoMachine{Nom: "a", Site: "Paris"}, Muette: true},
		{InfoMachine: flotte.InfoMachine{Nom: "b", Site: "Lyon"}, Muette: true},
		{InfoMachine: flotte.InfoMachine{Nom: "c", Site: "Lyon"}},
	}
	f := lireFiltresMachines("site", "", "").avecListes("Paris", "")
	if c := f.compter(ms); c.Toutes != 1 || c.Problemes != 1 {
		t.Errorf("site=Paris : Toutes(%d) Problèmes(%d), attendu 1 et 1", c.Toutes, c.Problemes)
	}
}

// « C'est moi » sur l'alerte SSH : code 2FA exigé si le mode administration est fermé.
func TestSSHConnueDepuisAlerteDemandeCode(t *testing.T) {
	b := nouveauBanc(t)
	b.machinesApprouvees(t, "web1")
	m := b.h.maintenant()
	res, err := b.c.db.Exec(`INSERT INTO alertes(regle, etiquettes, gravite, etat, debut, active_le) VALUES ('SshAdresseNouvelle', ?, 'warning', 'active', ?, ?)`,
		`{"alertname":"SshAdresseNouvelle","instance":"web1","adresse":"198.51.100.23"}`, m.UnixMilli(), m.UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	page := "/alertes/" + strconv.FormatInt(id, 10)
	c, _ := b.premierAdmin("admin", mdpAdmin)
	b.post(c, "/mode-admin/quitter", url.Values{"csrf": {b.csrf(c, "/compte")}})
	b.post(c, page, url.Values{"csrf": {b.csrf(c, page)}, "action": {"ssh_connue"}, "portee": {"toutes"}})
	var n int
	_ = b.c.db.QueryRow(`SELECT count(*) FROM ssh_connues`).Scan(&n)
	if n != 0 {
		t.Errorf("sans code, mode administration fermé : %d adresse(s) ajoutée(s), attendu 0", n)
	}
}

// Logs en direct : rafale signalée, paquet en retard retrouvé, pas de doublon.
func TestLogsDirectRafaleEtRetard(t *testing.T) {
	b := nouveauBanc(t)
	m := b.h.maintenant()
	jx, err := journaux.Ouvrir(journaux.Options{Dossier: t.TempDir(), Maintenant: b.h.maintenant})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = jx.Fermer() })
	b.c.opt.Logs = jx
	ctx := context.Background()
	f, _ := lireFiltresLogs(map[string][]string{"periode": {"1h"}}, m)

	// Rafale de 250 lignes : 200 affichées + une ligne d'avertissement.
	depuis := m.Add(-10 * time.Second).UnixMilli()
	var ls []stockage.Ligne
	for i := 0; i < 250; i++ {
		ls = append(ls, stockage.Ligne{T: depuis + 1 + int64(i), Msg: fmt.Sprintf("l%03d", i), Champs: map[string]string{"host": "web1"}})
	}
	_ = jx.Ecrire(ctx, ls)
	out, _ := b.c.lignesDepuis(ctx, f, depuis, m, nil)
	if d := out.Lignes[len(out.Lignes)-1]; d.Host != "mirador" || d.Classe != "warn" {
		t.Errorf("rafale non signalée : dernière ligne %+v", d)
	}

	// Paquet en retard (machine B, heures plus anciennes que A). Les lignes
	// de la rafale, avant l'ouverture du direct, ne sont pas renvoyées.
	depuis = m.UnixMilli()
	vus := &suiviDirect{vus: map[cleLigneVue]bool{}, debut: depuis - 1}
	_ = jx.Ecrire(ctx, []stockage.Ligne{{T: depuis + 5000, Msg: "A", Champs: map[string]string{"host": "a"}}})
	m2 := m.Add(6 * time.Second)
	out, _ = b.c.lignesDepuis(ctx, f, depuis, m2, vus)
	depuis = out.Fin
	_ = jx.Ecrire(ctx, []stockage.Ligne{{T: m.UnixMilli() + 3000, Msg: "B", Champs: map[string]string{"host": "b"}}})
	out2, _ := b.c.lignesDepuis(ctx, f, depuis, m2.Add(time.Second), vus)
	if len(out2.Lignes) != 1 || out2.Lignes[0].Msg != "B" {
		t.Errorf("paquet en retard : %+v, attendu la ligne B seule (sans renvoyer A)", out2.Lignes)
	}
	if out2.Fin < out.Fin {
		t.Errorf("curseur reculé : %d < %d", out2.Fin, out.Fin)
	}
}
