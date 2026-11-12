// SPDX-License-Identifier: GPL-3.0-or-later

package collect

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"mirador-stack/internal/agent/metrics"
)

// MetriqueDetecte : « ceci existe sur la machine mais n'est pas surveillé ».
// L'interface s'en sert pour proposer de l'activer (SPEC.md § 19). Étiquettes :
// genre (ventilateurs, zfs, mdraid, service) et nom.
const MetriqueDetecte = "mirador_agent_detecte"

// Materiel regroupe trois petits collecteurs, éteints par défaut, lus sans
// commande ni droit particulier (idées reprises de Beszel, SPEC.md § 19) :
//
//	node_hwmon_fan_rpm{chip,sensor}        vitesse des ventilateurs (/sys/class/hwmon/*/fan*_input)
//	node_zfs_zpool_state{zpool,state}      1 pour l'état courant du pool, et toujours state="online" (0 ou 1)
//	node_md_disks_required{device}         disques prévus dans la grappe RAID logiciel
//	node_md_degraded{device}               disques manquants (0 = grappe saine)
//
// Un genre éteint n'envoie qu'une série mirador_agent_detecte par élément
// trouvé : l'interface propose alors de l'activer. Rythme lent ; la liste des
// éléments est relue toutes les 10 minutes.
type Materiel struct {
	sys, proc                 string
	ventilateurs, zfs, mdraid bool
	lect                      lecteur
	decouvert                 time.Time

	fans    []sonde
	pools   []element
	grappes []element
}

// element : un pool ZFS ou une grappe RAID, et le dossier où le lire.
type element struct{ nom, dossier string }

// NewMateriel crée le collecteur ; chaque booléen active l'envoi d'un genre.
func NewMateriel(sys, proc string, ventilateurs, zfs, mdraid bool) *Materiel {
	return &Materiel{sys: sys, proc: proc, ventilateurs: ventilateurs, zfs: zfs, mdraid: mdraid}
}

func (m *Materiel) Nom() string { return "materiel" }
func (m *Materiel) Lent() bool  { return true }

var (
	reFan = regexp.MustCompile(`^fan(\d+)_input$`)
	// Nom d'un pool ZFS ou d'une grappe : caractères sûrs seulement.
	reNomElement = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,63}$`)
)

func (m *Materiel) decouvrir() {
	m.fans = m.fans[:0]
	dirs, _ := filepath.Glob(filepath.Join(m.sys, "class", "hwmon", "hwmon*"))
	sort.Strings(dirs)
	for _, dir := range dirs {
		fichiers, _ := os.ReadDir(dir)
		chip := ""
		for _, f := range fichiers {
			x := reFan.FindStringSubmatch(f.Name())
			if x == nil {
				continue
			}
			if chip == "" {
				chip = nomPuce(dir)
			}
			m.fans = append(m.fans, sonde{chemin: filepath.Join(dir, f.Name()),
				labels: [2]metrics.Label{metrics.L("chip", chip), metrics.L("sensor", "fan"+x[1])}})
		}
	}
	m.pools = lister(filepath.Join(m.proc, "spl", "kstat", "zfs", "*", "state"), 1)
	m.grappes = lister(filepath.Join(m.sys, "block", "md*", "md", "degraded"), 2)
}

// lister : éléments trouvés par un motif ; le nom est le dossier situé
// « remonter » niveaux au-dessus du fichier trouvé.
func lister(motif string, remonter int) []element {
	chemins, _ := filepath.Glob(motif)
	sort.Strings(chemins)
	var out []element
	for _, c := range chemins {
		dossier := filepath.Dir(c)
		nom := dossier
		for i := 1; i < remonter; i++ {
			nom = filepath.Dir(nom)
		}
		if n := filepath.Base(nom); reNomElement.MatchString(n) && len(out) < 64 {
			out = append(out, element{nom: n, dossier: dossier})
		}
	}
	return out
}

func (m *Materiel) Collecter(b *metrics.Batch) error {
	if time.Since(m.decouvert) > 10*time.Minute {
		m.decouvrir()
		m.decouvert = time.Now()
	}
	// Ventilateurs.
	if !m.ventilateurs {
		if len(m.fans) > 0 {
			b.Add(MetriqueDetecte, float64(len(m.fans)), metrics.L("genre", "ventilateurs"), metrics.L("nom", "capteurs"))
		}
	} else {
		for i := range m.fans {
			s := &m.fans[i]
			data, err := m.lect.lire(s.chemin)
			if err != nil {
				continue // capteur momentanément illisible
			}
			if v, ok := entier(trim(data)); ok {
				b.Add("node_hwmon_fan_rpm", float64(v), s.labels[:]...)
			}
		}
	}
	// Pools ZFS : le fichier « state » contient ONLINE, DEGRADED, FAULTED…
	for _, p := range m.pools {
		if !m.zfs {
			b.Add(MetriqueDetecte, 1, metrics.L("genre", "zfs"), metrics.L("nom", p.nom))
			continue
		}
		data, err := m.lect.lire(filepath.Join(p.dossier, "state"))
		if err != nil {
			continue
		}
		etat := strings.ToLower(string(bytes.TrimSpace(data)))
		if !reNomElement.MatchString(etat) {
			continue
		}
		pool := metrics.L("zpool", p.nom)
		b.Add("node_zfs_zpool_state", bool01(etat == "online"), pool, metrics.L("state", "online"))
		if etat != "online" {
			b.Add("node_zfs_zpool_state", 1, pool, metrics.L("state", etat))
		}
	}
	// RAID logiciel : « degraded » = nombre de disques manquants.
	for _, g := range m.grappes {
		if !m.mdraid {
			b.Add(MetriqueDetecte, 1, metrics.L("genre", "mdraid"), metrics.L("nom", g.nom))
			continue
		}
		dev := metrics.L("device", g.nom)
		if data, err := m.lect.lire(filepath.Join(g.dossier, "degraded")); err == nil {
			if v, ok := entier(trim(data)); ok {
				b.Add("node_md_degraded", float64(v), dev)
			}
		}
		if data, err := m.lect.lire(filepath.Join(g.dossier, "raid_disks")); err == nil {
			if v, ok := entier(trim(data)); ok {
				b.Add("node_md_disks_required", float64(v), dev)
			}
		}
	}
	return nil
}
