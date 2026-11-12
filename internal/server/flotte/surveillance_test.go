// SPDX-License-Identifier: GPL-3.0-or-later

package flotte

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"mirador-stack/internal/protocole"
)

// Les réglages de surveillance ne partent que vers les agents qui les
// connaissent : un ancien agent refuserait tout le document (SPEC § 19).
func TestSurveillanceSelonCapacite(t *testing.T) {
	b := nouveauBanc(t)
	token, _ := TokenManuel(ctx, b.db, "pc", "admin", t0)
	id, _ := IDMachine(ctx, b.db, "pc")
	portee := "machine:" + itoa(id)
	if err := DefinirConfig(ctx, b.db, "defaut", []byte(`{"intervalle":"60s"}`), "admin", t0); err != nil {
		t.Fatal(err)
	}
	err := ModifierConfig(ctx, b.db, portee, func(m map[string]any) {
		m["zfs"] = true
		m["temperatures"] = false
		m["systemd_validation"] = true
		m["systemd_surveiller"] = []string{"ssh.service"}
		m["systemd_ignorer"] = []string{"fwupd-refresh.service"}
	}, "admin", t0)
	if err != nil {
		t.Fatal(err)
	}

	// Ancien agent (pas d'en-tête) : seules les clés qu'il connaît.
	ancien := b.requete(http.MethodGet, protocole.CheminConfig, token, nil, "")
	if ancien.Code != http.StatusOK || ancien.Body.String() != `{"intervalle":"60s","systemd_ignorer":["fwupd-refresh.service"]}` {
		t.Fatalf("ancien agent : %d %s", ancien.Code, ancien.Body)
	}
	// Agent récent : tout le document, et un ETag différent.
	recent := b.requete(http.MethodGet, protocole.CheminConfig, token, nil, "", protocole.EnteteCapacites, protocole.CapaciteSurveillance)
	for _, attendu := range []string{`"zfs":true`, `"temperatures":false`, `"systemd_surveiller":["ssh.service"]`} {
		if !strings.Contains(recent.Body.String(), attendu) {
			t.Errorf("agent récent : %s absent de %s", attendu, recent.Body)
		}
	}
	if ancien.Header().Get("ETag") == recent.Header().Get("ETag") {
		t.Error("les deux documents doivent avoir des ETag différents")
	}
	if w := b.requete(http.MethodGet, protocole.CheminConfig, token, nil, "", "If-None-Match", ancien.Header().Get("ETag")); w.Code != http.StatusNotModified {
		t.Errorf("ETag de l'ancien agent : %d", w.Code)
	}

	// Configuration effective, et retrait d'une clé (nil).
	c, err := ConfigEffective(ctx, b.db, id)
	if err != nil || c.ZFS == nil || !*c.ZFS || c.Temperatures == nil || *c.Temperatures || c.Ventilateurs != nil {
		t.Fatalf("configuration effective : %+v %v", c, err)
	}
	if err := ModifierConfig(ctx, b.db, portee, func(m map[string]any) { m["zfs"] = nil }, "admin", t0); err != nil {
		t.Fatal(err)
	}
	if c, _ := ConfigEffective(ctx, b.db, id); c.ZFS != nil || c.Temperatures == nil {
		t.Errorf("après retrait de zfs : %+v", c)
	}
	// Une valeur refusée par l'agent est refusée ici aussi, sans rien changer.
	if err := ModifierConfig(ctx, b.db, portee, func(m map[string]any) { m["systemd_surveiller"] = []string{"rm -rf /"} }, "admin", t0); err == nil {
		t.Error("nom de service invalide accepté")
	}
	// Chaque changement laisse une ligne d'audit.
	var n int
	_ = b.db.QueryRow(`SELECT count(*) FROM audit WHERE action = 'config_agent' AND objet = ?`, portee).Scan(&n)
	if n != 2 {
		t.Errorf("lignes d'audit : %d (2 attendues)", n)
	}
}

// Éléments choisis un par un (SPEC § 21) : seulement pour un agent qui
// annonce « elements » ; un agent 0.2.12 (« surveillance » seule) ne les
// reçoit pas.
func TestElementsSelonCapacite(t *testing.T) {
	b := nouveauBanc(t)
	token, _ := TokenManuel(ctx, b.db, "pc", "admin", t0)
	id, _ := IDMachine(ctx, b.db, "pc")
	err := ModifierConfig(ctx, b.db, "machine:"+itoa(id), func(m map[string]any) {
		m["zfs"] = true
		m["ignorer_disques"] = []string{"sdb"}
		m["certificats_ajout"] = []string{"/etc/nginx/ssl/*.crt"}
	}, "admin", t0)
	if err != nil {
		t.Fatal(err)
	}
	v0212 := b.requete(http.MethodGet, protocole.CheminConfig, token, nil, "", protocole.EnteteCapacites, protocole.CapaciteSurveillance)
	if v0212.Body.String() != `{"zfs":true}` {
		t.Fatalf("agent 0.2.12 : %s", v0212.Body)
	}
	recent := b.requete(http.MethodGet, protocole.CheminConfig, token, nil, "", protocole.EnteteCapacites, protocole.Capacites)
	for _, attendu := range []string{`"zfs":true`, `"ignorer_disques":["sdb"]`, `"certificats_ajout":["/etc/nginx/ssl/*.crt"]`} {
		if !strings.Contains(recent.Body.String(), attendu) {
			t.Errorf("agent récent : %s absent de %s", attendu, recent.Body)
		}
	}
	// Un chemin de certificat hors des dossiers autorisés est refusé dès l'enregistrement.
	err = ModifierConfig(ctx, b.db, "machine:"+itoa(id), func(m map[string]any) {
		m["certificats_ajout"] = []string{"/etc/shadow"}
	}, "admin", t0)
	if err == nil {
		t.Error("/etc/shadow accepté comme chemin de certificat")
	}
}

