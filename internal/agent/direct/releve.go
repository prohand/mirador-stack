// SPDX-License-Identifier: GPL-3.0-or-later

// Package direct : relevé « en direct » de l'agent (SPEC § 97). Une fois par
// seconde, tant que le serveur le demande : CPU, mémoire, réseau, disques et
// les processus les plus gourmands, lus dans /proc et /sys (lecture seule,
// aucun programme lancé, aucun droit root).
package direct

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"mirador-stack/internal/protocole"
)

// Releveur garde les compteurs du relevé précédent (les débits et le CPU
// sont des différences entre deux relevés).
type Releveur struct {
	Proc, Sys, Passwd string // /proc, /sys, /etc/passwd (remplacés dans les tests)

	// SourceProcessus (mirador-collector) : processus relevés par le
	// compagnon sans réseau, qui voit toute la machine (SPEC § 120.1).
	// ok faux : relevé local (seulement les processus visibles d'ici).
	SourceProcessus func(maintenant time.Time) (l []protocole.ProcessusDirect, ok bool)

	avant      time.Time
	cpuTotal   uint64
	cpuRepos   uint64
	netRecu    uint64
	netEnvoye  uint64
	disqueLu   uint64
	disqueEcr  uint64
	ticksProc  map[int]uint64
	physiques  map[string]bool // cartes réseau et disques physiques (relus chaque minute)
	physLu     time.Time
	utilisat   map[uint32]string
	utilisatLu time.Time
	pageOctets float64
	ticksSec   float64
	// tampon : lecture des /proc/<pid>/stat, réutilisée d'un processus à
	// l'autre (relecture perf du 27/11 : os.ReadFile allouait à chaque fois).
	tampon []byte
}

// Nouveau prépare un releveur sur la machine.
func Nouveau() *Releveur {
	return &Releveur{Proc: "/proc", Sys: "/sys", Passwd: "/etc/passwd"}
}

func (r *Releveur) init() {
	if r.ticksProc == nil {
		r.ticksProc = map[int]uint64{}
		r.pageOctets = float64(os.Getpagesize())
		r.ticksSec = 100 // USER_HZ : 100 sur Linux (toutes architectures courantes)
	}
}

// Relever : un relevé. Le premier ne donne que les valeurs instantanées
// (mémoire, charge) : les débits et le CPU demandent deux relevés.
func (r *Releveur) Relever(maintenant time.Time) protocole.Direct {
	r.init()
	d := protocole.Direct{T: maintenant.UnixMilli()}
	ecart := maintenant.Sub(r.avant).Seconds()
	premier := r.avant.IsZero() || ecart <= 0
	if maintenant.Sub(r.physLu) > time.Minute {
		r.physiques, r.physLu = r.lirePhysiques(), maintenant
	}

	// CPU (première ligne de /proc/stat) et nombre de cœurs.
	if b, err := os.ReadFile(filepath.Join(r.Proc, "stat")); err == nil {
		sc := bufio.NewScanner(bytes.NewReader(b))
		for sc.Scan() {
			l := sc.Text()
			if strings.HasPrefix(l, "cpu ") {
				champs := strings.Fields(l)[1:]
				var total, repos uint64
				for i, c := range champs {
					v, _ := strconv.ParseUint(c, 10, 64)
					if i < 8 { // user nice system idle iowait irq softirq steal (guest déjà compté dans user)
						total += v
					}
					if i == 3 || i == 4 {
						repos += v
					}
				}
				if !premier && total > r.cpuTotal {
					// iowait peut reculer (proc(5)) : sans garde, la
					// soustraction non signée débordait et le CPU tombait à 0 %.
					var dRepos uint64
					if repos > r.cpuRepos {
						dRepos = repos - r.cpuRepos
					}
					d.CPU = 100 * (1 - float64(dRepos)/float64(total-r.cpuTotal))
					d.CPU = min(100, max(0, d.CPU))
				}
				r.cpuTotal, r.cpuRepos = total, repos
			} else if strings.HasPrefix(l, "cpu") {
				d.Coeurs++
			}
		}
	}
	if b, err := os.ReadFile(filepath.Join(r.Proc, "loadavg")); err == nil {
		if f := strings.Fields(string(b)); len(f) > 0 {
			d.Charge, _ = strconv.ParseFloat(f[0], 64)
		}
	}
	// Mémoire utilisée = totale − disponible (comme la page de la machine).
	if b, err := os.ReadFile(filepath.Join(r.Proc, "meminfo")); err == nil {
		var total, dispo float64
		sc := bufio.NewScanner(bytes.NewReader(b))
		for sc.Scan() {
			f := strings.Fields(sc.Text())
			if len(f) < 2 {
				continue
			}
			v, _ := strconv.ParseFloat(f[1], 64)
			switch f[0] {
			case "MemTotal:":
				total = v * 1024
			case "MemAvailable:":
				dispo = v * 1024
			}
		}
		d.MemTotale, d.MemUtilisee = total, max(0, total-dispo)
	}
	// Réseau : cartes physiques (sinon toutes, hors lo et interfaces virtuelles).
	if recu, envoye, ok := r.lireReseau(); ok {
		if !premier && recu >= r.netRecu && envoye >= r.netEnvoye {
			d.NetRecu, d.NetEnvoye = float64(recu-r.netRecu)/ecart, float64(envoye-r.netEnvoye)/ecart
		}
		r.netRecu, r.netEnvoye = recu, envoye
	}
	// Disques : disques entiers physiques (ni partitions, ni LVM, ni loop).
	if lu, ecr, ok := r.lireDisques(); ok {
		if !premier && lu >= r.disqueLu && ecr >= r.disqueEcr {
			d.DisqueLu, d.DisqueEcrit = float64(lu-r.disqueLu)/ecart, float64(ecr-r.disqueEcr)/ecart
		}
		r.disqueLu, r.disqueEcr = lu, ecr
	}
	if l, ok := r.sourceExterne(maintenant); ok {
		d.Processus = l
	} else {
		d.Processus = r.lireProcessus(premier, ecart, maintenant)
	}
	r.avant = maintenant
	return d
}

