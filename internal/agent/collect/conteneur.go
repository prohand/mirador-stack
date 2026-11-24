// SPDX-License-Identifier: GPL-3.0-or-later

package collect

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// Agent dans un conteneur (LXC, Docker, systemd-nspawn…) : /sys et /proc y
// montrent le matériel de l'hôte (capteurs de /sys/class/hwmon, ventilateurs,
// pools ZFS, grappes RAID). Les envoyer ferait croire que le conteneur est une
// machine physique (modèle « Machine physique » coché tout seul) et doublerait
// les alertes de l'hôte (retour du 24/11 : pbs et pdm, conteneurs LXC).
// L'agent ne lit donc pas ce matériel dans un conteneur : c'est l'agent de
// l'hôte qui le surveille.

var reGenreConteneur = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// detecterConteneur : genre de conteneur (« lxc », « docker »…), ou "" sur
// une machine ou une VM. racine : « / » (autre chose pour les tests).
//   - /run/systemd/container : écrit par systemd au démarrage d'un conteneur
//     (LXC, nspawn, podman…), lisible par tous ;
//   - /.dockerenv : conteneur Docker (sans systemd en général).
func detecterConteneur(racine string) string {
	if data, err := os.ReadFile(filepath.Join(racine, "run", "systemd", "container")); err == nil { // #nosec G304 -- chemin fixe
		if g := strings.TrimSpace(string(data)); reGenreConteneur.MatchString(g) {
			return g
		}
		return "conteneur"
	}
	if _, err := os.Stat(filepath.Join(racine, ".dockerenv")); err == nil {
		return "docker"
	}
	return ""
}

var (
	conteneurUneFois sync.Once
	conteneurGenre   string
)

// Conteneur : genre de conteneur où tourne l'agent ("" : machine ou VM).
// Lu une seule fois.
func Conteneur() string {
	conteneurUneFois.Do(func() { conteneurGenre = detecterConteneur("/") })
	return conteneurGenre
}
