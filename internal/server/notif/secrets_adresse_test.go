// SPDX-License-Identifier: GPL-3.0-or-later

package notif

import (
	"bytes"
	"strings"
	"testing"
)

// Audit du 16/11 (SPEC § 34) : un secret gardé (« *** » ou vide) ne suit pas
// un changement de destination ; il doit être ressaisi.
func TestSecretGardeSiAdresseInchangee(t *testing.T) {
	cle := bytes.Repeat([]byte{5}, 32)
	anciens, err := PreparerSecrets(Canaux{
		Ntfy:  &Ntfy{URL: "https://ntfy.exemple.fr/alertes", Jeton: "jeton-ntfy"},
		Email: &Email{Serveur: "smtp.exemple.fr:587", MotDePasse: "mot-de-passe"},
		Olvid: &Olvid{Adresse: "mirador-olvid:50051", Cle: "cle-client"},
	}, Canaux{}, cle)
	if err != nil {
		t.Fatal(err)
	}
	// Même serveur ntfy, autre sujet : le jeton est gardé.
	c, err := PreparerSecrets(Canaux{Ntfy: &Ntfy{URL: "https://NTFY.exemple.fr/autre-sujet", Jeton: "***"}}, anciens, cle)
	if err != nil || c.Ntfy.Jeton != anciens.Ntfy.Jeton {
		t.Fatalf("sujet changé sur le même serveur : %v", err)
	}
	// Destination changée, secret laissé vide ou masqué : refusé.
	for nom, nouveaux := range map[string]Canaux{
		"ntfy":  {Ntfy: &Ntfy{URL: "https://attaquant.example/x", Jeton: "***"}},
		"ntfy2": {Ntfy: &Ntfy{URL: "http://ntfy.exemple.fr/alertes", Jeton: ""}}, // même hôte, plus de TLS
		"email": {Email: &Email{Serveur: "smtp.attaquant.example:587", MotDePasse: ""}},
		"olvid": {Olvid: &Olvid{Adresse: "203.0.113.9:50051", Cle: "***"}},
		// Le texte chiffré recopié tel quel (import) ne suit pas non plus.
		"import": {Ntfy: &Ntfy{URL: "https://attaquant.example/x", Jeton: anciens.Ntfy.Jeton}},
	} {
		if _, err := PreparerSecrets(nouveaux, anciens, cle); err == nil || !strings.Contains(err.Error(), "saisir de nouveau") {
			t.Errorf("%s : secret gardé malgré le changement d'adresse (%v)", nom, err)
		}
	}
	// Destination changée ET secret ressaisi : accepté, nouveau secret chiffré.
	c, err = PreparerSecrets(Canaux{Ntfy: &Ntfy{URL: "https://ntfy.sh/x", Jeton: "nouveau-jeton"}}, anciens, cle)
	if err != nil || c.Ntfy.Jeton == anciens.Ntfy.Jeton || c.Ntfy.Jeton == "nouveau-jeton" {
		t.Fatalf("secret ressaisi : %v", err)
	}
	// Canal sans secret enregistré : rien à protéger, l'adresse change librement.
	if _, err := PreparerSecrets(Canaux{Ntfy: &Ntfy{URL: "https://ntfy.sh/x"}}, Canaux{Ntfy: &Ntfy{URL: "https://a.example/y"}}, cle); err != nil {
		t.Errorf("canal sans jeton : %v", err)
	}
}