func (r *Releveur) sourceExterne(maintenant time.Time) ([]protocole.ProcessusDirect, bool) {
	if r.SourceProcessus == nil {
		return nil, false
	}
	return r.SourceProcessus(maintenant)
}

// ReleverProcessus : seulement les processus les plus gourmands en CPU
// depuis l'appel précédent (compagnon de mirador-collector).
func (r *Releveur) ReleverProcessus(maintenant time.Time) []protocole.ProcessusDirect {
	r.init()
	ecart := maintenant.Sub(r.avant).Seconds()
	premier := r.avant.IsZero() || ecart <= 0
	l := r.lireProcessus(premier, ecart, maintenant)
	r.avant = maintenant
	return l
}

// virtuelles : interfaces qui ne sortent pas de la machine (ou qui doublent une carte).
var virtuelles = []string{"lo", "docker", "br-", "veth", "virbr", "cni", "flannel", "cali", "vnet", "tap", "fwbr", "fwpr", "fwln", "vmbr", "bond", "tun", "wg"}

// lirePhysiques : cartes réseau et disques qui ont un « device » dans /sys
// (matériel réel ou virtio d'une VM).
func (r *Releveur) lirePhysiques() map[string]bool {
	out := map[string]bool{}
	for _, dossier := range []string{"class/net", "block"} {
		entrees, _ := os.ReadDir(filepath.Join(r.Sys, dossier))
		for _, e := range entrees {
			if _, err := os.Stat(filepath.Join(r.Sys, dossier, e.Name(), "device")); err == nil {
				out[e.Name()] = true
			}
		}
	}
	return out
}

func (r *Releveur) lireReseau() (recu, envoye uint64, ok bool) {
	b, err := os.ReadFile(filepath.Join(r.Proc, "net", "dev"))
	if err != nil {
		return 0, 0, false
	}
	type compteur struct{ rx, tx uint64 }
	var phys, autres compteur
	nbPhys := 0
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		nom, reste, trouve := strings.Cut(sc.Text(), ":")
		if !trouve {
			continue
		}
		nom = strings.TrimSpace(nom)
		f := strings.Fields(reste)
		if len(f) < 9 {
			continue
		}
		rx, _ := strconv.ParseUint(f[0], 10, 64)
		tx, _ := strconv.ParseUint(f[8], 10, 64)
		if r.physiques[nom] {
			phys.rx, phys.tx = phys.rx+rx, phys.tx+tx
			nbPhys++
			continue
		}
		virtuelle := false
		for _, p := range virtuelles {
			virtuelle = virtuelle || strings.HasPrefix(nom, p)
		}
		if !virtuelle {
			autres.rx, autres.tx = autres.rx+rx, autres.tx+tx
		}
	}
	if nbPhys > 0 {
		return phys.rx, phys.tx, true
	}
	return autres.rx, autres.tx, true // conteneur : eth0 sans « device »
}

