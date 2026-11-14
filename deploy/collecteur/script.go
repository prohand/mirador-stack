// SPDX-License-Identifier: GPL-3.0-or-later

// Package installcollecteur embarque le script d'installation de
// mirador-collector dans mirador-server (téléchargé en HTTPS, SPEC § 23.13).
package installcollecteur

import _ "embed" // script embarqué

// Script : mirador-collector-install.sh, tel que dans le dépôt.
//
//go:embed mirador-collector-install.sh
var Script []byte
