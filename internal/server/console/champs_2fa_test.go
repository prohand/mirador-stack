// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// Vérification du 22/11 : tous les champs du code 2FA portent les repères
// des gestionnaires de mots de passe (Bitwarden, Vaultwarden, navigateurs) :
// autocomplete « one-time-code », mention TOTP et saisie numérique.
func TestChampsCode2FAReconnus(t *testing.T) {
	reChamp := regexp.MustCompile(`<input[^>]*\bname="code"[^>]*>`)
	n := 0
	err := fs.WalkDir(fichiers, "gabarits", func(chemin string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() || !strings.HasSuffix(chemin, ".html") {
			return err
		}
		b, err := fs.ReadFile(fichiers, chemin)
		if err != nil {
			return err
		}
		for _, champ := range reChamp.FindAllString(string(b), -1) {
			n++
			for _, attendu := range []string{`autocomplete="one-time-code"`, `inputmode="numeric"`, `TOTP`, `pattern="[0-9 ]{6,7}"`} {
				if !strings.Contains(champ, attendu) {
					t.Errorf("%s : champ du code 2FA sans %s : %s", chemin, attendu, champ)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n < 4 {
		t.Fatalf("%d champs trouvés : recherche à revoir", n)
	}
}
