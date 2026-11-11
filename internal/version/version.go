// SPDX-License-Identifier: GPL-3.0-or-later

// Package version porte la version des binaires mirador.
//
// Les valeurs sont injectées à la compilation par le Makefile :
//
//	-ldflags "-X mirador-stack/internal/version.Version=0.1.0"
package version

// Version du binaire (« dev » quand on compile sans le Makefile).
var Version = "dev"

// Commit Git du binaire (vide hors Makefile).
var Commit = ""

// ProtocoleAgent est la version du protocole agent → serveur (§ 5.2 de SPEC.md).
// Elle apparaît dans l'URL : /api/agent/v1/...
const ProtocoleAgent = 1

// Complete renvoie « 0.1.0 (abc1234) », ou la version seule si le commit est inconnu.
func Complete() string {
	if Commit == "" {
		return Version
	}
	return Version + " (" + Commit + ")"
}
