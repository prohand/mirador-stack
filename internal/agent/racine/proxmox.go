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
