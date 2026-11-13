// SPDX-License-Identifier: GPL-3.0-or-later

package racine

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Liste des invités d'un nœud Proxmox (SPEC.md § 23.7, étape 6 partie 1).
//
// /etc/pve est réservé à root (groupe www-data) : l'agent ne peut pas le
// lire. L'assistant y lit, en lecture seule et sur des chemins FIGÉS :
//   - /etc/pve/.vmlist : tous les invités du cluster et leur nœud ;
//   - /etc/pve/local/qemu-server/<id>.conf, /etc/pve/local/lxc/<id>.conf :
//     nom, mémoire et cœurs configurés (rien d'autre n'est gardé : pas de
//     mot de passe, pas de clé SSH, pas de ligne « args »).
// La consommation (CPU, mémoire, disques, réseau) est lue par l'agent
// lui-même dans les cgroups (collect.Proxmox).

// dossierPVE : remplacé dans les tests.
var dossierPVE = "/etc/pve"

// Invite : un invité de CE nœud (en marche ou arrêté).
type Invite struct {
	ID      string `json:"id"`
	Type    string `json:"type"` // qemu ou lxc
	Nom     string `json:"nom,omitempty"`
	Memoire int64  `json:"memoire_mo,omitempty"` // mémoire configurée (Mio)
	Coeurs  int    `json:"coeurs,omitempty"`
}

// Proxmox : résultat de la lecture.
type Proxmox struct {
	Noeud   string   `json:"noeud"`
	Invites []Invite `json:"invites"`
}

// Bornes : un cluster normal en est très loin.
const (
	maxInvitesPVE    = 2000
	tailleMaxVMList  = 1 << 20
	tailleMaxConfPVE = 64 << 10
)

