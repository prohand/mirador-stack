// SPDX-License-Identifier: GPL-3.0-or-later

package taches

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Historique des exécutions (SPEC § 95, demande du 20/11) : le serveur note
// chaque minute la dernière exécution de chaque tâche (table
// taches_executions, 60 lignes au plus par tâche). Il en tire :
//   - la frise des dernières exécutions ;
//   - la durée habituelle (médiane) : « durée anormale », « tourne trop longtemps » ;
//   - la taille habituelle de chaque VM sauvegardée : « taille en baisse ».

// GardeesParTache : exécutions gardées par tâche.
const GardeesParTache = 60

// Execution : une exécution terminée.
type Execution struct {
	Debut, Fin time.Time
	Code       int
	Octets     float64            // sauvegardes : total écrit
	VMs        map[string]float64 // sauvegardes : octets par VM réussie
}

// Duree : durée de l'exécution (0 : début inconnu).
func (e Execution) Duree() time.Duration {
	if e.Debut.IsZero() || e.Fin.Before(e.Debut) {
		return 0
	}
	return e.Fin.Sub(e.Debut)
}

// Noteur : note les nouvelles exécutions. Il retient la dernière fin notée
// de chaque tâche : une exécution déjà vue ne coûte aucune écriture.
type Noteur struct {
	mu   sync.Mutex
	vues map[string]int64
}

// Noter : enregistre la dernière exécution d'une tâche si elle est nouvelle.
func (n *Noteur) Noter(ctx context.Context, db *sql.DB, cle string, r Releve) error {
	if r.Fin.IsZero() || cle == "" {
		return nil
	}
	fin := r.Fin.Unix()
	n.mu.Lock()
	if n.vues == nil {
		n.vues = map[string]int64{}
	}
	deja := n.vues[cle] == fin
	n.mu.Unlock()
	if deja {
		return nil
	}
	var debut int64
	if !r.Debut.IsZero() && !r.Debut.After(r.Fin) {
		debut = r.Debut.Unix()
	}
	var total float64
	vms := map[string]float64{}
	for _, v := range r.VMs {
		if v.OK && v.Octets > 0 && len(vms) < 300 {
			vms[v.ID] = v.Octets
		}
		total += v.Octets
	}
	brut := ""
	if len(vms) > 0 {
		b, _ := json.Marshal(vms)
		brut = string(b)
	}
	if _, err := db.ExecContext(ctx, `INSERT OR IGNORE INTO taches_executions(cle, fin, debut, code, octets, vms) VALUES (?, ?, ?, ?, ?, ?)`,
		cle, fin, debut, r.Code, total, brut); err != nil {
		return err
	}
	// Au plus GardeesParTache lignes pour cette tâche.
	if _, err := db.ExecContext(ctx, `DELETE FROM taches_executions WHERE cle = ? AND fin < (
		SELECT fin FROM taches_executions WHERE cle = ? ORDER BY fin DESC LIMIT 1 OFFSET ?)`, cle, cle, GardeesParTache-1); err != nil {
		return err
	}
	n.mu.Lock()
	n.vues[cle] = fin
	n.mu.Unlock()
	return nil
}

