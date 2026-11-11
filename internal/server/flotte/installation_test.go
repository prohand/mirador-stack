// SPDX-License-Identifier: GPL-3.0-or-later

package flotte

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	installagent "mirador-stack/deploy/agent"
	"mirador-stack/internal/protocole"
)

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
	// Version globale publiée : elle passe avant.
	publie := []byte("binaire publié 1.2.0")
	hp := sha256.Sum256(publie)
	_ = os.MkdirAll(b.f.opt.DossierVersions, 0o750)
	_ = os.WriteFile(filepath.Join(b.f.opt.DossierVersions, nomBinaire("1.2.0", "amd64")), publie, 0o600)
	_, _ = b.db.Exec(`INSERT INTO versions_agent(version, architecture, sha256, taille, signature, publiee_le) VALUES ('1.2.0', 'amd64', ?, ?, 'x', 1)`,
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
