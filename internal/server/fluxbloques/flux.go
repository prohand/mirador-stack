// SPDX-License-Identifier: GPL-3.0-or-later

package fluxbloques

import (
	"context"
	"strings"
	"sync"
	"time"

	"mirador-stack/internal/protocole"
	"mirador-stack/internal/server/stockage"
)

// Lecture des blocages d'une machine (réglage « Pare-feu », SPEC § 136).
const (
	LectureAuto     = "auto"     // toute ligne de blocage du noyau
	LecturePrefixes = "prefixes" // seulement les lignes de ces préfixes
	LectureNon      = "non"      // rien n'est lu ni compté
)

// Reglage : comment lire le pare-feu d'une machine (ou d'un équipement).
type Reglage struct {
	Lecture      string   `json:"lecture"`
	Prefixes     []string `json:"prefixes,omitempty"`
	Entrants     bool     `json:"entrants"`
	Sortants     bool     `json:"sortants"`
	GarderLignes bool     `json:"garder_lignes"` // lignes du pare-feu gardées aussi dans les logs
}

// ReglageDefaut : lecture automatique des entrants, lignes non gardées
// (choix du 27/11).
func ReglageDefaut() Reglage { return Reglage{Lecture: LectureAuto, Entrants: true} }

// Limites du réglage.
const (
	MaxPrefixes      = 10
	TailleMaxPrefixe = 40
)

// Normaliser borne et nettoie le réglage (valeurs inconnues : défaut).
func (r Reglage) Normaliser() Reglage {
	if r.Lecture != LecturePrefixes && r.Lecture != LectureNon {
		r.Lecture = LectureAuto
	}
	var ps []string
	for _, p := range r.Prefixes {
		p = strings.TrimSpace(p)
		if p == "" || len(p) > TailleMaxPrefixe || strings.ContainsAny(p, "\n\r\t") || len(ps) >= MaxPrefixes {
			continue
		}
		ps = append(ps, p)
	}
	r.Prefixes = ps
	if r.Lecture == LecturePrefixes && len(ps) == 0 {
		r.Lecture = LectureAuto
	}
	return r
}

// accepte : le sens est-il lu pour ce réglage ?
func (r Reglage) accepte(sens string) bool {
	if sens == protocole.Sortant {
		return r.Sortants
	}
	return r.Entrants
}

// prefixeOK : la ligne passe-t-elle le filtre des préfixes ?
func (r Reglage) prefixeOK(msg string) bool {
	if r.Lecture != LecturePrefixes {
		return true
	}
	p := protocole.PrefixeLigne(msg)
	for _, x := range r.Prefixes {
		if strings.HasPrefix(p, x) {
			return true
		}
	}
	return false
}

// Flux : les deux sens (deux magasins, deux bases) et le réglage de chaque
// machine.
type Flux struct {
	Entrants, Sortants *Magasin

	mu       sync.RWMutex
	reglages map[string]Reglage // par nom de machine ; absent : défaut
}

// NouveauFlux assemble les deux magasins.
func NouveauFlux(entrants, sortants *Magasin) *Flux {
	return &Flux{Entrants: entrants, Sortants: sortants, reglages: map[string]Reglage{}}
}

// Sens : le magasin d'un sens (entrants par défaut).
func (x *Flux) Sens(sens string) *Magasin {
	if sens == protocole.Sortant {
		return x.Sortants
	}
	return x.Entrants
}

// DefinirReglages remplace les réglages (lus en base par la console).
func (x *Flux) DefinirReglages(r map[string]Reglage) {
	c := make(map[string]Reglage, len(r))
	for k, v := range r {
		c[k] = v.Normaliser()
	}
	x.mu.Lock()
	x.reglages = c
	x.mu.Unlock()
}

// Reglage d'une machine (défaut si rien n'est réglé).
func (x *Flux) Reglage(machine string) Reglage {
	x.mu.RLock()
	defer x.mu.RUnlock()
	if r, ok := x.reglages[machine]; ok {
		return r
	}
	return ReglageDefaut()
}

// Lignes : branchement sur l'ingestion des logs, avant écriture.
//   - Comptages d'un collecteur (ident IdentFlux) : comptés selon le
//     réglage de l'équipement, jamais rangés dans les logs.
//   - Lignes du pare-feu du noyau (« IN=… OUT=… SRC=… ») : comptées selon
//     le réglage de la machine, gardées dans les logs seulement si
//     « garder les lignes » est coché ou si la lecture est désactivée.
//   - Une ligne de comptage envoyée par un agent reste un log ordinaire
//     (seul un collecteur peut annoncer N blocages).
func (x *Flux) Lignes(collecteur bool, lignes []stockage.Ligne) []stockage.Ligne {
	garder := lignes[:0]
	for _, l := range lignes {
		comptage := collecteur && l.Champs["ident"] == protocole.IdentFlux
		hote := l.Champs["host"]
		r := x.Reglage(hote)
		pareFeu := comptage || (r.Lecture != LectureNon && strings.Contains(l.Msg, "SRC=") && strings.Contains(l.Msg, "IN=") &&
			strings.Contains(l.Msg, "OUT=") && r.prefixeOK(l.Msg))
		if pareFeu && r.Lecture != LectureNon {
			if f, ok := protocole.LireFluxBloque(l.Msg, comptage); ok && r.accepte(f.Sens) {
				x.Sens(f.Sens).Recevoir(hote, f, time.UnixMilli(l.T))
			}
		}
		if comptage || (pareFeu && !r.GarderLignes) {
			continue
		}
		garder = append(garder, l)
	}
	return garder
}

// Boucle : écriture et nettoyage des deux magasins.
func (x *Flux) Boucle(ctx context.Context) {
	var wg sync.WaitGroup
	for _, m := range []*Magasin{x.Entrants, x.Sortants} {
		wg.Add(1)
		go func() { defer wg.Done(); m.Boucle(ctx) }()
	}
	wg.Wait()
}
