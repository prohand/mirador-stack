// SPDX-License-Identifier: GPL-3.0-or-later

//go:build !unix

package majagent

import "io/fs"

// droitsSurs : l'agent ne tourne que sous Linux ; ailleurs, refus.
func droitsSurs(fs.FileInfo) error { return ErrDroitsCles }
