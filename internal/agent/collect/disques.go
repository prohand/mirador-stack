// SPDX-License-Identifier: GPL-3.0-or-later

package collect

import (
	"bytes"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"syscall"
	"time"

	"mirador-stack/internal/agent/metrics"
)

// filtre garde en mémoire, pour chaque nom déjà vu, s'il est exclu ou non :
// l'expression régulière n'est évaluée qu'une fois par nom.
type filtre struct {
	re    *regexp.Regexp
	noms  map[string]bool // noms exacts retirés dans l'interface (SPEC.md § 21)
	vu    map[string]bool // nom → exclu ?
	maxVu int
}

func newFiltre(expr string, noms ...string) *filtre {
	f := &filtre{vu: make(map[string]bool), maxVu: 4096}
	if expr != "" {
		f.re = regexp.MustCompile(expr) // déjà validée par config.Valider
	}
	if len(noms) > 0 {
		f.noms = make(map[string]bool, len(noms))
		for _, n := range noms {
			f.noms[n] = true
		}
	}
	return f
}

// exclu dit si nom doit être ignoré. nom doit être une chaîne internée.
func (f *filtre) exclu(nom string) bool {
	if f.re == nil && f.noms == nil {
		return false
	}
	if e, ok := f.vu[nom]; ok {
		return e
	}
	if len(f.vu) >= f.maxVu {
		clear(f.vu)
	}
	e := f.noms[nom] || (f.re != nil && f.re.MatchString(nom))
	f.vu[nom] = e
	return e
}

// --- Disques (/proc/diskstats) --------------------------------------------------

// Disques produit node_disk_read_bytes_total, node_disk_written_bytes_total et
// node_disk_io_time_seconds_total (étiquette device).
type Disques struct {
	chemin string
	lect   lecteur
	intern *metrics.Interner
	filtre *filtre
}

// NewDisques crée le collecteur ; exclure est l'expression des disques
// ignorés, ignorer la liste des disques retirés un par un.
func NewDisques(proc, exclure string, ignorer ...string) *Disques {
	return &Disques{
		chemin: filepath.Join(proc, "diskstats"),
		intern: metrics.NewInterner(1024),
		filtre: newFiltre(exclure, ignorer...),
	}
}

func (d *Disques) Nom() string { return "disques" }
func (d *Disques) Lent() bool  { return false }

// Taille d'un secteur dans /proc/diskstats : toujours 512 octets.
const tailleSecteur = 512

func (d *Disques) Collecter(b *metrics.Batch) error {
	data, err := d.lect.lire(d.chemin)
	if err != nil {
		return err
	}
	for len(data) > 0 {
		var ligne []byte
		ligne, data = ligneSuivante(data)
		// Colonnes : majeur mineur nom puis les compteurs (doc noyau iostats).
		var champs [14][]byte
		reste := ligne
		n := 0
		for n < len(champs) {
			var c []byte
			c, reste = champSuivant(reste)
			if len(c) == 0 {
				break
			}
			champs[n] = c
			n++
		}
		if n < 14 {
			continue
		}
		nom := d.intern.Get(champs[2])
		if d.filtre.exclu(nom) {
			continue
		}
		secteursLus, ok1 := entier(champs[5])
		secteursEcrits, ok2 := entier(champs[9])
		msIO, ok3 := entier(champs[12])
		if !ok1 || !ok2 || !ok3 {
			continue
		}
		dev := metrics.L("device", nom)
		b.Add("node_disk_read_bytes_total", float64(secteursLus*tailleSecteur), dev)
		b.Add("node_disk_written_bytes_total", float64(secteursEcrits*tailleSecteur), dev)
		b.Add("node_disk_io_time_seconds_total", float64(msIO)/1000, dev)
	}
	return nil
}

// --- Réseau (/proc/net/dev) ---------------------------------------------------------

// Reseau produit node_network_{receive,transmit}_{bytes,errs,drop}_total.
type Reseau struct {
	chemin string
	lect   lecteur
	intern *metrics.Interner
	filtre *filtre
}

// NewReseau crée le collecteur ; exclure est l'expression des interfaces
// ignorées, ignorer la liste des interfaces retirées une par une.
func NewReseau(proc, exclure string, ignorer ...string) *Reseau {
	return &Reseau{
		chemin: filepath.Join(proc, "net", "dev"),
		intern: metrics.NewInterner(1024),
		filtre: newFiltre(exclure, ignorer...),
	}
}

func (r *Reseau) Nom() string { return "reseau" }
func (r *Reseau) Lent() bool  { return false }

// Colonnes utiles de /proc/net/dev (après « interface: ») et métrique produite.
var colonnesReseau = [...]struct {
	col int
	nom string
}{
	{0, "node_network_receive_bytes_total"},
	{2, "node_network_receive_errs_total"},
	{3, "node_network_receive_drop_total"},
	{8, "node_network_transmit_bytes_total"},
	{10, "node_network_transmit_errs_total"},
	{11, "node_network_transmit_drop_total"},
}

func (r *Reseau) Collecter(b *metrics.Batch) error {
	data, err := r.lect.lire(r.chemin)
	if err != nil {
		return err
	}
	for len(data) > 0 {
		var ligne []byte
		ligne, data = ligneSuivante(data)
		i := bytes.IndexByte(ligne, ':')
		if i < 0 {
			continue // les deux lignes d'en-tête
		}
		nom := r.intern.Get(bytes.TrimSpace(ligne[:i]))
		if r.filtre.exclu(nom) {
			continue
		}
		var cols [16]uint64
		reste := ligne[i+1:]
		n := 0
		for n < len(cols) {
			var c []byte
			c, reste = champSuivant(reste)
			v, ok := entier(c)
			if !ok {
				break
			}
			cols[n] = v
			n++
		}
		if n < 16 {
			continue
		}
		dev := metrics.L("device", nom)
		for _, c := range colonnesReseau {
			b.Add(c.nom, float64(cols[c.col]), dev)
		}
	}
	return nil
}

