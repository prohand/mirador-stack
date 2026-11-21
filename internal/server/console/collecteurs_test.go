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
	if !strings.Contains(corps, "/api/agent/v1/install/collecteur -o mirador-collector-install.sh") || !strings.Contains(corps, "--auto-update") ||
		!strings.Contains(corps, "--serveur https://ADRESSE-INGESTION --nom collecteur-lan --auto-update") {
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
	if _, corps, _ = b.get(c, "/machines/ajouter?quoi=equipement"); !strings.Contains(corps, `<select name="collecteur">`) {
		t.Fatal("choix « Relevé par » absent")
	}
	_, corps, _ = b.post(c, "/machines/equipement", url.Values{"csrf": {csrf}, "nom": {"imprimante"}, "collecteur": {"collecteur-lan"}})
	if strings.Contains(corps, "Brancher «") {
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
	// Avancement (SPEC § 32) : compte DNS à régler, demande attendue.
	if _, corps, _ := b.get(c, "/machines/collecteur-lan"); !strings.Contains(corps, `id="syslog"`) ||
		!strings.Contains(corps, `href="/reglages/certificats">Réglages › Certificats</a>`) ||
		!strings.Contains(corps, `<li class="av-fait"><span class="av-puce" aria-hidden="true"></span><div><strong>Syslog du collecteur</strong>`) ||
		!strings.Contains(corps, `<li class="av-encours"><span class="av-puce" aria-hidden="true"></span><div><strong>Demande du collecteur</strong>`) ||
		!strings.Contains(corps, `data-recharger="15"`) || strings.Contains(corps, "dns_desec_jeton") {
		t.Error("avancement du certificat absent, ou réglages DNS encore sur la page du collecteur")
	}
	// Réglages DNS (deSEC) dans Réglages › Certificats : jeton chiffré, jamais réaffiché.
	b.post(c, "/reglages/certificats", url.Values{"csrf": {csrf}, "action": {"reglages"}, "fournisseur": {"desec"},
		"domaine": {"exemple.org"}, "dns_desec_jeton": {"jeton-desec-tres-secret"}, "attente_max": {"300"}})
	_, corps, _ := b.get(c, "/reglages/certificats")
	if strings.Contains(corps, "jeton-desec-tres-secret") || !strings.Contains(corps, "déjà enregistré") || !strings.Contains(corps, "one <code>exemple.org</code>") ||
		!strings.Contains(corps, `<option value="300" selected>5 min</option>`) || !strings.Contains(corps, "(300 s au plus)") {
		t.Error("jeton deSEC réaffiché ou non enregistré")
	}
	// Autre fournisseur (OVHcloud) : clé d'application réaffichée, secrets non.
	b.post(c, "/reglages/certificats", url.Values{"csrf": {csrf}, "action": {"reglages"}, "fournisseur": {"ovh"},
		"domaine": {"exemple.org"}, "dns_ovh_point": {"ovh-eu"}, "dns_ovh_cle_application": {"cle-app-visible"},
		"dns_ovh_secret_application": {"secret-app-cache"}, "dns_ovh_cle_consommateur": {"cle-conso-cachee"}})
	_, corps, _ = b.get(c, "/reglages/certificats")
	if !strings.Contains(corps, `value="ovh" selected`) || !strings.Contains(corps, "cle-app-visible") ||
		strings.Contains(corps, "secret-app-cache") || strings.Contains(corps, "cle-conso-cachee") {
		t.Error("réglages OVHcloud mal enregistrés ou secrets réaffichés")
	}
	// Certificat obtenu (simulé) : posé dans la configuration du collecteur.
	if _, err := b.c.db.Exec(`INSERT INTO certificats_syslog(machine_id, nom, csr, certificat, fin, obtenu_le) VALUES (?, 'syslog.lan.exemple.org', 'x', 'CHAINE-PEM', 4102444800, 1)`, idC); err != nil {
		t.Fatal(err)
	}
	if cfg, _ = flotte.ConfigEffective(ctx, b.c.db, idC); cfg.Collecteur.Syslog.Certificat != "CHAINE-PEM" {
		t.Errorf("certificat non posé : %q", cfg.Collecteur.Syslog.Certificat)
	}
	if _, corps, _ = b.get(c, "/machines/collecteur-lan"); !strings.Contains(corps, `<li class="av-fait"><span class="av-puce" aria-hidden="true"></span><div><strong>Certificat</strong>`) ||
		!strings.Contains(corps, "valable jusqu&#39;au 01/01/2100") {
		t.Error("certificat obtenu non affiché")
	}
	// Démarche en cours : étape et fin de l'attente DNS ; puis échec.
	_, _ = b.c.db.Exec(`UPDATE certificats_syslog SET etape = 'propagation', etape_info = 'visible sur 1/2 serveurs DNS', etape_le = ?, etape_jusqua = ? WHERE machine_id = ?`,
		b.h.maintenant().Unix(), b.h.maintenant().Add(time.Minute).Unix(), idC)
	if _, corps, _ = b.get(c, "/reglages/certificats"); !strings.Contains(corps, "Let&#39;s Encrypt : attente de la publication DNS (4/6)") ||
		!strings.Contains(corps, "visible sur 1/2 serveurs DNS · au plus jusqu&#39;à "+b.h.maintenant().Add(time.Minute).In(fuseau.Load()).Format("15:04")) {
		t.Errorf("étape en cours absente : %s", section(corps, "Certificats des collecteurs"))
	}
	_, _ = b.c.db.Exec(`UPDATE certificats_syslog SET etape = 'validation', erreur = 'ACME : défi refusé', essai_le = ? WHERE machine_id = ?`, b.h.maintenant().Unix(), idC)
	if _, corps, _ = b.get(c, "/machines/collecteur-lan"); !strings.Contains(corps, `<li class="av-erreur"><span class="av-puce" aria-hidden="true"></span><div><strong>Let&#39;s Encrypt : vérification par Let&#39;s Encrypt</strong>`) ||
		!strings.Contains(corps, "nouvel essai seul vers") {
		t.Error("échec et son étape absents")
	}
	_, _ = b.c.db.Exec(`UPDATE certificats_syslog SET etape = '', erreur = '' WHERE machine_id = ?`, idC)
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

// SNMP v3 et sondes dans l'interface (SPEC § 23.15) : mots de passe
// obligatoires puis gardés, chiffrés en base, jamais réaffichés.
func TestSNMPInterface(t *testing.T) {
	b := nouveauBanc(t)
	c, secret := b.premierAdmin("admin", mdpAdmin)
	ctx := context.Background()
	csrf := b.csrf(c, "/machines")
	if _, err := b.c.db.Exec(`INSERT INTO machines(nom, statut, type, cree_le) VALUES ('collecteur-lan', 'approuvee', 'collecteur', 1), ('nas', 'approuvee', 'agent', 1)`); err != nil {
		t.Fatal(err)
	}
	idC, _ := flotte.IDMachine(ctx, b.c.db, "collecteur-lan")
	idNas, _ := flotte.IDMachine(ctx, b.c.db, "nas")
	_ = flotte.RattacherEquipement(ctx, b.c.db, idC, idNas, "admin", time.Now())
	b.h.avancer(31 * time.Second)
	f := url.Values{"csrf": {csrf}, "action": {"snmp"}, "machine": {"nas"}, "adresse": {"192.168.1.20"}, "prereglage": {"synology"},
		"utilisateur": {"mirador"}, "auth": {"SHA256"}, "chiffrement": {"AES"}, "code": {b.code(secret)}}
	b.post(c, "/machines/collecteur-lan", f)
	if doc, _, _ := flotte.ConfigMachine(ctx, b.c.db, idC); strings.Contains(string(doc), "snmp") {
		t.Fatal("cible sans mots de passe acceptée")
	}
	f.Del("code")
	f.Set("mot_de_passe", "auth-tres-secret")
	f.Set("cle_chiffrement", "priv-tres-secret")
	b.post(c, "/machines/collecteur-lan", f)
	doc, _, _ := flotte.ConfigMachine(ctx, b.c.db, idC)
	if !strings.Contains(string(doc), `"utilisateur":"mirador"`) || strings.Contains(string(doc), "tres-secret") || !strings.Contains(string(doc), "chiffre:v1:") {
		t.Fatalf("mots de passe non chiffrés en base : %s", doc)
	}
	_, corps, _ := b.get(c, "/machines/collecteur-lan")
	if !strings.Contains(corps, `id="snmp"`) || strings.Contains(corps, "tres-secret") || strings.Contains(corps, "chiffre:v1:") {
		t.Error("page SNMP : bloc absent ou mot de passe affiché")
	}
	// Modification sans mots de passe : gardés, tant que l'adresse ne change pas.
	f.Set("mot_de_passe", "")
	f.Set("cle_chiffrement", "")
	f.Set("auth", "SHA512")
	b.post(c, "/machines/collecteur-lan", f)
	doc2, _, _ := flotte.ConfigMachine(ctx, b.c.db, idC)
	if !strings.Contains(string(doc2), `"auth":"SHA512"`) || strings.Count(string(doc2), "chiffre:v1:") != 2 {
		t.Fatalf("modification : %s", doc2)
	}
	// Adresse changée sans ressaisir les mots de passe : refusé (audit du
	// 16/11 : un secret gardé ne part pas vers un autre équipement).
	f.Set("adresse", "192.168.1.21:161")
	b.post(c, "/machines/collecteur-lan", f)
	if d, _, _ := flotte.ConfigMachine(ctx, b.c.db, idC); strings.Contains(string(d), "192.168.1.21:161") {
		t.Fatalf("adresse changée avec les anciens mots de passe : %s", d)
	}
	// Avec les mots de passe : accepté.
	f.Set("mot_de_passe", "tres-secret-auth")
	f.Set("cle_chiffrement", "tres-secret-priv")
	b.post(c, "/machines/collecteur-lan", f)
	doc2, _, _ = flotte.ConfigMachine(ctx, b.c.db, idC)
	if !strings.Contains(string(doc2), "192.168.1.21:161") || strings.Count(string(doc2), "chiffre:v1:") != 2 {
		t.Fatalf("adresse changée avec ses mots de passe : %s", doc2)
	}
	f.Set("mot_de_passe", "")
	f.Set("cle_chiffrement", "")
	// Adresse publique refusée.
	f.Set("adresse", "8.8.8.8")
	b.post(c, "/machines/collecteur-lan", f)
	if doc3, _, _ := flotte.ConfigMachine(ctx, b.c.db, idC); strings.Contains(string(doc3), "8.8.8.8") {
		t.Error("adresse publique acceptée")
	}
	// Sondes.
	b.post(c, "/machines/collecteur-lan", url.Values{"csrf": {csrf}, "action": {"sondes"}, "sondes": {"192.168.1.20:5001 nas\n192.168.1.20:443 nas NAS.lan"}})
	cfg, _ := flotte.ConfigEffective(ctx, b.c.db, idC)
	if len(cfg.Collecteur.Sondes) != 2 || cfg.Collecteur.Sondes[1].Nom != "nas.lan" {
		t.Fatalf("sondes : %+v", cfg.Collecteur.Sondes)
	}
	// Retirer.
	b.post(c, "/machines/collecteur-lan", url.Values{"csrf": {csrf}, "action": {"snmp_retirer"}, "machine": {"nas"}})
	if cfg, _ = flotte.ConfigEffective(ctx, b.c.db, idC); len(cfg.Collecteur.SNMP) != 0 {
		t.Error("cible SNMP non retirée")
	}
}

// Contrôleur UniFi dans l'interface (SPEC § 23.16) : secret chiffré, gardé
// à la modification, empreinte normalisée, adresse du LAN seulement.
func TestUniFiInterface(t *testing.T) {
	b := nouveauBanc(t)
	c, secret := b.premierAdmin("admin", mdpAdmin)
	ctx := context.Background()
	csrf := b.csrf(c, "/machines")
	if _, err := b.c.db.Exec(`INSERT INTO machines(nom, statut, type, cree_le) VALUES ('collecteur-lan', 'approuvee', 'collecteur', 1), ('unifi', 'approuvee', 'agent', 1)`); err != nil {
		t.Fatal(err)
	}
	idC, _ := flotte.IDMachine(ctx, b.c.db, "collecteur-lan")
	idU, _ := flotte.IDMachine(ctx, b.c.db, "unifi")
	_ = flotte.RattacherEquipement(ctx, b.c.db, idC, idU, "admin", time.Now())
	b.h.avancer(31 * time.Second)
	emp := strings.Repeat("AB:", 31) + "AB"
	f := url.Values{"csrf": {csrf}, "action": {"unifi"}, "machine": {"unifi"}, "adresse": {"https://192.168.1.5/"}, "mode": {"compte"},
		"utilisateur": {"lecture"}, "secret": {"mot-de-passe-unifi"}, "empreinte": {emp}, "sites": {"default"}, "code": {b.code(secret)}}
	b.post(c, "/machines/collecteur-lan", f)
	doc, _, _ := flotte.ConfigMachine(ctx, b.c.db, idC)
	if !strings.Contains(string(doc), `"adresse":"https://192.168.1.5"`) || strings.Contains(string(doc), "mot-de-passe-unifi") ||
		!strings.Contains(string(doc), `"empreinte":"`+strings.Repeat("ab", 32)+`"`) {
		t.Fatalf("UniFi en base : %s", doc)
	}
	_, corps, _ := b.get(c, "/machines/collecteur-lan")
	if !strings.Contains(corps, "compte <code>lecture</code>") || strings.Contains(corps, "mot-de-passe-unifi") {
		t.Error("page : relevé UniFi absent ou secret affiché")
	}
	// Adresse changée sans ressaisir le secret : refusé (audit du 16/11).
	f.Del("code")
	f.Set("secret", "")
	f.Set("adresse", "https://unifi.lan")
	b.post(c, "/machines/collecteur-lan", f)
	cfg, _ := flotte.ConfigEffective(ctx, b.c.db, idC)
	if cfg.Collecteur.UniFi == nil || cfg.Collecteur.UniFi.Adresse != "https://192.168.1.5" {
		t.Fatalf("adresse changée avec l'ancien secret : %+v", cfg.Collecteur.UniFi)
	}
	// Avec le secret : accepté ; ensuite, modification sans secret (même
	// adresse) : gardé. Adresse publique refusée.
	f.Set("secret", "mot-de-passe-unifi")
	b.post(c, "/machines/collecteur-lan", f)
	f.Set("secret", "")
	f.Set("sites", "default, annexe")
	b.post(c, "/machines/collecteur-lan", f)
	cfg, _ = flotte.ConfigEffective(ctx, b.c.db, idC)
	if cfg.Collecteur.UniFi == nil || cfg.Collecteur.UniFi.Adresse != "https://unifi.lan" || len(cfg.Collecteur.UniFi.Sites) != 2 ||
		!strings.HasPrefix(cfg.Collecteur.UniFi.MotDePasse, "chiffre:v1:") {
		t.Fatalf("modification : %+v", cfg.Collecteur.UniFi)
	}
	f.Set("adresse", "https://8.8.8.8")
	b.post(c, "/machines/collecteur-lan", f)
	if cfg, _ = flotte.ConfigEffective(ctx, b.c.db, idC); cfg.Collecteur.UniFi.Adresse == "https://8.8.8.8" {
		t.Error("adresse publique acceptée")
	}
	b.post(c, "/machines/collecteur-lan", url.Values{"csrf": {csrf}, "action": {"unifi_retirer"}})
	if cfg, _ = flotte.ConfigEffective(ctx, b.c.db, idC); cfg.Collecteur.UniFi != nil {
		t.Error("relevé UniFi non retiré")
	}
}
