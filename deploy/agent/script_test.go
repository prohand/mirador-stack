// SPDX-License-Identifier: GPL-3.0-or-later

package installagent

import (
	"os"
	"os/exec"
	"strings"
	"testing"
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
