// SPDX-License-Identifier: GPL-3.0-or-later

package installagent

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	installcollecteur "mirador-stack/deploy/collecteur"
	"mirador-stack/internal/protocole"
	"mirador-stack/internal/release"
)

// TestScriptSyntaxe : le script embarqué est du bash valide (bash -n).
func TestScriptSyntaxe(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash absent")
	}
	cmd := exec.Command("bash", "-n", "mirador-agent-install.sh")
	if sortie, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("bash -n : %v\n%s", err, sortie)
	}
}

// fonctionScript : texte d'une fonction du script (de « nom() { » à « } »).
func fonctionScript(t *testing.T, nom string) string {
	t.Helper()
	lignes := strings.Split(string(Script), "\n")
	for i, l := range lignes {
		if l != nom+"() {" {
			continue
		}
		for j := i + 1; j < len(lignes); j++ {
			if lignes[j] == "}" {
				return strings.Join(lignes[i:j+1], "\n")
			}
		}
	}
	t.Fatalf("fonction %s introuvable dans le script", nom)
	return ""
}

// TestPlageRelais : plage du relais Docker (SPEC § 126) : privée, /24 à
// /29, alignée ; passerelle .1 et relais .2 du bloc.
func TestPlageRelais(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash absent")
	}
	f := fonctionScript(t, "plage_relais")
	cas := []struct {
		plage, attendu string // attendu : "passerelle relais", ou "refus"
	}{
		{"172.31.254.0/29", "172.31.254.1 172.31.254.2"},
		{"10.99.0.8/29", "10.99.0.9 10.99.0.10"},
		{"192.168.200.0/24", "192.168.200.1 192.168.200.2"},
		{"172.254.254.0/29", "refus"}, // publique (hors 172.16/12)
		{"172.15.0.0/29", "refus"},
		{"8.8.8.0/29", "refus"},
		{"10.99.0.4/29", "refus"}, // pas au début d'un bloc /29
		{"300.1.1.0/29", "refus"},
		{"172.31.254.0/30", "refus"},
		{"172.31.254.0/29x", "refus"},
		{"", "refus"},
	}
	for _, c := range cas {
		code := `die() { echo refus; exit 0; }` + "\n" + f + "\n" +
			`plage_relais "$1"; echo "$PROXY_PASSERELLE $PROXY_IP"`
		sortie, err := exec.Command("bash", "-c", code, "test", c.plage).CombinedOutput() // #nosec G204 -- code du dépôt, plage de test
		if err != nil {
			t.Fatalf("%q : %v\n%s", c.plage, err, sortie)
		}
		if g := strings.TrimSpace(string(sortie)); g != c.attendu {
			t.Errorf("plage_relais %q = %q, attendu %q", c.plage, g, c.attendu)
		}
	}
}

// fonctionsSignature : fonctions de vérification du manifeste, communes aux
// scripts de l'agent et du collecteur.
var fonctionsSignature = []string{"valeur_entete", "champ_json", "openssl_recent", "verifier_manifeste", "version_plus_ancienne"}

// TestFonctionsSignatureIdentiques : les deux scripts vérifient la signature
// de la même façon (fonctions recopiées à l'identique).
func TestFonctionsSignatureIdentiques(t *testing.T) {
	for _, nom := range fonctionsSignature {
		a := fonctionScript(t, nom)
		c := fonctionDans(t, string(installcollecteur.Script), nom)
		if a != c {
			t.Errorf("fonction %s différente entre les scripts de l'agent et du collecteur", nom)
		}
	}
}

// fonctionDans : comme fonctionScript, dans un autre texte.
func fonctionDans(t *testing.T, texte, nom string) string {
	t.Helper()
	lignes := strings.Split(texte, "\n")
	for i, l := range lignes {
		if l != nom+"() {" {
			continue
		}
		for j := i + 1; j < len(lignes); j++ {
			if lignes[j] == "}" {
				return strings.Join(lignes[i:j+1], "\n")
			}
		}
	}
	t.Fatalf("fonction %s introuvable", nom)
	return ""
}

// opensslRecent : openssl 3 (ou plus) est-il là pour les essais ?
func opensslRecent() bool {
	sortie, err := exec.Command("openssl", "version").Output()
	if err != nil {
		return false
	}
	champs := strings.Fields(string(sortie))
	return len(champs) >= 2 && champs[0] == "OpenSSL" && !strings.HasPrefix(champs[1], "0.") && !strings.HasPrefix(champs[1], "1.")
}

