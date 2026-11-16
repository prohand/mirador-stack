// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"mirador-stack/internal/server/flotte"
)

// Livraison 3 des métriques (SPEC § 31.3) : les plus gourmands d'une
// machine, carte de chaleur du parc sur 24 h, écran mural.

// --- Les plus gourmands ------------------------------------------------------

// ligneGourmand : un conteneur, sa part (texte et barre).
type ligneGourmand struct {
	Nom, Texte string
	Pct        int // 0 à 100 (largeur de la barre)
}

type vueGourmands struct {
	CPU, Memoire []ligneGourmand
}

const nbGourmands = 5

// gourmands : 5 conteneurs qui prennent le plus de processeur et de
// mémoire (Docker relevé par l'agent). Les processus ne sont pas relevés.
func (c *Console) gourmands(ctx context.Context, m flotte.InfoMachine, t time.Time) *vueGourmands {
	if c.lecteur == nil || !reValeurVariable.MatchString(m.Nom) {
		return nil
	}
	i := `instance="` + m.Nom + `"`
	var v vueGourmands
	// Processeur : part de la machine entière (tous les cœurs).
	if ech, err := c.instant(ctx, fmt.Sprintf(`topk(%d, 100 * sum by (name) (rate(node_docker_container_cpu_seconds_total{%s}[5m]))
		/ ignoring(name) group_left count(node_cpu_seconds_total{%s, mode="idle"}))`, nbGourmands, i, i), t); err == nil {
		sort.Slice(ech, func(a, b int) bool { return ech[a].Valeur > ech[b].Valeur })
		for _, e := range ech {
			if math.IsNaN(e.Valeur) {
				continue
			}
			v.CPU = append(v.CPU, ligneGourmand{Nom: e.Etiquettes.Get("name"), Texte: formater(e.Valeur, "percent", &un), Pct: pctBarre(e.Valeur)})
		}
	}
	// Mémoire : octets et part de la mémoire de la machine.
	total := 0.0
	if ech, err := c.instant(ctx, `max(node_memory_MemTotal_bytes{`+i+`})`, t); err == nil && len(ech) == 1 {
		total = ech[0].Valeur
	}
	if ech, err := c.instant(ctx, fmt.Sprintf(`topk(%d, max by (name) (node_docker_container_memory_bytes{%s}))`, nbGourmands, i), t); err == nil {
		sort.Slice(ech, func(a, b int) bool { return ech[a].Valeur > ech[b].Valeur })
		for _, e := range ech {
			l := ligneGourmand{Nom: e.Etiquettes.Get("name"), Texte: formater(e.Valeur, "bytes", nil)}
			if total > 0 {
				l.Pct = pctBarre(100 * e.Valeur / total)
				l.Texte += " · " + formater(100*e.Valeur/total, "percent", &zero)
			}
			v.Memoire = append(v.Memoire, l)
		}
	}
	if len(v.CPU) == 0 && len(v.Memoire) == 0 {
		return nil
	}
	return &v
}

var un = 1

// --- Carte de chaleur du parc --------------------------------------------------

// celluleChaleur : une heure d'une machine (classe de couleur, bulle).
type celluleChaleur struct {
	Classe, Titre string
}

type ligneChaleur struct {
	Nom       string
	Texte     string // nom, site, type, groupe : pour le filtre du tableau du parc (SPEC § 37)
	pic       float64
	Cellules  []celluleChaleur
	Maximum   string // plus forte heure, en fin de ligne
	ClasseMax string
}

type vueChaleur struct {
	Titre  string
	Heures []string // un libellé toutes les 3 heures (8 en tout)
	Lignes []ligneChaleur
	// Cachees : machines au-delà des 100 les plus chargées, non affichées.
	Cachees int
}

// lignesChaleurVisibles : lignes montrées d'office, les autres derrière
// « Voir les N autres » (déplié par mirador.js, qui tient compte des filtres).
const lignesChaleurVisibles = 10

// Moyenne par heure sur 24 h (une requête par mesure pour tout le parc).
var exprsChaleur = []struct{ titre, expr string }{
	{"Processeur", `100 * (1 - avg by (instance) (rate(node_cpu_seconds_total{mode="idle"}[1h])))`},
	{"Mémoire", `100 * (1 - max by (instance) (avg_over_time(node_memory_MemAvailable_bytes[1h])) / max by (instance) (node_memory_MemTotal_bytes))`},
}

const machinesChaleurMax = 100

type cacheChaleur struct {
	mu     sync.Mutex
	vues   []vueChaleur
	expire time.Time
}

// classeChaleur : 6 teintes, du calme (0) au saturé (5).
func classeChaleur(v float64) string {
	switch {
	case math.IsNaN(v):
		return "ch-vide"
	case v >= 90:
		return "ch-5"
	case v >= 75:
		return "ch-4"
	case v >= 60:
		return "ch-3"
	case v >= 40:
		return "ch-2"
	case v >= 20:
		return "ch-1"
	}
	return "ch-0"
}

