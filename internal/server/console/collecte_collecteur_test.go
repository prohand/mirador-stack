// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/tsdb"

	"mirador-stack/internal/server/alertes"
	"mirador-stack/internal/server/flotte"
)

// Nouvelle page du collecteur (maquette validée le 22/11, SPEC § 119) :
// sondes et émetteurs ajoutés et retirés un par un, tableau « équipement ×
// méthode » rempli d'après les mesures reçues.
func TestOngletCollecte(t *testing.T) {
	b := nouveauBanc(t)
	c, secret := b.premierAdmin("admin", mdpAdmin)
	ctx := context.Background()
	_, _ = b.c.db.Exec(`INSERT INTO machines(nom, statut, type, cree_le) VALUES ('collecteur-lan', 'approuvee', 'collecteur', 1), ('nas', 'approuvee', 'agent', 1), ('fw', 'approuvee', 'agent', 1)`)
	idC, _ := flotte.IDMachine(ctx, b.c.db, "collecteur-lan")
	for _, n := range []string{"nas", "fw"} {
		id, _ := flotte.IDMachine(ctx, b.c.db, n)
		_ = flotte.RattacherEquipement(ctx, b.c.db, idC, id, "admin", time.Now())
	}
	csrf := b.csrf(c, "/machines")
	poster := func(v url.Values) {
		v.Set("csrf", csrf)
		b.post(c, "/machines/collecteur-lan", v)
	}
	// Premier envoi avec le code 2FA : le mode administration s'ouvre.
	poster(url.Values{"action": {"sonde_ajouter"}, "sonde_adresse": {"192.168.1.20:5001"}, "machine": {"nas"}, "code": {b.code(secret)}})
	poster(url.Values{"action": {"sonde_ajouter"}, "sonde_adresse": {"192.168.1.1:10443"}, "machine": {"fw"}, "sonde_nom": {"FW.lan"}})
	// Même adresse : remplacée, pas doublée.
	poster(url.Values{"action": {"sonde_ajouter"}, "sonde_adresse": {"192.168.1.20:5001"}, "machine": {"nas"}, "sonde_nom": {"nas.lan"}})
	cfg, _ := flotte.ConfigEffective(ctx, b.c.db, idC)
	if cfg.Collecteur == nil || len(cfg.Collecteur.Sondes) != 2 || cfg.Collecteur.Sondes[1].Nom != "nas.lan" || cfg.Collecteur.Sondes[0].Nom != "fw.lan" {
		t.Fatalf("sondes : %+v", cfg.Collecteur)
	}
	poster(url.Values{"action": {"sonde_retirer"}, "sonde_adresse": {"192.168.1.1:10443"}})
	if cfg, _ = flotte.ConfigEffective(ctx, b.c.db, idC); len(cfg.Collecteur.Sondes) != 1 {
		t.Fatalf("sonde non retirée : %+v", cfg.Collecteur.Sondes)
	}

	// Émetteurs : refusés tant que l'écoute n'est pas réglée.
	poster(url.Values{"action": {"emetteur_ajouter"}, "emetteur_adresse": {"192.168.1.1"}, "machine": {"fw"}})
	if cfg, _ = flotte.ConfigEffective(ctx, b.c.db, idC); cfg.Collecteur.Syslog != nil {
		t.Fatal("émetteur ajouté sans écoute")
	}
	poster(url.Values{"action": {"syslog"}, "ecoute": {"192.168.1.10:6515"}, "nom_certificat": {"syslog.lan.exemple.org"}})
	poster(url.Values{"action": {"emetteur_ajouter"}, "emetteur_adresse": {"192.168.1.1"}, "machine": {"fw"}})
	poster(url.Values{"action": {"emetteur_ajouter"}, "emetteur_adresse": {"192.168.1.20"}, "machine": {"nas"}})
	poster(url.Values{"action": {"emetteur_ajouter"}, "emetteur_adresse": {"8.8.8.8"}, "machine": {"nas"}}) // publique : refusée
	// Réglage de l'écoute de nouveau : les émetteurs sont gardés.
	poster(url.Values{"action": {"syslog"}, "ecoute": {"192.168.1.10:6516"}, "nom_certificat": {"syslog.lan.exemple.org"}})
	cfg, _ = flotte.ConfigEffective(ctx, b.c.db, idC)
	if s := cfg.Collecteur.Syslog; s == nil || len(s.Emetteurs) != 2 || s.Ecoute != "192.168.1.10:6516" {
		t.Fatalf("syslog : %+v", s)
	}
	poster(url.Values{"action": {"emetteur_retirer"}, "emetteur_adresse": {"192.168.1.20"}})
	if cfg, _ = flotte.ConfigEffective(ctx, b.c.db, idC); len(cfg.Collecteur.Syslog.Emetteurs) != 1 || cfg.Collecteur.Syslog.Emetteurs[0].Machine != "fw" {
		t.Fatalf("émetteur non retiré : %+v", cfg.Collecteur.Syslog.Emetteurs)
	}

	// Mesures : sonde du NAS valide 61 j, syslog du pare-feu reçu, 3 refus.
	db, err := tsdb.Open(t.TempDir(), nil, nil, tsdb.DefaultOptions(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	m := b.h.maintenant()
	app := db.Appender(context.Background())
	fin := float64(m.Add(61*24*time.Hour + time.Hour).Unix())
	for i, ts := 0, m.Add(-2*time.Hour); !ts.After(m); i, ts = i+1, ts.Add(time.Minute) {
		_, _ = app.Append(0, labels.FromStrings("__name__", "ssl_cert_not_after_timestamp_seconds", "job", "certs", "instance", "nas", "path", "192.168.1.20:5001"), ts.UnixMilli(), fin)
		_, _ = app.Append(0, labels.FromStrings("__name__", "probe_success", "job", "certs", "instance", "nas", "path", "192.168.1.20:5001"), ts.UnixMilli(), 1)
		_, _ = app.Append(0, labels.FromStrings("__name__", "mirador_collecteur_syslog_lignes_total", "instance", "collecteur-lan", "equipement", "fw"), ts.UnixMilli(), float64(i*20))
		_, _ = app.Append(0, labels.FromStrings("__name__", "mirador_collecteur_syslog_refus_total", "instance", "collecteur-lan", "motif", "emetteur"), ts.UnixMilli(), float64(i/40))
	}
	_ = app.Commit()
	b.c.lecteur = alertes.NouveauLecteur(db, nil)
	b.h.avancer(2 * time.Minute) // cache des requêtes de la console
	_, page, _ := b.get(c, "/machines/collecteur-lan?pour=fw")
	for _, attendu := range []string{
		`<span>:5001<small>expire dans 61 j</small></span>`, // sonde du NAS
		`<span>reçu<small>`,                     // syslog du pare-feu
		`<span class="pastille ok">61 j</span>`, // carte des sondes
		`>3</b>`,                                // refus sur 24 h
		`href="/machines/collecteur-lan?ajout=snmp&amp;pour=nas#snmp"`,
		`name="sonde_adresse" value="192.168.1.20:5001"`,
		`name="emetteur_adresse" value="192.168.1.1"`,
		`<option selected>fw</option>`, // ?pour=fw : équipement choisi d'avance
	} {
		if !strings.Contains(page, attendu) {
			t.Errorf("page du collecteur : %q absent", attendu)
		}
	}
	if strings.Contains(page, "<textarea") {
		t.Error("zone de texte encore présente")
	}
}

// Retour du 22/11 : « on ne voit pas Telegraf ici ». Un équipement qui
// envoie lui-même (son token) a sa colonne « Envoi direct » ; sans token
// nulle part, pas de colonne.
func TestCollecteEnvoiDirect(t *testing.T) {
	b := nouveauBanc(t)
	c, _ := b.premierAdmin("admin", mdpAdmin)
	ctx := context.Background()
	_, _ = b.c.db.Exec(`INSERT INTO machines(nom, statut, type, cree_le) VALUES ('collecteur-lan', 'approuvee', 'collecteur', 1), ('nas', 'approuvee', 'agent', 1), ('fw', 'approuvee', 'agent', 1)`)
	idC, _ := flotte.IDMachine(ctx, b.c.db, "collecteur-lan")
	for _, n := range []string{"nas", "fw"} {
		id, _ := flotte.IDMachine(ctx, b.c.db, n)
		_ = flotte.RattacherEquipement(ctx, b.c.db, idC, id, "admin", time.Now())
	}
	if _, corps, _ := b.get(c, "/machines/collecteur-lan"); strings.Contains(corps, ">Envoi direct</th>") {
		t.Error("colonne « Envoi direct » sans aucun token")
	}
	if _, err := flotte.TokenManuel(ctx, b.c.db, "fw", "admin", b.h.maintenant()); err != nil {
		t.Fatal(err)
	}
	_, corps, _ := b.get(c, "/machines/collecteur-lan")
	if !strings.Contains(corps, ">Envoi direct</th>") || !strings.Contains(corps, "token prêt") {
		t.Error("envoi direct de fw absent")
	}
}
