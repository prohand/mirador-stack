// SPDX-License-Identifier: GPL-3.0-or-later

package motifs

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Registre : motifs de toutes les lignes reçues (SPEC § 52), pour « vu la
// 1re fois » et « nouveau » (jamais vu sur les 7 jours d'avant). En mémoire,
// écrit en base toutes les 5 minutes (seulement ce qui a changé).
//
// Coût mesuré (prototype, 18/11) : 1,2 µs par ligne en moyenne, 4 Mo pour
// 5 000 motifs (plafond). Ici avec le remplacement par expressions
// régulières de la page Logs : mesuré dans BenchmarkRegistre.
type Registre struct {
	db      *sql.DB
	journal *slog.Logger

	mu      sync.Mutex
	seaux   map[seau][]*entree
	total   int
	sales   map[*entree]bool
	oublies []int64 // identifiants à effacer en base
	debut   int64   // début de l'apprentissage (secondes Unix)
	horloge uint64
}

type entree struct {
	id               int64
	jetons           []string
	premier, dernier int64
	n, nEcrit        int64
	vu               uint64
}

// Bornes.
const (
	MaxMotifs          = 5000
	periodeEcriture    = 5 * time.Minute
	cleDebut           = "motifs_logs_debut"
	DureeNouveau       = 24 * time.Hour     // « nouveau » : vu la 1re fois depuis moins de 24 h…
	DureeApprentissage = 7 * 24 * time.Hour // … et rien d'équivalent dans les 7 jours d'avant
)

// OuvrirRegistre : registre relu en base.
func OuvrirRegistre(ctx context.Context, db *sql.DB, journal *slog.Logger, maintenant time.Time) (*Registre, error) {
	if journal == nil {
		journal = slog.New(slog.DiscardHandler)
	}
	r := &Registre{db: db, journal: journal, seaux: map[seau][]*entree{}, sales: map[*entree]bool{}}
	var v string
	err := db.QueryRowContext(ctx, `SELECT valeur FROM reglages WHERE cle = ?`, cleDebut).Scan(&v)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		r.debut = maintenant.Unix()
		if _, err := db.ExecContext(ctx, `INSERT INTO reglages(cle, valeur, modifie_le, modifie_par) VALUES (?, ?, ?, 'mirador')`,
			cleDebut, strconv.FormatInt(r.debut, 10), r.debut); err != nil {
			return nil, err
		}
	case err != nil:
		return nil, err
	default:
		r.debut, _ = strconv.ParseInt(v, 10, 64)
	}
	rows, err := db.QueryContext(ctx, `SELECT id, gabarit, premier_le, dernier_le, n FROM motifs_logs`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		e := &entree{}
		var g string
		if err := rows.Scan(&e.id, &g, &e.premier, &e.dernier, &e.n); err != nil {
			return nil, err
		}
		e.jetons, e.nEcrit = splitGabarit(g), e.n
		k := seauDe(e.jetons)
		r.seaux[k] = append(r.seaux[k], e)
		r.total++
	}
	return r, rows.Err()
}

func splitGabarit(g string) []string {
	if g == "" {
		return nil
	}
	return splitEspace(g)
}

func splitEspace(g string) []string {
	var out []string
	debut := 0
	for i := 0; i <= len(g); i++ {
		if i == len(g) || g[i] == ' ' {
			if i > debut {
				out = append(out, g[debut:i])
			}
			debut = i + 1
		}
	}
	return out
}

// Ajouter : une ligne reçue (t : secondes Unix).
func (r *Registre) Ajouter(msg string, t int64) {
	js := Jetons(msg)
	k := seauDe(js)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.horloge++
	var meilleur *entree
	mieux := -1.0
	for _, e := range r.seaux[k] {
		if s := ressemblance(e.jetons, js); s > mieux {
			mieux, meilleur = s, e
		}
	}
	if meilleur != nil && mieux >= Seuil {
		fusionner(meilleur.jetons, js)
		meilleur.n++
		if t > meilleur.dernier {
			meilleur.dernier = t
		}
		meilleur.vu = r.horloge
		r.sales[meilleur] = true
		return
	}
	e := &entree{jetons: append([]string(nil), js...), premier: t, dernier: t, n: 1, vu: r.horloge}
	r.seaux[k] = append(r.seaux[k], e)
	r.total++
	r.sales[e] = true
	if r.total > MaxMotifs {
		r.oublier()
	}
}

