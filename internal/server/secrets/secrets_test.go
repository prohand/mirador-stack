// SPDX-License-Identifier: GPL-3.0-or-later

package secrets

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestSecrets(t *testing.T) {
	cle := bytes.Repeat([]byte{1}, 32)
	v, err := Chiffrer(cle, "sauvegarde.s3.secret", "mot-de-passe-très-secret")
	if err != nil || !EstChiffre(v) || strings.Contains(v, "secret") {
		t.Fatalf("%q %v", v, err)
	}
	if c, err := Dechiffrer(cle, "sauvegarde.s3.secret", v); err != nil || c != "mot-de-passe-très-secret" {
		t.Fatalf("%q %v", c, err)
	}
	if _, err := Dechiffrer(cle, "autre.usage", v); !errors.Is(err, ErrSecret) {
		t.Error("secret déplacé dans un autre champ accepté")
	}
	if _, err := Dechiffrer(bytes.Repeat([]byte{2}, 32), "sauvegarde.s3.secret", v); !errors.Is(err, ErrSecret) {
		t.Error("autre clé acceptée")
	}
	if _, err := Dechiffrer(cle, "sauvegarde.s3.secret", v[:len(v)-4]+"AAAA"); !errors.Is(err, ErrSecret) {
		t.Error("valeur modifiée acceptée")
	}
	if _, err := Dechiffrer(cle, "x", "en clair"); err == nil {
		t.Error("valeur en clair acceptée")
	}
	if v2, _ := Chiffrer(cle, "sauvegarde.s3.secret", "mot-de-passe-très-secret"); v2 == v {
		t.Error("même chiffré deux fois")
	}
}

// Audit du 16/11 (SPEC § 34) : secrets chiffrés masqués dans l'audit et
// l'export « sans secret », puis repris à l'import.
func TestMasquerEtGarder(t *testing.T) {
	cle := bytes.Repeat([]byte{3}, 32)
	a, _ := Chiffrer(cle, "essai", "mot de passe du NAS")
	b, _ := Chiffrer(cle, "essai", "mot de passe du routeur")
	doc := `{"collecteur":{"snmp":[{"machine":"nas","adresse":"192.0.2.10","mot_de_passe":"` + a + `"},{"machine":"routeur","mot_de_passe":"` + b + `"}],"unifi":{"cle_api":"` + a + `"}},"intervalle":"30s"}`
	masque := MasquerJSON(doc)
	if strings.Contains(masque, prefixe) || strings.Count(masque, Masque) != 3 || !strings.Contains(masque, `"intervalle":"30s"`) {
		t.Fatalf("masquage : %s", masque)
	}
	if MasquerJSON(`{"a":1}`) != `{"a":1}` || MasquerJSON("") != "" {
		t.Error("document sans secret modifié")
	}
	if strings.Contains(MasquerJSON("pas du JSON "+a), prefixe) {
		t.Error("secret gardé dans un document illisible")
	}
	// Import : « *** » reprend la valeur actuelle, même si l'ordre de la
	// liste a changé (rapprochement par « machine »).
	var actuel, nouveau any
	_ = json.Unmarshal([]byte(doc), &actuel)
	_ = json.Unmarshal([]byte(`{"collecteur":{"snmp":[{"machine":"routeur","mot_de_passe":"***"},{"machine":"nas","mot_de_passe":"***"}],"unifi":{"cle_api":"***"}}}`), &nouveau)
	garde, err := Garder(nouveau, actuel)
	if err != nil {
		t.Fatal(err)
	}
	liste := garde.(map[string]any)["collecteur"].(map[string]any)["snmp"].([]any)
	if liste[0].(map[string]any)["mot_de_passe"] != b || liste[1].(map[string]any)["mot_de_passe"] != a {
		t.Error("secrets repris sur le mauvais équipement")
	}
	// Rien à reprendre : refusé (jamais « *** » enregistré comme mot de passe).
	_ = json.Unmarshal([]byte(`{"collecteur":{"snmp":[{"machine":"inconnu","mot_de_passe":"***"}]}}`), &nouveau)
	if _, err := Garder(nouveau, actuel); err == nil {
		t.Error("« *** » sans valeur actuelle accepté")
	}
}