var reDisqueEntier = regexp.MustCompile(`^(sd[a-z]+|vd[a-z]+|xvd[a-z]+|hd[a-z]+|nvme[0-9]+n[0-9]+|mmcblk[0-9]+)$`)

func (r *Releveur) lireDisques() (lu, ecrit uint64, ok bool) {
	b, err := os.ReadFile(filepath.Join(r.Proc, "diskstats"))
	if err != nil {
		return 0, 0, false
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 10 {
			continue
		}
		nom := f[2]
		if !r.physiques[nom] && !reDisqueEntier.MatchString(nom) {
			continue
		}
		l, _ := strconv.ParseUint(f[5], 10, 64)
		e, _ := strconv.ParseUint(f[9], 10, 64)
		lu, ecrit = lu+l*512, ecrit+e*512
	}
	return lu, ecrit, true
}

// --- Processus ------------------------------------------------------------------------

type proc struct {
	pid   int
	nom   string
	ticks uint64
	rss   float64
	cpu   float64
}

// scanner : tous les processus, avec leur CPU depuis le passage précédent.
func (r *Releveur) scanner(premier bool, ecart float64) []proc {
	entrees, err := os.ReadDir(r.Proc)
	if err != nil {
		return nil
	}
	vus := make(map[int]uint64, len(r.ticksProc)+16)
	liste := make([]proc, 0, 256)
	for _, e := range entrees {
		if c := e.Name()[0]; c < '1' || c > '9' {
			continue // pas un processus (« acpi », « meminfo »…) : évite l'erreur allouée par Atoi
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		b, err := lireDans(filepath.Join(r.Proc, e.Name(), "stat"), &r.tampon)
		if err != nil {
			continue // processus fini entre-temps
		}
		p, ok := analyserStat(b)
		if !ok {
			continue
		}
		p.pid = pid
		p.rss *= r.pageOctets
		vus[pid] = p.ticks
		if a, ok := r.ticksProc[pid]; ok && !premier && p.ticks >= a {
			p.cpu = float64(p.ticks-a) / r.ticksSec / ecart * 100
		}
		liste = append(liste, p)
	}
	r.ticksProc = vus
	return liste
}

// annoter : utilisateur et VM / conteneur des processus retenus.
func (r *Releveur) annoter(liste []proc, maintenant time.Time) []protocole.ProcessusDirect {
	if maintenant.Sub(r.utilisatLu) > 5*time.Minute {
		r.utilisat, r.utilisatLu = lirePasswd(r.Passwd), maintenant
	}
	out := make([]protocole.ProcessusDirect, 0, len(liste))
	for _, p := range liste {
		x := protocole.ProcessusDirect{PID: p.pid, Nom: p.nom, CPU: arrondi(p.cpu), Memoire: p.rss}
		if st, err := os.Stat(filepath.Join(r.Proc, strconv.Itoa(p.pid))); err == nil {
			if s, ok := st.Sys().(*syscall.Stat_t); ok {
				x.Utilisateur = r.utilisat[s.Uid]
				if x.Utilisateur == "" {
					x.Utilisateur = strconv.FormatUint(uint64(s.Uid), 10)
				}
			}
		}
		if b, err := os.ReadFile(filepath.Join(r.Proc, strconv.Itoa(p.pid), "cgroup")); err == nil {
			x.Groupe = groupeCgroup(string(b))
		}
		out = append(out, x)
	}
	return out
}

func parCPU(l []proc) {
	sort.Slice(l, func(i, j int) bool {
		if l[i].cpu != l[j].cpu {
			return l[i].cpu > l[j].cpu
		}
		return l[i].rss > l[j].rss
	})
}

// lireProcessus : les processus les plus gourmands en CPU (puis en mémoire).
func (r *Releveur) lireProcessus(premier bool, ecart float64, maintenant time.Time) []protocole.ProcessusDirect {
	liste := r.scanner(premier, ecart)
	parCPU(liste)
	return r.annoter(liste[:min(len(liste), protocole.DirectMaxProcessus)], maintenant)
}

// Classement : les n processus les plus gourmands en CPU (depuis l'appel
// précédent) et les n plus gourmands en mémoire, en un seul passage dans
// /proc (« cause probable » des alertes, SPEC § 98). Le premier appel ne
// connaît pas encore le CPU.
func (r *Releveur) Classement(maintenant time.Time, n int) (cpu, mem []protocole.ProcessusDirect) {
	r.init()
	ecart := maintenant.Sub(r.avant).Seconds()
	premier := r.avant.IsZero() || ecart <= 0
	liste := r.scanner(premier, ecart)
	r.avant = maintenant
	parCPU(liste)
	if !premier {
		var actifs []proc
		for _, p := range liste[:min(len(liste), n)] {
			if p.cpu >= 0.5 { // un processus endormi n'est pas une cause
				actifs = append(actifs, p)
			}
		}
		cpu = r.annoter(actifs, maintenant)
	}
	sort.Slice(liste, func(i, j int) bool { return liste[i].rss > liste[j].rss })
	mem = r.annoter(liste[:min(len(liste), n)], maintenant)
	return cpu, mem
}

func arrondi(v float64) float64 { return float64(int(v*10+0.5)) / 10 }

// analyserStat : nom, temps CPU (utime + stime, en ticks) et mémoire
// résidente (pages) de /proc/<pid>/stat. Le nom est entre parenthèses et
// peut contenir des espaces ou des parenthèses : on coupe à la dernière.
func analyserStat(b []byte) (proc, bool) {
	var p proc
	i, j := bytes.IndexByte(b, '('), bytes.LastIndexByte(b, ')')
	if i < 0 || j < i {
		return p, false
	}
	// Après « ) » : état(0) … utime(11) stime(12) … rss(21). Champs lus
	// sur place, sans découpage (pas d'allocation).
	var f [22][]byte
	reste := b[j+1:]
	for k := range f {
		reste = bytes.TrimLeft(reste, " \t\n")
		fin := bytes.IndexAny(reste, " \t\n")
		if fin < 0 {
			fin = len(reste)
		}
		if fin == 0 {
			return p, false // moins de 22 champs
		}
		f[k], reste = reste[:fin], reste[fin:]
	}
	u, e1 := strconv.ParseUint(string(f[11]), 10, 64)
	s, e2 := strconv.ParseUint(string(f[12]), 10, 64)
	rss, e3 := strconv.ParseInt(string(f[21]), 10, 64)
	if e1 != nil || e2 != nil || e3 != nil {
		return p, false
	}
	p.nom = string(b[i+1 : j])
	p.ticks, p.rss = u+s, float64(max(0, rss))
	return p, true
}

// lireDans lit un petit fichier de /proc dans *tampon (réutilisé) ; le
// résultat n'est valable que jusqu'au prochain appel. Appels système
// directs, comme pour /proc/self/statm (agent.go) : pas d'os.File à
// allouer ni de fstat en plus, pour chacun des centaines de processus.
func lireDans(chemin string, tampon *[]byte) ([]byte, error) {
	fd, err := syscall.Open(chemin, syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	defer syscall.Close(fd)
	if cap(*tampon) == 0 {
		*tampon = make([]byte, 0, 4<<10)
	}
	b := (*tampon)[:0]
	for {
		if len(b) == cap(b) {
			if cap(b) >= 64<<10 {
				break // borne : /proc/<pid>/stat fait moins de 1 Kio
			}
			b = append(b, 0)[:len(b)]
		}
		n, err := syscall.Read(fd, b[len(b):cap(b)])
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			return nil, err
		}
		if n <= 0 {
			break // fin du fichier
		}
		b = b[:len(b)+n]
	}
	*tampon = b
	return b, nil
}

var (
	reCgroupVM     = regexp.MustCompile(`qemu\.slice/([0-9]+)\.scope`)
	reCgroupLXC    = regexp.MustCompile(`/lxc(?:\.payload)?[./]([0-9]+)`)
	reCgroupDocker = regexp.MustCompile(`docker[-/]([0-9a-f]{12})`)
)

// groupeCgroup : VM, conteneur LXC ou Docker du processus, d'après son cgroup.
func groupeCgroup(c string) string {
	if m := reCgroupVM.FindStringSubmatch(c); m != nil {
		return "VM " + m[1]
	}
	if m := reCgroupLXC.FindStringSubmatch(c); m != nil {
		return "CT " + m[1]
	}
	if m := reCgroupDocker.FindStringSubmatch(c); m != nil {
		return "docker " + m[1]
	}
	return ""
}

// lirePasswd : uid → nom (fichier lisible par tous).
func lirePasswd(chemin string) map[uint32]string {
	out := map[uint32]string{}
	b, err := os.ReadFile(chemin) // #nosec G304 -- chemin figé (/etc/passwd), remplacé seulement dans les tests
	if err != nil {
		return out
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		f := strings.Split(sc.Text(), ":")
		if len(f) < 3 {
			continue
		}
		if uid, err := strconv.ParseUint(f[2], 10, 32); err == nil && len(f[0]) <= 32 {
			out[uint32(uid)] = f[0]
		}
	}
	return out
}
