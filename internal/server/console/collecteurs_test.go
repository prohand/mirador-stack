// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"mirador-stack/internal/server/flotte"
)

// Collecteur dans l'interface (SPEC § 23.13) : jeton « Collecteur du LAN »,
// rattachement d'un équipement, équipement créé « relevé par ».
func TestCollecteurInterface(t *testing.T) {
	b := nouveauBanc(t)
	c, secret := b.premierAdmin("admin", mdpAdmin)
	ctx := context.Background()
	csrf := b.csrf(c, "/machines")
	b.h.avancer(31 * time.Second)
	_, corps, _ := b.post(c, "/machines/jeton", url.Values{"csrf": {csrf}, "type_machine": {"collecteur"}, "nom": {"collecteur-lan"},
		"duree": {"1h"}, "maj_auto": {"1"}, "code": {b.code(secret)}})
	if !strings.Contains(corps, "/api/agent/v1/install/collecteur -o mirador-collector-install.sh") || !strings.Contains(corps, "--auto-update") {
		t.Fatal("commande d'installation du collecteur absente")
	}
	// Collecteur approuvé (simulé) et une machine ordinaire.
	if _, err := b.c.db.Exec(`INSERT INTO machines(nom, statut, type, cree_le) VALUES ('collecteur-lan', 'approuvee', 'collecteur', 1), ('nas', 'approuvee', 'agent', 1)`); err != nil {
		t.Fatal(err)
	}
	idC, _ := flotte.IDMachine(ctx, b.c.db, "collecteur-lan")
	if _, corps, _ = b.get(c, "/machines/collecteur-lan"); !strings.Contains(corps, `id="equipements"`) || !strings.Contains(corps, "<option>nas</option>") {
		t.Fatal("bloc des équipements absent")
	}
	// Mode administration ouvert : plus de code.
	b.post(c, "/machines/collecteur-lan", url.Values{"csrf": {csrf}, "action": {"rattacher"}, "equipement": {"nas"}})
	if eqs, _ := flotte.EquipementsDe(ctx, b.c.db, idC); len(eqs) != 1 || eqs[0].Nom != "nas" {
		t.Fatalf("rattachement : %+v", eqs)
	}
	if _, corps, _ = b.get(c, "/machines/nas"); !strings.Contains(corps, `Relevé par le collecteur <a href="/machines/collecteur-lan">`) {
		t.Error("« relevé par » absent de la page de l'équipement")
	}
	// Équipement sans agent créé « relevé par » : pas de token, rattaché.
	if _, corps, _ = b.get(c, "/machines"); !strings.Contains(corps, `<select name="collecteur">`) {
		t.Fatal("choix « Relevé par » absent")
	}
	_, corps, _ = b.post(c, "/machines/equipement", url.Values{"csrf": {csrf}, "nom": {"imprimante"}, "collecteur": {"collecteur-lan"}})
	if strings.Contains(corps, "Token de") {
		t.Error("token créé pour un équipement relevé par un collecteur")
	}
	if eqs, _ := flotte.EquipementsDe(ctx, b.c.db, idC); len(eqs) != 2 {
		t.Fatalf("équipement relevé non rattaché : %+v", eqs)
	}
	// Détacher depuis la page du collecteur.
	b.post(c, "/machines/collecteur-lan", url.Values{"csrf": {csrf}, "action": {"detacher"}, "equipement": {"nas"}})
	if eqs, _ := flotte.EquipementsDe(ctx, b.c.db, idC); len(eqs) != 1 || eqs[0].Nom != "imprimante" {
		t.Fatalf("détachement : %+v", eqs)
	}
	// Sans code ni mode administration : refusé.
	b.h.avancer(DureeModeAdmin + time.Minute)
	b.post(c, "/machines/collecteur-lan", url.Values{"csrf": {csrf}, "action": {"rattacher"}, "equipement": {"nas"}})
	if eqs, _ := flotte.EquipementsDe(ctx, b.c.db, idC); len(eqs) != 1 {
		t.Error("rattachement sans code 2FA")
	}
}

