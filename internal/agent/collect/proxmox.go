// SPDX-License-Identifier: GPL-3.0-or-later

package collect

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"mirador-stack/internal/agent/metrics"
	"mirador-stack/internal/agent/racine"
)

// Proxmox lit la consommation des invités (VM et LXC) d'un nœud Proxmox
// directement dans les cgroups du noyau, sans API, sans pvesh et sans root
// (SPEC.md § 23.7, étape 6 partie 1). Il remplace le « metric server » de
// Proxmox (flux InfluxDB) :
//
//	mirador_pve_invite_en_marche{id,type}                  1 (invités arrêtés : absents)
//	mirador_pve_invite_cpu_secondes_total{id,type}         cpu.stat usage_usec
//	mirador_pve_invite_memoire_octets{id,type}             memory.current - inactive_file
//	mirador_pve_invite_disque_lus_octets_total{id,type}    io.stat rbytes (tous disques)
//	mirador_pve_invite_disque_ecrits_octets_total{id,type} io.stat wbytes
//	mirador_pve_invite_pression_secondes_total{id,type,ressource,niveau}
//	mirador_pve_invite_reseau_recus_octets_total{id,type}   vu de l'invité
//	mirador_pve_invite_reseau_envoyes_octets_total{id,type} vu de l'invité
//	mirador_pve_vms_disque_lus_octets_total, _ecrits_  total de toutes les VM
//
// Nom, mémoire et cœurs configurés : lus par l'assistant root dans /etc/pve
// (réservé à root), envoyés par le collecteur Racine (mirador_pve_invite_info).
//
// Chemins (cgroups v2, Proxmox 7 et plus) :
//
//	VM  : <sys>/fs/cgroup/qemu.slice/<id>.scope/
//	LXC : <sys>/fs/cgroup/lxc/<id>/
//	réseau : <sys>/class/net/tap<id>i<n> (VM), veth<id>i<n> (LXC)
//
// Sur une machine sans Proxmox, les deux dossiers n'existent pas : le
// collecteur n'est pas lancé (ProxmoxPresent).
type Proxmox struct {
	cgroup, net string
	lect        lecteur
	// Chemins déjà calculés (un invité ou une interface ne change pas de
	// fichiers) : pas d'allocation à chaque cycle. Vidés s'ils grossissent.
	fichiers   map[string]*fichiersInvite
	interfaces map[string]*fichiersInterface
	// Volumes (disques) des VM : id → fichiers « stat » de /sys/block,
	// relistés toutes les cyclesVolumes (un disque ajouté est vu en ~5 min).
	vols   map[string][]string
	cycles int
	// DisquesRacine : disques des VM mesurés par l'assistant root
	// (/proc/<pid>/io), pour les VM sans io.stat ni volume (disque en
	// fichier qcow2). Mis à jour au rythme réglé (1 à 15 min).
	DisquesRacine func() map[string]racine.DisqueVM
	dev           string // racine de /dev (tests)
	block         string // <sys>/block
}

// cyclesVolumes : volumes relistés tous les 10 cycles.
const cyclesVolumes = 10

// fichiersInvite : fichiers lus pour un invité.
type fichiersInvite struct {
	id, typ                   string
	cpu, memoire, memStat, io string
	pression                  [len(sourcesPressionInvite)]string
	labels                    [2]metrics.Label
}

// fichiersInterface : compteurs d'une interface tap/veth.
type fichiersInterface struct {
	id, tx, rx string
}

// maxCache : au-delà, les caches sont vidés (invités créés et supprimés sans fin).
const maxCache = 4096

// NewProxmox crée le collecteur (sys : racine de /sys, remplacée dans les tests).
func NewProxmox(sys string) *Proxmox {
	return &Proxmox{cgroup: filepath.Join(sys, "fs", "cgroup"), net: filepath.Join(sys, "class", "net"),
		block: filepath.Join(sys, "block"), dev: "/dev",
		fichiers: map[string]*fichiersInvite{}, interfaces: map[string]*fichiersInterface{}}
}

// ProxmoxPresent : vrai sur un nœud Proxmox (dossiers des invités dans les
// cgroups). Vérifié au démarrage de l'agent.
func ProxmoxPresent(sys string) bool {
	for _, d := range []string{"qemu.slice", "lxc"} {
		if info, err := os.Stat(filepath.Join(sys, "fs", "cgroup", d)); err == nil && info.IsDir() {
			return true
		}
	}
	return false
}

