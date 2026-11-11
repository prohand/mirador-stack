// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"mirador-stack/internal/release"
)

func phraseFixe(p string) lirePhrase {
	return func(string) ([]byte, error) { return []byte(p), nil }
}

func TestParcoursComplet(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("binaire ELF requis")
	}
	dir := t.TempDir()
	cle := filepath.Join(dir, "cle.json")
	var so, se bytes.Buffer
	if code := lancer([]string{"cle", "-sortie", cle}, &so, &se, phraseFixe("phrase de passe de test")); code != 0 {
		t.Fatalf("cle : %s", se.String())
	}
	if info, _ := os.Stat(cle); info.Mode().Perm() != 0o600 {
		t.Errorf("droits du fichier de clé : %v", info.Mode().Perm())
	}
	// Jamais d'écrasement.
	if code := lancer([]string{"cle", "-sortie", cle}, &so, &se, phraseFixe("phrase de passe de test")); code == 0 {
		t.Fatal("clé écrasée")
	}
	var f release.FichierCle
	brut, _ := os.ReadFile(cle)
	_ = json.Unmarshal(brut, &f)
	if !strings.Contains(so.String(), f.Publique) {
		t.Error("clé publique non affichée")
	}

	exe, _ := os.Executable()
	binaire := filepath.Join(dir, "mirador-agent-linux-"+runtime.GOARCH)
	contenu, _ := os.ReadFile(exe)
	_ = os.WriteFile(binaire, contenu, 0o755)
	if code := lancer([]string{"signer", "-cle", cle, "-version", "1.2.3", "-arch", runtime.GOARCH, binaire}, &so, &se, phraseFixe("mauvaise phrase !!")); code == 0 {
		t.Fatal("signé avec une mauvaise phrase")
	}
	se.Reset()
	if code := lancer([]string{"signer", "-cle", cle, "-version", "1.2.3", "-arch", runtime.GOARCH, binaire}, &so, &se, phraseFixe("phrase de passe de test")); code != 0 {
		t.Fatalf("signer : %s", se.String())
	}
	so.Reset()
	if code := lancer([]string{"verifier", "-publique", f.Publique, binaire + ".manifeste.json", binaire}, &so, &se, nil); code != 0 || !strings.Contains(so.String(), "OK") {
		t.Fatalf("verifier : %s %s", so.String(), se.String())
	}
	// Phrases différentes à la création.
	n := 0
	alterne := func(string) ([]byte, error) { n++; return []byte(strings.Repeat("x", 12+n)), nil }
	if code := lancer([]string{"cle", "-sortie", filepath.Join(dir, "autre.json")}, &so, &se, alterne); code == 0 {
		t.Error("phrases différentes acceptées")
	}
}
