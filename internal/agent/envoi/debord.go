// SPDX-License-Identifier: GPL-3.0-or-later

package envoi

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Debord : tampon sur disque des lots qui ne tiennent plus en mémoire
// (SPEC § 100, demande du 20/11). Pendant une coupure réseau, la file en
// mémoire se remplit ; au lieu de jeter ses plus anciens lots, elle les
// écrit ici, dans la limite de Max octets (au-delà, les plus anciens du
// disque sont jetés). Au retour du réseau, ils partent en premier : l'ordre
// est gardé. Ils survivent à un redémarrage de l'agent.
//
// Coût : aucune écriture tant que le réseau marche ; pendant une coupure,
// un fichier par lot débordé (quelques dizaines de Ko), Max au plus.
type Debord struct {
	Dossier string
	Max     int64

	fichiers []fichierLot // du plus ancien au plus récent
	taille   int64
	suivant  uint64
}

type fichierLot struct {
	num    uint64
	taille int64
}

const extLot = ".lot"

// OuvrirDebord : dossier créé au besoin (0700) ; les lots d'avant un
// redémarrage sont repris. Un fichier inattendu est ignoré (jamais lu).
func OuvrirDebord(dossier string, quota int64, maxLot int) (*Debord, error) {
	if dossier == "" || quota <= 0 {
		return nil, errors.New("tampon disque désactivé")
	}
	if err := os.MkdirAll(dossier, 0o700); err != nil {
		return nil, err
	}
	d := &Debord{Dossier: dossier, Max: quota}
	entrees, err := os.ReadDir(dossier)
	if err != nil {
		return nil, err
	}
	for _, e := range entrees {
		nom, ok := strings.CutSuffix(e.Name(), extLot)
		if !ok || !e.Type().IsRegular() {
			continue
		}
		n, err := strconv.ParseUint(nom, 10, 64)
		if err != nil {
			continue
		}
		info, err := e.Info()
		if err != nil || info.Size() == 0 || info.Size() > int64(maxLot) {
			_ = os.Remove(filepath.Join(dossier, e.Name())) // lot abîmé ou trop gros : jeté
			continue
		}
		d.fichiers = append(d.fichiers, fichierLot{num: n, taille: info.Size()})
		d.taille += info.Size()
		d.suivant = max(d.suivant, n+1)
	}
	sort.Slice(d.fichiers, func(i, j int) bool { return d.fichiers[i].num < d.fichiers[j].num })
	return d, nil
}

func (d *Debord) chemin(n uint64) string {
	return filepath.Join(d.Dossier, fmt.Sprintf("%020d%s", n, extLot))
}

// ecrire : un lot de plus sur disque. Renvoie le nombre de lots jetés pour
// tenir dans Max (les plus anciens du disque).
func (d *Debord) ecrire(lot []byte) (jetes int, err error) {
	if int64(len(lot)) > d.Max {
		return 1, nil
	}
	for d.taille+int64(len(lot)) > d.Max && len(d.fichiers) > 0 {
		d.retirerPremier()
		jetes++
	}
	n := d.suivant
	d.suivant++
	tmp := d.chemin(n) + ".tmp"
	if err := os.WriteFile(tmp, lot, 0o600); err != nil {
		_ = os.Remove(tmp)
		return jetes + 1, err
	}
	if err := os.Rename(tmp, d.chemin(n)); err != nil {
		_ = os.Remove(tmp)
		return jetes + 1, err
	}
	d.fichiers = append(d.fichiers, fichierLot{num: n, taille: int64(len(lot))})
	d.taille += int64(len(lot))
	return jetes, nil
}

// premier : le plus ancien lot du disque (nil : aucun). Un fichier illisible
// est jeté et le suivant est essayé.
func (d *Debord) premier() []byte {
	for len(d.fichiers) > 0 {
		b, err := os.ReadFile(d.chemin(d.fichiers[0].num))
		if err == nil && len(b) > 0 {
			return b
		}
		d.retirerPremier()
	}
	return nil
}

func (d *Debord) retirerPremier() {
	if len(d.fichiers) == 0 {
		return
	}
	f := d.fichiers[0]
	_ = os.Remove(d.chemin(f.num))
	d.taille -= f.taille
	d.fichiers = d.fichiers[1:]
}

// Octets : taille des lots en attente sur disque.
func (d *Debord) Octets() int64 { return d.taille }