func (p *Proxmox) Nom() string { return "proxmox" }
func (p *Proxmox) Lent() bool  { return false }

// reInterfaceInvite : tap<id>i<n> (VM) ou veth<id>i<n> (LXC).
var reInterfaceInvite = regexp.MustCompile(`^(tap|veth)([1-9][0-9]{2,8})i[0-9]{1,3}$`)

// invite : fichiers d'un invité (calculés une seule fois).
func (p *Proxmox) invite(parent, nom, id, typ string) *fichiersInvite {
	cle := typ + "/" + nom
	if f, ok := p.fichiers[cle]; ok {
		return f
	}
	if len(p.fichiers) >= maxCache {
		clear(p.fichiers)
	}
	d := filepath.Join(parent, nom)
	f := &fichiersInvite{id: id, typ: typ, cpu: filepath.Join(d, "cpu.stat"), memoire: filepath.Join(d, "memory.current"),
		memStat: filepath.Join(d, "memory.stat"), io: filepath.Join(d, "io.stat"),
		labels: [2]metrics.Label{metrics.L("id", id), metrics.L("type", typ)}}
	for i, src := range sourcesPressionInvite {
		f.pression[i] = filepath.Join(d, src[0]+".pressure")
	}
	p.fichiers[cle] = f
	return f
}

func (p *Proxmox) Collecter(b *metrics.Batch) error {
	var invites []*fichiersInvite
	qemu := filepath.Join(p.cgroup, "qemu.slice")
	if entrees, err := os.ReadDir(qemu); err == nil {
		for _, e := range entrees {
			if id, ok := strings.CutSuffix(e.Name(), ".scope"); ok && e.IsDir() && idInvite(id) {
				invites = append(invites, p.invite(qemu, e.Name(), id, "qemu"))
			}
		}
	}
	lxc := filepath.Join(p.cgroup, "lxc")
	if entrees, err := os.ReadDir(lxc); err == nil {
		for _, e := range entrees {
			if e.IsDir() && idInvite(e.Name()) {
				invites = append(invites, p.invite(lxc, e.Name(), e.Name(), "lxc"))
			}
		}
	}
	if len(invites) == 0 {
		return nil
	}
	// Total de toutes les VM, toutes les 30 s : io.stat de qemu.slice
	// (présent quand la racine active le contrôleur io, cas de Proxmox 9).
	if lus, ecrits, ok := p.io(filepath.Join(qemu, "io.stat")); ok {
		b.Add("mirador_pve_vms_disque_lus_octets_total", float64(lus))
		b.Add("mirador_pve_vms_disque_ecrits_octets_total", float64(ecrits))
	}
	reseau := p.lireReseau()
	var disquesRacine map[string]racine.DisqueVM
	if p.DisquesRacine != nil {
		disquesRacine = p.DisquesRacine()
	}
	if p.cycles%cyclesVolumes == 0 {
		p.listerVolumes()
	}
	p.cycles++
	for _, inv := range invites {
		l := inv.labels[:]
		b.Add("mirador_pve_invite_en_marche", 1, l...)
		if v, ok := p.valeur(inv.cpu, "usage_usec "); ok {
			b.Add("mirador_pve_invite_cpu_secondes_total", float64(v)/1e6, l...)
		}
		if cur, ok := p.nombre(inv.memoire); ok {
			// Comme `docker stats` : le cache de fichiers inactif ne compte pas.
			if inactif, ok := p.valeur(inv.memStat, "inactive_file "); ok && inactif < cur {
				cur -= inactif
			}
			b.Add("mirador_pve_invite_memoire_octets", float64(cur), l...)
		}
		// Disques : io.stat du cgroup s'il existe (LXC) ; sinon (VM : Proxmox
		// n'active pas le contrôleur io pour qemu.slice) les volumes ZFS ou
		// LVM de l'invité, vus comme des disques du nœud.
		lus, ecrits, ok := p.io(inv.io)
		if !ok {
			lus, ecrits, ok = p.volumes(inv.id)
		}
		if !ok && inv.typ == "qemu" && disquesRacine != nil {
			var d racine.DisqueVM
			if d, ok = disquesRacine[inv.id]; ok {
				lus, ecrits = d.Lus, d.Ecrits
			}
		}
		if ok {
			b.Add("mirador_pve_invite_disque_lus_octets_total", float64(lus), l...)
			b.Add("mirador_pve_invite_disque_ecrits_octets_total", float64(ecrits), l...)
		}
		for i, src := range sourcesPressionInvite {
			if v, ok := p.pression(inv.pression[i], src[1]); ok {
				b.Add("mirador_pve_invite_pression_secondes_total", v, l[0], l[1],
					metrics.L("ressource", src[0]), metrics.L("niveau", src[2]))
			}
		}
		if r, ok := reseau[inv.id]; ok {
			b.Add("mirador_pve_invite_reseau_recus_octets_total", float64(r[0]), l...)
			b.Add("mirador_pve_invite_reseau_envoyes_octets_total", float64(r[1]), l...)
		}
	}
	return nil
}

