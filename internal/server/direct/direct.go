// SPDX-License-Identifier: GPL-3.0-or-later

// Package direct : mode « en direct » de la page d'une machine (SPEC § 97).
//
// Tant qu'une page regarde une machine (abonné), le serveur demande à son
// agent un relevé par seconde. Les relevés restent en mémoire (5 min par
// machine au plus) : ils ne sont jamais écrits dans la base de métriques.
//
// Déroulé :
//   - la page s'abonne (Abonner) : la demande est posée pour 30 s et l'agent
//     de la machine est réveillé (Signal) s'il attend sa configuration ;
//   - l'agent envoie ses relevés (Recevoir) ; chaque relevé prolonge la
//     demande de 30 s tant qu'une page regarde ;
//   - la page part : plus rien ne prolonge, l'agent s'arrête dans les 30 s.
package direct

import (
	"sync"
	"time"

	"mirador-stack/internal/protocole"
)

const (
	// Garde : durée d'une demande, prolongée à chaque relevé tant qu'une page regarde.
	Garde = 30 * time.Second
	// Gardes : relevés gardés par machine (5 min à une seconde).
	Gardes = 300
	// MaxAbonnes : pages ouvertes en direct, toutes machines (borne mémoire et connexions).
	MaxAbonnes = 50
	// oubli : une machine sans page ni relevé depuis ce délai est oubliée.
	oubli = 10 * time.Minute
)

// Abonne : une page qui regarde une machine.
type Abonne struct {
	C       chan protocole.Direct // relevés (perdus si la page ne suit pas)
	machine int64
}

type etat struct {
	expire  time.Time
	points  []protocole.Direct // du plus ancien au plus récent
	abonnes map[*Abonne]struct{}
	signal  chan struct{} // fermé pour réveiller l'agent
	vu      time.Time     // dernier relevé ou dernier abonnement
}

// Hub : état du direct de toutes les machines.
type Hub struct {
	mu         sync.Mutex
	machines   map[int64]*etat
	nbAbonnes  int
	maintenant func() time.Time
}

// Nouveau prépare le hub.
func Nouveau() *Hub {
	return &Hub{machines: map[int64]*etat{}, maintenant: time.Now}
}

// etatDe : état de la machine (créé au besoin) ; vu est mis à jour : une
// machine dont l'agent attend sa configuration n'est jamais oubliée (sinon
// le canal sur lequel il attend serait perdu et il ne serait plus réveillé).
func (h *Hub) etatDe(machine int64) *etat {
	e := h.machines[machine]
	if e == nil {
		e = &etat{abonnes: map[*Abonne]struct{}{}, signal: make(chan struct{})}
		h.machines[machine] = e
	}
	e.vu = h.maintenant()
	return e
}

// oublier : machines sans page ni relevé depuis longtemps (appelé sous verrou).
func (h *Hub) oublier(t time.Time) {
	for id, e := range h.machines {
		if len(e.abonnes) == 0 && t.Sub(e.vu) > oubli {
			delete(h.machines, id)
		}
	}
}

// Abonner : une page commence à regarder une machine. Renvoie les relevés
// déjà en mémoire (pour remplir les courbes tout de suite) ; nil, false si
// trop de pages sont déjà ouvertes en direct.
func (h *Hub) Abonner(machine int64) (*Abonne, []protocole.Direct, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.nbAbonnes >= MaxAbonnes {
		return nil, nil, false
	}
	t := h.maintenant()
	h.oublier(t)
	e := h.etatDe(machine)
	a := &Abonne{C: make(chan protocole.Direct, 8), machine: machine}
	e.abonnes[a] = struct{}{}
	h.nbAbonnes++
	e.vu = t
	if e.expire.Before(t.Add(Garde)) {
		e.expire = t.Add(Garde)
	}
	// Réveille l'agent qui attend sa configuration.
	close(e.signal)
	e.signal = make(chan struct{})
	return a, append([]protocole.Direct(nil), e.points...), true
}

// Desabonner : la page est fermée ou cachée. La demande n'est plus
// prolongée : l'agent s'arrête au plus tard 30 s après.
func (h *Hub) Desabonner(a *Abonne) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if e := h.machines[a.machine]; e != nil {
		if _, ok := e.abonnes[a]; ok {
			delete(e.abonnes, a)
			h.nbAbonnes--
		}
	}
}

// Reste : secondes de direct restantes pour la machine (0 : pas de direct).
func (h *Hub) Reste(machine int64) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.resteSousVerrou(machine)
}

func (h *Hub) resteSousVerrou(machine int64) int {
	e := h.machines[machine]
	if e == nil {
		return 0
	}
	r := e.expire.Sub(h.maintenant())
	if r <= 0 {
		return 0
	}
	return int((r + time.Second - 1) / time.Second)
}

// Signal : canal fermé quand une page commence à regarder la machine
// (réveille la demande de configuration gardée ouverte de son agent).
func (h *Hub) Signal(machine int64) <-chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.etatDe(machine).signal
}

// Recevoir : un relevé de l'agent (déjà validé). Renvoie les secondes de
// direct restantes (0 : l'agent s'arrête). Un relevé reçu sans demande
// en cours est ignoré.
func (h *Hub) Recevoir(machine int64, d protocole.Direct) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	t := h.maintenant()
	e := h.machines[machine]
	if e == nil || !e.expire.After(t) {
		return 0
	}
	e.vu = t
	if len(e.abonnes) > 0 {
		e.expire = t.Add(Garde) // une page regarde encore : on prolonge
	}
	if len(e.points) >= Gardes {
		copy(e.points, e.points[1:])
		e.points = e.points[:Gardes-1]
	}
	e.points = append(e.points, d)
	for a := range e.abonnes {
		select {
		case a.C <- d:
		default: // page lente : ce relevé est perdu pour elle
		}
	}
	return h.resteSousVerrou(machine)
}

// NbAbonnes : pages ouvertes en direct (mesures, tests).
func (h *Hub) NbAbonnes() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.nbAbonnes
}
