// SPDX-License-Identifier: GPL-3.0-or-later

package majagent

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"mirador-stack/internal/protocole"
)

// cleTest crée une paire de clés le temps du test (aucune clé n'est versionnée).
func cleTest(t *testing.T) (string, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(pub), priv
}

func manifeste(priv ed25519.PrivateKey, version string, binaire []byte, retour bool) *protocole.Manifeste {
	somme := sha256.Sum256(binaire)
	m := &protocole.Manifeste{Version: version, Architecture: runtime.GOARCH,
		SHA256: hex.EncodeToString(somme[:]), Taille: int64(len(binaire)), RetourAutorise: retour}
	m.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(priv, m.MessageSigne()))
	return m
}

func TestVerifierManifeste(t *testing.T) {
	pub, priv := cleTest(t)
	autrePub, autrePriv := cleTest(t)
	bin := []byte("binaire de test")

	if err := VerifierManifeste(manifeste(priv, "1.2.0", bin, false), "1.1.0", pub); err != nil {
		t.Errorf("manifeste valide refusé : %v", err)
	}
	// Rotation : deux clés intégrées, signé par la seconde.
	if err := VerifierManifeste(manifeste(autrePriv, "1.2.0", bin, false), "1.1.0", pub+","+autrePub); err != nil {
		t.Errorf("clé de rotation refusée : %v", err)
	}
	cas := map[string]struct {
		m      *protocole.Manifeste
		cles   string
		erreur error
	}{
		"aucune clé intégrée":     {manifeste(priv, "1.2.0", bin, false), "", ErrSansCle},
		"signé par une autre clé": {manifeste(autrePriv, "1.2.0", bin, false), pub, ErrSignature},
		"version plus ancienne":   {manifeste(priv, "1.0.9", bin, false), pub, ErrRetourArriere},
		"retour arrière signé":    {manifeste(priv, "1.0.9", bin, true), pub, nil},
	}
	for nom, c := range cas {
		t.Run(nom, func(t *testing.T) {
			err := VerifierManifeste(c.m, "1.1.0", c.cles)
			if !errors.Is(err, c.erreur) && !(err == nil && c.erreur == nil) {
				t.Errorf("erreur %v, attendu %v", err, c.erreur)
			}
		})
	}
	// Champ modifié après signature (ex. serveur compromis qui change la version
	// ou retire l'interdiction de retour arrière) : refusé.
	m := manifeste(priv, "1.0.9", bin, false)
	m.RetourAutorise = true
	if err := VerifierManifeste(m, "1.1.0", pub); !errors.Is(err, ErrSignature) {
		t.Errorf("manifeste modifié accepté : %v", err)
	}
	m = manifeste(priv, "1.2.0", bin, false)
	m.Architecture = "riscv64"
	if err := VerifierManifeste(m, "1.1.0", pub); !errors.Is(err, ErrSignature) {
		t.Errorf("architecture modifiée acceptée : %v", err)
	}
}

func TestVerifierBinaire(t *testing.T) {
	_, priv := cleTest(t)
	bin := []byte("vrai binaire")
	m := manifeste(priv, "1.2.0", bin, false)
	if err := VerifierBinaire(m, bin); err != nil {
		t.Fatal(err)
	}
	if err := VerifierBinaire(m, []byte("faux binaire")); !errors.Is(err, ErrEmpreinte) {
		t.Errorf("binaire modifié accepté : %v", err)
	}
}

func TestComparerVersions(t *testing.T) {
	for _, c := range []struct {
		a, b string
		r    int
	}{{"1.2.3", "1.2.3", 0}, {"1.10.0", "1.9.9", 1}, {"v2.0.0", "1.99.99", 1}, {"0.1.0", "0.1.1", -1}} {
		if r, err := ComparerVersions(c.a, c.b); err != nil || r != c.r {
			t.Errorf("%s vs %s = %d (%v), attendu %d", c.a, c.b, r, err, c.r)
		}
	}
	if _, err := ComparerVersions("dev", "1.0.0"); err == nil {
		t.Error("« dev » ne doit pas être comparable")
	}
}

