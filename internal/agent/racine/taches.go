// SPDX-License-Identifier: GPL-3.0-or-later

package racine

import (
	"bufio"
	"bytes"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Tâches Proxmox (SPEC § 80) : dernière exécution des sauvegardes et des
// tâches d'entretien, lues dans le journal des tâches terminées :
//   - Proxmox VE : /var/log/pve/tasks/index (sauvegardes vzdump) ;
//   - Proxmox Backup Server : /var/log/proxmox-backup/tasks/archive
//     (sauvegardes reçues, ramasse-miettes, vérification, nettoyage,
//     synchronisation).
//
// Une ligne : « UPID:… <fin en hexadécimal> <statut> ». Le statut vaut « OK »,
// « WARNINGS: n », ou le message d'erreur. Seule la fin du fichier est lue
// (256 Kio) : la dernière exécution de chaque tâche y est. Aucun programme
// lancé, aucune écriture.

// TacheProxmox : dernière exécution d'une tâche.
type TacheProxmox struct {
	Source  string    `json:"source"` // pve ou pbs
	Nom     string    `json:"nom"`    // « vzdump », « vzdump 101 », « backup store1:vm/100 »…
	Debut   time.Time `json:"debut"`
	Fin     time.Time `json:"fin"`
	OK      bool      `json:"ok"`
	Message string    `json:"message,omitempty"` // statut si ce n'est pas « OK » (120 caractères au plus)
	// Detail : sauvegardes vzdump (SPEC § 89) et tâches PBS (§ 95).
	Detail *DetailSauvegarde `json:"detail,omitempty"`
	upid   string            // pour retrouver le journal de la tâche
}

// Fichiers lus (variables : remplacées dans les tests).
var (
	indexPVE   = "/var/log/pve/tasks/index"
	archivePBS = "/var/log/proxmox-backup/tasks/archive"
)

const (
	finLueTaches = 256 << 10 // octets lus à la fin du fichier
	maxTaches    = 60        // tâches gardées par fichier
)

// typesSuivis : types de tâches retenus, par source.
var typesSuivis = map[string]map[string]bool{
	"pve": {"vzdump": true},
	"pbs": {"backup": true, "garbage_collection": true, "verify": true, "verificationjob": true,
		"prune": true, "prunejob": true, "syncjob": true},
}

// lireTachesProxmox : tâches des deux fichiers (absents hors Proxmox : rien).
func lireTachesProxmox() []TacheProxmox {
	var out []TacheProxmox
	for _, f := range []struct{ chemin, source string }{{indexPVE, "pve"}, {archivePBS, "pbs"}} {
		if b, err := lireFin(f.chemin, finLueTaches); err == nil {
			out = append(out, analyserTaches(b, f.source)...)
		}
	}
	completerSauvegardes(out, time.Now())
	completerPBS(out) // SPEC § 95
	return out
}

// lireFin : les n derniers octets d'un fichier (sans suivre de lien). Le
// dossier appartient à un compte de service exposé au web (www-data,
// backup) : jamais de lien, de tube ni de périphérique (relecture du 21/11).
func lireFin(chemin string, n int64) ([]byte, error) {
	fd, err := ouvrirSur(chemin, false, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = fd.Close() }()
	if st, err := fd.Stat(); err == nil && st.Size() > n {
		if _, err := fd.Seek(st.Size()-n, io.SeekStart); err != nil {
			return nil, err
		}
	}
	return io.ReadAll(io.LimitReader(fd, n))
}

// analyserTaches : dernière exécution de chaque tâche suivie.
func analyserTaches(b []byte, source string) []TacheProxmox {
	dernieres := map[string]TacheProxmox{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 4096), 64<<10)
	for sc.Scan() {
		t, ok := analyserLigneTache(sc.Text(), source)
		if !ok {
			continue
		}
		if d, deja := dernieres[t.Nom]; !deja || t.Debut.After(d.Debut) {
			dernieres[t.Nom] = t
		}
	}
	out := make([]TacheProxmox, 0, len(dernieres))
	for _, t := range dernieres {
		out = append(out, t)
	}
	// Au-delà de maxTaches : les plus récentes sont gardées (relecture du
	// 21/11 : la coupe par ordre alphabétique gardait les « backup … » et
	// faisait disparaître nettoyage, vérification, élagage et synchro).
	if len(out) > maxTaches {
		// D'abord les tâches qui ne sont pas des sauvegardes d'un groupe
		// (nettoyage, vérification, élagage, synchro, vzdump global), puis
		// les plus récentes.
		groupe := func(t TacheProxmox) bool { return strings.HasPrefix(t.Nom, "backup ") }
		sort.Slice(out, func(i, j int) bool {
			if gi, gj := groupe(out[i]), groupe(out[j]); gi != gj {
				return !gi
			}
			return out[i].Debut.After(out[j].Debut)
		})
		out = out[:maxTaches]
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Nom < out[j].Nom })
	return out
}

// analyserLigneTache : « UPID:nœud:pid:pstart[:n°]:début:type:id:utilisateur: fin statut ».
// Proxmox VE a 7 champs dans l'UPID, Proxmox Backup Server 8 (n° de tâche en plus).
func analyserLigneTache(ligne, source string) (TacheProxmox, bool) {
	var t TacheProxmox
	if !strings.HasPrefix(ligne, "UPID:") {
		return t, false
	}
	i := strings.Index(ligne, ": ")
	if i < 0 {
		return t, false
	}
	champs := strings.Split(ligne[len("UPID:"):i], ":")
	var debutHex, typ, id string
	switch len(champs) {
	case 7:
		debutHex, typ, id = champs[3], champs[4], champs[5]
	case 8:
		debutHex, typ, id = champs[4], champs[5], champs[6]
	default:
		return t, false
	}
	if !typesSuivis[source][typ] {
		return t, false
	}
	reste := strings.SplitN(ligne[i+2:], " ", 2)
	if len(reste) != 2 {
		return t, false
	}
	debut, e1 := strconv.ParseInt(debutHex, 16, 64)
	fin, e2 := strconv.ParseInt(reste[0], 16, 64)
	if e1 != nil || e2 != nil || debut <= 0 || fin < debut {
		return t, false
	}
	id = strings.ReplaceAll(id, `\x3a`, ":")
	nom := typ
	if id != "" {
		nom += " " + id
	}
	if len(nom) > 100 {
		nom = nom[:100]
	}
	statut := strings.TrimSpace(reste[1])
	t = TacheProxmox{Source: source, Nom: nom, Debut: time.Unix(debut, 0).UTC(), Fin: time.Unix(fin, 0).UTC(), upid: ligne[:i+1],
		OK: statut == "OK" || strings.HasPrefix(statut, "WARNINGS")}
	if statut != "OK" {
		if len(statut) > 120 {
			statut = statut[:120]
		}
		t.Message = statut
	}
	return t, true
}
