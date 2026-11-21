// SPDX-License-Identifier: GPL-3.0-or-later

// Package relance : redémarrage sur place de l'agent ou du collecteur après
// un changement de configuration.
//
// Relecture du 21/11 (SPEC § 117) : chaque changement de configuration
// arrêtait le programme et systemd le relançait 10 s plus tard
// (RestartSec=10) ; le syslog du collecteur était coupé pendant ce temps.
// Surtout, les unités limitent à 5 démarrages en 5 minutes : au 6e
// changement (plusieurs sondes ajoutées à la suite), systemd ne relançait
// plus rien et le programme restait arrêté.
//
// Le programme se relance donc lui-même (execve, même PID) : systemd ne
// voit pas de redémarrage. Une mise à jour ou un retour arrière, eux,
// passent toujours par systemd (contrôle du démarrage de la nouvelle
// version).
package relance

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// Code : code de sortie de lancer() qui demande une relance sur place.
const Code = 75

// Executer remplace le programme par lui-même, mêmes arguments et même
// environnement. Ne revient qu'en cas d'échec (systemd relance alors comme
// avant).
func Executer() error {
	chemin := os.Args[0]
	if !filepath.IsAbs(chemin) {
		var err error
		if chemin, err = os.Executable(); err != nil {
			return err
		}
	}
	if _, err := os.Stat(chemin); err != nil { // #nosec G703 -- chemin du programme lui-même (os.Args[0])
		return errors.Join(errors.New("programme introuvable"), err)
	}
	return syscall.Exec(chemin, os.Args, os.Environ()) // #nosec G204 G702 -- le programme lui-même, avec ses propres arguments
}
