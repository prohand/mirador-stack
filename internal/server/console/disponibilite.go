// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"mirador-stack/internal/server/flotte"
	"mirador-stack/internal/server/notif"
)

// Page « Disponibilité » (SPEC § 49, idée reprise de New Relic « service
// levels ») : part du temps où chaque machine et chaque sonde a marché sur
// la période, comparée à un objectif.
//
// Calculée à partir de l'historique des alertes, sans nouvelle donnée :
//   - machine : alertes des « règles de panne » (Réglages › Notifications :
//     AgentMuet…) pour son nom, hors maintenances ponctuelles déclarées ;
//   - sonde : alertes SondeEnEchec pour son nom.
// Coût : une requête SQL sur les alertes de la période, calcul en mémoire.

// ObjectifDefaut : objectif quand rien n'est réglé (%).
const ObjectifDefaut = 99.5

const cleObjectifs = "objectifs_disponibilite"

type jourDispo struct {
	Classe, Titre string
}

type ligneDispo struct {
	Cle, Nom, Sorte, Lien string
	Objectif              string // « 99,5 »
	Atteint               string
	Sous                  bool // sous l'objectif
	Jours                 []jourDispo
	Arret, Permis         string
	Marge                 int // % de la marge restante (0 à 100)
	ClasseMarge           string
	Depasse               string // « Dépassé de 2 h 05 »
	atteint               float64
}

type donneesDispo struct {
	Periode     string // 30, 90, mois
	Lignes      []ligneDispo
	NbJours     int
	ReglesPanne string
}

// intervalle : [debut, fin) en secondes Unix.
type intervalle struct{ d, f int64 }

// reunir : intervalles triés et réunis.
func reunir(iv []intervalle) []intervalle {
	sort.Slice(iv, func(i, j int) bool { return iv[i].d < iv[j].d })
	var out []intervalle
	for _, x := range iv {
		if n := len(out); n > 0 && x.d <= out[n-1].f {
			if x.f > out[n-1].f {
				out[n-1].f = x.f
			}
			continue
		}
		out = append(out, x)
	}
	return out
}

// retirer : iv privés des trous (maintenances).
func retirer(iv, trous []intervalle) []intervalle {
	for _, t := range reunir(trous) {
		var out []intervalle
		for _, x := range iv {
			if t.f <= x.d || t.d >= x.f {
				out = append(out, x)
				continue
			}
			if x.d < t.d {
				out = append(out, intervalle{x.d, t.d})
			}
			if t.f < x.f {
				out = append(out, intervalle{t.f, x.f})
			}
		}
		iv = out
	}
	return iv
}

// dureeDans : total dans [debut, fin).
func dureeDans(iv []intervalle, debut, fin int64) int64 {
	var n int64
	for _, x := range iv {
		d, f := max(x.d, debut), min(x.f, fin)
		if f > d {
			n += f - d
		}
	}
	return n
}

// dureeArret : « 0 », « 12 min », « 5 h 41 », « 2 j 3 h ».
func dureeArret(s int64) string {
	d := time.Duration(s) * time.Second
	switch {
	case d < time.Minute:
		if s == 0 {
			return "0"
		}
		return "moins d'1 min"
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h %02d", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%d j %d h", int(d.Hours())/24, int(d.Hours())%24)
}

// pct : « 99,86 », « 99,5 », « 100 » (zéros inutiles retirés après la virgule).
func pct(v float64) string {
	if v >= 99.995 {
		return "100"
	}
	s := strconv.FormatFloat(v, 'f', 2, 64)
	s = strings.TrimRight(s, "0")
	s = strings.TrimSuffix(s, ".")
	return virgule(s)
}

func (c *Console) lireObjectifs(ctx context.Context) map[string]float64 {
	out := map[string]float64{}
	var v string
	if c.db.QueryRowContext(ctx, `SELECT valeur FROM reglages WHERE cle = ?`, cleObjectifs).Scan(&v) == nil {
		_ = json.Unmarshal([]byte(v), &out)
	}
	return out
}

