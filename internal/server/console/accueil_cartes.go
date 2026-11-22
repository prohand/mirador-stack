// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"mirador-stack/internal/server/auth"
)

// Vue d'ensemble réorganisable (SPEC § 83, idée reprise de Checkmate) :
// « Modifier la vue d'ensemble » permet de déplacer les tuiles et les
// blocs, et d'en masquer. Une disposition pour l'ordinateur, une pour le
// téléphone, gardées avec le compte (même document que le menu, § 79).
// L'affichage n'a besoin d'aucun script : classes d'ordre o-N / t-N et
// cache-o / cache-t, comme le menu.

// Tuiles (dans la rangée du haut) puis blocs (la page), ordre par défaut.
var (
	tuilesAccueil = []string{"machines", "alertes", "sondes", "certificats", "stockage"}
	blocsAccueil  = []string{"bandeau", "tuiles", "sites", "alertes-cours", "prises", "masquees", "attente", "regarder", "apercu", "parc", "chaleur", "bas"}
	// « sauvegardes » (carte des dernières 24 h) retirée le 20/11 à la demande,
	// « notifications » le 22/11 (dans la ligne du bas, SPEC § 120) : une
	// disposition enregistrée qui les cite encore est acceptée et ignorée.
	clesRetirees = []string{"sauvegardes", "notifications"}
	// cachesParDefaut : blocs masqués tant que la disposition n'a pas été
	// changée (nouvelle présentation du 22/11 : le tableau du parc et la
	// carte de chaleur complète restent à un clic dans « Modifier la vue »).
	cachesParDefaut = map[string]bool{"parc": true, "chaleur": true}
)

func cleAccueil(c string) bool {
	for _, l := range [][]string{tuilesAccueil, blocsAccueil, clesRetirees} {
		for _, x := range l {
			if x == c {
				return true
			}
		}
	}
	return false
}

// prefAccueil : disposition de la vue d'ensemble, par mode.
type prefAccueil struct {
	Ordi prefMode `json:"ordi"`
	Tel  prefMode `json:"tel"`
}

// rangsListe : rang de chaque clé de la liste (ordre voulu, puis défaut).
func rangsListe(ordre []string, defaut []string) map[string]int {
	r := map[string]int{}
	dans := map[string]bool{}
	for _, c := range defaut {
		dans[c] = true
	}
	n := 0
	for _, c := range ordre {
		if _, deja := r[c]; dans[c] && !deja {
			r[c] = n
			n++
		}
	}
	for _, c := range defaut {
		if _, deja := r[c]; !deja {
			r[c] = n
			n++
		}
	}
	return r
}

// classesAccueil : clé → « o-N t-N [cache-o] [cache-t] ».
func classesAccueil(u auth.Utilisateur) map[string]string {
	p := lirePrefMenu(u.Menu).Accueil
	out := map[string]string{}
	for _, l := range [][]string{tuilesAccueil, blocsAccueil} {
		ro, rt := rangsListe(p.Ordi.Ordre, l), rangsListe(p.Tel.Ordre, l)
		for _, c := range l {
			cl := "o-" + strconv.Itoa(ro[c]) + " t-" + strconv.Itoa(rt[c])
			if p.Ordi.cache(c) || (cachesParDefaut[c] && p.Ordi.parDefaut()) {
				cl += " cache-o"
			}
			if p.Tel.cache(c) || (cachesParDefaut[c] && p.Tel.parDefaut()) {
				cl += " cache-t"
			}
			out[c] = cl
		}
	}
	return out
}

// postAccueil : enregistre la disposition d'un mode (JSON envoyé par « Terminé »).
func (c *Console) postAccueil(q *req) {
	var d demandeMenu
	if err := json.NewDecoder(io.LimitReader(q.r.Body, 4096)).Decode(&d); err != nil || (d.Mode != "ordi" && d.Mode != "tel") {
		http.Error(q.w, "demande illisible", http.StatusBadRequest)
		return
	}
	var m prefMode
	if !d.Defaut {
		vus := map[string]bool{}
		for _, l := range [][]string{d.Ordre, d.Caches} {
			for _, k := range l {
				if !cleAccueil(k) {
					http.Error(q.w, errors.New("carte inconnue").Error(), http.StatusBadRequest)
					return
				}
			}
		}
		for _, k := range d.Ordre {
			if vus[k] {
				http.Error(q.w, "carte en double", http.StatusBadRequest)
				return
			}
			vus[k] = true
		}
		if len(d.Caches) > len(tuilesAccueil)+len(blocsAccueil)-1 {
			http.Error(q.w, "tout ne peut pas être masqué", http.StatusBadRequest)
			return
		}
		m = prefMode{Ordre: d.Ordre, Caches: d.Caches}
	}
	pref := lirePrefMenu(q.u.Menu)
	if d.Mode == "ordi" {
		pref.Accueil.Ordi = m
	} else {
		pref.Accueil.Tel = m
	}
	texte := ""
	if !(pref.Ordi.vide() && pref.Tel.vide() && pref.Accueil.Ordi.vide() && pref.Accueil.Tel.vide()) {
		b, _ := json.Marshal(pref)
		texte = string(b)
	}
	if err := auth.DefinirMenu(q.r.Context(), c.db, q.u.ID, texte); err != nil {
		http.Error(q.w, err.Error(), http.StatusBadRequest)
		return
	}
	c.oublierComptes()
	q.w.WriteHeader(http.StatusNoContent)
}

// parDefaut : aucune disposition enregistrée pour ce mode.
func (p prefMode) parDefaut() bool { return len(p.Ordre) == 0 && len(p.Caches) == 0 }