// Historiques : dernières exécutions des tâches demandées (les plus
// récentes d'abord), n au plus par tâche.
func Historiques(ctx context.Context, db *sql.DB, cles []string, n int) (map[string][]Execution, error) {
	out := map[string][]Execution{}
	if len(cles) == 0 {
		return out, nil
	}
	// Par paquets de 200 clés (limite des paramètres SQLite loin).
	for i := 0; i < len(cles); i += 200 {
		lot := cles[i:min(i+200, len(cles))]
		args := make([]any, len(lot))
		for k, c := range lot {
			args[k] = c
		}
		// Seuls des « ? » sont ajoutés à la requête ; les clés sont des paramètres.
		requete := `SELECT cle, fin, debut, code, octets, vms FROM taches_executions
			WHERE cle IN (?` + strings.Repeat(",?", len(lot)-1) + `) ORDER BY cle, fin DESC` // #nosec G202 -- marqueurs « ? » seulement
		rows, err := db.QueryContext(ctx, requete, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var cle, vms string
			var fin, debut int64
			var e Execution
			if err := rows.Scan(&cle, &fin, &debut, &e.Code, &e.Octets, &vms); err != nil {
				_ = rows.Close()
				return nil, err
			}
			if len(out[cle]) >= n {
				continue
			}
			e.Fin = time.Unix(fin, 0)
			if debut > 0 {
				e.Debut = time.Unix(debut, 0)
			}
			if vms != "" {
				_ = json.Unmarshal([]byte(vms), &e.VMs)
			}
			out[cle] = append(out[cle], e)
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Anomalie : quelque chose d'inhabituel dans la dernière exécution.
type Anomalie struct {
	Type  string // duree, encours, taille
	Court string // « Durée anormale »
	Texte string // « a duré 2 h 10, d'habitude 20 min »
}

// Seuils des anomalies.
const (
	minHistorique  = 3                // exécutions réussies nécessaires
	facteurDuree   = 3                // « anormale » : 3 fois la durée habituelle
	ecartDureeMin  = 5 * time.Minute  // … et au moins 5 min de plus
	ecartEnCoursMn = 30 * time.Minute // en cours : au moins 30 min de plus
	baisseTaille   = 0.5              // taille divisée par 2 ou plus
	tailleMinSuivi = 100 << 20        // VM de moins de 100 Mo : non comparées
)

// DureeHabituelle : médiane des durées des exécutions réussies précédant
// fin (20 dernières). Zéro : pas assez d'historique.
func DureeHabituelle(hist []Execution, fin time.Time) time.Duration {
	var ds []time.Duration
	for _, e := range hist {
		if (fin.IsZero() || e.Fin.Before(fin)) && e.Code == 0 && e.Duree() > 0 {
			ds = append(ds, e.Duree())
			if len(ds) == 20 {
				break
			}
		}
	}
	if len(ds) < minHistorique {
		return 0
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	return ds[len(ds)/2]
}

// Anomalies : dernière exécution comparée aux précédentes.
func Anomalies(r Releve, hist []Execution, maintenant time.Time) []Anomalie {
	var out []Anomalie
	// En cours (début après la dernière fin) depuis trop longtemps.
	if !r.Debut.IsZero() && r.Debut.After(r.Fin) {
		if h := DureeHabituelle(hist, time.Time{}); h > 0 {
			en := maintenant.Sub(r.Debut)
			if en > facteurDuree*h && en-h >= ecartEnCoursMn {
				out = append(out, Anomalie{Type: "encours", Court: "Tourne trop longtemps",
					Texte: "en cours depuis " + DureeTexte(en) + ", d'habitude " + DureeTexte(h)})
			}
		}
	} else if !r.Fin.IsZero() && !r.Debut.IsZero() {
		// Dernière exécution beaucoup plus longue que d'habitude.
		if h := DureeHabituelle(hist, r.Fin); h > 0 {
			d := r.Fin.Sub(r.Debut)
			if d > facteurDuree*h && d-h >= ecartDureeMin {
				out = append(out, Anomalie{Type: "duree", Court: "Durée anormale",
					Texte: "a duré " + DureeTexte(d) + ", d'habitude " + DureeTexte(h)})
			}
		}
	}
	// Sauvegarde : VM beaucoup plus petite que la fois d'avant.
	var baisses []string
	for _, v := range r.VMs {
		if !v.OK || v.Octets <= 0 {
			continue
		}
		for _, e := range hist {
			if !e.Fin.Before(r.Fin) {
				continue
			}
			avant, ok := e.VMs[v.ID]
			if !ok {
				continue
			}
			if avant >= tailleMinSuivi && v.Octets < avant*baisseTaille {
				nom := v.ID
				if v.Nom != "" {
					nom += " " + v.Nom
				}
				baisses = append(baisses, nom+" : "+TailleTexte(avant)+" → "+TailleTexte(v.Octets))
			}
			break // comparée à la dernière sauvegarde réussie de cette VM
		}
	}
	if len(baisses) > 0 {
		t := strings.Join(baisses[:min(3, len(baisses))], " ; ")
		if len(baisses) > 3 {
			t += fmt.Sprintf(" (+%d)", len(baisses)-3)
		}
		out = append(out, Anomalie{Type: "taille", Court: "Taille en baisse", Texte: t})
	}
	return out
}

// AvecAnomalies : une tâche « OK » avec une anomalie passe « à surveiller ».
func AvecAnomalies(e Etat, a []Anomalie) Etat {
	e.Anomalies = a
	if len(a) > 0 && e.Classe == "ok" {
		e.Classe, e.Texte, e.Probleme = "attention", a[0].Court, true
	}
	return e
}

// DureeTexte : « 2 h 10 », « 25 min », « 40 s ».
func DureeTexte(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%d j", d/(24*time.Hour))
	case d >= time.Hour:
		if m := (d % time.Hour) / time.Minute; m > 0 {
			return fmt.Sprintf("%d h %02d", d/time.Hour, m)
		}
		return fmt.Sprintf("%d h", d/time.Hour)
	case d >= time.Minute:
		return fmt.Sprintf("%d min", d/time.Minute)
	}
	return fmt.Sprintf("%d s", d/time.Second)
}

// TailleTexte : « 2,1 Go », « 300 Mo » (comme le reste de la console).
func TailleTexte(o float64) string {
	unites := []string{"o", "Ko", "Mo", "Go", "To"}
	i := 0
	for o >= 1024 && i < len(unites)-1 {
		o /= 1024
		i++
	}
	if o < 10 && i > 0 {
		return strings.Replace(fmt.Sprintf("%.1f %s", o, unites[i]), ".", ",", 1)
	}
	return fmt.Sprintf("%.0f %s", o, unites[i])
}