// idInvite : identifiant Proxmox (100 à 999999999), sans expression régulière.
func idInvite(s string) bool {
	return len(s) >= 3 && len(s) <= 9 && s[0] != '0' && chiffres(s)
}

// chiffres : s n'a que des chiffres (au moins un).
func chiffres(s string) bool {
	if s == "" || len(s) > 12 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// Fichier de pression du cgroup et ligne (mêmes cas que Pression, pour le nœud).
var sourcesPressionInvite = [5][3]string{
	{"cpu", "some ", "some"}, {"io", "some ", "some"}, {"io", "full ", "full"},
	{"memory", "some ", "some"}, {"memory", "full ", "full"},
}

// lireReseau : octets reçus et envoyés par invité, VUS DE L'INVITÉ (ce que
// l'hôte envoie sur tap/veth, l'invité le reçoit), toutes interfaces.
func (p *Proxmox) lireReseau() map[string][2]uint64 {
	entrees, err := os.ReadDir(p.net)
	if err != nil {
		return nil
	}
	out := map[string][2]uint64{}
	for _, e := range entrees {
		f := p.interfaceInvite(e.Name())
		if f == nil {
			continue
		}
		envoyeParHote, ok1 := p.nombre(f.tx)
		recuParHote, ok2 := p.nombre(f.rx)
		if !ok1 || !ok2 {
			continue
		}
		v := out[f.id]
		v[0] += envoyeParHote
		v[1] += recuParHote
		out[f.id] = v
	}
	return out
}

// interfaceInvite : fichiers d'une interface tap<id>i<n> ou veth<id>i<n>
// (nil pour les autres interfaces).
func (p *Proxmox) interfaceInvite(nom string) *fichiersInterface {
	if f, ok := p.interfaces[nom]; ok {
		return f
	}
	m := reInterfaceInvite.FindStringSubmatch(nom)
	var f *fichiersInterface
	if m != nil {
		stats := filepath.Join(p.net, nom, "statistics")
		f = &fichiersInterface{id: m[2], tx: filepath.Join(stats, "tx_bytes"), rx: filepath.Join(stats, "rx_bytes")}
	}
	if len(p.interfaces) >= maxCache {
		clear(p.interfaces)
	}
	p.interfaces[nom] = f // nil gardé aussi : l'expression n'est essayée qu'une fois
	return f
}

// nombre : fichier contenant un seul entier (memory.current, statistics/*).
func (p *Proxmox) nombre(chemin string) (uint64, bool) {
	data, err := p.lect.lire(chemin)
	if err != nil {
		return 0, false
	}
	return entier(bytes.TrimSpace(data))
}

// valeur : ligne « <cle> <entier> » (cpu.stat, memory.stat).
func (p *Proxmox) valeur(chemin, cle string) (uint64, bool) {
	data, err := p.lect.lire(chemin)
	if err != nil {
		return 0, false
	}
	for len(data) > 0 {
		var ligne []byte
		ligne, data = ligneSuivante(data)
		if reste := apresPrefixe(ligne, cle); reste != nil {
			return entier(bytes.TrimSpace(reste))
		}
	}
	return 0, false
}

// io : somme de rbytes et wbytes sur toutes les lignes (un disque par ligne :
// « 8:0 rbytes=1 wbytes=2 rios=3 wios=4 dbytes=0 dios=0 »).
func (p *Proxmox) io(chemin string) (lus, ecrits uint64, ok bool) {
	data, err := p.lect.lire(chemin)
	if err != nil {
		return 0, 0, false
	}
	for len(data) > 0 {
		var ligne []byte
		ligne, data = ligneSuivante(data)
		_, reste := champSuivant(ligne) // numéro du disque
		for len(reste) > 0 {
			var champ []byte
			champ, reste = champSuivant(reste)
			if len(champ) == 0 {
				break
			}
			if v := apresPrefixe(champ, "rbytes="); v != nil {
				if n, ok := entier(v); ok {
					lus += n
				}
			} else if v := apresPrefixe(champ, "wbytes="); v != nil {
				if n, ok := entier(v); ok {
					ecrits += n
				}
			}
		}
	}
	return lus, ecrits, true
}

// pression : total (secondes) de la ligne « some » ou « full ».
func (p *Proxmox) pression(chemin, debut string) (float64, bool) {
	data, err := p.lect.lire(chemin)
	if err != nil {
		return 0, false
	}
	for len(data) > 0 {
		var ligne []byte
		ligne, data = ligneSuivante(data)
		reste := apresPrefixe(ligne, debut)
		if reste == nil {
			continue
		}
		if i := bytes.LastIndex(reste, []byte("total=")); i >= 0 {
			if v, ok := entier(bytes.TrimSpace(reste[i+6:])); ok {
				return float64(v) / 1e6, true
			}
		}
	}
	return 0, false
}

// Noms des volumes de VM créés par Proxmox :
//   - ZFS : /dev/zvol/<pool>/[…/]vm-<id>-disk-<n> → ../../zdN
//   - LVM : /sys/block/dm-N/dm/name = « <vg>-vm--<id>--disk--<n> » (LVM
//     double les tirets du nom du volume).
//
// Partitions (« -partN ») et modèles (« base-… ») ignorés.
var (
	reVolumeZFS = regexp.MustCompile(`^vm-([1-9][0-9]{2,8})-disk-[0-9]+$`)
	reVolumeLVM = regexp.MustCompile(`(^|[^-])-vm--([1-9][0-9]{2,8})--disk--[0-9]+$`)
)

// listerVolumes : refait la table id → fichiers stat des volumes.
func (p *Proxmox) listerVolumes() {
	vols := map[string][]string{}
	// ZFS : liens de /dev/zvol (deux niveaux au plus : pool/dataset).
	racine := filepath.Join(p.dev, "zvol")
	_ = filepath.WalkDir(racine, func(chemin string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if strings.Count(strings.TrimPrefix(chemin, racine), "/") > 3 {
				return filepath.SkipDir
			}
			return nil
		}
		m := reVolumeZFS.FindStringSubmatch(d.Name())
		if m == nil || d.Type()&os.ModeSymlink == 0 {
			return nil
		}
		cible, err := os.Readlink(chemin)
		if err != nil {
			return nil
		}
		if nom := filepath.Base(cible); strings.HasPrefix(nom, "zd") && chiffres(strings.TrimPrefix(nom, "zd")) {
			vols[m[1]] = append(vols[m[1]], filepath.Join(p.block, nom, "stat"))
		}
		return nil
	})
	// LVM : nom de chaque dm-N.
	if entrees, err := os.ReadDir(p.block); err == nil {
		for _, e := range entrees {
			if !strings.HasPrefix(e.Name(), "dm-") {
				continue
			}
			brut, err := p.lect.lire(filepath.Join(p.block, e.Name(), "dm", "name"))
			if err != nil {
				continue
			}
			if m := reVolumeLVM.FindStringSubmatch(string(bytes.TrimSpace(brut))); m != nil {
				vols[m[2]] = append(vols[m[2]], filepath.Join(p.block, e.Name(), "stat"))
			}
		}
	}
	p.vols = vols
}

// volumes : octets lus et écrits sur les volumes d'une VM (fichier « stat »
// d'un disque : 3e champ = secteurs lus, 7e = secteurs écrits, 512 octets).
func (p *Proxmox) volumes(id string) (lus, ecrits uint64, ok bool) {
	for _, chemin := range p.vols[id] {
		data, err := p.lect.lire(chemin)
		if err != nil {
			continue
		}
		var champs [7]uint64
		reste := data
		n := 0
		for n < 7 {
			var c []byte
			c, reste = champSuivant(reste)
			v, bon := entier(bytes.TrimSpace(c))
			if !bon {
				break
			}
			champs[n] = v
			n++
		}
		if n == 7 {
			lus += champs[2] * 512
			ecrits += champs[6] * 512
			ok = true
		}
	}
	return lus, ecrits, ok
}
