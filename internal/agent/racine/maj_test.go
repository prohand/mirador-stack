// SPDX-License-Identifier: GPL-3.0-or-later

package racine

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"mirador-stack/internal/agent/config"
	"mirador-stack/internal/agent/majagent"
	"mirador-stack/internal/protocole"
	"mirador-stack/internal/version"
)

// Mise à jour de l'assistant root par lui-même (SPEC § 22.5).
func TestMettreAJourAssistant(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	_, autre, _ := ed25519.GenerateKey(rand.Reader)
	ancienneCle, ancienneVersion, ancienDossier := majagent.ClesPubliques, version.Version, dossierAssistant
	t.Cleanup(func() {
		majagent.ClesPubliques, version.Version, dossierAssistant = ancienneCle, ancienneVersion, ancienDossier
	})
	majagent.ClesPubliques = base64.StdEncoding.EncodeToString(pub)
	version.Version = "1.0.0"

	racineDir, bin := t.TempDir(), t.TempDir()
	dossierAssistant = racineDir + "/"
	cible := filepath.Join(racineDir, "mirador-agent")
	_ = os.WriteFile(cible, []byte("assistant 1.0.0"), 0o755)
	cfg := config.Agent{MajAuto: true, MajRacine: true, DossierBin: bin}

	poser := func(v string, cle ed25519.PrivateKey, contenu string) {
		t.Helper()
		nom := "mirador-agent-" + v
		h := sha256.Sum256([]byte(contenu))
		m := protocole.Manifeste{Version: v, Architecture: runtime.GOARCH, SHA256: hex.EncodeToString(h[:]), Taille: int64(len(contenu))}
		m.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(cle, m.MessageSigne()))
		brut, _ := json.Marshal(m)
		_ = os.Remove(filepath.Join(bin, nom))
		_ = os.WriteFile(filepath.Join(bin, nom), []byte(contenu), 0o755)
		_ = os.WriteFile(filepath.Join(bin, nom+".manifeste.json"), brut, 0o644)
		_ = os.Remove(filepath.Join(bin, "current"))
		_ = os.Symlink(nom, filepath.Join(bin, "current"))
	}
	contenu := func() string { b, _ := os.ReadFile(cible); return string(b) }

	// Rien à faire : réglage local qui l'interdit, ou pas de mise à jour auto.
	poser("1.1.0", priv, "assistant 1.1.0")
	for _, c := range []config.Agent{{MajAuto: true, MajRacine: false, DossierBin: bin}, {MajAuto: false, MajRacine: true, DossierBin: bin}} {
		if msg, err := MettreAJourAssistant(c, cible); msg != "" || err != nil || contenu() != "assistant 1.0.0" {
			t.Fatalf("interdit, mais : %q %v", msg, err)
		}
	}
	// Version encore à l'essai : on attend.
	_ = os.WriteFile(filepath.Join(bin, "essai.json"), []byte("{}"), 0o600)
	if msg, _ := MettreAJourAssistant(cfg, cible); msg != "" {
		t.Fatal("mise à jour pendant l'essai de l'agent")
	}
	_ = os.Remove(filepath.Join(bin, "essai.json"))
	// Hors du dossier de l'assistant : jamais.
	if msg, _ := MettreAJourAssistant(cfg, filepath.Join(t.TempDir(), "x")); msg != "" {
		t.Fatal("remplacement hors du dossier de l'assistant")
	}
	// Signée par une autre clé : refusée.
	poser("1.1.0", autre, "assistant pirate")
	if _, err := MettreAJourAssistant(cfg, cible); err == nil || contenu() != "assistant 1.0.0" {
		t.Fatalf("signature d'une autre clé acceptée : %v", err)
	}
	// Binaire changé après la signature : refusé (empreinte).
	poser("1.1.0", priv, "assistant 1.1.0")
	_ = os.WriteFile(filepath.Join(bin, "mirador-agent-1.1.0"), []byte("assistant 1.1.X"), 0o755)
	if _, err := MettreAJourAssistant(cfg, cible); err == nil || contenu() != "assistant 1.0.0" {
		t.Fatalf("binaire modifié accepté : %v", err)
	}
	// Binaire remplacé par un lien : refusé (lien jamais suivi).
	poser("1.1.0", priv, "assistant 1.1.0")
	_ = os.Remove(filepath.Join(bin, "mirador-agent-1.1.0"))
	_ = os.Symlink("/etc/passwd", filepath.Join(bin, "mirador-agent-1.1.0"))
	if _, err := MettreAJourAssistant(cfg, cible); err == nil || contenu() != "assistant 1.0.0" {
		t.Fatalf("lien suivi : %v", err)
	}
	// Version plus ancienne : jamais (même signée).
	poser("0.9.0", priv, "assistant 0.9.0")
	if msg, err := MettreAJourAssistant(cfg, cible); msg != "" || err != nil || contenu() != "assistant 1.0.0" {
		t.Fatalf("retour arrière : %q %v", msg, err)
	}
	// Manifeste absent (bug du 13/11) : rien d'installé, mais signalé.
	poser("1.1.0", priv, "assistant 1.1.0")
	_ = os.Remove(filepath.Join(bin, "mirador-agent-1.1.0.manifeste.json"))
	if _, err := MettreAJourAssistant(cfg, cible); err == nil || !strings.Contains(err.Error(), "manifeste signé absent") || contenu() != "assistant 1.0.0" {
		t.Fatalf("manifeste absent : %v", err)
	}
	// Cas normal : signée, confirmée, plus récente → remplacée, ancienne gardée.
	poser("1.1.0", priv, "assistant 1.1.0")
	msg, err := MettreAJourAssistant(cfg, cible)
	if err != nil || !strings.Contains(msg, "1.0.0 → 1.1.0") || contenu() != "assistant 1.1.0" {
		t.Fatalf("mise à jour : %q %v (%q)", msg, err, contenu())
	}
	if b, _ := os.ReadFile(cible + ".precedent"); string(b) != "assistant 1.0.0" {
		t.Error("ancienne version non gardée")
	}
}
