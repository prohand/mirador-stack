// SPDX-License-Identifier: GPL-3.0-or-later

package collect

import (
	"bytes"
	"path/filepath"

	"mirador-stack/internal/agent/metrics"
)

// userHZ : unité des temps CPU de /proc/stat (centièmes de seconde sur Linux,
// valeur utilisée aussi par node_exporter).
const userHZ = 100.0

// --- CPU et heure de démarrage (/proc/stat) ----------------------------------

// CPU produit node_cpu_seconds_total{cpu,mode} et node_boot_time_seconds.
type CPU struct {
	chemin string // <proc>/stat, calculé une fois (évite une allocation par cycle)
	lect   lecteur
	intern *metrics.Interner
}

// NewCPU crée le collecteur CPU. proc est la racine de /proc.
func NewCPU(proc string) *CPU {
	return &CPU{chemin: filepath.Join(proc, "stat"), intern: metrics.NewInterner(1024)}
}

func (c *CPU) Nom() string { return "cpu" }
func (c *CPU) Lent() bool  { return false }

// Ordre des colonnes de /proc/stat (les 8 premières).
var modesCPU = [8]string{"user", "nice", "system", "idle", "iowait", "irq", "softirq", "steal"}

func (c *CPU) Collecter(b *metrics.Batch) error {
	data, err := c.lect.lire(c.chemin)
	if err != nil {
		return err
	}
	for len(data) > 0 {
		var ligne []byte
		ligne, data = ligneSuivante(data)
		if reste := apresPrefixe(ligne, "cpu"); reste != nil {
			if len(reste) == 0 || reste[0] == ' ' {
				continue // ligne « cpu » = total, recalculé côté serveur
			}
			num, reste := champSuivant(reste)
			cpu := c.intern.Get(num)
			for i := 0; i < len(modesCPU); i++ {
				var champ []byte
				champ, reste = champSuivant(reste)
				v, ok := entier(champ)
				if !ok {
					break
				}
				b.Add("node_cpu_seconds_total", float64(v)/userHZ,
					metrics.L("cpu", cpu), metrics.L("mode", modesCPU[i]))
			}
		} else if reste := apresPrefixe(ligne, "btime "); reste != nil {
			if v, ok := entier(bytes.TrimSpace(reste)); ok {
				b.Add("node_boot_time_seconds", float64(v))
			}
		}
	}
	return nil
}

// --- Charge (/proc/loadavg) ---------------------------------------------------

// Charge produit node_load1, node_load5, node_load15.
type Charge struct {
	chemin string
	lect   lecteur
}

func NewCharge(proc string) *Charge { return &Charge{chemin: filepath.Join(proc, "loadavg")} }
func (c *Charge) Nom() string       { return "charge" }
func (c *Charge) Lent() bool        { return false }

var nomsCharge = [3]string{"node_load1", "node_load5", "node_load15"}

func (c *Charge) Collecter(b *metrics.Batch) error {
	data, err := c.lect.lire(c.chemin)
	if err != nil {
		return err
	}
	for _, nom := range nomsCharge {
		var champ []byte
		champ, data = champSuivant(data)
		if v, ok := decimal(champ); ok {
			b.Add(nom, v)
		}
	}
	return nil
}

// --- Mémoire (/proc/meminfo) ----------------------------------------------------

// Memoire produit les node_memory_*_bytes utiles (SPEC.md § 4.1).
type Memoire struct {
	chemin string
	lect   lecteur
}

func NewMemoire(proc string) *Memoire { return &Memoire{chemin: filepath.Join(proc, "meminfo")} }
func (m *Memoire) Nom() string        { return "memoire" }
func (m *Memoire) Lent() bool         { return false }

// Clés de /proc/meminfo gardées, et nom de la métrique produite.
var clesMemoire = map[string]string{
	"MemTotal":     "node_memory_MemTotal_bytes",
	"MemAvailable": "node_memory_MemAvailable_bytes",
	"SwapTotal":    "node_memory_SwapTotal_bytes",
	"SwapFree":     "node_memory_SwapFree_bytes",
	"Cached":       "node_memory_Cached_bytes",
	"Buffers":      "node_memory_Buffers_bytes",
}

