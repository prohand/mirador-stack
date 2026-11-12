// SPDX-License-Identifier: GPL-3.0-or-later

package config

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"

	"mirador-stack/internal/protocole"
)

// FichierIdentifiantMachine : identifiant stable de l'installation (créé par
// systemd au premier démarrage, différent sur chaque machine).
const FichierIdentifiantMachine = "/etc/machine-id"

// EntetesMachine ajoute à des en-têtes d'envoi l'empreinte de la machine,
// seulement vers mirador-server (jamais vers une autre destination).
func (a *Agent) EntetesMachine(entetes map[string]string) map[string]string {
	if a.Serveur == "" {
		return entetes
	}
	if e := EmpreinteMachine(""); e != "" {
		entetes[protocole.EnteteEmpreinte] = e
	}
	return entetes
}

// EmpreinteMachine : empreinte de la machine envoyée au serveur (SPEC.md
// § 19.8, idée reprise de Beszel). Si l'administrateur lie le token à la
// machine, un token copié sur une autre machine est refusé.
//
// C'est l'empreinte SHA-256 de /etc/machine-id (32 chiffres hexadécimaux),
// jamais l'identifiant lui-même : il ne quitte pas la machine. Vide si le
// fichier manque (l'en-tête n'est alors pas envoyé).
func EmpreinteMachine(chemin string) string {
	if chemin == "" {
		chemin = FichierIdentifiantMachine
	}
	brut, err := os.ReadFile(chemin) // #nosec G304 -- chemin fixe (constante ci-dessus ; variable dans les tests)
	id := strings.TrimSpace(string(brut))
	if err != nil || len(id) < 16 || len(id) > 128 {
		return ""
	}
	h := sha256.Sum256([]byte("mirador-empreinte-machine:" + id))
	return hex.EncodeToString(h[:16])
}