// TestVerifierManifeste : la vérification EN SHELL (openssl) du manifeste
// signé par le code Go du dépôt (revue de sécurité du 28/11, M2) : signature,
// produit, architecture, empreinte, taille ; refus dans tous les autres cas.
func TestVerifierManifeste(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash absent")
	}
	if !opensslRecent() {
		t.Skip("openssl 3 absent")
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pubAutre, privAutre, _ := ed25519.GenerateKey(rand.Reader)
	cle := base64.StdEncoding.EncodeToString(pub)
	cleAutre := base64.StdEncoding.EncodeToString(pubAutre)

	dir := t.TempDir()
	binaire := make([]byte, 4096)
	_, _ = rand.Read(binaire)
	cheminBinaire := filepath.Join(dir, "binaire")
	if err := os.WriteFile(cheminBinaire, binaire, 0o600); err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(binaire)
	signer := func(m protocole.Manifeste, p ed25519.PrivateKey) protocole.Manifeste {
		m.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(p, m.MessageSigne()))
		return m
	}
	agent := signer(protocole.Manifeste{Version: "1.2.3", Architecture: "amd64", SHA256: hex.EncodeToString(h[:]), Taille: int64(len(binaire))}, priv)
	collecteur := signer(protocole.Manifeste{Produit: protocole.ProduitCollecteur, Version: "0.7.1", Architecture: "arm64",
		SHA256: hex.EncodeToString(h[:]), Taille: int64(len(binaire)), RetourAutorise: true, RetourJusqua: 1999999999}, priv)
	versionChangee := agent
	versionChangee.Version = "9.9.9" // signature d'origine gardée
	tailleChangee := signer(protocole.Manifeste{Version: "1.2.3", Architecture: "amd64", SHA256: hex.EncodeToString(h[:]), Taille: 12}, priv)
	autreBinaire := signer(protocole.Manifeste{Version: "1.2.3", Architecture: "amd64", SHA256: strings.Repeat("ab", 32), Taille: int64(len(binaire))}, priv)

	// entetes : fichier d'en-têtes façon curl (-D) ou wget (-S, lignes décalées).
	entetes := func(m *protocole.Manifeste, signe string, wget bool) string {
		var b strings.Builder
		decalage := ""
		if wget {
			decalage = "  "
		}
		b.WriteString(decalage + "HTTP/1.1 200 OK\r\n")
		b.WriteString(decalage + "X-Mirador-SHA256: " + hex.EncodeToString(h[:]) + "\r\n")
		if m != nil {
			brut, _ := json.Marshal(m)
			b.WriteString(decalage + "X-Mirador-Manifeste: " + base64.StdEncoding.EncodeToString(brut) + "\r\n")
		}
		if signe != "" {
			b.WriteString(decalage + "X-Mirador-Signe: " + signe + "\r\n")
		}
		f := filepath.Join(t.TempDir(), "entetes")
		_ = os.WriteFile(f, []byte(b.String()), 0o600)
		return f
	}
	code := "die() { echo \"refus : $*\"; exit 0; }\n"
	for _, nom := range fonctionsSignature {
		code += fonctionScript(t, nom) + "\n"
	}
	code += `verifier_manifeste "$1" "$2" "$3" "$4" "$5"; echo "ok $VERSION_SIGNEE $RETOUR_SIGNE $RETOUR_JUSQUA"`

	cas := []struct {
		nom, entetes, produit, arch, cles, attendu string
	}{
		{"agent, curl", entetes(&agent, "oui", false), "mirador-agent", "amd64", cle, "ok 1.2.3 non 0"},
		{"agent, wget", entetes(&agent, "oui", true), "mirador-agent", "amd64", cle, "ok 1.2.3 non 0"},
		{"deux clés, la 2e est la bonne", entetes(&agent, "oui", false), "mirador-agent", "amd64", cleAutre + "\n" + cle, "ok 1.2.3 non 0"},
		{"collecteur, retour signé", entetes(&collecteur, "oui", false), "mirador-collector", "arm64", cle, "ok 0.7.1 oui 1999999999"},
		{"mauvaise clé", entetes(&agent, "oui", false), "mirador-agent", "amd64", cleAutre, "refus : SIGNATURE INVALIDE"},
		{"signé par une autre clé", entetes(ptr(signer(agent, privAutre)), "oui", false), "mirador-agent", "amd64", cle, "refus : SIGNATURE INVALIDE"},
		{"version changée", entetes(&versionChangee, "oui", false), "mirador-agent", "amd64", cle, "refus : SIGNATURE INVALIDE"},
		{"autre produit", entetes(&agent, "oui", false), "mirador-collector", "amd64", cle, "refus : manifeste signé d'un autre produit"},
		{"autre architecture", entetes(&agent, "oui", false), "mirador-agent", "arm64", cle, "refus : manifeste signé d'une autre architecture"},
		{"autre binaire", entetes(&autreBinaire, "oui", false), "mirador-agent", "amd64", cle, "refus : le binaire reçu ne correspond pas au manifeste signé (empreinte)"},
		{"autre taille", entetes(&tailleChangee, "oui", false), "mirador-agent", "amd64", cle, "refus : le binaire reçu ne correspond pas au manifeste signé (taille)"},
		{"binaire embarqué non signé", entetes(nil, "non", false), "mirador-agent", "amd64", cle, "refus : le serveur n'a qu'un binaire « embarqué » NON SIGNÉ"},
		{"serveur ancien", entetes(nil, "", false), "mirador-agent", "amd64", cle, "refus : le serveur n'a pas envoyé de manifeste signé"},
	}
	for _, c := range cas {
		sortie, err := exec.Command("bash", "-c", code, "test", c.entetes, cheminBinaire, c.produit, c.arch, c.cles).CombinedOutput() // #nosec G204 -- code du dépôt, fichiers de test
		if err != nil {
			t.Fatalf("%s : %v\n%s", c.nom, err, sortie)
		}
		if g := strings.TrimSpace(string(sortie)); !strings.HasPrefix(g, c.attendu) {
			t.Errorf("%s : %q, attendu %q…", c.nom, g, c.attendu)
		}
	}
}