func (m *Memoire) Collecter(b *metrics.Batch) error {
	data, err := m.lect.lire(m.chemin)
	if err != nil {
		return err
	}
	for len(data) > 0 {
		var ligne []byte
		ligne, data = ligneSuivante(data)
		i := bytes.IndexByte(ligne, ':')
		if i < 0 {
			continue
		}
		nom, ok := clesMemoire[string(ligne[:i])] // pas d'allocation (index de map)
		if !ok {
			continue
		}
		champ, reste := champSuivant(ligne[i+1:])
		v, ok := entier(champ)
		if !ok {
			continue
		}
		unite, _ := champSuivant(reste)
		if string(unite) == "kB" {
			v *= 1024
		}
		b.Add(nom, float64(v))
	}
	return nil
}

// --- vmstat (/proc/vmstat) -----------------------------------------------------

// VMStat produit node_vmstat_pswpin, pswpout et oom_kill (alerte OOM, § 4.6 bis).
type VMStat struct {
	chemin string
	lect   lecteur
}

func NewVMStat(proc string) *VMStat { return &VMStat{chemin: filepath.Join(proc, "vmstat")} }
func (v *VMStat) Nom() string       { return "vmstat" }
func (v *VMStat) Lent() bool        { return false }

var clesVMStat = map[string]string{
	"pswpin":   "node_vmstat_pswpin",
	"pswpout":  "node_vmstat_pswpout",
	"oom_kill": "node_vmstat_oom_kill",
}

func (v *VMStat) Collecter(b *metrics.Batch) error {
	data, err := v.lect.lire(v.chemin)
	if err != nil {
		return err
	}
	for len(data) > 0 {
		var ligne []byte
		ligne, data = ligneSuivante(data)
		cle, reste := champSuivant(ligne)
		nom, ok := clesVMStat[string(cle)]
		if !ok {
			continue
		}
		champ, _ := champSuivant(reste)
		if val, ok := entier(champ); ok {
			b.Add(nom, float64(val))
		}
	}
	return nil
}

// --- Pression (PSI, /proc/pressure) --------------------------------------------

// Pression produit les node_pressure_*_seconds_total (noyaux ≥ 4.20 avec PSI).
type Pression struct {
	chemins [len(sourcesPression)]string // <proc>/pressure/<fichier>
	lect    lecteur
	absente bool // PSI désactivé : on ne réessaie plus
}

func NewPression(proc string) *Pression {
	p := &Pression{}
	for i, src := range sourcesPression {
		p.chemins[i] = filepath.Join(proc, "pressure", src[0])
	}
	return p
}
func (p *Pression) Nom() string { return "pression" }
func (p *Pression) Lent() bool  { return false }

// Fichier, début de ligne et métrique produite (mêmes noms que node_exporter).
var sourcesPression = [5][3]string{
	{"cpu", "some ", "node_pressure_cpu_waiting_seconds_total"},
	{"io", "some ", "node_pressure_io_waiting_seconds_total"},
	{"io", "full ", "node_pressure_io_stalled_seconds_total"},
	{"memory", "some ", "node_pressure_memory_waiting_seconds_total"},
	{"memory", "full ", "node_pressure_memory_stalled_seconds_total"},
}

func (p *Pression) Collecter(b *metrics.Batch) error {
	if p.absente {
		return nil
	}
	for i, src := range sourcesPression {
		data, err := p.lect.lire(p.chemins[i])
		if err != nil {
			if src[0] == "cpu" {
				p.absente = true // pas de PSI sur ce noyau
				return nil
			}
			continue
		}
		for len(data) > 0 {
			var ligne []byte
			ligne, data = ligneSuivante(data)
			reste := apresPrefixe(ligne, src[1])
			if reste == nil {
				continue
			}
			// Dernier champ : total=<microsecondes>
			if i := bytes.LastIndex(reste, []byte("total=")); i >= 0 {
				if v, ok := entier(bytes.TrimSpace(reste[i+6:])); ok {
					b.Add(src[2], float64(v)/1e6)
				}
			}
		}
	}
	return nil
}
