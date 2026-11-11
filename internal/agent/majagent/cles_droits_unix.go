// SPDX-License-Identifier: GPL-3.0-or-later

//go:build unix

package majagent

import (
	"io/fs"
	"syscall"
)

// proprietaireCles : propriétaire exigé du fichier de clés (root ; changé
// seulement par les tests).
var proprietaireCles uint32

// droitsSurs : fichier à root, sans écriture pour le groupe ni les autres.
func droitsSurs(st fs.FileInfo) error {
	s, ok := st.Sys().(*syscall.Stat_t)
	if !ok || s.Uid != proprietaireCles || st.Mode().Perm()&0o022 != 0 {
		return ErrDroitsCles
	}
	return nil
}