// --- Systèmes de fichiers (/proc/self/mountinfo + statfs) -----------------------------

// statfsFunc permet de remplacer l'appel système dans les tests.
type statfsFunc func(chemin string, st *syscall.Statfs_t) error

// SystemesFichiers produit node_filesystem_{size,avail}_bytes et
// node_filesystem_files{,_free} (étiquettes device, fstype, mountpoint).
type SystemesFichiers struct {
	chemin       string
	lect         lecteur
	intern       *metrics.Interner
	exclMontages *filtre
	exclTypes    *filtre
	typesDistant *filtre // NFS, CIFS… : lus avec un délai maximal
	statfs       statfsFunc
	delaiDistant time.Duration

	mu      sync.Mutex
	bloques map[string]bool // montages distants dont le dernier statfs n'est pas revenu
}

// NewSystemesFichiers crée le collecteur ; ignorer : montages retirés un
// par un dans l'interface (chemins exacts).
func NewSystemesFichiers(proc, exclMontages, exclTypes, typesDistant string, ignorer ...string) *SystemesFichiers {
	return &SystemesFichiers{
		chemin:       filepath.Join(proc, "self", "mountinfo"),
		intern:       metrics.NewInterner(4096),
		exclMontages: newFiltre(exclMontages, ignorer...),
		exclTypes:    newFiltre(exclTypes),
		typesDistant: newFiltre(typesDistant),
		statfs:       syscall.Statfs,
		delaiDistant: 5 * time.Second,
		bloques:      make(map[string]bool),
	}
}

func (s *SystemesFichiers) Nom() string { return "fs" }
func (s *SystemesFichiers) Lent() bool  { return false }

func (s *SystemesFichiers) Collecter(b *metrics.Batch) error {
	data, err := s.lect.lire(s.chemin)
	if err != nil {
		return err
	}
	var st syscall.Statfs_t
	for len(data) > 0 {
		var ligne []byte
		ligne, data = ligneSuivante(data)
		// Format (man proc_pid_mountinfo) :
		// id parent maj:min racine POINT options [champs optionnels] - TYPE SOURCE options
		reste := ligne
		var point []byte
		for i := 0; i < 5; i++ {
			point, reste = champSuivant(reste)
		}
		sep := bytes.Index(reste, []byte(" - "))
		if sep < 0 || len(point) == 0 {
			continue
		}
		typ, apres := champSuivant(reste[sep+3:])
		source, _ := champSuivant(apres)

		fstype := s.intern.Get(typ)
		if s.exclTypes.exclu(fstype) {
			continue
		}
		montage := s.intern.Get(point)
		if bytes.IndexByte(point, '\\') >= 0 {
			// Espaces et caractères spéciaux sont codés en octal (\040) :
			// décodage rare, donc l'allocation est acceptable.
			montage = s.intern.Get([]byte(decoderOctal(montage)))
		}
		if s.exclMontages.exclu(montage) {
			continue
		}
		if s.typesDistant.exclu(fstype) {
			if !s.statfsAvecDelai(montage, &st) {
				continue
			}
		} else if s.statfs(montage, &st) != nil {
			continue // droits insuffisants ou montage disparu entre-temps
		}
		labels := [3]metrics.Label{
			metrics.L("device", s.intern.Get(source)),
			metrics.L("fstype", fstype),
			metrics.L("mountpoint", montage),
		}
		bs := uint64(st.Bsize) // #nosec G115 -- taille de bloc toujours positive
		b.Add("node_filesystem_size_bytes", float64(st.Blocks*bs), labels[:]...)
		b.Add("node_filesystem_avail_bytes", float64(st.Bavail*bs), labels[:]...)
		b.Add("node_filesystem_files", float64(st.Files), labels[:]...)
		b.Add("node_filesystem_files_free", float64(st.Ffree), labels[:]...)
	}
	return nil
}

// statfsAvecDelai appelle statfs dans une goroutine, avec un délai. Un serveur
// NFS mort bloque statfs indéfiniment : on n'attend pas, et on ne relance pas
// d'appel sur ce montage tant que le précédent n'est pas revenu.
func (s *SystemesFichiers) statfsAvecDelai(montage string, st *syscall.Statfs_t) bool {
	s.mu.Lock()
	if s.bloques[montage] {
		s.mu.Unlock()
		return false
	}
	s.bloques[montage] = true
	s.mu.Unlock()

	fini := make(chan error, 1)
	var local syscall.Statfs_t
	go func() {
		err := s.statfs(montage, &local)
		s.mu.Lock()
		delete(s.bloques, montage)
		s.mu.Unlock()
		fini <- err
	}()
	t := time.NewTimer(s.delaiDistant)
	defer t.Stop()
	select {
	case err := <-fini:
		if err != nil {
			return false
		}
		*st = local
		return true
	case <-t.C:
		return false
	}
}

// decoderOctal remplace les séquences \ooo (ex. \040 = espace) de mountinfo.
func decoderOctal(s string) string {
	var out []byte
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				out = append(out, byte(v))
				i += 3
				continue
			}
		}
		out = append(out, s[i])
	}
	return string(out)
}