// chaleurParc : processeur et mémoire de chaque machine, heure par heure
// sur 24 h ; gardé 5 minutes.
func (c *Console) chaleurParc(ctx context.Context, ms []flotte.InfoMachine, t time.Time) []vueChaleur {
	if c.lecteur == nil {
		return nil
	}
	c.chaleur.mu.Lock()
	if t.Before(c.chaleur.expire) {
		v := c.chaleur.vues
		c.chaleur.mu.Unlock()
		return v
	}
	c.chaleur.mu.Unlock()
	fin := t.Truncate(time.Hour)
	debut := fin.Add(-23 * time.Hour)
	// Toutes les machines approuvées : le tri par pic se fait après, et
	// seules les 100 plus chargées sont gardées (SPEC § 37).
	var noms []string
	textes := map[string]string{}
	for _, m := range ms {
		if m.Statut == "approuvee" {
			noms = append(noms, m.Nom)
			textes[m.Nom] = strings.Join([]string{m.Nom, m.Site, m.Env, m.Groupe}, " ")
		}
	}
	sort.Strings(noms)
	var vues []vueChaleur
	for _, ex := range exprsChaleur {
		v := vueChaleur{Titre: ex.titre}
		for h := 0; h < 24; h += 3 {
			v.Heures = append(v.Heures, debut.Add(time.Duration(h-1)*time.Hour).In(fuseau.Load()).Format("15")+" h")
		}
		courbes, _, err := c.lecteur.Plage(ctx, ex.expr, debut, fin, time.Hour)
		if err != nil {
			continue
		}
		parNom := map[string][]float64{}
		for _, cb := range courbes {
			vals := make([]float64, 24)
			for k := range vals {
				vals[k] = math.NaN()
			}
			for k, ts := range cb.T {
				if h := int((ts/1000 - debut.Unix()) / 3600); h >= 0 && h < 24 {
					vals[h] = cb.V[k]
				}
			}
			parNom[cb.Etiquettes.Get("instance")] = vals
		}
		for _, nom := range noms {
			vals, ok := parNom[nom]
			if !ok {
				continue
			}
			l := ligneChaleur{Nom: nom, Texte: textes[nom]}
			pire := math.NaN()
			for h, x := range vals {
				fh := debut.Add(time.Duration(h) * time.Hour).In(fuseau.Load())
				titre := nom + " · " + fh.Add(-time.Hour).Format("15 h") + "–" + fh.Format("15 h") + " · "
				if math.IsNaN(x) {
					titre += "pas de données"
				} else {
					titre += formater(x, "percent", &zero)
					if math.IsNaN(pire) || x > pire {
						pire = x
					}
				}
				l.Cellules = append(l.Cellules, celluleChaleur{Classe: classeChaleur(x), Titre: titre})
			}
			l.pic = pire
			if !math.IsNaN(pire) {
				l.Maximum, l.ClasseMax = formater(pire, "percent", &zero), classeChaleur(pire)
			}
			v.Lignes = append(v.Lignes, l)
		}
		ordonnerChaleur(&v)
		vues = append(vues, v)
	}
	c.chaleur.mu.Lock()
	c.chaleur.vues, c.chaleur.expire = vues, t.Add(5*time.Minute)
	c.chaleur.mu.Unlock()
	return vues
}

// ordonnerChaleur : les machines les plus chargées (pic sur 24 h) en haut,
// sans données en bas, puis par nom ; au-delà de 100, le reste est compté
// (« + N non affichées ») mais pas montré.
func ordonnerChaleur(v *vueChaleur) {
	sort.SliceStable(v.Lignes, func(i, j int) bool {
		a, b := v.Lignes[i], v.Lignes[j]
		if math.IsNaN(a.pic) != math.IsNaN(b.pic) {
			return !math.IsNaN(a.pic)
		}
		if !math.IsNaN(a.pic) && a.pic != b.pic {
			return a.pic > b.pic
		}
		return a.Nom < b.Nom
	})
	if len(v.Lignes) > machinesChaleurMax {
		v.Cachees = len(v.Lignes) - machinesChaleurMax
		v.Lignes = v.Lignes[:machinesChaleurMax]
	}
}

// --- Écran mural --------------------------------------------------------------

// pageMur : vue d'ensemble pour un écran au mur (sans menu, grands
// caractères, thème sombre, rechargée toute seule toutes les 30 s).
func (c *Console) pageMur(q *req) {
	d := c.donneesAccueil(q)
	q.w.Header().Set("Cache-Control", "no-store")
	c.pages.executer(q.w, http.StatusOK, "ecran", Page{Titre: "Écran mural", Utilisateur: q.u.Nom, CSRF: q.s.CSRF,
		Version: c.opt.Version, D: donneesMur{donneesAccueil: d, Heure: q.t.In(fuseau.Load()).Format("15:04"),
			Jour: strings.TrimSpace(libelleJourCourt(q.t))}})
}

type donneesMur struct {
	donneesAccueil
	Heure, Jour string
}

// libelleJourCourt : « dimanche 15/11 ».
func libelleJourCourt(t time.Time) string {
	t = t.In(fuseau.Load())
	jours := []string{"dimanche", "lundi", "mardi", "mercredi", "jeudi", "vendredi", "samedi"}
	return jours[t.Weekday()] + " " + t.Format("02/01")
}
