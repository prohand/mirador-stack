// SPDX-License-Identifier: GPL-3.0-or-later

package collect

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDetecterConteneur(t *testing.T) {
	cas := []struct {
		nom     string
		fichier string // relatif à la racine ; "" : aucun
		contenu string
		attendu string
	}{
		{"machine ou VM", "", "", ""},
		{"LXC", "run/systemd/container", "lxc\n", "lxc"},
		{"nspawn", "run/systemd/container", "systemd-nspawn", "systemd-nspawn"},
		{"contenu bizarre", "run/systemd/container", "<script>", "conteneur"},
		{"Docker sans systemd", ".dockerenv", "", "docker"},
	}
	for _, c := range cas {
		t.Run(c.nom, func(t *testing.T) {
			racine := t.TempDir()
			if c.fichier != "" {
				chemin := filepath.Join(racine, c.fichier)
				if err := os.MkdirAll(filepath.Dir(chemin), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(chemin, []byte(c.contenu), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if g := detecterConteneur(racine); g != c.attendu {
				t.Errorf("detecterConteneur = %q, attendu %q", g, c.attendu)
			}
		})
	}
}