// pannes : intervalles de panne par élément (« m:machine », « s:sonde »).
func (c *Console) pannes(ctx context.Context, regles []string, debut, fin time.Time) map[string][]intervalle {
	out := map[string][]intervalle{}
	panne := map[string]bool{}
	for _, r := range regles {
		panne[r] = true
	}
	rows, err := c.db.QueryContext(ctx, `SELECT regle, etiquettes, debut, fin FROM alertes
		WHERE active_le > 0 AND debut < ? AND (fin = 0 OR fin > ?)`, fin.UnixMilli(), debut.UnixMilli())
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var regle, etq string
		var d, f int64
		if rows.Scan(&regle, &etq, &d, &f) != nil {
			continue
		}
		if regle != "SondeEnEchec" && !panne[regle] {
			continue
		}
		var e map[string]string
		if json.Unmarshal([]byte(etq), &e) != nil {
			continue
		}
		if f == 0 {
			f = fin.UnixMilli()
		}
		iv := intervalle{d / 1000, f / 1000}
		if regle == "SondeEnEchec" {
			out["s:"+e["sonde"]] = append(out["s:"+e["sonde"]], iv)
		} else if inst := e["instance"]; inst != "" {
			out["m:"+inst] = append(out["m:"+inst], iv)
		}
	}
	return out
}

// maintenancesMachines : maintenances ponctuelles « machine:NOM ».
func (c *Console) maintenancesMachines(ctx context.Context, debut, fin time.Time) map[string][]intervalle {
	out := map[string][]intervalle{}
	rows, err := c.db.QueryContext(ctx, `SELECT cible, debut, fin FROM maintenances WHERE recurrence = '' AND cible LIKE 'machine:%'
		AND debut < ? AND fin > ?`, fin.Unix(), debut.Unix())
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var cible string
		var d, f int64
		if rows.Scan(&cible, &d, &f) == nil {
			nom := strings.TrimPrefix(cible, "machine:")
			out["m:"+nom] = append(out["m:"+nom], intervalle{d, f})
		}
	}
	return out
}

func (c *Console) pageDisponibilite(q *req) {
	ctx := q.r.Context()
	d := donneesDispo{Periode: q.r.URL.Query().Get("periode")}
	loc := fuseau.Load()
	fin := q.t
	var debut time.Time
	switch d.Periode {
	case "90":
		debut = fin.Add(-90 * 24 * time.Hour)
	case "mois":
		l := fin.In(loc)
		debut = time.Date(l.Year(), l.Month(), 1, 0, 0, 0, 0, loc)
	default:
		d.Periode = "30"
		debut = fin.Add(-30 * 24 * time.Hour)
	}
	// Jours entiers (fuseau de l'interface), le dernier jusqu'à maintenant.
	j0 := debut.In(loc)
	j0 = time.Date(j0.Year(), j0.Month(), j0.Day(), 0, 0, 0, 0, loc)
	var jours []time.Time
	for j := j0; j.Before(fin); j = j.AddDate(0, 0, 1) {
		jours = append(jours, j)
	}
	d.NbJours = len(jours)

	r, _ := notif.LireReglages(ctx, c.db)
	d.ReglesPanne = strings.Join(r.ReglesPanne, ", ")
	pannes := c.pannes(ctx, r.ReglesPanne, debut, fin)
	maint := c.maintenancesMachines(ctx, debut, fin)
	objectifs := c.lireObjectifs(ctx)

	type element struct {
		cle, nom, sorte, lien string
		suivi                 time.Time
	}
	var els []element
	if ms, err := flotte.ListerMachines(ctx, c.db); err == nil {
		for _, m := range ms {
			if m.Statut != "approuvee" {
				continue
			}
			sorte := "machine"
			if m.Type == "collecteur" {
				sorte = "collecteur"
			}
			els = append(els, element{"m:" + m.Nom, m.Nom, sorte, "/machines/" + m.Nom, time.Unix(m.CreeLe, 0)})
		}
	}
	if ss, err := flotte.LireSondes(ctx, c.db); err == nil {
		for _, s := range ss {
			els = append(els, element{"s:" + s.Nom, s.Nom, "sonde " + s.Type, "/sondes", time.Time{}})
		}
	}
	for _, e := range els {
		iv := reunir(retirer(pannes[e.cle], maint[e.cle]))
		suiviDebut := debut
		if e.suivi.After(suiviDebut) {
			suiviDebut = e.suivi
		}
		total := fin.Unix() - suiviDebut.Unix()
		if total <= 0 {
			continue
		}
		arret := dureeDans(iv, suiviDebut.Unix(), fin.Unix())
		obj := ObjectifDefaut
		if v, ok := objectifs[e.cle]; ok && v > 0 && v <= 100 {
			obj = v
		}
		atteint := 100 * (1 - float64(arret)/float64(total))
		permis := int64((1 - obj/100) * float64(total))
		l := ligneDispo{Cle: e.cle, Nom: e.nom, Sorte: e.sorte, Lien: e.lien, Objectif: pct(obj), Atteint: pct(atteint),
			Sous: atteint < obj, Arret: dureeArret(arret), Permis: dureeArret(permis), atteint: atteint}
		switch {
		case permis <= 0 && arret > 0:
			l.Marge, l.ClasseMarge, l.Depasse = 100, "k", "Dépassé de "+dureeArret(arret)
		case arret > permis:
			l.Marge, l.ClasseMarge, l.Depasse = 100, "k", "Dépassé de "+dureeArret(arret-permis)
		default:
			if permis > 0 {
				l.Marge = int(100 * float64(permis-arret) / float64(permis))
			} else {
				l.Marge = 100
			}
			if l.Marge < 50 {
				l.ClasseMarge = "a"
			}
		}
		for k, j := range jours {
			jf := j.AddDate(0, 0, 1)
			if k == len(jours)-1 || jf.After(fin) {
				jf = fin
			}
			titre := j.Format("02/01")
			switch {
			case jf.Before(suiviDebut) || jf.Equal(suiviDebut):
				l.Jours = append(l.Jours, jourDispo{"n", titre + " : pas encore suivi"})
				continue
			}
			a := dureeDans(iv, max(j.Unix(), suiviDebut.Unix()), jf.Unix())
			switch {
			case a == 0:
				l.Jours = append(l.Jours, jourDispo{"", titre + " : aucun arrêt"})
			case a < 3600:
				l.Jours = append(l.Jours, jourDispo{"a", titre + " : arrêt " + dureeArret(a)})
			default:
				l.Jours = append(l.Jours, jourDispo{"k", titre + " : arrêt " + dureeArret(a)})
			}
		}
		d.Lignes = append(d.Lignes, l)
	}
	sort.SliceStable(d.Lignes, func(i, j int) bool {
		if d.Lignes[i].Sous != d.Lignes[j].Sous {
			return d.Lignes[i].Sous
		}
		return d.Lignes[i].atteint < d.Lignes[j].atteint
	})
	c.rendre(q, "disponibilite", "Disponibilité", "alertes", d)
}