// oublier (sous verrou) : le dixième des motifs les moins récemment vus.
func (r *Registre) oublier() {
	type rang struct {
		dernier int64
		vu      uint64
	}
	var tous []rang
	for _, s := range r.seaux {
		for _, e := range s {
			tous = append(tous, rang{e.dernier, e.vu})
		}
	}
	sort.Slice(tous, func(i, j int) bool { return tous[i].dernier < tous[j].dernier })
	limite := tous[len(tous)/10].dernier
	for k, s := range r.seaux {
		garde := s[:0]
		for _, e := range s {
			if e.dernier > limite || e.vu == r.horloge { // celui qui vient d'arriver reste
				garde = append(garde, e)
				continue
			}
			r.total--
			delete(r.sales, e)
			if e.id != 0 {
				r.oublies = append(r.oublies, e.id)
			}
		}
		if len(garde) == 0 {
			delete(r.seaux, k)
		} else {
			r.seaux[k] = garde
		}
	}
}

// Ecrire : motifs changés depuis la dernière écriture, et oubliés.
func (r *Registre) Ecrire(ctx context.Context) error {
	r.mu.Lock()
	type ligne struct {
		e                   *entree
		id                  int64
		gabarit             string
		premier, dernier, n int64
	}
	var lignes []ligne
	for e := range r.sales {
		lignes = append(lignes, ligne{e, e.id, Gabarit(e.jetons), e.premier, e.dernier, e.n})
	}
	oublies := r.oublies
	r.sales, r.oublies = map[*entree]bool{}, nil
	r.mu.Unlock()
	if len(lignes) == 0 && len(oublies) == 0 {
		return nil
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // sans effet après Commit
	ids := map[*entree]int64{}
	for _, l := range lignes {
		if l.id == 0 {
			res, err := tx.ExecContext(ctx, `INSERT INTO motifs_logs(gabarit, premier_le, dernier_le, n) VALUES (?, ?, ?, ?)`,
				l.gabarit, l.premier, l.dernier, l.n)
			if err != nil {
				return err
			}
			if ids[l.e], err = res.LastInsertId(); err != nil {
				return err
			}
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE motifs_logs SET gabarit = ?, dernier_le = ?, n = ? WHERE id = ?`,
			l.gabarit, l.dernier, l.n, l.id); err != nil {
			return err
		}
	}
	for _, id := range oublies {
		if _, err := tx.ExecContext(ctx, `DELETE FROM motifs_logs WHERE id = ?`, id); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	r.mu.Lock()
	for e, id := range ids {
		e.id = id
	}
	r.mu.Unlock()
	return nil
}

// Boucle : écriture toutes les 5 min, et une dernière à l'arrêt.
func (r *Registre) Boucle(ctx context.Context) {
	t := time.NewTicker(periodeEcriture)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			fin, annuler := context.WithTimeout(context.Background(), 5*time.Second)
			if err := r.Ecrire(fin); err != nil {
				r.journal.Warn("registre des motifs : écriture", "erreur", err)
			}
			annuler()
			return
		case <-t.C:
			if err := r.Ecrire(ctx); err != nil {
				r.journal.Warn("registre des motifs : écriture", "erreur", err)
			}
		}
	}
}

// Vu : première fois qu'un motif de même forme a été vu (secondes Unix).
// Le gabarit cherché et celui du registre peuvent être plus ou moins
// généraux l'un que l'autre : l'un doit couvrir l'autre.
func (r *Registre) Vu(js []string) (premier int64, ok bool) {
	k := seauDe(js)
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.seaux[k] {
		if Couvre(e.jetons, js) || Couvre(js, e.jetons) {
			if !ok || e.premier < premier {
				premier, ok = e.premier, true
			}
		}
	}
	return premier, ok
}

// Nouveau : vu la 1re fois depuis moins de 24 h, après au moins 7 jours
// d'apprentissage (sinon tout serait « nouveau » au début).
func (r *Registre) Nouveau(premier int64, maintenant time.Time) bool {
	return maintenant.Unix()-premier < int64(DureeNouveau/time.Second) &&
		premier-r.debut >= int64(DureeApprentissage/time.Second)
}

// Apprentissage : vrai pendant les 7 premiers jours.
func (r *Registre) Apprentissage(maintenant time.Time) bool {
	return maintenant.Unix()-r.debut < int64(DureeApprentissage/time.Second)
}

// Nombre : motifs en mémoire.
func (r *Registre) Nombre() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.total
}
