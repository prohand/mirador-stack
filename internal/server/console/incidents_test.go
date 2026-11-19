// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"mirador-stack/internal/server/flotte"
	"mirador-stack/internal/server/servicesmetier"
	essais "mirador-stack/internal/sondes"
)

// Incidents (SPEC § 54) : regroupement par machine / service / site,
// titre, frise (changement d'avant), notes.
func TestIncidents(t *testing.T) {
	b := nouveauBanc(t)
	m := b.h.maintenant()
	b.machinesApprouvees(t, "vm-docker", "nas", "pve1", "pve2")
	_, _ = b.c.db.Exec(`UPDATE machines SET site = 'maison'`)
	if err := servicesmetier.Enregistrer(t.Context(), b.c.db, []servicesmetier.Service{{Nom: "Domotique",
		Composants: []servicesmetier.Composant{{Sorte: "sonde", Nom: "HA", Cle: true}, {Sorte: "machine", Nom: "vm-docker"}}}}, "test", "", m); err != nil {
		t.Fatal(err)
	}
	alerte := func(regle, etq string, debut, fin time.Duration) int64 {
		t.Helper()
		res, err := b.c.db.Exec(`INSERT INTO alertes(regle, etiquettes, gravite, etat, debut, active_le, fin, resume) VALUES (?, ?, 'critical', 'resolue', ?, ?, ?, '')`,
			regle, etq, m.Add(-debut).UnixMilli(), m.Add(-debut).UnixMilli(), m.Add(-fin).UnixMilli())
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		return id
	}
	// Incident 1 : machine muette puis sonde du même service, 1 min après.
	premier := alerte("AgentMuet", `{"instance":"vm-docker"}`, 3*time.Hour, 2*time.Hour+40*time.Minute)
	alerte("SondeEnEchec", `{"instance":"lan","sonde":"HA"}`, 3*time.Hour-time.Minute, 2*time.Hour+41*time.Minute)
	// Incident 2 : deux machines du site muettes (2 jours avant).
	alerte("AgentMuet", `{"instance":"pve1"}`, 48*time.Hour, 47*time.Hour)
	alerte("AgentMuet", `{"instance":"pve2"}`, 48*time.Hour-2*time.Minute, 47*time.Hour)
	// Sans lien : une alerte isolée sur nas, 1 jour avant.
	alerte("DisquePlein", `{"instance":"nas"}`, 24*time.Hour, 23*time.Hour)
	// Ping de nas fait par le collecteur « lan » (SPEC § 60) : avec nas,
	// pas avec le collecteur.
	_, _ = b.c.db.Exec(`INSERT INTO machines(nom, statut, type, cree_le) VALUES ('lan', 'approuvee', 'collecteur', 1)`)
	if err := flotte.EnregistrerSondes(t.Context(), b.c.db, []essais.Sonde{{Nom: "nas ping", Type: essais.Ping, Cible: "192.168.1.5", Depuis: "lan", Machine: "nas"}}, "test", m); err != nil {
		t.Fatal(err)
	}
	alerte("SondeEnEchec", `{"instance":"lan","sonde":"nas ping"}`, 24*time.Hour-time.Minute, 23*time.Hour)
	_, _ = b.c.db.Exec(`INSERT INTO changements(machine, le, type, detail) VALUES ('vm-docker', ?, 'paquets', 'openssl 1 → 2, curl 1 → 2')`, m.Add(-3*time.Hour-8*time.Minute).Unix())

	incs := b.c.incidentsDe(t.Context(), m.Add(-30*24*time.Hour), m)
	if len(incs) != 3 {
		t.Fatalf("%d incidents : %+v", len(incs), incs)
	}
	titres := map[string]int{}
	for _, i := range incs {
		titres[i.Titre] = len(i.Alertes)
	}
	for titre, n := range map[string]int{"Domotique : AgentMuet · 2 alertes": 2, "Site maison injoignable (2 machines) · 2 alertes": 2, "nas : DisquePlein · 2 alertes": 2} {
		if titres[titre] != n {
			t.Errorf("%q : %d alertes (%v)", titre, titres[titre], titres)
		}
	}
	cl, _ := b.premierAdmin("admin", mdpAdmin)
	lien := "/incidents/" + strconv.FormatInt(premier, 10)
	b.post(cl, lien, url.Values{"csrf": {b.csrf(cl, lien)}, "note": {"retour au noyau précédent"}})
	_, corps, _ := b.get(cl, lien)
	for _, attendu := range []string{"Domotique : AgentMuet", `class="frise"`, "Paquets : 2 paquets mis à jour", "retour au noyau précédent",
		"SondeEnEchec", "service Domotique"} {
		if !strings.Contains(corps, attendu) {
			t.Errorf("page de l'incident : %q absent", attendu)
		}
	}
	_, corps, _ = b.get(cl, "/incidents")
	if !strings.Contains(corps, "Site maison injoignable") || !strings.Contains(corps, lien) {
		t.Error("liste des incidents")
	}
	_, corps, _ = b.get(cl, "/alertes/"+strconv.FormatInt(premier, 10))
	if !strings.Contains(corps, "fait partie de l'<a href=\""+lien) {
		t.Error("lien depuis la page de l'alerte")
	}
	_, corps, _ = b.get(cl, "/evenements")
	if !strings.Contains(corps, "retour au noyau précédent") {
		t.Error("note absente du journal")
	}
}
