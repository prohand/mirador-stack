// SPDX-License-Identifier: GPL-3.0-or-later

package racine

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Détail des sauvegardes Proxmox VE (SPEC § 89, retour du 20/11 : « pas
// possible de récupérer d'autres infos utiles ? ») : pour la dernière
// exécution de chaque sauvegarde vzdump, l'assistant root lit
//   - son journal (/var/log/pve/tasks/<x>/<UPID>, 256 Kio au plus) :
//     stockage, chaque VM ou conteneur (OK ou échec, durée, taille,
//     message d'erreur) ;
//   - les travaux planifiés (/etc/pve/jobs.cfg) : planning, stockage,
//     commentaire ; prochaine exécution pour les plannings simples.
// Lecture seule ; aucun programme lancé.

// VMSauvegarde : une VM ou un conteneur dans une sauvegarde.
type VMSauvegarde struct {
	ID      string  `json:"id"`
	Type    string  `json:"type,omitempty"` // qemu ou lxc
	Nom     string  `json:"nom,omitempty"`
	OK      bool    `json:"ok"`
	Duree   int     `json:"duree,omitempty"`  // secondes
	Octets  float64 `json:"octets,omitempty"` // taille écrite ou transférée
	Message string  `json:"message,omitempty"`
}

// DetailSauvegarde : détail de la dernière exécution d'une sauvegarde vzdump.
type DetailSauvegarde struct {
	Stockage    string         `json:"stockage,omitempty"`
	Planning    string         `json:"planning,omitempty"`    // « 02:00 », « sat 03:00 »… (jobs.cfg)
	Commentaire string         `json:"commentaire,omitempty"` // commentaire du travail planifié
	Prochaine   time.Time      `json:"prochaine,omitempty"`   // d'après le planning (formes simples)
	VMs         []VMSauvegarde `json:"vms,omitempty"`
	// Erreurs : lignes « ERROR » du journal (20 au plus, SPEC § 93).
	Erreurs []string `json:"erreurs,omitempty"`
}

// Fichiers lus (variables : remplacées dans les tests).
var (
	dossierTachesPVE = "/var/log/pve/tasks"
	travauxPVE       = "/etc/pve/jobs.cfg"
)

const (
	journalLuMax = 256 << 10 // octets lus d'un journal de sauvegarde
	maxVMs       = 200
	maxErreurs   = 20
)

var (
	reStockage = regexp.MustCompile(`--storage\s+(\S+)`)
	reDebutVM  = regexp.MustCompile(`^INFO: Starting Backup of VM (\d+) \((qemu|lxc)\)`)
	reNomVM    = regexp.MustCompile(`^INFO: (?:VM|CT) Name: (.+)$`)
	reFinVM    = regexp.MustCompile(`^INFO: Finished Backup of VM (\d+) \((\d+):(\d+):(\d+)\)`)
	reEchecVM  = regexp.MustCompile(`^ERROR: Backup of VM (\d+) failed - (.+)$`)
	reTaille   = regexp.MustCompile(`(?:archive file size:|transferred|had to backup)\s+([\d.]+)\s*([KMGT]i?B|B)\b`)
)

// cheminJournal : fichier du journal d'une tâche, d'après son UPID. Proxmox
// range les journaux dans un sous-dossier nommé d'un chiffre hexadécimal
// (celui de la fin de l'heure de début) : les 16 sont essayés.
func cheminJournal(upid string) string {
	if upid == "" || strings.ContainsAny(upid, "/\x00") {
		return ""
	}
	for _, d := range "0123456789ABCDEF" {
		c := filepath.Join(dossierTachesPVE, string(d), upid)
		if st, err := os.Lstat(c); err == nil && st.Mode().IsRegular() {
			return c
		}
	}
	return ""
}

// lireDebut : les n premiers octets d'un fichier.
func lireDebut(chemin string, n int64) ([]byte, error) {
	fd, err := os.Open(chemin) // #nosec G304 -- chemin construit sous /var/log/pve/tasks à partir d'un UPID vérifié
	if err != nil {
		return nil, err
	}
	defer func() { _ = fd.Close() }()
	b := make([]byte, n)
	k, err := fd.Read(b)
	if k > 0 {
		return b[:k], nil
	}
	return nil, err
}

// tailleOctets : « 4.12GB », « 32.00 GiB » → octets.
func tailleOctets(v, unite string) float64 {
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0
	}
	mult := map[string]float64{"B": 1, "KB": 1 << 10, "KiB": 1 << 10, "MB": 1 << 20, "MiB": 1 << 20,
		"GB": 1 << 30, "GiB": 1 << 30, "TB": 1 << 40, "TiB": 1 << 40}[unite]
	return f * mult
}

