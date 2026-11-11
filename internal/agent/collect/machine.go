// SPDX-License-Identifier: GPL-3.0-or-later

package collect

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"mirador-stack/internal/agent/metrics"
)

// --- uname et heure ----------------------------------------------------------------

// Uname produit node_uname_info (sert à l'alerte AgentMuet) et node_time_seconds.
//
// Les valeurs d'uname ne changent qu'au redémarrage (sauf le nom d'hôte) : elles
// sont relues au plus une fois par heure.
type Uname struct {
	labels     [6]metrics.Label
	lu         time.Time
	maintenant func() time.Time
}

func NewUname() *Uname       { return &Uname{maintenant: time.Now} }
func (u *Uname) Nom() string { return "uname" }
func (u *Uname) Lent() bool  { return false }

func (u *Uname) Collecter(b *metrics.Batch) error {
	now := u.maintenant()
	if now.Sub(u.lu) > time.Hour {
		var ut syscall.Utsname
		if err := syscall.Uname(&ut); err != nil {
			return err
		}
		u.labels = [6]metrics.Label{
			metrics.L("domainname", chaineUts(ut.Domainname[:])),
			metrics.L("machine", chaineUts(ut.Machine[:])),
			metrics.L("nodename", chaineUts(ut.Nodename[:])),
			metrics.L("release", chaineUts(ut.Release[:])),
			metrics.L("sysname", chaineUts(ut.Sysname[:])),
			metrics.L("version", chaineUts(ut.Version[:])),
		}
		u.lu = now
	}
	b.Add("node_uname_info", 1, u.labels[:]...)
	b.Add("node_time_seconds", float64(now.UnixNano())/1e9)
	return nil
}

// chaineUts convertit un champ de Utsname (octets terminés par 0).
func chaineUts[T int8 | uint8](champ []T) string {
	var sb strings.Builder
	for _, c := range champ {
		if c == 0 {
			break
		}
		sb.WriteByte(byte(c))
	}
	return sb.String()
}

// --- Températures (/sys/class/hwmon et /sys/class/thermal) ----------------------------

// sonde : un fichier de température et ses étiquettes, découverts une fois.
type sonde struct {
	chemin   string
	labels   [2]metrics.Label
	diviseur float64 // hwmon : millidegrés
	nom      string
}

// Temperatures produit node_hwmon_temp_celsius{chip,sensor} et
// node_thermal_zone_temp{type,zone}. Rythme lent (60 s par défaut).
//
// La liste des sondes est construite au démarrage puis toutes les 10 minutes
// (lire les dossiers à chaque cycle coûterait des allocations inutiles).
type Temperatures struct {
	sys       string
	lect      lecteur
	sondes    []sonde
	decouvert time.Time
}

func NewTemperatures(sys string) *Temperatures { return &Temperatures{sys: sys} }
func (t *Temperatures) Nom() string            { return "temperatures" }
func (t *Temperatures) Lent() bool             { return true }

func (t *Temperatures) Collecter(b *metrics.Batch) error {
	if time.Since(t.decouvert) > 10*time.Minute {
		t.sondes = decouvrirSondes(t.sys)
		t.decouvert = time.Now()
	}
	for i := range t.sondes {
		s := &t.sondes[i]
		data, err := t.lect.lire(s.chemin)
		if err != nil {
			continue // capteur momentanément illisible
		}
		v, ok := entierSigne(trim(data))
		if !ok {
			continue
		}
		b.Add(s.nom, float64(v)/s.diviseur, s.labels[:]...)
	}
	return nil
}

var reTemp = regexp.MustCompile(`^temp(\d+)_input$`)

// decouvrirSondes liste les capteurs de température présents.
func decouvrirSondes(sys string) []sonde {
	var sondes []sonde
	dirs, _ := filepath.Glob(filepath.Join(sys, "class", "hwmon", "hwmon*"))
	sort.Strings(dirs)
	for _, dir := range dirs {
		chip := nomPuce(dir)
		fichiers, _ := os.ReadDir(dir)
		for _, f := range fichiers {
			m := reTemp.FindStringSubmatch(f.Name())
			if m == nil {
				continue
			}
			sondes = append(sondes, sonde{
				chemin:   filepath.Join(dir, f.Name()),
				labels:   [2]metrics.Label{metrics.L("chip", chip), metrics.L("sensor", "temp"+m[1])},
				diviseur: 1000,
				nom:      "node_hwmon_temp_celsius",
			})
		}
	}
	zones, _ := filepath.Glob(filepath.Join(sys, "class", "thermal", "thermal_zone*"))
	sort.Strings(zones)
	for _, z := range zones {
		typ, err := os.ReadFile(filepath.Join(z, "type")) // #nosec G304 -- chemin sous /sys
		if err != nil {
			continue
		}
		num := strings.TrimPrefix(filepath.Base(z), "thermal_zone")
		sondes = append(sondes, sonde{
			chemin:   filepath.Join(z, "temp"),
			labels:   [2]metrics.Label{metrics.L("type", strings.TrimSpace(string(typ))), metrics.L("zone", num)},
			diviseur: 1000,
			nom:      "node_thermal_zone_temp",
		})
	}
	return sondes
}

// nomPuce construit l'étiquette « chip » comme node_exporter : d'après le
// chemin du périphérique (stable), sinon le fichier « name », sinon le nom du
// dossier hwmonN.
func nomPuce(dir string) string {
	if dev, err := filepath.EvalSymlinks(filepath.Join(dir, "device")); err == nil {
		nom := nettoyer(filepath.Base(dev))
		typ := nettoyer(filepath.Base(filepath.Dir(dev)))
		if typ != "" && nom != "" {
			return typ + "_" + nom
		}
		if nom != "" {
			return nom
		}
	}
	if brut, err := os.ReadFile(filepath.Join(dir, "name")); err == nil { // #nosec G304 -- chemin sous /sys
		if n := nettoyer(string(brut)); n != "" {
			return n
		}
	}
	if reel, err := filepath.EvalSymlinks(dir); err == nil {
		return nettoyer(filepath.Base(reel))
	}
	return nettoyer(filepath.Base(dir))
}

var reInvalide = regexp.MustCompile(`[^a-z0-9:_]`)

// nettoyer : minuscules, caractères non autorisés remplacés par « _ ».
func nettoyer(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	return strings.Trim(reInvalide.ReplaceAllString(s, "_"), "_")
}

// entierSigne lit un entier éventuellement négatif (températures sous zéro).
func entierSigne(b []byte) (int64, bool) {
	neg := len(b) > 0 && b[0] == '-'
	if neg {
		b = b[1:]
	}
	v, ok := entier(b)
	if !ok || v > 1<<62 {
		return 0, false
	}
	if neg {
		return -int64(v), true
	}
	return int64(v), true
}

// trim enlève les espaces et retours à la ligne en fin de contenu.
func trim(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == ' ') {
		b = b[:len(b)-1]
	}
	return b
}
