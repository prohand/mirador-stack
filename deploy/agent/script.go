// SPDX-License-Identifier: GPL-3.0-or-later

// Package installagent embarque le script d'installation de l'agent dans
// mirador-server : la machine le télécharge en HTTPS (avec son jeton
// d'enrôlement) au lieu de le copier à la main (demande du 11/11).
package installagent

import _ "embed" // script embarqué

// Script : mirador-agent-install.sh, tel que dans le dépôt.
//
//go:embed mirador-agent-install.sh
var Script []byte
