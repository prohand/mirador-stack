// SPDX-License-Identifier: GPL-3.0-or-later

package release

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"mirador-stack/internal/agent/majagent"
	"mirador-stack/internal/protocole"
)

// scriptMajServeur : script d'installation de mirador-maj-serveur (VPS).
const scriptMajServeur = "../../deploy/serveur/installer-maj-serveur.sh"

// fonctionVerifier extrait du script la fonction verifier_signature (entre
// ses deux marqueurs), pour la lancer seule, sans root ni réseau.
func fonctionVerifier(t *testing.T) string {
	t.Helper()
	brut, err := os.ReadFile(scriptMajServeur)
	if err != nil {
		t.Fatal(err)
	}
	s := string(brut)
	debut := strings.Index(s, "# --- début verifier_signature")
	fin := strings.Index(s, "# --- fin verifier_signature")
	if debut < 0 || fin < debut {
		t.Fatal("marqueurs de verifier_signature introuvables dans le script")
	}
	return s[debut:fin]
}

// opensslRawin : OpenSSL 3 ou plus récent (option -rawin de pkeyutl).
func opensslRawin(t *testing.T) {
	t.Helper()
	sortie, err := exec.Command("openssl", "version").Output()
	if err != nil || !strings.HasPrefix(string(sortie), "OpenSSL 3") && !strings.HasPrefix(string(sortie), "OpenSSL 4") {
		t.Skip("OpenSSL 3 requis :", strings.TrimSpace(string(sortie)), err)
	}
}

// TestScriptMajServeurVerifieLaSignature : la vérification en shell
// (openssl) du script d'installation accepte un manifeste signé par
// mirador-release, et refuse tout le reste (revue du 28/11, M3).
func TestScriptMajServeurVerifieLaSignature(t *testing.T) {
	opensslRawin(t)
	fonction := fonctionVerifier(t)
	f, _ := NouvelleCle(phrase)
	brutCle, _ := json.Marshal(f)
	priv, err := LireCle(brutCle, phrase)
	if err != nil {
		t.Fatal(err)
	}
	autre, _ := NouvelleCle(phrase)
	binaire := executable(t)
	dossier := t.TempDir()
	cheminBin := filepath.Join(dossier, "mirador-maj-serveur")
	if err := os.WriteFile(cheminBin, binaire, 0o600); err != nil {
		t.Fatal(err)
	}
	// Manifeste écrit comme le fait mirador-release (JSON indenté).
	ecrire := func(nom string, m *protocole.Manifeste) string {
		b, _ := json.MarshalIndent(m, "", "  ")
		c := filepath.Join(dossier, nom)
		if err := os.WriteFile(c, append(b, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
		return c
	}
	m, err := Signer(priv, binaire, ProduitMajServeur, "1.2.3", runtime.GOARCH, false)
	if err != nil {
		t.Fatal(err)
	}
	bon := ecrire("bon.json", m)
	agent, _ := Signer(priv, binaire, protocole.ProduitAgent, "1.2.3", runtime.GOARCH, false)
	manAgent := ecrire("agent.json", agent)
	collecteur, _ := Signer(priv, binaire, protocole.ProduitCollecteur, "1.2.3", runtime.GOARCH, false)
	manCollecteur := ecrire("collecteur.json", collecteur)
	modifie := filepath.Join(dossier, "modifie")
	if err := os.WriteFile(modifie, append(binaire, 0), 0o600); err != nil {
		t.Fatal(err)
	}

	lancer := func(bin, man, cle, version, arch string) error {
		script := fonction + "\nverifier_signature \"$1\" \"$2\" \"$3\" mirador-maj-serveur \"$4\" \"$5\"\n"
		cmd := exec.Command("sh", "-c", script, "essai", bin, man, cle, version, arch) // #nosec G204 -- script du dépôt, essai local
		sortie, err := cmd.CombinedOutput()
		t.Logf("%s", sortie)
		return err
	}
	if err := lancer(cheminBin, bon, f.Publique, "1.2.3", runtime.GOARCH); err != nil {
		t.Fatalf("manifeste signé refusé par le script : %v", err)
	}
	refus := map[string][]string{
		"autre clé":             {cheminBin, bon, autre.Publique, "1.2.3", runtime.GOARCH},
		"binaire modifié":       {modifie, bon, f.Publique, "1.2.3", runtime.GOARCH},
		"autre version":         {cheminBin, bon, f.Publique, "1.2.4", runtime.GOARCH},
		"autre architecture":    {cheminBin, bon, f.Publique, "1.2.3", "riscv64"},
		"manifeste de l'agent":  {cheminBin, manAgent, f.Publique, "1.2.3", runtime.GOARCH},
		"manifeste collecteur":  {cheminBin, manCollecteur, f.Publique, "1.2.3", runtime.GOARCH},
		"manifeste inexistant":  {cheminBin, filepath.Join(dossier, "absent"), f.Publique, "1.2.3", runtime.GOARCH},
		"manifeste sans format": {cheminBin, cheminBin, f.Publique, "1.2.3", runtime.GOARCH},
	}
	for nom, a := range refus {
		if err := lancer(a[0], a[1], a[2], a[3], a[4]); err == nil {
			t.Errorf("%s : accepté par le script", nom)
		}
	}
}

// TestProduitMajServeur : manifeste du produit mirador-maj-serveur, jamais
// accepté par l'agent ni le collecteur ; pas de retour arrière.
func TestProduitMajServeur(t *testing.T) {
	f, _ := NouvelleCle(phrase)
	brutCle, _ := json.Marshal(f)
	priv, err := LireCle(brutCle, phrase)
	if err != nil {
		t.Fatal(err)
	}
	binaire := executable(t)
	m, err := Signer(priv, binaire, ProduitMajServeur, "1.2.3", runtime.GOARCH, false)
	if err != nil || m.Produit != ProduitMajServeur {
		t.Fatalf("mirador-maj-serveur : %+v %v", m, err)
	}
	if err := Verifier(m, binaire, f.Publique); err != nil {
		t.Fatal(err)
	}
	if err := majagent.VerifierManifeste(m, "1.0.0", f.Publique); err == nil {
		t.Error("manifeste de mirador-maj-serveur accepté par l'agent")
	}
	majagent.ProduitCourant = protocole.ProduitCollecteur
	defer func() { majagent.ProduitCourant = protocole.ProduitAgent }()
	if err := majagent.VerifierManifeste(m, "1.0.0", f.Publique); err == nil {
		t.Error("manifeste de mirador-maj-serveur accepté par le collecteur")
	}
	if _, err := Signer(priv, binaire, ProduitMajServeur, "1.2.3", runtime.GOARCH, true); err == nil {
		t.Error("retour arrière signé pour mirador-maj-serveur")
	}
}
