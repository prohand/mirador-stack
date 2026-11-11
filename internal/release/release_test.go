// SPDX-License-Identifier: GPL-3.0-or-later

package release

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"runtime"
	"strings"
	"testing"

	"mirador-stack/internal/agent/majagent"
)

func init() {
	argonMemoire = 64 * 1024 // tests plus rapides ; le format garde les paramètres
	argonTemps = 1
}

var phrase = []byte("une phrase de passe assez longue")

func TestCleChiffree(t *testing.T) {
	f, err := NouvelleCle(phrase)
	if err != nil {
		t.Fatal(err)
	}
	brut, _ := json.Marshal(f)
	if bytes.Contains(brut, []byte("PRIVATE")) || f.Chiffre == "" {
		t.Fatal("fichier inattendu")
	}
	priv, err := LireCle(brut, phrase)
	if err != nil {
		t.Fatal(err)
	}
	if base64.StdEncoding.EncodeToString(priv.Public().(ed25519.PublicKey)) != f.Publique {
		t.Fatal("clé publique différente")
	}
	if _, err := LireCle(brut, []byte("mauvaise phrase de passe")); !errors.Is(err, ErrPhrase) {
		t.Fatalf("mauvaise phrase : %v", err)
	}
	// Toute modification du fichier est détectée.
	modifs := map[string]func(*FichierCle){
		"clé publique remplacée": func(f *FichierCle) { f.Publique = base64.StdEncoding.EncodeToString(make([]byte, 32)) },
		"paramètre changé":       func(f *FichierCle) { f.Temps++ },
		"chiffré modifié": func(f *FichierCle) {
			b, _ := base64.StdEncoding.DecodeString(f.Chiffre)
			b[0] ^= 1
			f.Chiffre = base64.StdEncoding.EncodeToString(b)
		},
		"mémoire énorme": func(f *FichierCle) { f.MemoireKio = 1 << 30 },
	}
	for nom, m := range modifs {
		g := *f
		m(&g)
		b, _ := json.Marshal(&g)
		if _, err := LireCle(b, phrase); err == nil {
			t.Errorf("%s : accepté", nom)
		}
	}
	if _, err := NouvelleCle([]byte("court")); err == nil {
		t.Error("phrase courte acceptée")
	}
	if _, err := LireCle([]byte(`{"format":"autre"}`), phrase); err == nil {
		t.Error("format inconnu accepté")
	}
}

// executable : un vrai binaire Linux (le binaire de test lui-même).
func executable(t *testing.T) []byte {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("binaire ELF requis")
	}
	chemin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(chemin)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSignatureAccepteeParLAgent(t *testing.T) {
	f, _ := NouvelleCle(phrase)
	brut, _ := json.Marshal(f)
	priv, err := LireCle(brut, phrase)
	if err != nil {
		t.Fatal(err)
	}
	binaire := executable(t)
	m, err := Signer(priv, binaire, "1.2.3", runtime.GOARCH, false)
	if err != nil {
		t.Fatal(err)
	}
	// Le vérificateur de l'agent (code réel) accepte le manifeste et le binaire.
	if err := majagent.VerifierManifeste(m, "1.0.0", f.Publique); err != nil {
		t.Fatalf("refusé par l'agent : %v", err)
	}
	if err := majagent.VerifierBinaire(m, binaire); err != nil {
		t.Fatalf("binaire refusé par l'agent : %v", err)
	}
	if err := Verifier(m, binaire, f.Publique); err != nil {
		t.Fatal(err)
	}
	if err := Verifier(m, append([]byte{0}, binaire...), f.Publique); err == nil {
		t.Error("binaire modifié accepté")
	}
	// Retour arrière : refusé par l'agent sauf autorisation signée.
	if err := majagent.VerifierManifeste(m, "2.0.0", f.Publique); err == nil {
		t.Error("version plus ancienne acceptée sans autorisation")
	}
	m2, _ := Signer(priv, binaire, "1.2.3", runtime.GOARCH, true)
	if err := majagent.VerifierManifeste(m2, "2.0.0", f.Publique); err != nil {
		t.Errorf("retour arrière autorisé refusé : %v", err)
	}
	// Mauvais fichiers.
	autre := map[string]string{"amd64": "arm64", "arm64": "amd64"}[runtime.GOARCH]
	if _, err := Signer(priv, binaire, "1.2.3", autre, false); err == nil || !strings.Contains(err.Error(), "pas pour") {
		t.Errorf("mauvaise architecture : %v", err)
	}
	if _, err := Signer(priv, []byte("#!/bin/sh\necho"), "1.2.3", "amd64", false); err == nil {
		t.Error("script signé")
	}
	if _, err := Signer(priv, binaire, "v1", runtime.GOARCH, false); err == nil {
		t.Error("version mal formée acceptée")
	}
}
