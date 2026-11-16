// SPDX-License-Identifier: GPL-3.0-or-later

package notif

import (
	"context"
	"strings"
	"testing"
	"time"

	"mirador-stack/internal/server/alertes"
)

// machinesSite : pose des machines en base (nom → site), vues il y a vu,
// puis force la relecture de la sourdine.
func (b *banc) machinesSite(vu time.Duration, nomSite ...string) {
	b.t.Helper()
	for i := 0; i+1 < len(nomSite); i += 2 {
		if _, err := b.db.Exec(`INSERT INTO machines(nom, site, type, statut, cree_le, vu_le) VALUES (?, ?, 'agent', 'approuvee', 0, ?)`,
			nomSite[i], nomSite[i+1], b.horloge.Add(-vu).Unix()); err != nil {
			b.t.Fatal(err)
		}
	}
	if err := b.g.recharger(context.Background(), true); err != nil {
		b.t.Fatal(err)
	}
}

// Coupure du 16/11 : toutes les machines d'un site se taisent à quelques
// secondes d'écart → un seul message « Site maison injoignable », puis un
// seul message au retour, même si elles reviennent l'une après l'autre.
func TestSiteInjoignableUnSeulMessage(t *testing.T) {
	b := nouveauBanc(t)
	b.machinesSite(time.Minute, "pve", "maison", "nas", "maison", "dns", "maison", "vps", "ovh")
	t0 := b.horloge
	pve := alerte(1, "AgentMuet", alertes.Critique, "pve", t0)
	b.debut(pve)
	if m := b.avancer(20 * time.Second); len(m) != 0 {
		t.Fatalf("panne d'une machine d'un site de 3 : annoncée sans attendre les autres : %+v", m)
	}
	nas := alerte(2, "AgentMuet", alertes.Critique, "nas", b.horloge)
	b.debut(nas)
	if m := b.avancer(40 * time.Second); len(m) != 0 {
		t.Fatalf("2 machines sur 3 : %+v", m)
	}
	dns := alerte(3, "AgentMuet", alertes.Critique, "dns", b.horloge)
	b.debut(dns)
	// Les 3 machines sont en panne : le site est injoignable, un message
	// après l'attente de groupe (10 s pour une critique).
	if s := b.g.sitesBas; len(s) != 0 {
		t.Fatalf("site injoignable avant le tour : %+v", s)
	}
	m := b.avancer(5 * time.Second)
	if bas := b.g.SitesInjoignables(); len(bas) != 1 || bas[0].Site != "maison" || len(bas[0].Machines) != 3 || !bas[0].Depuis.Equal(t0) {
		t.Fatalf("site injoignable : %+v", bas)
	}
	m = append(m, b.avancer(10*time.Second)...)
	if len(m) != 1 || m[0].Site != "maison" || len(m[0].Actives) != 3 || m[0].Type != TypeAlerte || !m[0].Son {
		t.Fatalf("un seul message pour le site attendu : %+v", m)
	}
	texte := Texte(m[0], Mise{Lien: "https://mirador.exemple"})
	for _, attendu := range []string{"Site maison injoignable", "Muettes : dns, nas, pve", "coupure d'internet ou de courant",
		"https://mirador.exemple/coupures?site=maison"} {
		if !strings.Contains(texte, attendu) {
			t.Errorf("message du site : %q absent de\n%s", attendu, texte)
		}
	}
	// Rien d'autre pendant la coupure (avant le rappel des critiques, 2 h).
	if m := b.avancer(30 * time.Minute); len(m) != 0 {
		t.Fatalf("pendant la coupure : %+v", m)
	}
	// Retour : les machines reviennent à 20 s d'écart → un seul message.
	b.fin(pve)
	if m := b.avancer(20 * time.Second); len(m) != 0 {
		t.Fatalf("retour partiel annoncé : %+v", m)
	}
	b.fin(nas)
	b.avancer(20 * time.Second)
	b.fin(dns)
	m = b.avancer(5 * time.Second)
	if len(m) != 1 || m[0].Site != "maison" || m[0].Type != TypeResolution || len(m[0].Resolues) != 3 || len(m[0].Actives) != 0 {
		t.Fatalf("un seul message de retour attendu : %+v", m)
	}
	texte = Texte(m[0], Mise{})
	if !strings.Contains(texte, "Site maison de nouveau joignable") || !strings.Contains(texte, "Coupure de 31m") {
		t.Errorf("message de retour :\n%s", texte)
	}
	if len(b.g.deplacees) != 0 || len(b.g.groupes) != 0 {
		t.Errorf("restes après la coupure : déplacées %v, groupes %d", b.g.deplacees, len(b.g.groupes))
	}
}

