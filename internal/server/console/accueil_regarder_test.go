// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"strings"
	"testing"

	"mirador-stack/internal/server/notif"
)

// Vue d'ensemble (maquette validée le 22/11, SPEC § 120) : bandeau du plus
// grave, liste « À regarder », aperçu par site, ligne de pied ; le tableau
// du parc et la carte de chaleur complète masqués par défaut.
func TestAccueilARegarder(t *testing.T) {
	b := nouveauBanc(t)
	b.machinesApprouvees(t, "web1", "pve1")
	_, _ = b.c.db.Exec(`INSERT INTO machines(nom, statut, type, site, cree_le, vu_le) VALUES ('nas-backup', 'approuvee', 'agent', 'maison', 1, 1)`)
	_, _ = b.c.db.Exec(`UPDATE machines SET maj_echec = '0.5.1 : pas de battement en 10 min, retour à 0.5.0' WHERE nom = 'pve1'`)
	_, _ = b.c.db.Exec(`INSERT INTO machines(nom, statut, cree_le) VALUES ('nouvelle', 'en_attente', 1)`)
	c, _ := b.premierAdmin("admin", mdpAdmin)
	_, corps, _ := b.get(c, "/")
	for _, attendu := range []string{
		`<div class="bandeau-accueil erreur">`, "<h2>1 machine ne répond plus</h2>",
		"<b>1 machine muette</b>", "<b>pve1 : mise à jour en échec</b>", "retour à 0.5.0",
		"<b>1 machine en attente d&#39;approbation</b>", "<b>Aucun canal de notification</b>",
		`<a class="pastille apercu-erreur" href="/machines/nas-backup">`, "<h3>maison",
		`data-bloc="sondes"`, `data-bloc="certificats"`, "Aucun certificat suivi", `class="pied-accueil`,
	} {
		if !strings.Contains(corps, attendu) {
			t.Errorf("vue d'ensemble : %q absent", attendu)
		}
	}
	// Masqués par défaut (ordinateur et téléphone), encore dans la page.
	for _, bloc := range []string{`data-parc data-bloc="parc"`, `data-bloc="chaleur"`} {
		i := strings.Index(corps, bloc)
		if i < 0 || !strings.Contains(corps[max(0, i-60):i], "cache-o cache-t") {
			t.Errorf("%s : pas masqué par défaut", bloc)
		}
	}
	// Sous-titre sans doublon avec le bandeau (retour du 22/11).
	if !strings.Contains(corps, "<p>1 site · mise à jour chaque minute</p>") || strings.Contains(corps, "3 machines ·") || strings.Contains(corps, "sur 3 répondent") {
		t.Error("sous-titre de la vue d'ensemble")
	}
	// Plus de case « Notifications » (dans la ligne du bas).
	if strings.Contains(corps, `data-bloc="notifications"`) {
		t.Error("case Notifications encore là")
	}
}

// Bandeau « Tout va bien » : rien qui répète la case Machines (retour du
// 22/11 : « Toujours doublon »).
func TestBandeauToutVaBien(t *testing.T) {
	b := nouveauBanc(t)
	d := &donneesAccueil{Machines: 5, SauvegardeOK: true, Canaux: []notif.EtatCanal{{}}}
	r := b.c.regarderAccueil(context.Background(), d, nil, b.h.maintenant())
	if r.Bandeau.Classe != "ok" || r.Bandeau.Texte != "rien à regarder pour l'instant" {
		t.Errorf("bandeau : %+v", r.Bandeau)
	}
	// Infos et points sans urgence comptés à part (SPEC § 127).
	d.Nouveautes = 2
	r = b.c.regarderAccueil(context.Background(), d, nil, b.h.maintenant())
	if r.Bandeau.Classe != "ok" || r.Bandeau.Texte != "1 info (éléments à valider)" {
		t.Errorf("bandeau avec une info : %+v", r.Bandeau)
	}
	d.Parc = []ligneParc{{Nom: "web1", PaquetsSecurite: 3}}
	r = b.c.regarderAccueil(context.Background(), d, nil, b.h.maintenant())
	if r.Bandeau.Classe != "ok" || r.Bandeau.Texte != "1 point sans urgence plus bas (paquets de sécurité) · 1 info (éléments à valider)" {
		t.Errorf("bandeau avec un point et une info : %+v", r.Bandeau)
	}
}

// Retour du 25/11 (SPEC § 127) : plus de « Tout va bien » avec des
// avertissements en cours ou une pastille orange ou rouge.
func TestBandeauAvertissements(t *testing.T) {
	b := nouveauBanc(t)
	d := &donneesAccueil{Machines: 2, SauvegardeOK: true, Canaux: []notif.EtatCanal{{}}, Avertissements: 2,
		Alertes: []resumeAlerte{{Regle: "LinuxServiceEnEchec", Gravite: "warning", Machine: "vps-web", Etat: "active"},
			{Regle: "DisqueBientotPlein", Gravite: "warning", Machine: "nas", Etat: "active"}}}
	r := b.c.regarderAccueil(context.Background(), d, nil, b.h.maintenant())
	if r.Bandeau.Classe != "attention" || r.Bandeau.Titre != "2 avertissements en cours" ||
		!strings.Contains(r.Bandeau.Texte, "(vps-web)") || !strings.Contains(r.Bandeau.Texte, "(nas)") || r.Bandeau.Lien != "/alertes" {
		t.Errorf("avertissements : %+v", r.Bandeau)
	}
	// Machine rouge (alerte critique prise en charge) : « 1 machine à regarder ».
	d.Avertissements, d.Alertes = 0, nil
	d.Parc = []ligneParc{{Nom: "pve1", ClasseEtat: "erreur"}, {Nom: "web1"}}
	d.Prises = []resumeAlerte{{Regle: "Muet", Gravite: "critical", Machine: "pve1", Etat: "active"}}
	r = b.c.regarderAccueil(context.Background(), d, nil, b.h.maintenant())
	if r.Bandeau.Classe != "attention" || r.Bandeau.Titre != "1 machine à regarder" ||
		r.Bandeau.Texte != "pve1 : 1 alerte prise en charge" || r.Bandeau.Lien != "/machines/pve1" {
		t.Errorf("machine à regarder : %+v", r.Bandeau)
	}
}
