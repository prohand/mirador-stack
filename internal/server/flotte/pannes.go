// SPDX-License-Identifier: GPL-3.0-or-later

package flotte

import (
	"context"
	"database/sql"
	"net/http"
	"sync"
	"time"

	"mirador-stack/internal/protocole"
)

// Pannes vues tout de suite par l'agent (SPEC § 98) : notées en base, puis
// le moteur d'alertes est réveillé 2 s plus tard (le temps que les
// métriques envoyées par l'agent au même moment soient écrites).

const (
	pannesParMinute  = 30  // par machine : un service qui tombe en boucle ne remplit pas la base
	pannesParMachine = 500 // lignes gardées par machine
	gardePannes      = 30 * 24 * time.Hour
	delaiReveil      = 2 * time.Second
	// ecartReveils (revue du 28/11, B10) : une même machine réveille le
	// moteur d'alertes au plus une fois par minute. Sans cela, 30 événements
	// par minute forçaient 30 évaluations complètes de toutes les règles.
	// La première panne réveille tout de suite ; les suivantes de la minute
	// sont vues au cycle normal du moteur (30 s à 1 min).
	ecartReveils = time.Minute
)

// Panne : une panne notée.
type Panne struct {
	Machine            string
	Le                 time.Time
	Type, Objet, Texte string
}

// limitePannes : compte par machine et par minute.
type limitePannes struct {
	mu      sync.Mutex
	minute  int64
	nb      map[int64]int
	reveils map[int64]time.Time // dernier réveil du moteur demandé par machine
}

// reveilPermis : la machine peut-elle réveiller le moteur d'alertes ? Une
// fois par minute au plus (ecartReveils). Mémoire bornée : entrées de plus
// d'une minute retirées quand la table dépasse 1 000 machines.
func (l *limitePannes) reveilPermis(machine int64, t time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.reveils == nil {
		l.reveils = map[int64]time.Time{}
	}
	if d, ok := l.reveils[machine]; ok && t.Sub(d) < ecartReveils {
		return false
	}
	if len(l.reveils) >= 1000 {
		for id, d := range l.reveils {
			if t.Sub(d) >= ecartReveils {
				delete(l.reveils, id)
			}
		}
	}
	l.reveils[machine] = t
	return true
}

func (l *limitePannes) permis(machine int64, t time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if m := t.Unix() / 60; m != l.minute || l.nb == nil {
		l.minute, l.nb = m, map[int64]int{}
	}
	l.nb[machine]++
	return l.nb[machine] <= pannesParMinute
}

func (f *Flotte) recevoirEvenement(w http.ResponseWriter, r *http.Request) {
	m, liberer, ok := f.opt.Ingestion.AccueillirLeger(w, r)
	if !ok {
		return
	}
	defer liberer()
	var e protocole.EvenementAgent
	if err := lireJSON(r, &e); err != nil {
		erreur(w, http.StatusBadRequest, "événement illisible")
		return
	}
	if err := e.Valider(); err != nil {
		erreur(w, http.StatusBadRequest, "événement invalide")
		return
	}
	t := f.opt.Maintenant()
	if !f.pannes.permis(m.ID, t) {
		erreur(w, http.StatusTooManyRequests, "trop d'événements")
		return
	}
	if e.Type == protocole.EvenementSSH { // connexion réussie (demande du 27/11)
		nouvelle, err := NoterConnexionSSH(r.Context(), f.opt.DB, m.ID, m.Nom, e, t)
		if err != nil {
			f.opt.Journal.Error("connexion SSH non notée", "machine", m.Nom, "erreur", err)
			erreur(w, http.StatusServiceUnavailable, "base indisponible")
			return
		}
		if nouvelle {
			f.opt.Journal.Warn("connexion SSH depuis une adresse nouvelle", "machine", m.Nom, "adresse", e.Objet)
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := NoterPanne(r.Context(), f.opt.DB, m.ID, t, e); err != nil {
		f.opt.Journal.Error("panne non notée", "machine", m.Nom, "erreur", err)
		erreur(w, http.StatusServiceUnavailable, "base indisponible")
		return
	}
	f.opt.Journal.Info("panne vue par l'agent", "machine", m.Nom, "type", e.Type, "objet", e.Objet)
	if f.opt.ReveilAlertes != nil && f.pannes.reveilPermis(m.ID, t) {
		time.AfterFunc(delaiReveil, f.opt.ReveilAlertes)
	}
	w.WriteHeader(http.StatusNoContent)
}

// NoterPanne : écrit la panne et fait le ménage de la machine (30 jours,
// 500 lignes au plus).
func NoterPanne(ctx context.Context, db *sql.DB, machine int64, t time.Time, e protocole.EvenementAgent) error {
	if _, err := db.ExecContext(ctx, `INSERT INTO pannes(machine_id, le, type, objet, texte) VALUES (?, ?, ?, ?, ?)`,
		machine, t.UnixMilli(), e.Type, e.Objet, e.Texte); err != nil {
		return err
	}
	_, err := db.ExecContext(ctx, `DELETE FROM pannes WHERE machine_id = ? AND (le < ? OR id <= (
		SELECT id FROM pannes WHERE machine_id = ? ORDER BY id DESC LIMIT 1 OFFSET ?))`,
		machine, t.Add(-gardePannes).UnixMilli(), machine, pannesParMachine)
	return err
}

// ListerPannes : pannes de la période (machine vide : toutes), les plus récentes d'abord.
func ListerPannes(ctx context.Context, db *sql.DB, machine string, debut, fin time.Time, max int) ([]Panne, error) {
	rows, err := db.QueryContext(ctx, `SELECT m.nom, p.le, p.type, p.objet, p.texte FROM pannes p JOIN machines m ON m.id = p.machine_id
		WHERE p.le >= ? AND p.le <= ? AND (? = '' OR m.nom = ?) ORDER BY p.le DESC LIMIT ?`,
		debut.UnixMilli(), fin.UnixMilli(), machine, machine, max)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }() // lecture seule
	var out []Panne
	for rows.Next() {
		var p Panne
		var le int64
		if err := rows.Scan(&p.Machine, &le, &p.Type, &p.Objet, &p.Texte); err != nil {
			return nil, err
		}
		p.Le = time.UnixMilli(le)
		out = append(out, p)
	}
	return out, rows.Err()
}
