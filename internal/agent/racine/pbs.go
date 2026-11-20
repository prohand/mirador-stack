// SPDX-License-Identifier: GPL-3.0-or-later

package racine

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Détail des tâches Proxmox Backup Server (SPEC § 95) : pour la dernière
// exécution de chaque tâche suivie (nettoyage, vérification, élagage,
// synchronisation), l'assistant root lit son journal
// (/var/log/proxmox-backup/tasks/<XX>/<UPID>, 256 Kio au plus) et en tire
// quelques chiffres et les lignes d'erreur. Lecture seule ; aucun programme
// lancé. Les formes de lignes viennent des journaux de PBS 2 et 3 ; une
// ligne inconnue est ignorée (rien n'est inventé).

var dossierTachesPBS = "/var/log/proxmox-backup/tasks"

// cheminJournalPBS : PBS range le journal d'une tâche dans le sous-dossier
// « pstart & 255 » en hexadécimal (2 chiffres). Si ce n'est pas là (autre
// version), les 256 sous-dossiers sont essayés.
func cheminJournalPBS(upid string) string {
	if upid == "" || strings.ContainsAny(upid, "/\x00") {
		return ""
	}
	essai := func(d string) string {
		c := filepath.Join(dossierTachesPBS, d, upid)
		if st, err := os.Lstat(c); err == nil && st.Mode().IsRegular() {
			return c
		}
		return ""
	}
	champs := strings.Split(strings.TrimPrefix(upid, "UPID:"), ":")
	if len(champs) >= 3 {
		if p, err := strconv.ParseUint(champs[2], 16, 64); err == nil {
			if c := essai(fmt.Sprintf("%02X", p&255)); c != "" {
				return c
			}
		}
	}
	for i := 0; i < 256; i++ {
		if c := essai(fmt.Sprintf("%02X", i)); c != "" {
			return c
		}
	}
	return ""
}

var (
	// « 2026-11-20T02:00:00+01:00: texte » : l'heure en tête est retirée.
	reHeurePBS = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\S+?: `)
	// Nettoyage (garbage collection) : « Removed garbage: 1.234 GiB ».
	reGCPBS = regexp.MustCompile(`^(Removed garbage|Pending removals|Original data usage|On-Disk usage|Deduplication factor):\s*(.+)$`)
	// Vérification : « verified 1234.56/2345.67 MiB in 12.34 seconds, … (0 errors) ».
	reVerifiePBS = regexp.MustCompile(`^verified .*\((\d+) errors?\)`)
	// Élagage : « remove vm/100/2026-… », « keep vm/100/2026-… ».
	reElaguePBS = regexp.MustCompile(`^(remove|keep)\s+\S+/\S+`)
	// Synchronisation : « sync group vm/100 », « sync snapshot … ».
	reSyncPBS = regexp.MustCompile(`^sync (group|snapshot)\s`)
)

var nomsGCPBS = map[string]string{
	"Removed garbage": "Place libérée", "Pending removals": "En attente de suppression",
	"Original data usage": "Données d'origine", "On-Disk usage": "Place sur le disque",
	"Deduplication factor": "Facteur de déduplication",
}

// analyserJournalPBS : chiffres et erreurs d'une tâche PBS, selon son type.
func analyserJournalPBS(b []byte, typ string) DetailSauvegarde {
	var d DetailSauvegarde
	verifies, erreursVerif, supprimes, gardes, groupes := 0, 0, 0, 0, 0
	listeEchecs := false
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 4096), 64<<10)
	for sc.Scan() {
		brut := sc.Text()
		l := strings.TrimSpace(reHeurePBS.ReplaceAllString(brut, ""))
		if l == "" {
			continue
		}
		// Erreurs : « TASK ERROR: … », « ERROR: … », et la liste des
		// instantanés dont la vérification a échoué.
		if e, ok := strings.CutPrefix(l, "TASK ERROR: "); ok {
			l = "ERROR: " + e
		}
		if e, ok := strings.CutPrefix(l, "ERROR: "); ok && len(d.Erreurs) < maxErreurs {
			d.Erreurs = append(d.Erreurs, raccourcirTexte(e, 160))
			continue
		}
		if strings.HasPrefix(l, "Failed to verify the following snapshots") {
			listeEchecs = true
			continue
		}
		if listeEchecs {
			if strings.HasPrefix(brut, "\t") || strings.HasPrefix(l, "vm/") || strings.HasPrefix(l, "ct/") || strings.HasPrefix(l, "host/") {
				if len(d.Erreurs) < maxErreurs {
					d.Erreurs = append(d.Erreurs, "vérification en échec : "+raccourcirTexte(l, 140))
				}
				continue
			}
			listeEchecs = false
		}
		switch {
		case typ == "garbage_collection":
			if m := reGCPBS.FindStringSubmatch(l); m != nil {
				d.Resume = append(d.Resume, InfoTache{Nom: nomsGCPBS[m[1]], Valeur: raccourcirTexte(m[2], 60)})
			}
		case typ == "verify" || typ == "verificationjob":
			if m := reVerifiePBS.FindStringSubmatch(l); m != nil {
				verifies++
				n, _ := strconv.Atoi(m[1])
				erreursVerif += n
			}
		case typ == "prune" || typ == "prunejob":
			if m := reElaguePBS.FindStringSubmatch(l); m != nil {
				if m[1] == "remove" {
					supprimes++
				} else {
					gardes++
				}
			}
		case typ == "syncjob":
			if reSyncPBS.MatchString(l) {
				groupes++
			}
		}
	}
	switch typ {
	case "verify", "verificationjob":
		if verifies > 0 {
			d.Resume = append(d.Resume, InfoTache{"Archives vérifiées", strconv.Itoa(verifies)},
				InfoTache{"Erreurs de vérification", strconv.Itoa(erreursVerif)})
		}
	case "prune", "prunejob":
		if supprimes+gardes > 0 {
			d.Resume = append(d.Resume, InfoTache{"Sauvegardes supprimées", strconv.Itoa(supprimes)},
				InfoTache{"Sauvegardes gardées", strconv.Itoa(gardes)})
		}
	case "syncjob":
		if groupes > 0 {
			d.Resume = append(d.Resume, InfoTache{"Éléments synchronisés", strconv.Itoa(groupes)})
		}
	}
	if len(d.Resume) > 8 {
		d.Resume = d.Resume[:8]
	}
	return d
}

// completerPBS : détail des tâches PBS de la liste (journal de la dernière exécution).
func completerPBS(taches []TacheProxmox) {
	for i := range taches {
		t := &taches[i]
		if t.Source != "pbs" {
			continue
		}
		typ, _, _ := strings.Cut(t.Nom, " ")
		c := cheminJournalPBS(t.upid)
		if c == "" {
			continue
		}
		b, err := lireDebut(c, journalLuMax)
		if err != nil {
			continue
		}
		if d := analyserJournalPBS(b, typ); len(d.Resume) > 0 || len(d.Erreurs) > 0 {
			t.Detail = &d
		}
	}
}
