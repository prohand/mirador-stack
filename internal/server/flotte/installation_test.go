// SPDX-License-Identifier: GPL-3.0-or-later

package flotte

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	installagent "mirador-stack/deploy/agent"
	installcollecteur "mirador-stack/deploy/collecteur"
	"mirador-stack/internal/agent/majagent"
	"mirador-stack/internal/protocole"
	"mirador-stack/internal/release"
)

// lireManifeste : manifeste de l'en-tête X-Mirador-Manifeste (nil s'il
// n'y en a pas).
func lireManifeste(t *testing.T, h http.Header) *protocole.Manifeste {
	t.Helper()
	v := h.Get(EnteteManifeste)
	if v == "" {
		return nil
	}
	brut, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		t.Fatalf("manifeste : base64 illisible : %v", err)
	}
	if strings.ContainsAny(string(brut), " \n") {
		t.Fatalf("manifeste : JSON sur plusieurs lignes ou avec espaces (illisible par le script) : %s", brut)
	}
	var m protocole.Manifeste
	if err := json.Unmarshal(brut, &m); err != nil {
		t.Fatalf("manifeste : JSON illisible : %v", err)
	}
	return &m
}

// Manifeste signé servi avec le binaire (revue de sécurité du 28/11, M2) :
// même réponse, même jeton ; « X-Mirador-Signe: non » pour un binaire
// embarqué qui ne correspond à aucune version publiée ; manifeste de la
// version publiée de même empreinte sinon.
func TestInstallationManifesteSigne(t *testing.T) {
	b := nouveauBanc(t)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	ancienne := majagent.ClesPubliques
	majagent.ClesPubliques = base64.StdEncoding.EncodeToString(pub)
	defer func() { majagent.ClesPubliques = ancienne }()

	embarque := filepath.Join(b.dir, "agent")
	b.f.opt.DossierAgent = embarque
	_ = os.MkdirAll(embarque, 0o750)
	contenu := []byte("binaire embarqué amd64, pas encore publié")
	_ = os.WriteFile(filepath.Join(embarque, "mirador-agent-linux-amd64"), contenu, 0o600)
	jeton, err := CreerJeton(ctx, b.db, time.Hour, "admin", t0)
	if err != nil {
		t.Fatal(err)
	}
	url := protocole.CheminInstallationBinaire + "?arch=amd64"

	// Sans jeton : refus, et pas de manifeste.
	if w := b.requete(http.MethodGet, url, "", nil, "198.51.100.40"); w.Code != http.StatusUnauthorized || w.Header().Get(EnteteManifeste) != "" {
		t.Fatalf("sans jeton : %d %q", w.Code, w.Header())
	}
	// Binaire embarqué non publié : servi, mais signalé non signé.
	w := b.requete(http.MethodGet, url, jeton, nil, "")
	if w.Code != http.StatusOK || w.Header().Get(EnteteSigne) != "non" || lireManifeste(t, w.Header()) != nil {
		t.Fatalf("embarqué non signé : %d %q", w.Code, w.Header())
	}
	// Le même binaire publié (signé) : son manifeste part avec lui, et la
	// version annoncée est celle du manifeste.
	m := signer(t, priv, "1.4.0", "amd64", contenu)
	if err := Publier(ctx, b.db, b.f.opt.DossierVersions, m, contenu, "admin", t0); err != nil {
		t.Fatal(err)
	}
	w = b.requete(http.MethodGet, url, jeton, nil, "")
	recu := lireManifeste(t, w.Header())
	if w.Code != http.StatusOK || w.Header().Get(EnteteSigne) != "oui" || recu == nil || w.Header().Get(EnteteVersion) != "1.4.0" {
		t.Fatalf("embarqué publié : %d %q", w.Code, w.Header())
	}
	if err := release.Verifier(recu, w.Body.Bytes(), majagent.ClesPubliques); err != nil || recu.ProduitEffectif() != protocole.ProduitAgent {
		t.Fatalf("manifeste servi invérifiable : %v %+v", err, recu)
	}
	// Version globale publiée (autre binaire) : elle passe avant, avec SON
	// manifeste, qui vérifie le binaire envoyé.
	publie := []byte("binaire publié 1.5.0")
	m2 := signer(t, priv, "1.5.0", "amd64", publie)
	if err := Publier(ctx, b.db, b.f.opt.DossierVersions, m2, publie, "admin", t0); err != nil {
		t.Fatal(err)
	}
	if err := DefinirCible(ctx, b.db, 0, "1.5.0", "admin", t0); err != nil {
		t.Fatal(err)
	}
	w = b.requete(http.MethodGet, url, jeton, nil, "")
	recu = lireManifeste(t, w.Header())
	if w.Code != http.StatusOK || w.Body.String() != string(publie) || recu == nil || recu.Version != "1.5.0" ||
		release.Verifier(recu, w.Body.Bytes(), majagent.ClesPubliques) != nil {
		t.Fatalf("version globale : %d %q", w.Code, w.Header())
	}
	// Collecteur embarqué sans version publiée : non signé (le manifeste de
	// l'agent n'est jamais servi pour lui).
	_ = os.WriteFile(filepath.Join(embarque, "mirador-collector-linux-amd64"), contenu, 0o600)
	w = b.requete(http.MethodGet, url+"&produit=mirador-collector", jeton, nil, "")
	if w.Code != http.StatusOK || w.Header().Get(EnteteSigne) != "non" || lireManifeste(t, w.Header()) != nil {
		t.Fatalf("collecteur non publié : %d %q", w.Code, w.Header())
	}
}