// analyserJournalSauvegarde : stockage et VM d'un journal vzdump.
func analyserJournalSauvegarde(b []byte) DetailSauvegarde {
	var d DetailSauvegarde
	parID := map[string]int{}
	vm := func(id string) *VMSauvegarde {
		if i, ok := parID[id]; ok {
			return &d.VMs[i]
		}
		if len(d.VMs) >= maxVMs {
			return nil
		}
		d.VMs = append(d.VMs, VMSauvegarde{ID: id})
		parID[id] = len(d.VMs) - 1
		return &d.VMs[len(d.VMs)-1]
	}
	var courante *VMSauvegarde
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 4096), 64<<10)
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		// Les lignes du journal sont précédées de l'heure sur certaines versions.
		if i := strings.Index(l, "INFO: "); i > 0 && i < 30 {
			l = l[i:]
		} else if i := strings.Index(l, "ERROR: "); i > 0 && i < 30 {
			l = l[i:]
		}
		// « ERROR: … » et « TASK ERROR: … » (ramené à « ERROR: » ci-dessus).
		if e, ok := strings.CutPrefix(l, "ERROR: "); ok && len(d.Erreurs) < maxErreurs {
			d.Erreurs = append(d.Erreurs, raccourcirTexte(e, 160))
		}
		switch {
		case d.Stockage == "" && strings.Contains(l, "starting new backup job: vzdump"):
			if m := reStockage.FindStringSubmatch(l); m != nil {
				d.Stockage = raccourcirTexte(m[1], 64)
			}
		case reDebutVM.MatchString(l):
			m := reDebutVM.FindStringSubmatch(l)
			if courante = vm(m[1]); courante != nil {
				courante.Type = m[2]
			}
		case reNomVM.MatchString(l):
			if courante != nil {
				courante.Nom = raccourcirTexte(reNomVM.FindStringSubmatch(l)[1], 64)
			}
		case reFinVM.MatchString(l):
			m := reFinVM.FindStringSubmatch(l)
			if v := vm(m[1]); v != nil {
				h, _ := strconv.Atoi(m[2])
				mi, _ := strconv.Atoi(m[3])
				s, _ := strconv.Atoi(m[4])
				v.OK, v.Duree = true, h*3600+mi*60+s
			}
			courante = nil
		case reEchecVM.MatchString(l):
			m := reEchecVM.FindStringSubmatch(l)
			if v := vm(m[1]); v != nil {
				v.OK, v.Message = false, raccourcirTexte(m[2], 120)
			}
			courante = nil
		default:
			if courante != nil {
				if m := reTaille.FindStringSubmatch(l); m != nil {
					courante.Octets = tailleOctets(m[1], m[2])
				}
			}
		}
	}
	return d
}

func raccourcirTexte(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		s = strings.ToValidUTF8(s[:n], "")
	}
	return s
}

// travailPVE : un travail de sauvegarde planifié (jobs.cfg).
type travailPVE struct {
	ID, Planning, Stockage, Commentaire string
	Actif                               bool
}

// lireTravaux : travaux « vzdump: » de jobs.cfg.
func lireTravaux(b []byte) []travailPVE {
	var out []travailPVE
	var cur *travailPVE
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		l := sc.Text()
		if strings.TrimSpace(l) == "" {
			cur = nil
			continue
		}
		if !strings.HasPrefix(l, " ") && !strings.HasPrefix(l, "\t") {
			cur = nil
			if id, ok := strings.CutPrefix(l, "vzdump:"); ok {
				out = append(out, travailPVE{ID: strings.TrimSpace(id), Actif: true})
				cur = &out[len(out)-1]
			}
			continue
		}
		if cur == nil {
			continue
		}
		cle, val, _ := strings.Cut(strings.TrimSpace(l), " ")
		val = strings.TrimSpace(val)
		switch cle {
		case "schedule":
			cur.Planning = raccourcirTexte(val, 64)
		case "storage":
			cur.Stockage = raccourcirTexte(val, 64)
		case "comment":
			cur.Commentaire = raccourcirTexte(val, 100)
		case "enabled":
			cur.Actif = val != "0"
		}
	}
	return out
}