var (
	reNoeudPVE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]{0,62}$`)
	reNomPVE   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)
)

// lireProxmox : nil, nil sur une machine sans Proxmox.
func lireProxmox() (*Proxmox, error) {
	brut, err := lireBorne(filepath.Join(dossierPVE, ".vmlist"), tailleMaxVMList)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	// Nom du nœud : cible du lien /etc/pve/local (« nodes/<nom> »).
	lien, err := os.Readlink(filepath.Join(dossierPVE, "local"))
	if err != nil {
		return nil, fmt.Errorf("nœud local : %w", err)
	}
	noeud := filepath.Base(lien)
	if !reNoeudPVE.MatchString(noeud) {
		return nil, fmt.Errorf("nom de nœud inattendu %q", noeud)
	}
	var liste struct {
		IDs map[string]struct {
			Node string `json:"node"`
			Type string `json:"type"`
		} `json:"ids"`
	}
	if err := json.Unmarshal(brut, &liste); err != nil {
		return nil, fmt.Errorf(".vmlist illisible : %w", err)
	}
	p := &Proxmox{Noeud: noeud, Invites: []Invite{}}
	for id, v := range liste.IDs {
		if v.Node != noeud || !reIDPVE.MatchString(id) || (v.Type != "qemu" && v.Type != "lxc") {
			continue
		}
		if len(p.Invites) >= maxInvitesPVE {
			return p, errors.New("trop d'invités : liste tronquée")
		}
		inv := Invite{ID: id, Type: v.Type}
		dossier := "qemu-server"
		if v.Type == "lxc" {
			dossier = "lxc"
		}
		// Fichier de configuration illisible : l'invité reste listé, sans nom.
		_ = lireConfPVE(filepath.Join(dossierPVE, "local", dossier, id+".conf"), &inv)
		p.Invites = append(p.Invites, inv)
	}
	sort.Slice(p.Invites, func(i, j int) bool { return p.Invites[i].ID < p.Invites[j].ID })
	return p, nil
}

var reIDPVE = regexp.MustCompile(`^[1-9][0-9]{2,8}$`)

// lireConfPVE : seulement les clés utiles de la configuration courante
// (tout ce qui suit le premier « [instantané] » est ignoré).
func lireConfPVE(chemin string, inv *Invite) error {
	brut, err := lireBorne(chemin, tailleMaxConfPVE)
	if err != nil {
		return err
	}
	sockets := 1
	sc := bufio.NewScanner(bytes.NewReader(brut))
	for sc.Scan() {
		ligne := sc.Text()
		if strings.HasPrefix(ligne, "[") {
			break // instantanés
		}
		cle, val, ok := strings.Cut(ligne, ":")
		if !ok {
			continue
		}
		val = strings.TrimSpace(val)
		switch cle {
		case "name", "hostname":
			if reNomPVE.MatchString(val) {
				inv.Nom = val
			}
		case "memory":
			if n, err := strconv.ParseInt(val, 10, 64); err == nil && n > 0 && n < 1<<24 {
				inv.Memoire = n
			}
		case "cores":
			if n, err := strconv.Atoi(val); err == nil && n > 0 && n <= 4096 {
				inv.Coeurs = n
			}
		case "sockets":
			if n, err := strconv.Atoi(val); err == nil && n > 0 && n <= 64 {
				sockets = n
			}
		}
	}
	if inv.Type == "qemu" {
		if inv.Coeurs == 0 {
			inv.Coeurs = 1 // défaut de Proxmox
		}
		inv.Coeurs *= sockets
	}
	return sc.Err()
}

// lireBorne : fichier ordinaire, sans suivre de lien, taille bornée (la
// taille annoncée des fichiers de pmxcfs n'est pas fiable : on compte les
// octets lus).
func lireBorne(chemin string, max int64) ([]byte, error) {
	b, err := lireSansLien(chemin, max)
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("%s : trop gros", chemin)
	}
	return b, nil
}

// --- Disques des VM (SPEC.md § 23.7) -----------------------------------------
//
// Proxmox n'active pas le contrôleur io pour les VM, et un disque en fichier
// (qcow2) n'a pas de volume à lire : seul /proc/<pid>/io du processus QEMU
// (lisible par root seulement) donne ses lectures et écritures réelles sur
// le stockage. L'assistant le lit à chaque passage ; l'agent le relance au
// rythme réglé dans l'interface (« Disques des VM Proxmox », 1 à 15 min) en
// demandant un passage « disques » (léger : rien d'autre n'est remesuré).

// DisqueVM : octets lus et écrits sur le stockage par le processus de la VM.
type DisqueVM struct {
	Lus    uint64 `json:"lus"`
	Ecrits uint64 `json:"ecrits"`
}

// Remplacés dans les tests.
var (
	dossierCgroup = "/sys/fs/cgroup"
	dossierProc   = "/proc"
)

// lireDisquesVM : nil hors Proxmox (pas de qemu.slice).
func lireDisquesVM() (map[string]DisqueVM, error) {
	slice := filepath.Join(dossierCgroup, "qemu.slice")
	entrees, err := os.ReadDir(slice)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string]DisqueVM{}
	var errs []error
	for _, e := range entrees {
		id, ok := strings.CutSuffix(e.Name(), ".scope")
		if !ok || !e.IsDir() || !reIDPVE.MatchString(id) {
			continue
		}
		brut, err := lireBorne(filepath.Join(slice, e.Name(), "cgroup.procs"), 64<<10)
		if err != nil {
			continue // VM arrêtée entre-temps
		}
		var d DisqueVM
		lu := false
		for _, ligne := range strings.Fields(string(brut)) {
			pid, err := strconv.Atoi(ligne)
			if err != nil || pid <= 0 {
				continue
			}
			l, e, err := lireIOProcessus(pid)
			if err != nil {
				errs = append(errs, fmt.Errorf("VM %s : %w", id, err))
				continue
			}
			d.Lus += l
			d.Ecrits += e
			lu = true
		}
		if lu {
			out[id] = d
		}
		if len(out) >= maxInvitesPVE {
			break
		}
	}
	if len(errs) > 0 && len(out) == 0 {
		return nil, errs[0] // une seule erreur suffit (souvent la même pour toutes)
	}
	return out, nil
}

// lireIOProcessus : read_bytes et write_bytes de /proc/<pid>/io (vraies
// lectures et écritures sur le stockage, tous les fils du processus).
func lireIOProcessus(pid int) (lus, ecrits uint64, err error) {
	brut, err := os.ReadFile(filepath.Join(dossierProc, strconv.Itoa(pid), "io")) // #nosec G304 -- /proc/<pid>/io, pid numérique lu dans le cgroup
	if err != nil {
		return 0, 0, err
	}
	trouve := 0
	for _, ligne := range strings.Split(string(brut), "\n") {
		cle, val, ok := strings.Cut(ligne, ":")
		if !ok {
			continue
		}
		n, err := strconv.ParseUint(strings.TrimSpace(val), 10, 64)
		if err != nil {
			continue
		}
		switch cle {
		case "read_bytes":
			lus, trouve = n, trouve+1
		case "write_bytes":
			ecrits, trouve = n, trouve+1
		}
	}
	if trouve != 2 {
		return 0, 0, errors.New("/proc/<pid>/io incomplet")
	}
	return lus, ecrits, nil
}