// Installation par HTTPS : script et binaire servis avec un jeton valable
// seulement ; binaire publié (version globale) en priorité, sinon embarqué.
func TestInstallationParTelechargement(t *testing.T) {
	b := nouveauBanc(t)
	embarque := filepath.Join(b.dir, "agent")
	b.f.opt.DossierAgent = embarque
	_ = os.MkdirAll(embarque, 0o750)
	contenu := []byte("binaire embarqué amd64")
	_ = os.WriteFile(filepath.Join(embarque, "mirador-agent-linux-amd64"), contenu, 0o600)

	jeton, err := CreerJeton(ctx, b.db, time.Hour, "admin", t0)
	if err != nil {
		t.Fatal(err)
	}
	// Binaire : sans jeton ou faux jeton, refus.
	for _, j := range []string{"", "mirador_enrol_faux_faux_faux_faux_faux"} {
		if w := b.requete(http.MethodGet, protocole.CheminInstallationBinaire+"?arch=amd64", j, nil, "198.51.100.30"); w.Code != http.StatusUnauthorized {
			t.Fatalf("jeton %q : %d", j, w.Code)
		}
	}
	// Script : public (sans jeton).
	w := b.requete(http.MethodGet, protocole.CheminInstallation, "", nil, "")
	h := sha256.Sum256(w.Body.Bytes())
	if w.Code != http.StatusOK || !strings.HasPrefix(w.Body.String(), "#!") || w.Body.Len() != len(installagent.Script) ||
		w.Header().Get(EnteteSHA256) != hex.EncodeToString(h[:]) {
		t.Fatalf("script : %d %d", w.Code, w.Body.Len())
	}
	// Binaire embarqué, architecture vérifiée.
	if w := b.requete(http.MethodGet, protocole.CheminInstallationBinaire+"?arch=../../etc", jeton, nil, ""); w.Code != http.StatusBadRequest {
		t.Fatalf("architecture piégée : %d", w.Code)
	}
	if w := b.requete(http.MethodGet, protocole.CheminInstallationBinaire+"?arch=arm64", jeton, nil, ""); w.Code != http.StatusNotFound {
		t.Fatalf("arm64 absent : %d", w.Code)
	}
	w = b.requete(http.MethodGet, protocole.CheminInstallationBinaire+"?arch=amd64", jeton, nil, "")
	he := sha256.Sum256(contenu)
	if w.Code != http.StatusOK || w.Body.String() != string(contenu) || w.Header().Get(EnteteSHA256) != hex.EncodeToString(he[:]) ||
		w.Header().Get(EnteteVersion) != "embarquée" {
		t.Fatalf("binaire embarqué : %d %q", w.Code, w.Header())
	}
	// Collecteur (SPEC § 23.13) : son script, et son binaire avec
	// « produit=mirador-collector » (produit piégé refusé).
	contenuC := []byte("collecteur embarqué amd64")
	_ = os.WriteFile(filepath.Join(embarque, "mirador-collector-linux-amd64"), contenuC, 0o600)
	if w := b.requete(http.MethodGet, protocole.CheminInstallationCollecteur, "", nil, ""); w.Code != http.StatusOK ||
		!strings.Contains(w.Body.String(), "mirador-collector-install.sh") || w.Body.Len() != len(installcollecteur.Script) {
		t.Fatalf("script du collecteur : %d", w.Code)
	}
	if w := b.requete(http.MethodGet, protocole.CheminInstallationBinaire+"?arch=amd64&produit=../agent", jeton, nil, ""); w.Code != http.StatusBadRequest {
		t.Fatalf("produit piégé : %d", w.Code)
	}
	if w := b.requete(http.MethodGet, protocole.CheminInstallationBinaire+"?arch=amd64&produit=mirador-collector", jeton, nil, ""); w.Code != http.StatusOK ||
		w.Body.String() != string(contenuC) {
		t.Fatalf("binaire du collecteur : %d %q", w.Code, w.Body.String())
	}
	// Version globale publiée : elle passe avant.
	publie := []byte("binaire publié 1.2.0")
	hp := sha256.Sum256(publie)
	_ = os.MkdirAll(b.f.opt.DossierVersions, 0o750)
	_ = os.WriteFile(filepath.Join(b.f.opt.DossierVersions, nomBinaire(protocole.ProduitAgent, "1.2.0", "amd64")), publie, 0o600)
	_, _ = b.db.Exec(`INSERT INTO versions_binaires(produit, version, architecture, sha256, taille, signature, publiee_le) VALUES ('mirador-agent', '1.2.0', 'amd64', ?, ?, 'x', 1)`,
		hex.EncodeToString(hp[:]), len(publie))
	_, _ = b.db.Exec(`INSERT INTO reglages(cle, valeur, modifie_le, modifie_par) VALUES (?, '"1.2.0"', 1, 'test')`, cleCible)
	w = b.requete(http.MethodGet, protocole.CheminInstallationBinaire+"?arch=amd64", jeton, nil, "")
	if w.Code != http.StatusOK || w.Body.String() != string(publie) || w.Header().Get(EnteteVersion) != "1.2.0" {
		t.Fatalf("binaire publié : %d %q", w.Code, w.Body.String())
	}
	// Jeton consommé (token remis) : plus de binaire.
	_, _ = b.db.Exec(`UPDATE jetons_enrolement SET token_remis_le = 1`)
	if w := b.requete(http.MethodGet, protocole.CheminInstallationBinaire+"?arch=amd64", jeton, nil, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("jeton déjà utilisé accepté : %d", w.Code)
	}
	// Machine déjà enrôlée qui relance le script : son token suffit ; un
	// token inconnu est refusé.
	token, err := TokenManuel(ctx, b.db, "deja-enrolee", "admin", t0)
	if err != nil {
		t.Fatal(err)
	}
	if w := b.requete(http.MethodGet, protocole.CheminInstallationBinaire+"?arch=amd64", token, nil, ""); w.Code != http.StatusOK {
		t.Fatalf("token de machine refusé : %d %s", w.Code, w.Body)
	}
	if w := b.requete(http.MethodGet, protocole.CheminInstallationBinaire+"?arch=amd64", "mirador_faux_token_faux_token_faux", nil, "198.51.100.31"); w.Code != http.StatusUnauthorized {
		t.Fatalf("token inconnu accepté : %d", w.Code)
	}
}
