// SPDX-License-Identifier: GPL-3.0-or-later

package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"mirador-stack/internal/server/config"
	"mirador-stack/internal/server/console"
	"mirador-stack/internal/server/secrets"
	"mirador-stack/internal/server/stockage"
)

// navigateur : client HTTP qui garde le cookie de session lui-même (le
// cookie est « Secure » ; le serveur de test parle en HTTP simple, derrière
// nginx en production).
type navigateur struct {
	t      *testing.T
	base   string
	cookie string
	c      *http.Client
}

func (n *navigateur) faire(methode, chemin string, v url.Values) (int, string, http.Header) {
	n.t.Helper()
	var corps io.Reader
	if v != nil {
		corps = strings.NewReader(v.Encode())
	}
	r, _ := http.NewRequest(methode, n.base+chemin, corps)
	if v != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if n.cookie != "" {
		r.AddCookie(&http.Cookie{Name: console.NomCookie, Value: n.cookie})
	}
	resp, err := n.c.Do(r)
	if err != nil {
		n.t.Fatal(err)
	}
	defer resp.Body.Close()
	for _, ck := range resp.Cookies() {
		if ck.Name == console.NomCookie {
			n.cookie = ck.Value
		}
	}
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

var reCSRFe2e = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

// TestInterfaceDeBoutEnBout : vrai serveur, données de métriques réelles,
// connexion avec 2FA, puis chaque page principale et chaque tableau.
func TestInterfaceDeBoutEnBout(t *testing.T) {
	dir := t.TempDir()
	cleF := filepath.Join(dir, "cle")
	if err := os.WriteFile(cleF, []byte("AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaut()
	cfg.EcouteAgents, cfg.EcouteInterface = portLibre(t), portLibre(t)
	cfg.Donnees, cfg.FichierCle = filepath.Join(dir, "donnees"), cleF
	s, err := Demarrer(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Arreter()
	ctx := context.Background()
	now := time.Now()

	// Une machine approuvée et 1 h de métriques au format node_exporter.
	if _, err := s.db.Exec(`INSERT INTO machines(id, nom, statut, cree_le, vu_le, site, env) VALUES (1, 'vm-test', 'approuvee', 1, ?, 'maison', 'vm')`, now.Unix()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO machines(id, nom, statut, cree_le, ip_enrolement) VALUES (2, 'nouvelle', 'en_attente', 1, '192.0.2.4')`); err != nil {
		t.Fatal(err)
	}
	serie := func(nom string, v func(i int) float64, etq ...string) stockage.Serie {
		sr := stockage.Serie{Labels: []stockage.Label{{Nom: "__name__", Valeur: nom}, {Nom: "instance", Valeur: "vm-test"},
			{Nom: "job", Valeur: "integrations/unix"}, {Nom: "site", Valeur: "maison"}, {Nom: "env", Valeur: "vm"}}}
		for i := 0; i+1 < len(etq); i += 2 {
			sr.Remplacer(etq[i], etq[i+1])
		}
		for i := 0; i < 120; i++ {
			sr.Points = append(sr.Points, stockage.Point{T: now.Add(time.Duration(i-119) * 30 * time.Second).UnixMilli(), V: v(i)})
		}
		return sr
	}
	donnees := []stockage.Serie{
		serie("node_uname_info", func(int) float64 { return 1 }),
		serie("node_cpu_seconds_total", func(i int) float64 { return float64(i) * 25 }, "cpu", "0", "mode", "idle"),
		serie("node_memory_MemAvailable_bytes", func(int) float64 { return 1 << 30 }),
		serie("node_memory_MemTotal_bytes", func(int) float64 { return 4 << 30 }),
		serie("node_load5", func(int) float64 { return 0.3 }),
		serie("node_filesystem_avail_bytes", func(int) float64 { return 10 << 30 }, "mountpoint", "/", "fstype", "ext4"),
		serie("node_filesystem_size_bytes", func(int) float64 { return 40 << 30 }, "mountpoint", "/", "fstype", "ext4"),
		serie("node_network_receive_bytes_total", func(i int) float64 { return float64(i) * 1e6 }, "device", "eth0"),
	}
	if err := s.met.Ecrire(ctx, donnees); err != nil {
		t.Fatal(err)
	}

	// Compte admin avec une 2FA connue.
	if err := console.CreerUtilisateur(ctx, s.db, "admin", console.RoleAdmin, "une longue phrase de passe", "test", "", now); err != nil {
		t.Fatal(err)
	}
	secret, _ := console.NouveauSecretTOTP()
	ch, _ := secrets.Chiffrer(s.cle, "console.totp", secret)
	if err := console.DefinirTOTP(ctx, s.db, "admin", ch, 0, "test", "", now); err != nil {
		t.Fatal(err)
	}
	n := &navigateur{t: t, base: "http://" + cfg.EcouteInterface, c: &http.Client{Timeout: 20 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	if code, _, _ := n.faire(http.MethodPost, "/connexion", url.Values{"nom": {"admin"}, "mot_de_passe": {"une longue phrase de passe"}}); code != http.StatusSeeOther {
		t.Fatalf("connexion : %d", code)
	}
	_, corps, _ := n.faire(http.MethodGet, "/connexion/code", nil)
	code2fa, _ := console.CodeTOTP(secret, time.Now())
	if code, _, h := n.faire(http.MethodPost, "/connexion/code", url.Values{"csrf": {reCSRFe2e.FindStringSubmatch(corps)[1]}, "code": {code2fa}}); code != http.StatusSeeOther || h.Get("Location") != "/" {
		t.Fatalf("code 2FA : %d", code)
	}

	pages := map[string][]string{
		"/":                    {"Vue d'ensemble", "vm-test", "en attente d'approbation"},
		"/machines":            {"vm-test", "nouvelle", "192.0.2.4"},
		"/machines/vm-test":    {"vm-test", "data-courbe=", "Occupation des systemes de fichiers"},
		"/mises-a-jour":        {"Versions publiées"},
		"/tableaux":            {"Serveurs Linux", "Proxmox"},
		"/fragments/accueil":   {"machines"},
		"/compte":              {"Mon compte"},
		"/securite":            {"Sessions ouvertes"},
		"/utilisateurs":        {"admin"},
		"/machines/inexistant": {"introuvable"},
	}
	for _, t2 := range []string{"serveurs-linux", "proxmox", "opnsense", "synology", "unifi", "certificats-tls"} {
		pages["/tableaux/"+t2] = []string{"<h1>"}
	}
	for chemin, attendus := range pages {
		code, corps, _ := n.faire(http.MethodGet, chemin, nil)
		if code != http.StatusOK && !(chemin == "/machines/inexistant" && code == http.StatusNotFound) {
			t.Errorf("%s : %d\n%s", chemin, code, corps)
			continue
		}
		for _, a := range attendus {
			if !strings.Contains(corps, a) {
				t.Errorf("%s : %q absent", chemin, a)
			}
		}
		if strings.Contains(corps, "erreur d'affichage") || strings.Contains(corps, "ZgotmplZ") {
			t.Errorf("%s : erreur de gabarit", chemin)
		}
	}
	// La vue d'ensemble calcule l'état du parc (CPU, mémoire 75 %…).
	_, corps, _ = n.faire(http.MethodGet, "/", nil)
	if !strings.Contains(corps, "75") {
		t.Error("mémoire (75 %) absente de l'état du parc")
	}
	// Données d'une courbe : JSON avec la série de la machine.
	_, corps, _ = n.faire(http.MethodGet, "/machines/vm-test", nil)
	m := regexp.MustCompile(`data-courbe="([^"]+)"`).FindStringSubmatch(corps)
	if m == nil {
		t.Fatal("aucune courbe")
	}
	code, js, h := n.faire(http.MethodGet, strings.ReplaceAll(m[1], "&amp;", "&"), nil)
	if code != 200 || !strings.HasPrefix(h.Get("Content-Type"), "application/json") || !strings.Contains(js, `"series":[{"nom":"vm-test`) {
		t.Errorf("courbe : %d %s", code, js[:min(len(js), 300)])
	}
	// Approuver la machine en attente depuis l'interface.
	_, corps, _ = n.faire(http.MethodGet, "/machines", nil)
	csrf := reCSRFe2e.FindStringSubmatch(corps)[1]
	if code, _, _ := n.faire(http.MethodPost, "/machines/nouvelle", url.Values{"csrf": {csrf}, "action": {"approuver"}, "avec_ip": {"1"}}); code != http.StatusSeeOther {
		t.Fatalf("approbation : %d", code)
	}
	var statut, ips string
	_ = s.db.QueryRow(`SELECT statut, ips_autorisees FROM machines WHERE nom = 'nouvelle'`).Scan(&statut, &ips)
	if statut != "approuvee" || ips != "192.0.2.4" {
		t.Errorf("après approbation : %s %q", statut, ips)
	}

	// --- Alertes et règles ---
	// Règle fabriquée depuis le modèle « CPU élevé » (seuil 1 % : la machine
	// de test est à ~17 %), testée sur 24 h, puis enregistrée.
	_, corps, _ = n.faire(http.MethodGet, "/regles/nouvelle", nil)
	csrf = reCSRFe2e.FindStringSubmatch(corps)[1]
	code, corps, _ = n.faire(http.MethodPost, "/regles/nouvelle", url.Values{"csrf": {csrf}, "modele": {"cpu"}, "nom": {"CpuTest"},
		"cible": {"machine:vm-test"}, "seuil": {"1"}, "pour": {"5m"}, "gravite": {"warning"}})
	if code != 200 || !strings.Contains(corps, "auraient déclenché") || !strings.Contains(corps, `instance=vm-test`) {
		t.Fatalf("modèle CPU : %d\n%s", code, corps)
	}
	expr := regexp.MustCompile(`(?s)<textarea name="expr"[^>]*>(.*?)</textarea>`).FindStringSubmatch(corps)[1]
	expr = strings.NewReplacer("&#34;", `"`, "&amp;", "&", "&gt;", ">", "&lt;", "<", "&#39;", "'").Replace(expr)
	code, _, _ = n.faire(http.MethodPost, "/regles/enregistrer", url.Values{"csrf": {csrf}, "nouvelle": {"1"}, "nom": {"CpuTest"},
		"groupe": {"perso"}, "expr": {expr}, "intervalle": {"1m"}, "pour": {"5m"}, "gravite": {"warning"}, "active": {"1"},
		"resume": {"CPU {{ $value }}"}, "bouton": {"enregistrer"}})
	if code != http.StatusSeeOther {
		t.Fatalf("enregistrement de la règle : %d", code)
	}
	for chemin, attendu := range map[string]string{"/regles": "CpuTest", "/regles/modifier/CpuTest": "node_cpu_seconds_total",
		"/regles/historique": "CpuTest", "/regles/export.yaml": "nom: CpuTest"} {
		if code, corps, _ := n.faire(http.MethodGet, chemin, nil); code != 200 || !strings.Contains(corps, attendu) {
			t.Errorf("%s : %d, %q absent", chemin, code, attendu)
		}
	}
	// Une alerte en cours : détail, accusé de réception, silence en un clic.
	if _, err := s.db.Exec(`INSERT INTO alertes(id, regle, etiquettes, gravite, etat, debut, active_le, resume)
		VALUES (42, 'CpuTest', '{"alertname":"CpuTest","instance":"vm-test","severity":"warning"}', 'warning', 'active', ?, ?, 'CPU <b>haut</b>')`,
		now.UnixMilli(), now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	code, corps, _ = n.faire(http.MethodGet, "/alertes/42", nil)
	if code != 200 || !strings.Contains(corps, `{alertname=&#34;CpuTest&#34;,instance=&#34;vm-test&#34;}`) || strings.Contains(corps, "<b>haut</b>") {
		t.Errorf("détail de l'alerte (échappement, sélecteur) : %d", code)
	}
	csrf = reCSRFe2e.FindStringSubmatch(corps)[1]
	n.faire(http.MethodPost, "/alertes/42", url.Values{"csrf": {csrf}, "action": {"acquitter"}, "commentaire": {"je regarde"}})
	n.faire(http.MethodPost, "/alertes/42", url.Values{"csrf": {csrf}, "action": {"silence"}, "duree": {"4h"}})
	var par string
	_ = s.db.QueryRow(`SELECT acquittee_par FROM alertes WHERE id = 42`).Scan(&par)
	var nbSil int
	_ = s.db.QueryRow(`SELECT count(*) FROM silences WHERE selecteur LIKE '%CpuTest%vm-test%'`).Scan(&nbSil)
	if par != "web:admin" || nbSil != 1 {
		t.Errorf("acquittement %q, silences %d", par, nbSil)
	}
	for chemin, attendu := range map[string]string{"/alertes": "CpuTest", "/sourdine": "CpuTest", "/envois": "Historique des envois",
		"/comparaison": "Comparaison avec vmalert", "/comparaison.csv": "regle,machine"} {
		if code, corps, _ := n.faire(http.MethodGet, chemin, nil); code != 200 || !strings.Contains(corps, attendu) {
			t.Errorf("%s : %d, %q absent", chemin, code, attendu)
		}
	}

	// --- Logs ---
	ligne := func(msg string, champs map[string]string) stockage.Ligne {
		champs["host"] = "vm-test"
		return stockage.Ligne{T: time.Now().UnixMilli(), Msg: msg, Champs: champs}
	}
	if err := s.jx.Ecrire(ctx, []stockage.Ligne{
		ligne("connexion refusée <script>alert(1)</script>", map[string]string{"job": "journal", "unit": "ssh.service", "level": "err"}),
		ligne("même ligne", map[string]string{"job": "journal", "unit": "cron.service"}),
		ligne("même ligne", map[string]string{"job": "journal", "unit": "cron.service"}),
		ligne("[12345.6] INPUT-DROP: IN=eth0 OUT= MAC=00 SRC=203.0.113.9 DST=192.0.2.1 PROTO=TCP SPT=4444 DPT=22", map[string]string{"job": "journal"}),
	}); err != nil {
		t.Fatal(err)
	}
	code, corps, _ = n.faire(http.MethodGet, "/logs?host=vm-test&periode=1h", nil)
	if code != 200 || strings.Contains(corps, "<script>alert") || !strings.Contains(corps, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Errorf("logs : %d ; le contenu doit être échappé", code)
	}
	if !strings.Contains(corps, "× 2") {
		t.Error("lignes répétées non repliées")
	}
	if _, corps, _ = n.faire(http.MethodGet, "/logs?mots=refus%C3%A9e&periode=1h", nil); !strings.Contains(corps, "connexion refus") {
		t.Error("recherche par mot")
	}
	if _, corps, _ = n.faire(http.MethodGet, "/flux-bloques?periode=1h", nil); !strings.Contains(corps, "203.0.113.9") || !strings.Contains(corps, "INPUT-DROP:") {
		t.Errorf("flux bloqués : %s", corps[strings.Index(corps, "<main>"):])
	}
	if code, _, _ = n.faire(http.MethodGet, "/logs?host=%22%3E%3Cscript%3E", nil); code != 200 {
		t.Errorf("filtre invalide : %d", code)
	}

	// --- Réglages ---
	for _, chemin := range []string{"/reglages", "/reglages/notifications", "/reglages/conservation", "/reglages/sauvegardes",
		"/reglages/agents", "/reglages/agents?portee=machine:1", "/reglages/interface", "/audit"} {
		if code, corps, _ := n.faire(http.MethodGet, chemin, nil); code != 200 || strings.Contains(corps, "erreur d'affichage") {
			t.Errorf("%s : %d", chemin, code)
		}
	}
	code2 := func() string {
		time.Sleep(10 * time.Millisecond)
		// Chaque code ne sert qu'une fois : on attend le pas suivant.
		for {
			c, _ := console.CodeTOTP(secret, time.Now())
			if c != code2fa {
				code2fa = c
				return c
			}
			time.Sleep(time.Second)
		}
	}
	_, corps, _ = n.faire(http.MethodGet, "/reglages/notifications", nil)
	csrf = reCSRFe2e.FindStringSubmatch(corps)[1]
	// Telegram configuré depuis le formulaire ; le jeton est chiffré en base et jamais réaffiché.
	formNotif := url.Values{"csrf": {csrf}, "mode": {"ombre"}, "fuseau": {"Europe/Paris"}, "grouper_par": {"alertname, instance"},
		"regles_panne": {"AgentMuet"}, "principal": {"telegram"}, "secours": {""}, "echecs": {"3"}, "retours": {"3"},
		"verif_telegram": {"5m"}, "verif_autres": {"1m"}, "tg_actif": {"1"}, "tg_jeton": {"123456789:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"},
		"tg_chat": {"-1001234567890"}, "olvid_confirmation": {"30s"}, "hb_intervalle": {"1m"}, "tous": {"critical"}}
	for _, g := range []string{"critical", "warning", "info"} {
		formNotif.Set("attente_"+g, "30s")
		formNotif.Set("intervalle_"+g, "5m")
		formNotif.Set("rappel_"+g, "12h")
	}
	formNotif.Set("code", code2())
	if code, _, _ := n.faire(http.MethodPost, "/reglages/notifications", formNotif); code != http.StatusSeeOther {
		t.Fatalf("réglages des notifications : %d", code)
	}
	_, corps, _ = n.faire(http.MethodGet, "/reglages/notifications", nil)
	var brutNotif string
	_ = s.db.QueryRow(`SELECT valeur FROM reglages WHERE cle = 'notifications'`).Scan(&brutNotif)
	if !strings.Contains(brutNotif, "chiffre:v1:") || strings.Contains(brutNotif, "AAAAAAAAAAAA") || strings.Contains(corps, "AAAAAAAAAAAA") {
		t.Errorf("jeton Telegram : doit être chiffré en base et jamais affiché\n%s", brutNotif)
	}
	// Un seul canal à la fois (réponse du 10/11) : « tous » est ignoré.
	if !strings.Contains(brutNotif, `"tous":[]`) {
		t.Errorf("envoi sur tous les canaux encore possible : %s", brutNotif)
	}
	if !strings.Contains(corps, "-1001234567890") || !strings.Contains(corps, "(enregistré)") {
		t.Error("réglages Telegram non affichés")
	}
	// Réduction de la conservation : confirmation + 2FA, puis différée et annulable.
	_, corps, _ = n.faire(http.MethodGet, "/reglages/conservation", nil)
	csrf = reCSRFe2e.FindStringSubmatch(corps)[1]
	formC := url.Values{"csrf": {csrf}, "action": {"enregistrer"}, "brut": {"2d"}, "agrege": {"30d"}, "logs": {"30d"},
		"max_brut": {"4096"}, "max_agrege": {"1024"}, "max_logs": {"3072"}}
	n.faire(http.MethodPost, "/reglages/conservation", formC)
	if a, _ := console.LireAttente(ctx, s.db); a != nil {
		t.Fatal("réduction acceptée sans confirmation")
	}
	formC.Set("confirmer", "1")
	formC.Set("code", code2())
	n.faire(http.MethodPost, "/reglages/conservation", formC)
	a, _ := console.LireAttente(ctx, s.db)
	if a == nil || time.Duration(a.Valeur.Brut) != 48*time.Hour {
		t.Fatalf("réduction non programmée : %+v", a)
	}
	if cons, _ := console.LireConservation(ctx, s.db); time.Duration(cons.Brut) != 7*24*time.Hour {
		t.Error("la réduction ne doit pas être appliquée tout de suite")
	}
	n.faire(http.MethodPost, "/reglages/conservation", url.Values{"csrf": {csrf}, "action": {"annuler"}})
	if a, _ := console.LireAttente(ctx, s.db); a != nil {
		t.Error("réduction non annulée")
	}
	// Configuration typée d'un agent ; une valeur hors bornes est refusée.
	// Sans code 2FA : refusé (revue du 11/11).
	n.faire(http.MethodPost, "/reglages/agents", url.Values{"csrf": {csrf}, "portee": {"machine:1"}, "intervalle": {"60s"}, "systemd": {"non"}})
	var sansCode string
	_ = s.db.QueryRow(`SELECT valeur FROM reglages WHERE cle = 'config_agent:machine:1'`).Scan(&sansCode)
	if sansCode != "" {
		t.Errorf("configuration des agents enregistrée sans code 2FA : %s", sansCode)
	}
	n.faire(http.MethodPost, "/reglages/agents", url.Values{"csrf": {csrf}, "portee": {"machine:1"}, "intervalle": {"60s"}, "systemd": {"non"}, "code": {code2()}})
	var docAgent string
	_ = s.db.QueryRow(`SELECT valeur FROM reglages WHERE cle = 'config_agent:machine:1'`).Scan(&docAgent)
	if !strings.Contains(docAgent, `"intervalle":"1m0s"`) && !strings.Contains(docAgent, `"intervalle":"60s"`) && !strings.Contains(docAgent, `"intervalle":"1m"`) {
		t.Errorf("configuration de l'agent : %s", docAgent)
	}
	n.faire(http.MethodPost, "/reglages/agents", url.Values{"csrf": {csrf}, "portee": {"defaut"}, "intervalle": {"1ms"}, "code": {code2()}})
	if err := s.db.QueryRow(`SELECT valeur FROM reglages WHERE cle = 'config_agent:defaut'`).Scan(&docAgent); err == nil {
		t.Errorf("intervalle hors bornes accepté : %s", docAgent)
	}
	// Test du masquage avec le code de l'agent.
	_, corps, _ = n.faire(http.MethodPost, "/reglages/agents/masquage", url.Values{"csrf": {csrf}, "ligne": {"password=secret123 ok"},
		"modeles": {"mots_de_passe"}})
	if strings.Contains(corps, "secret123") && !strings.Contains(corps, "password=secret123 ok</textarea>") {
		t.Error("masquage : mot de passe visible dans le résultat")
	}
	if !strings.Contains(corps, "***") {
		t.Error("masquage : résultat sans ***")
	}
	// Export puis import (règles) ; l'export sans secret ne contient aucun chiffré.
	code, yml, _ := n.faire(http.MethodGet, "/reglages/export.yaml", nil)
	if code != 200 || !strings.Contains(yml, "version: 1") || !strings.Contains(yml, "CpuTest") || strings.Contains(yml, "chiffre:v1:") {
		t.Errorf("export : %d\n%.500s", code, yml)
	}
	if _, err := console.LireConfiguration(strings.NewReader(yml)); err != nil {
		t.Errorf("l'export ne se relit pas : %v", err)
	}
	// Journal d'audit : les actions de l'interface y sont, sans secret.
	_, corps, _ = n.faire(http.MethodGet, "/audit", nil)
	if !strings.Contains(corps, "web:admin") || !strings.Contains(corps, "reglages_notifications") || strings.Contains(corps, "AAAAAAAAAAAA") {
		t.Error("audit incomplet ou avec un secret")
	}
	// Olvid pas activé : ni onglet ni page (renvoi vers Notifications).
	if code, _, _ := n.faire(http.MethodGet, "/reglages/olvid", nil); code != http.StatusSeeOther {
		t.Errorf("page Olvid non activé : %d (303 attendu)", code)
	}
	if _, corps, _ := n.faire(http.MethodGet, "/reglages/conservation", nil); strings.Contains(corps, `href="/reglages/olvid"`) {
		t.Error("onglet Olvid affiché alors qu'Olvid n'est pas activé")
	}
}
