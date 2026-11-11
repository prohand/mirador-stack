// SPDX-License-Identifier: GPL-3.0-or-later

package majagent

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func clePubTest(t *testing.T) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(pub)
}

func TestValiderCles(t *testing.T) {
	a, b, c := clePubTest(t), clePubTest(t), clePubTest(t)
	ls, err := ValiderCles("# commentaire\n" + a + "\n" + a + ", " + b + "\n")
	if err != nil || len(ls) != 2 {
		t.Fatalf("%v %v", ls, err)
	}
	if _, err := ValiderCles(a + "," + b + "," + c); err == nil {
		t.Error("3 clés acceptées")
	}
	if _, err := ValiderCles("pas-une-cle"); err == nil {
		t.Error("clé invalide acceptée")
	}
	if ls, err := ValiderCles(""); err != nil || len(ls) != 0 {
		t.Error("vide refusé")
	}
}

func TestClesInstallees(t *testing.T) {
	ancien, ancienProp := ClesPubliques, proprietaireCles
	defer func() { ClesPubliques, proprietaireCles = ancien, ancienProp }()
	ClesPubliques = ""
	proprietaireCles = uint32(os.Getuid()) // fichier créé par le test

	dir := t.TempDir()
	chemin := filepath.Join(dir, "cles-publiques.txt")
	// Fichier absent : aucune clé (mises à jour impossibles), pas d'erreur.
	if c, err := ClesInstallees(chemin); err != nil || c != "" {
		t.Fatalf("absent : %q %v", c, err)
	}
	a := clePubTest(t)
	if err := os.WriteFile(chemin, []byte("# posée à l'installation\n"+a+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if c, err := ClesInstallees(chemin); err != nil || c != a {
		t.Fatalf("fichier : %q %v", c, err)
	}
	// Clé intégrée + clé du fichier.
	b := clePubTest(t)
	ClesPubliques = b
	if c, err := ClesInstallees(chemin); err != nil || len(strings.Split(c, ",")) != 2 {
		t.Fatalf("intégrée + fichier : %q %v", c, err)
	}
	ClesPubliques = ""
	// Modifiable par le groupe ou les autres : refusé.
	_ = os.Chmod(chemin, 0o664)
	if _, err := ClesInstallees(chemin); !errors.Is(err, ErrDroitsCles) {
		t.Errorf("groupe en écriture : %v", err)
	}
	_ = os.Chmod(chemin, 0o644)
	// Autre propriétaire que celui attendu (root en vrai) : refusé.
	proprietaireCles = uint32(os.Getuid()) + 1
	if _, err := ClesInstallees(chemin); !errors.Is(err, ErrDroitsCles) {
		t.Errorf("autre propriétaire : %v", err)
	}
	proprietaireCles = uint32(os.Getuid())
	// Lien symbolique : refusé (l'agent pourrait pointer ailleurs).
	lien := filepath.Join(dir, "lien.txt")
	_ = os.Symlink(chemin, lien)
	if _, err := ClesInstallees(lien); err == nil {
		t.Error("lien accepté")
	}
	// Contenu invalide : refusé.
	_ = os.WriteFile(chemin, []byte("n'importe quoi\n"), 0o644)
	if _, err := ClesInstallees(chemin); err == nil {
		t.Error("contenu invalide accepté")
	}
}