// Une seule machine du site en panne : message normal, 2 min plus tard.
// Machine seule sur son site (ou sans site) : aucun délai en plus.
func TestSiteUneMachineSeule(t *testing.T) {
	b := nouveauBanc(t)
	b.machinesSite(time.Minute, "pve", "maison", "nas", "maison", "vps", "ovh")
	b.debut(alerte(1, "AgentMuet", alertes.Critique, "pve", b.horloge))
	b.debut(alerte(2, "AgentMuet", alertes.Critique, "vps", b.horloge))
	m := b.avancer(15 * time.Second)
	if len(m) != 1 || m[0].Actives[0].Machine() != "vps" || m[0].Site != "" {
		t.Fatalf("machine seule sur son site : envoi sans délai attendu : %+v", m)
	}
	if m := b.avancer(time.Minute); len(m) != 0 {
		t.Fatalf("pve annoncée avant AttenteSite : %+v", m)
	}
	m = b.avancer(time.Minute)
	if len(m) != 1 || m[0].Site != "" || m[0].Actives[0].Machine() != "pve" {
		t.Fatalf("pve seule en panne : message normal attendu : %+v", m)
	}
	// Une autre règle (pas une panne) n'attend pas.
	b.debut(alerte(3, "CpuEleve", alertes.Critique, "nas", b.horloge))
	if m := b.avancer(15 * time.Second); len(m) != 1 {
		t.Fatalf("CpuEleve retardée : %+v", m)
	}
}

// Une machine déjà en panne et annoncée, puis le reste du site tombe : le
// message du site la reprend, sans la réannoncer comme nouvelle.
// Une machine éteinte depuis plus de 24 h ne compte pas dans le site.
func TestSitePanneDejaAnnonceeEtMachineAncienne(t *testing.T) {
	b := nouveauBanc(t)
	b.machinesSite(time.Minute, "pve", "maison", "nas", "maison")
	b.machinesSite(48*time.Hour, "vieux", "maison") // vu il y a 2 jours
	pve := alerte(1, "AgentMuet", alertes.Critique, "pve", b.horloge)
	b.debut(pve)
	if m := b.avancer(2*time.Minute + 5*time.Second); len(m) != 1 || m[0].Site != "" {
		t.Fatalf("pve seule : %+v", m)
	}
	b.debut(alerte(2, "AgentMuet", alertes.Critique, "nas", b.horloge))
	m := b.avancer(15 * time.Second)
	if len(m) != 1 || m[0].Site != "maison" || m[0].Nouvelles != 1 || len(m[0].Actives) != 2 {
		t.Fatalf("site (vieux ignoré) : %+v", m)
	}
}

// Équipement relevé par un collecteur : ne compte pas dans le site (il
// suit déjà son collecteur par la dépendance).
func TestSiteSansEquipements(t *testing.T) {
	b := nouveauBanc(t)
	b.machinesSite(time.Minute, "pve", "maison", "collecteur", "maison", "nas", "maison")
	if _, err := b.db.Exec(`INSERT INTO equipements_collecteur(machine_id, collecteur_id, rattache_le, rattache_par)
		SELECT (SELECT id FROM machines WHERE nom = 'nas'), (SELECT id FROM machines WHERE nom = 'collecteur'), 0, 'test'`); err != nil {
		t.Fatal(err)
	}
	if err := b.g.recharger(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if got := b.g.sourdine.parSite["maison"]; len(got) != 2 || got[0] != "collecteur" || got[1] != "pve" {
		t.Fatalf("machines du site : %v", got)
	}
}

// Coût du calcul fait toutes les 5 s : 300 machines sur 30 sites, toutes en
// panne (cas le plus lourd).
func BenchmarkSitesInjoignables(b *testing.B) {
	s := &Sourdine{reglesPanne: map[string]bool{"AgentMuet": true}, sites: map[string]string{}, parSite: map[string][]string{}}
	var actives []alertes.Alerte
	t0 := time.Now()
	for i := range 300 {
		nom := "m" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		site := "s" + string(rune('a'+i%30))
		s.sites[nom] = site
		s.parSite[site] = append(s.parSite[site], nom)
		actives = append(actives, alerte(int64(i), "AgentMuet", alertes.Critique, nom, t0))
	}
	b.ReportAllocs()
	for b.Loop() {
		if len(s.sitesInjoignables(actives)) != 30 {
			b.Fatal("30 sites attendus")
		}
	}
}

// Titre court (push, ntfy, e-mail) et lien de la notification push.
func TestTitreSite(t *testing.T) {
	a := alerte(1, "AgentMuet", alertes.Critique, "pve", time.Now())
	m := &Message{Site: "maison", Actives: []alertes.Alerte{a, a}}
	if got := m.TitreSite(); got != "Site maison injoignable (2 machines)" {
		t.Errorf("titre : %q", got)
	}
	if c := chargeDe(m, Mise{}, 1); c.Titre != m.TitreSite() || c.Lien != "/coupures?site=maison" {
		t.Errorf("push : %+v", c)
	}
	m = &Message{Site: "maison", Resolues: []alertes.Alerte{a}}
	if got := m.TitreSite(); got != "Site maison de nouveau joignable" {
		t.Errorf("titre du retour : %q", got)
	}
	if (&Message{Actives: []alertes.Alerte{a}}).TitreSite() != "" {
		t.Error("titre de site pour un message normal")
	}
}