func TestInstallerConfirmerRetour(t *testing.T) {
	dir := t.TempDir()
	_, priv := cleTest(t)
	// Version actuelle installée.
	if err := os.WriteFile(filepath.Join(dir, "mirador-agent-1.1.0"), []byte("ancien"), 0o755); err != nil { // #nosec G306 -- test
		t.Fatal(err)
	}
	if err := os.Symlink("mirador-agent-1.1.0", filepath.Join(dir, "current")); err != nil {
		t.Fatal(err)
	}
	inst := NewInstallateur(dir, "/inutile")
	var teste string
	inst.SelfTest = func(_ context.Context, chemin string) error { teste = chemin; return nil }

	bin := []byte("nouveau binaire")
	if err := inst.Installer(context.Background(), manifeste(priv, "1.2.0", bin, false), bin); err != nil {
		t.Fatal(err)
	}
	if lien, _ := os.Readlink(filepath.Join(dir, "current")); lien != "mirador-agent-1.2.0" {
		t.Errorf("lien current = %q", lien)
	}
	if filepath.Base(teste) != "mirador-agent-1.2.0" {
		t.Errorf("le self-test doit porter sur le nouveau binaire : %q", teste)
	}
	info, _ := os.Stat(filepath.Join(dir, "mirador-agent-1.2.0"))
	if info.Mode().Perm() != 0o755 {
		t.Errorf("droits du binaire : %v", info.Mode().Perm())
	}
	if lien, _ := os.Readlink(filepath.Join(dir, "precedent")); lien != "mirador-agent-1.1.0" {
		t.Errorf("lien precedent = %q", lien)
	}
	e, _ := inst.EnEssai()
	if e == nil || e.Precedent != "mirador-agent-1.1.0" {
		t.Fatalf("essai mal noté : %+v", e)
	}

	// Le nouvel agent ne s'est pas signalé : retour arrière.
	if err := inst.RetourArriere("pas de battement en 5 min"); err != nil {
		t.Fatal(err)
	}
	if lien, _ := os.Readlink(filepath.Join(dir, "current")); lien != "mirador-agent-1.1.0" {
		t.Errorf("retour arrière : lien current = %q", lien)
	}
	if _, err := os.Stat(filepath.Join(dir, "echec.json")); err != nil {
		t.Error("l'échec doit être noté pour le serveur")
	}

	// Nouvelle tentative, cette fois confirmée : l'ancienne version reste
	// (retour manuel possible), les autres binaires sont supprimés.
	if err := os.WriteFile(filepath.Join(dir, "mirador-agent-0.9.0"), []byte("très ancien"), 0o755); err != nil { // #nosec G306 -- test
		t.Fatal(err)
	}
	if err := inst.Installer(context.Background(), manifeste(priv, "1.2.0", bin, false), bin); err != nil {
		t.Fatal(err)
	}
	if err := inst.Confirmer(); err != nil {
		t.Fatal(err)
	}
	for nom, attendu := range map[string]bool{"mirador-agent-1.2.0": true, "mirador-agent-1.1.0": true, "mirador-agent-0.9.0": false} {
		_, err := os.Stat(filepath.Join(dir, nom))
		if (err == nil) != attendu {
			t.Errorf("%s présent=%v, attendu %v", nom, err == nil, attendu)
		}
	}
}

func TestInstallerSelfTestEchoue(t *testing.T) {
	dir := t.TempDir()
	_, priv := cleTest(t)
	if err := os.Symlink("mirador-agent-1.1.0", filepath.Join(dir, "current")); err != nil {
		t.Fatal(err)
	}
	inst := NewInstallateur(dir, "/inutile")
	inst.SelfTest = func(context.Context, string) error { return errors.New("plante au démarrage") }
	bin := []byte("binaire cassé")
	if err := inst.Installer(context.Background(), manifeste(priv, "1.2.0", bin, false), bin); err == nil {
		t.Fatal("un binaire qui rate son self-test ne doit pas être activé")
	}
	if lien, _ := os.Readlink(filepath.Join(dir, "current")); lien != "mirador-agent-1.1.0" {
		t.Errorf("le lien ne doit pas changer : %q", lien)
	}
	if _, err := os.Stat(filepath.Join(dir, "mirador-agent-1.2.0")); err == nil {
		t.Error("le binaire refusé doit être supprimé")
	}
}