// Syslog TLS dans l'interface (SPEC § 23.14) : réglages vérifiés, posés
// dans la configuration du collecteur ; certificat ajouté par le serveur ;
// jeton deSEC chiffré et jamais réaffiché.
func TestSyslogInterface(t *testing.T) {
	b := nouveauBanc(t)
	c, secret := b.premierAdmin("admin", mdpAdmin)
	ctx := context.Background()
	csrf := b.csrf(c, "/machines")
	if _, err := b.c.db.Exec(`INSERT INTO machines(nom, statut, type, cree_le) VALUES ('collecteur-lan', 'approuvee', 'collecteur', 1), ('nas', 'approuvee', 'agent', 1)`); err != nil {
		t.Fatal(err)
	}
	idC, _ := flotte.IDMachine(ctx, b.c.db, "collecteur-lan")
	idNas, _ := flotte.IDMachine(ctx, b.c.db, "nas")
	if err := flotte.RattacherEquipement(ctx, b.c.db, idC, idNas, "admin", time.Now()); err != nil {
		t.Fatal(err)
	}
	b.h.avancer(31 * time.Second)
	formulaire := url.Values{"csrf": {csrf}, "action": {"syslog"}, "ecoute": {"203.0.113.5:6514"}, "nom_certificat": {"syslog.lan.exemple.org"},
		"emetteurs": {"192.168.1.20 nas"}, "code": {b.code(secret)}}
	b.post(c, "/machines/collecteur-lan", formulaire)
	if doc, _, _ := flotte.ConfigMachine(ctx, b.c.db, idC); strings.Contains(string(doc), "syslog") {
		t.Fatal("écoute sur une adresse publique acceptée")
	}
	formulaire.Set("ecoute", "192.168.1.10:6514")
	formulaire.Set("emetteurs", "192.168.1.30 inconnu")
	formulaire.Del("code")
	b.post(c, "/machines/collecteur-lan", formulaire)
	if doc, _, _ := flotte.ConfigMachine(ctx, b.c.db, idC); strings.Contains(string(doc), "syslog") {
		t.Fatal("émetteur d'un équipement non rattaché accepté")
	}
	formulaire.Set("emetteurs", "192.168.1.20 nas\n# commentaire\n192.168.2.0/24 nas")
	b.post(c, "/machines/collecteur-lan", formulaire)
	cfg, err := flotte.ConfigEffective(ctx, b.c.db, idC)
	if err != nil || cfg.Collecteur == nil || cfg.Collecteur.Syslog == nil || len(cfg.Collecteur.Syslog.Emetteurs) != 2 || cfg.Collecteur.Syslog.Certificat != "" {
		t.Fatalf("syslog enregistré : %+v %v", cfg.Collecteur, err)
	}
	if _, corps, _ := b.get(c, "/machines/collecteur-lan"); !strings.Contains(corps, `id="syslog"`) || !strings.Contains(corps, "Pas encore de demande du collecteur") {
		t.Error("état du certificat absent")
	}
	// Réglages deSEC : jeton chiffré, jamais réaffiché.
	b.post(c, "/machines/collecteur-lan", url.Values{"csrf": {csrf}, "action": {"certificat_reglages"}, "domaine": {"exemple.org"}, "jeton": {"jeton-desec-tres-secret"}})
	_, corps, _ := b.get(c, "/machines/collecteur-lan")
	if strings.Contains(corps, "jeton-desec-tres-secret") || !strings.Contains(corps, "déjà enregistré") {
		t.Error("jeton deSEC réaffiché ou non enregistré")
	}
	// Certificat obtenu (simulé) : posé dans la configuration du collecteur.
	if _, err := b.c.db.Exec(`INSERT INTO certificats_syslog(machine_id, nom, csr, certificat, fin, obtenu_le) VALUES (?, 'syslog.lan.exemple.org', 'x', 'CHAINE-PEM', 4102444800, 1)`, idC); err != nil {
		t.Fatal(err)
	}
	if cfg, _ = flotte.ConfigEffective(ctx, b.c.db, idC); cfg.Collecteur.Syslog.Certificat != "CHAINE-PEM" {
		t.Errorf("certificat non posé : %q", cfg.Collecteur.Syslog.Certificat)
	}
	if _, corps, _ = b.get(c, "/machines/collecteur-lan"); !strings.Contains(corps, "Obtenu") {
		t.Error("certificat obtenu non affiché")
	}
	// Équipement détaché : ses émetteurs disparaissent de la configuration.
	_ = flotte.DetacherEquipement(ctx, b.c.db, idNas, "admin", time.Now())
	if cfg, err = flotte.ConfigEffective(ctx, b.c.db, idC); err != nil || len(cfg.Collecteur.Syslog.Emetteurs) != 0 {
		t.Errorf("émetteurs d'un équipement détaché : %+v %v", cfg.Collecteur.Syslog, err)
	}
	// Arrêter le syslog.
	b.post(c, "/machines/collecteur-lan", url.Values{"csrf": {csrf}, "action": {"syslog_retirer"}})
	if doc, _, _ := flotte.ConfigMachine(ctx, b.c.db, idC); strings.Contains(string(doc), "syslog") {
		t.Error("syslog non retiré")
	}
}
