// SPDX-License-Identifier: GPL-3.0-or-later

package racine

import (
	"errors"
	"os"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
)

// ouvrirSur : ouverture par root d'un fichier dont le chemin (ou un dossier
// du chemin) peut appartenir à un compte sans droits (relecture du 21/11).
//
//  1. ouverture « O_PATH » : aucun effet sur la cible (un périphérique comme
//     /dev/watchdog n'est pas réellement ouvert), lien final suivi ou non ;
//  2. contrôles sur ce descripteur : fichier ordinaire, et chemin réel
//     accepté par verifier (nil : pas de contrôle de chemin) ;
//  3. lecture par /proc/self/fd/N : exactement le fichier contrôlé, même si
//     un lien est changé entre-temps (plus de course entre contrôle et
//     ouverture).
func ouvrirSur(chemin string, suivreLien bool, verifier func(reel string) bool) (*os.File, error) {
	drapeaux := unix.O_PATH | unix.O_CLOEXEC
	if !suivreLien {
		drapeaux |= unix.O_NOFOLLOW
	}
	fd, err := unix.Open(chemin, drapeaux, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = unix.Close(fd) }()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return nil, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, errors.New("pas un fichier ordinaire")
	}
	proc := "/proc/self/fd/" + strconv.Itoa(fd)
	if verifier != nil {
		reel, err := os.Readlink(proc)
		if err != nil || !verifier(reel) {
			return nil, errors.New("fichier hors des dossiers autorisés")
		}
	}
	f, err := os.OpenFile(proc, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOCTTY, 0) // #nosec G304 -- descripteur contrôlé ci-dessus
	if err != nil {
		return nil, err
	}
	// Même fichier que celui contrôlé (garde-fou).
	if info, err := f.Stat(); err != nil || !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, errors.New("pas un fichier ordinaire")
	} else if s, ok := info.Sys().(*syscall.Stat_t); !ok || s.Ino != st.Ino || s.Dev != st.Dev {
		_ = f.Close()
		return nil, errors.New("fichier changé pendant l'ouverture")
	}
	return f, nil
}
