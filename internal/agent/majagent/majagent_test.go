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
	"time"

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
	if retour {
		m.RetourJusqua = time.Now().Add(time.Hour).Unix()
	}
	m.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(priv, m.MessageSigne()))
	return m
}

// manifesteRetour : retour autorisé jusqu'à jusqua (0 : sans date, ancien format).
func manifesteRetour(priv ed25519.PrivateKey, version string, binaire []byte, jusqua int64) *protocole.Manifeste {
	m := manifeste(priv, version, binaire, true)
	m.RetourJusqua = jusqua
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
		// Relecture du 21/11 : autorisation de retour limitée dans le temps.
		"retour arrière expiré":    {manifesteRetour(priv, "1.0.9", bin, time.Now().Add(-time.Minute).Unix()), pub, ErrRetourArriere},
		"retour arrière sans date": {manifesteRetour(priv, "1.0.9", bin, 0), pub, ErrRetourArriere},
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
	var teste []byte
	inst.SelfTest = func(_ context.Context, chemin string) error { teste, _ = os.ReadFile(chemin); return nil }

	bin := []byte("nouveau binaire")
	if err := inst.Installer(context.Background(), manifeste(priv, "1.2.0", bin, false), bin); err != nil {
		t.Fatal(err)
	}
	if lien, _ := os.Readlink(filepath.Join(dir, "current")); lien != "mirador-agent-1.2.0" {
		t.Errorf("lien current = %q", lien)
	}
	if string(teste) != "nouveau binaire" {
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
	for nom, attendu := range map[string]bool{
		"mirador-agent-1.2.0": true, "mirador-agent-1.1.0": true, "mirador-agent-0.9.0": false,
		// Manifeste de la version en service : gardé pour l'assistant root (bug du 13/11).
		"mirador-agent-1.2.0.manifeste.json": true,
	} {
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

// Relecture du 21/11 : self-test raté → le binaire du même nom déjà présent
// (ici la version « precedent ») n'est pas supprimé, et aucun essai fantôme.
func TestSelfTestRateGardePrecedent(t *testing.T) {
	_, priv := cleTest(t)
	dir := t.TempDir()
	for _, v := range []string{"1.0.0", "1.1.0"} {
		_ = os.WriteFile(filepath.Join(dir, "mirador-agent-"+v), []byte("ancien "+v), 0o755)
	}
	_ = os.Symlink("mirador-agent-1.1.0", filepath.Join(dir, "current"))
	_ = os.Symlink("mirador-agent-1.0.0", filepath.Join(dir, "precedent"))
	inst := NewInstallateur(dir, "/inutile")
	inst.SelfTest = func(context.Context, string) error { return errors.New("trop lent") }
	bin := []byte("retour vers 1.0.0")
	if err := inst.Installer(context.Background(), manifeste(priv, "1.0.0", bin, true), bin); err == nil {
		t.Fatal("self-test raté accepté")
	}
	if b, err := os.ReadFile(filepath.Join(dir, "precedent")); err != nil || string(b) != "ancien 1.0.0" {
		t.Errorf("version précédente perdue : %q %v", b, err)
	}
	if e, _ := inst.EnEssai(); e != nil {
		t.Error("essai fantôme")
	}
}

// Relecture du 21/11 : une demande « Réessayer » ne sert qu'une fois, même
// avec une horloge d'agent en retard sur celle du serveur.
func TestReessaiUneFois(t *testing.T) {
	dir := t.TempDir()
	inst := NewInstallateur(dir, "/inutile")
	clic := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	retard := clic.Add(-15 * time.Minute) // horloge de l'agent : 15 min de retard
	inst.maintenant = func() time.Time { return retard }
	noter := func() {
		_ = inst.ecrireJSON("echec.json", map[string]string{"version": "1.2.0", "raison": "x", "date": inst.maintenant().UTC().Format(time.RFC3339)})
	}
	noter()
	if !inst.EffacerEchecAvant(clic) {
		t.Fatal("première demande non prise en compte")
	}
	noter() // la version réessayée échoue encore
	for i := 0; i < 3; i++ {
		if inst.EffacerEchecAvant(clic) {
			t.Fatalf("même demande reprise (tour %d) : boucle", i)
		}
	}
	if !inst.EffacerEchecAvant(clic.Add(time.Minute)) {
		t.Error("nouvelle demande ignorée")
	}
}
