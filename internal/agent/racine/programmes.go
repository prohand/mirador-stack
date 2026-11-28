// SPDX-License-Identifier: GPL-3.0-or-later

package racine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// programmes : les seuls programmes que l'assistant peut lancer. Les chemins
// et les arguments sont figés ici ; aucun ne vient de la configuration.
type programmes interface {
	// iptablesInput : « iptables -S » (toutes les chaînes de la table
	// filter : politique d'INPUT et règles de journalisation).
	iptablesInput() ([]byte, error)
	// nftRuleset : « nft list ruleset ».
	nftRuleset() ([]byte, error)
	// smartScan : « smartctl --scan-open --json ».
	smartScan() ([]byte, error)
	// smartDisque : « smartctl --json --all --nocheck=standby [-d type] disque ».
	smartDisque(disque, typ string) ([]byte, error)
}

// Chemins figés (Debian / Ubuntu).
const (
	cheminIptables = "/usr/sbin/iptables"
	cheminNft      = "/usr/sbin/nft"
	cheminSmartctl = "/usr/sbin/smartctl"
)

var errAbsent = errors.New("programme absent")

type programmesSysteme struct {
	delai time.Duration
}

func programmesReels() programmes { return programmesSysteme{delai: time.Minute} }

func (p programmesSysteme) iptablesInput() ([]byte, error) {
	return p.lancer(cheminIptables, "-S")
}

func (p programmesSysteme) nftRuleset() ([]byte, error) {
	return p.lancer(cheminNft, "list", "ruleset")
}

func (p programmesSysteme) smartScan() ([]byte, error) {
	return p.lancer(cheminSmartctl, "--scan-open", "--json")
}

func (p programmesSysteme) smartDisque(disque, typ string) ([]byte, error) {
	// --nocheck=standby : un disque en veille n'est pas réveillé (moins
	// d'usure, idée reprise de Nyx).
	args := []string{"--json", "--all", "--nocheck=standby"}
	if typ != "" {
		args = append(args, "-d", typ)
	}
	args = append(args, disque)
	return p.lancer(cheminSmartctl, args...)
}

// tailleMaxSortie : au-delà, la sortie est refusée (protection mémoire).
const tailleMaxSortie = 4 << 20

// lancer exécute un programme figé, avec un environnement minimal et un délai.
//
// Défense en profondeur : le programme doit appartenir à root et ne pas être
// modifiable par un autre utilisateur, sinon on refuse de le lancer en root.
func (p programmesSysteme) lancer(chemin string, args ...string) ([]byte, error) {
	if err := verifierProgramme(chemin); err != nil {
		return nil, err
	}
	ctx, annuler := context.WithTimeout(context.Background(), p.delai)
	defer annuler()
	cmd := exec.CommandContext(ctx, chemin, args...) // #nosec G204 -- chemins et arguments figés dans ce fichier
	cmd.Env = []string{"LANG=C", "LC_ALL=C", "PATH=/usr/sbin:/usr/bin:/sbin:/bin"}
	// Sortie bornée PENDANT la lecture (revue du 28/11) : avant, tout était
	// gardé en mémoire puis refusé à la fin, jusqu'à une minute de sortie.
	sortie := &ecrivainBorne{max: tailleMaxSortie, depasse: annuler}
	cmd.Stdout = sortie
	err := cmd.Run()
	if sortie.trop {
		return nil, fmt.Errorf("%s : sortie trop longue", chemin)
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && sortie.Len() > 0 {
		// smartctl renvoie un code non nul pour signaler un état (bits
		// d'avertissement) tout en donnant un JSON valide : on garde la sortie.
		return sortie.Bytes(), nil
	}
	return sortie.Bytes(), err
}

// errSortieTropLongue : renvoyée par ecrivainBorne au-delà de max.
var errSortieTropLongue = errors.New("sortie trop longue")

// ecrivainBorne garde au plus max octets. Au-delà, il refuse l'écriture
// (le programme reçoit EPIPE à sa prochaine écriture) et appelle depasse
// (arrêt du programme) : rien n'est lu ni gardé au-delà de max.
//
// Le tampon est un champ nommé, PAS intégré : intégré, bytes.Buffer
// apporterait sa méthode ReadFrom, qu'io.Copy (os/exec) appellerait à la
// place de Write, sans aucune limite.
type ecrivainBorne struct {
	tampon  bytes.Buffer
	max     int
	trop    bool
	depasse func()
}

func (e *ecrivainBorne) Write(p []byte) (int, error) {
	if e.trop || e.tampon.Len()+len(p) > e.max {
		if !e.trop && e.depasse != nil {
			e.depasse()
		}
		e.trop = true
		return 0, errSortieTropLongue
	}
	return e.tampon.Write(p)
}

// Len et Bytes : contenu gardé.
func (e *ecrivainBorne) Len() int      { return e.tampon.Len() }
func (e *ecrivainBorne) Bytes() []byte { return e.tampon.Bytes() }

// verifierProgramme refuse un programme absent, n'appartenant pas à root, ou
// modifiable par le groupe ou par tous.
func verifierProgramme(chemin string) error {
	info, err := os.Stat(chemin)
	if err != nil {
		return errAbsent
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != 0 {
		return fmt.Errorf("%s n'appartient pas à root : refusé", chemin)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s est modifiable par d'autres que root : refusé", chemin)
	}
	return nil
}