var joursPVE = map[string]time.Weekday{"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday,
	"thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday}

// prochainePVE : prochaine exécution d'un planning Proxmox simple :
// « HH:MM », « daily », « jours HH:MM » (jours : « sat », « mon,wed »,
// « mon..fri »). Autre forme : zéro (planning seulement affiché).
func prochainePVE(planning string, apres time.Time) time.Time {
	p := strings.TrimSpace(strings.ToLower(planning))
	switch p {
	case "daily":
		p = "00:00"
	case "weekly":
		p = "mon 00:00"
	}
	champs := strings.Fields(p)
	jours := map[time.Weekday]bool{}
	heure := ""
	switch len(champs) {
	case 1:
		heure = champs[0]
		for j := time.Sunday; j <= time.Saturday; j++ {
			jours[j] = true
		}
	case 2:
		heure = champs[1]
		for _, part := range strings.Split(champs[0], ",") {
			if a, z, ok := strings.Cut(part, ".."); ok {
				ja, oka := joursPVE[a]
				jz, okz := joursPVE[z]
				if !oka || !okz {
					return time.Time{}
				}
				for j := ja; ; j = (j + 1) % 7 {
					jours[j] = true
					if j == jz {
						break
					}
				}
			} else if j, ok := joursPVE[part]; ok {
				jours[j] = true
			} else {
				return time.Time{}
			}
		}
	default:
		return time.Time{}
	}
	h, err := time.Parse("15:04", heure)
	if err != nil {
		if h, err = time.Parse("15:4", heure); err != nil {
			return time.Time{}
		}
	}
	loc := apres.Location()
	for k := 0; k <= 7; k++ {
		j := apres.AddDate(0, 0, k)
		c := time.Date(j.Year(), j.Month(), j.Day(), h.Hour(), h.Minute(), 0, 0, loc)
		if jours[c.Weekday()] && c.After(apres) {
			return c
		}
	}
	return time.Time{}
}

// completerSauvegardes : détail des sauvegardes vzdump de la liste (journal
// de la dernière exécution, travail planifié correspondant).
func completerSauvegardes(taches []TacheProxmox, maintenant time.Time) {
	var travaux []travailPVE
	if b, err := lireDebut(travauxPVE, 64<<10); err == nil {
		for _, t := range lireTravaux(b) {
			if t.Actif {
				travaux = append(travaux, t)
			}
		}
	}
	sort.Slice(travaux, func(i, j int) bool { return travaux[i].ID < travaux[j].ID })
	for i := range taches {
		t := &taches[i]
		if t.Source != "pve" || !strings.HasPrefix(t.Nom, "vzdump") {
			continue
		}
		var d DetailSauvegarde
		if c := cheminJournal(t.upid); c != "" {
			if b, err := lireDebut(c, journalLuMax); err == nil {
				d = analyserJournalSauvegarde(b)
			}
		}
		// Travail planifié : « vzdump » sans numéro = sauvegarde planifiée.
		// Celui qui vise le même stockage, sinon le seul ; la prochaine
		// exécution est la plus proche des travaux retenus.
		if t.Nom == "vzdump" && len(travaux) > 0 {
			var retenus []travailPVE
			for _, w := range travaux {
				if d.Stockage != "" && w.Stockage == d.Stockage {
					retenus = append(retenus, w)
				}
			}
			if len(retenus) == 0 && len(travaux) == 1 {
				retenus = travaux
			}
			var plannings, commentaires []string
			for _, w := range retenus {
				if w.Planning != "" {
					plannings = append(plannings, w.Planning)
				}
				if w.Commentaire != "" {
					commentaires = append(commentaires, w.Commentaire)
				}
				if p := prochainePVE(w.Planning, maintenant); !p.IsZero() && (d.Prochaine.IsZero() || p.Before(d.Prochaine)) {
					d.Prochaine = p
				}
				if d.Stockage == "" {
					d.Stockage = w.Stockage
				}
			}
			d.Planning = raccourcirTexte(strings.Join(plannings, " ; "), 100)
			d.Commentaire = raccourcirTexte(strings.Join(commentaires, " ; "), 100)
		}
		if d.Stockage != "" || len(d.VMs) > 0 || d.Planning != "" || len(d.Erreurs) > 0 {
			t.Detail = &d
		}
	}
}
