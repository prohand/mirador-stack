// SPDX-License-Identifier: GPL-3.0-or-later

package collect

import (
	"bufio"
	"io"
	"os"
	"strings"
	"time"
)

// Paquets installés (suivi des changements, SPEC § 47) : lus dans
// /var/log/dpkg.log (lisible par tous sur Debian et Ubuntu), au fil de
// l'eau. Seules les lignes « upgrade » et « install » comptent :
//
//	2026-11-18 03:12:40 upgrade openssl:amd64 3.0.15-1 3.0.15-2
//
// Les installations des dernières 48 h sont envoyées en
// apt_package_installed{package, from, to} (valeur : heure, secondes Unix),
// 200 au plus. Aucun programme lancé ; à chaque passage, un stat() du
// fichier, et la lecture des seules lignes nouvelles.

const (
	cheminDpkgLog     = "/var/log/dpkg.log"
	dureeInstallees   = 48 * time.Hour
	maxInstallees     = 200
	maxLectureDpkgLog = 4 << 20 // premier passage : 4 Mio de fin de fichier au plus
)

type paquetInstalle struct {
	nom, avant, apres string
	le                time.Time
}

type journalDpkg struct {
	chemin  string
	taille  int64 // octets déjà lus
	modifie time.Time
	liste   []paquetInstalle
}

// lire : lignes nouvelles depuis le dernier passage. Renvoie vrai si un
// paquet a été installé depuis (la liste « à mettre à jour » est alors
// recalculée plus tôt).
func (j *journalDpkg) lire(maintenant time.Time) bool {
	fi, err := os.Stat(j.chemin)
	if err != nil || !fi.Mode().IsRegular() {
		return false
	}
	if fi.Size() == j.taille && fi.ModTime().Equal(j.modifie) {
		return false
	}
	premier := j.modifie.IsZero()
	debut := j.taille
	if fi.Size() < j.taille { // fichier tourné (logrotate) : on repart du début
		debut = 0
	}
	if premier && fi.Size()-debut > maxLectureDpkgLog {
		debut = fi.Size() - maxLectureDpkgLog
	}
	f, err := os.Open(j.chemin)
	if err != nil {
		return false
	}
	defer f.Close()
	if _, err := f.Seek(debut, io.SeekStart); err != nil {
		return false
	}
	nouveau := false
	sc := bufio.NewScanner(io.LimitReader(f, fi.Size()-debut))
	sc.Buffer(make([]byte, 0, 4096), 64<<10)
	if debut > 0 && premier {
		sc.Scan() // ligne coupée par le saut
	}
	for sc.Scan() {
		if p, ok := lireLigneDpkg(sc.Text()); ok && maintenant.Sub(p.le) < dureeInstallees {
			j.liste = append(j.liste, p)
			nouveau = nouveau || !premier
		}
	}
	j.taille, j.modifie = fi.Size(), fi.ModTime()
	// Ménage : 48 h, 200 au plus (les plus récentes).
	garde := j.liste[:0]
	for _, p := range j.liste {
		if maintenant.Sub(p.le) < dureeInstallees {
			garde = append(garde, p)
		}
	}
	if len(garde) > maxInstallees {
		garde = garde[len(garde)-maxInstallees:]
	}
	j.liste = garde
	return nouveau
}

// lireLigneDpkg : « date heure upgrade|install paquet:arch avant après ».
func lireLigneDpkg(l string) (paquetInstalle, bool) {
	c := strings.Fields(l)
	if len(c) != 6 || (c[2] != "upgrade" && c[2] != "install") {
		return paquetInstalle{}, false
	}
	le, err := time.ParseInLocation("2006-01-02 15:04:05", c[0]+" "+c[1], time.Local)
	if err != nil {
		return paquetInstalle{}, false
	}
	nom, _, _ := strings.Cut(c[3], ":")
	avant := c[4]
	if avant == "<none>" {
		avant = ""
	}
	if nom == "" || len(nom) > 128 || len(avant) > 128 || len(c[5]) > 128 {
		return paquetInstalle{}, false
	}
	return paquetInstalle{nom: nom, avant: avant, apres: c[5], le: le}, true
}