func (c *Console) postDisponibilite(q *req) {
	ctx := q.r.Context()
	if err := c.verifierCode(q); err != nil {
		c.retour(q, "/disponibilite", err, "")
		return
	}
	objectifs := c.lireObjectifs(ctx)
	cle, v := strings.TrimSpace(q.r.PostFormValue("cle")), strings.TrimSpace(strings.ReplaceAll(q.r.PostFormValue("objectif"), ",", "."))
	f, err := strconv.ParseFloat(v, 64)
	if cle == "" || len(cle) > 200 || (!strings.HasPrefix(cle, "m:") && !strings.HasPrefix(cle, "s:")) {
		c.retour(q, "/disponibilite", errors.New("élément inconnu"), "")
		return
	}
	if err != nil || f < 50 || f > 100 {
		c.retour(q, "/disponibilite", errors.New("objectif : un pourcentage entre 50 et 100 (ex. 99,5)"), "")
		return
	}
	objectifs[cle] = f
	brut, _ := json.Marshal(objectifs)
	tx, err := c.db.BeginTx(ctx, nil)
	if err == nil {
		defer tx.Rollback() //nolint:errcheck // sans effet après Commit
		_, err = tx.ExecContext(ctx, `INSERT INTO reglages(cle, valeur, modifie_le, modifie_par) VALUES (?, ?, ?, ?)
			ON CONFLICT(cle) DO UPDATE SET valeur = excluded.valeur, modifie_le = excluded.modifie_le, modifie_par = excluded.modifie_par`,
			cleObjectifs, string(brut), q.t.Unix(), q.qui())
		if err == nil {
			err = Auditer(ctx, tx, q.qui(), "objectif_disponibilite", cle, "", v, q.ip.String(), q.t)
		}
		if err == nil {
			err = tx.Commit()
		}
	}
	c.retour(q, "/disponibilite", err, "objectif enregistré")
}