func ptr(m protocole.Manifeste) *protocole.Manifeste { return &m }

// TestVerifierManifesteRelease : un manifeste fabriqué par mirador-release
// (release.Signer, sur un vrai exécutable Linux) passe la vérification shell.
func TestVerifierManifesteRelease(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash absent")
	}
	if !opensslRecent() || (runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64") || runtime.GOOS != "linux" {
		t.Skip("openssl 3, Linux amd64 ou arm64 nécessaires")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	binaire, err := os.ReadFile(exe) // #nosec G304 -- l'exécutable du test lui-même
	if err != nil {
		t.Fatal(err)
	}
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	for _, produit := range []string{protocole.ProduitAgent, protocole.ProduitCollecteur} {
		m, err := release.Signer(priv, binaire, produit, "2.0.0", runtime.GOARCH, false)
		if err != nil {
			t.Fatal(err)
		}
		brut, _ := json.Marshal(m)
		dir := t.TempDir()
		entetes := filepath.Join(dir, "entetes")
		_ = os.WriteFile(entetes, []byte("HTTP/1.1 200 OK\r\nX-Mirador-Manifeste: "+base64.StdEncoding.EncodeToString(brut)+"\r\nX-Mirador-Signe: oui\r\n"), 0o600)
		code := "die() { echo \"refus : $*\"; exit 0; }\n"
		for _, nom := range fonctionsSignature {
			code += fonctionScript(t, nom) + "\n"
		}
		code += `verifier_manifeste "$1" "$2" "$3" "$4" "$5"; echo "ok $VERSION_SIGNEE"`
		sortie, err := exec.Command("bash", "-c", code, "test", entetes, exe, produit, runtime.GOARCH, base64.StdEncoding.EncodeToString(pub)).CombinedOutput() // #nosec G204 -- code du dépôt, fichiers de test
		if err != nil || strings.TrimSpace(string(sortie)) != "ok 2.0.0" {
			t.Fatalf("%s : %v %q", produit, err, sortie)
		}
	}
}

// TestVersionPlusAncienne : refus du retour vers une version plus ancienne.
func TestVersionPlusAncienne(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash absent")
	}
	f := fonctionScript(t, "version_plus_ancienne")
	cas := map[[2]string]string{
		{"0.6.9", "0.6.34"}: "oui", {"0.6.34", "0.6.34"}: "non", {"0.7.0", "0.6.34"}: "non",
		{"1.0.0", ""}: "non", {"0.6.1", "dev"}: "non",
	}
	for c, attendu := range cas {
		sortie, err := exec.Command("bash", "-c", f+"\nif version_plus_ancienne \"$1\" \"$2\"; then echo oui; else echo non; fi", "test", c[0], c[1]).CombinedOutput() // #nosec G204 -- code du dépôt
		if err != nil {
			t.Fatalf("%v : %v", c, err)
		}
		if g := strings.TrimSpace(string(sortie)); g != attendu {
			t.Errorf("version_plus_ancienne %s %s = %s, attendu %s", c[0], c[1], g, attendu)
		}
	}
}

// TestScriptEmbarque : le script embarqué est bien celui du dépôt.
func TestScriptEmbarque(t *testing.T) {
	disque, err := os.ReadFile("mirador-agent-install.sh")
	if err != nil {
		t.Fatal(err)
	}
	if string(disque) != string(Script) {
		t.Error("script embarqué différent du fichier")
	}
}
