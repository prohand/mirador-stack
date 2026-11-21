// SPDX-License-Identifier: GPL-3.0-or-later

package flotte

import (
	"sort"
	"sync"
	"time"
)

// Bouton « Vérifier maintenant » d'une sonde (SPEC § 108). Les demandes
// sont gardées en mémoire (perdues au redémarrage du serveur : sans
// conséquence, la sonde tourne de toute façon à son rythme) et données au
// collecteur dans la réponse à sa demande de configuration, déjà ouverte en
// permanence (aucun port ouvert sur le collecteur). Les sondes du serveur
// passent par EssaisServeur.

// MachineServeur : « machine » des sondes faites par le serveur lui-même.
const MachineServeur int64 = 0

// garderEssai : au-delà, une demande non prise est oubliée.
const garderEssai = 5 * time.Minute

var demandesEssai = struct {
	mu sync.Mutex
	m  map[int64]map[string]int64 // machine → sonde → numéro (ns)
}{m: map[int64]map[string]int64{}}

// essaisServeur : sondes du serveur à essayer tout de suite (canal borné :
// une demande de trop est perdue, la précédente suffit).
var essaisServeur = make(chan string, 16)

// EssaisServeur : lu par la boucle des sondes du serveur.
func EssaisServeur() <-chan string { return essaisServeur }

// DemanderEssai : essai immédiat de la sonde nom, faite par machine.
func DemanderEssai(machine int64, nom string, t time.Time) {
	if machine == MachineServeur {
		select {
		case essaisServeur <- nom:
		default:
		}
		return
	}
	demandesEssai.mu.Lock()
	if demandesEssai.m[machine] == nil {
		demandesEssai.m[machine] = map[string]int64{}
	}
	demandesEssai.m[machine][nom] = t.UnixNano()
	demandesEssai.mu.Unlock()
	SignalerConfig()
}

// essaisDe : demandes plus récentes que vu, et le numéro de la plus récente.
// Les demandes trop anciennes sont retirées au passage.
func essaisDe(machine, vu int64, t time.Time) (dernier int64, noms []string) {
	demandesEssai.mu.Lock()
	defer demandesEssai.mu.Unlock()
	limite := t.Add(-garderEssai).UnixNano()
	for nom, n := range demandesEssai.m[machine] {
		if n < limite {
			delete(demandesEssai.m[machine], nom)
			continue
		}
		dernier = max(dernier, n)
		if n > vu {
			noms = append(noms, nom)
		}
	}
	sort.Strings(noms)
	return dernier, noms
}