// La capacité annoncée au battement est gardée sur la fiche de la machine.
func TestCapaciteAuBattement(t *testing.T) {
	b := nouveauBanc(t)
	token, _ := TokenManuel(ctx, b.db, "pc", "admin", t0)
	corps, _ := json.Marshal(protocole.Battement{Version: "1.2.3", Architecture: "amd64", Demarrage: t0})
	lire := func() bool {
		ms, _ := ListerMachines(ctx, b.db)
		return len(ms) == 1 && ms[0].Surveillance()
	}
	b.requete(http.MethodPost, protocole.CheminBattement, token, corps, "")
	_ = b.f.EcrireBattements(ctx)
	if lire() {
		t.Error("sans en-tête, la machine ne doit pas annoncer la surveillance")
	}
	b.requete(http.MethodPost, protocole.CheminBattement, token, corps, "", protocole.EnteteCapacites, "surveillance")
	_ = b.f.EcrireBattements(ctx)
	if !lire() {
		t.Error("capacité « surveillance » non gardée")
	}
	// En-tête fantaisiste : ignoré.
	b.requete(http.MethodPost, protocole.CheminBattement, token, corps, "", protocole.EnteteCapacites, "<script>")
	_ = b.f.EcrireBattements(ctx)
	if lire() {
		t.Error("en-tête invalide accepté")
	}
}

// Token lié à la machine (SPEC § 19.8) : refusé s'il est présenté par une
// autre machine, ou sans empreinte ; rien ne change tant qu'il n'est pas lié.
func TestTokenLieALaMachine(t *testing.T) {
	b := nouveauBanc(t)
	token, _ := TokenManuel(ctx, b.db, "pc", "admin", t0)
	const moi, autre = "0123456789abcdef0123456789abcdef", "ffffffffffffffffffffffffffffffff"
	battre := func(empreinte string) int {
		corps, _ := json.Marshal(protocole.Battement{Version: "1.2.3", Architecture: "amd64", Demarrage: t0})
		if empreinte == "" {
			return b.requete(http.MethodPost, protocole.CheminBattement, token, corps, "").Code
		}
		return b.requete(http.MethodPost, protocole.CheminBattement, token, corps, "", protocole.EnteteEmpreinte, empreinte).Code
	}
	// Pas d'empreinte connue : impossible de lier.
	if err := LierEmpreinte(ctx, b.db, "pc", true, "admin", t0); err == nil {
		t.Fatal("lien accepté sans empreinte reçue")
	}
	// Pas lié : tout passe, et l'empreinte reçue est gardée.
	if battre("") != http.StatusNoContent || battre(moi) != http.StatusNoContent {
		t.Fatal("un token non lié doit passer avec ou sans empreinte")
	}
	if err := b.f.opt.Ingestion.Auth().EcrireVus(ctx); err != nil {
		t.Fatal(err)
	}
	if ms, _ := ListerMachines(ctx, b.db); ms[0].EmpreinteVue != moi || ms[0].EmpreinteLiee != "" {
		t.Fatalf("empreinte vue : %+v", ms[0])
	}
	if err := LierEmpreinte(ctx, b.db, "pc", true, "admin", t0); err != nil {
		t.Fatal(err)
	}
	b.f.opt.Ingestion.Auth().Oublier()
	if c := battre(moi); c != http.StatusNoContent {
		t.Errorf("la bonne machine est refusée : %d", c)
	}
	for nom, e := range map[string]string{"autre machine": autre, "sans empreinte": "", "empreinte mal formée": "<script>"} {
		if c := battre(e); c != http.StatusForbidden {
			t.Errorf("%s : %d (403 attendu)", nom, c)
		}
	}
	// Métriques et configuration : même refus.
	if w := b.requete(http.MethodGet, protocole.CheminConfig, token, nil, "", protocole.EnteteEmpreinte, autre); w.Code != http.StatusForbidden {
		t.Errorf("configuration servie à une autre machine : %d", w.Code)
	}
	// Délier : tout repasse.
	if err := LierEmpreinte(ctx, b.db, "pc", false, "admin", t0); err != nil {
		t.Fatal(err)
	}
	b.f.opt.Ingestion.Auth().Oublier()
	if c := battre(autre); c != http.StatusNoContent {
		t.Errorf("après avoir délié : %d", c)
	}
}
