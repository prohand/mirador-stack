// SPDX-License-Identifier: GPL-3.0-or-later

package flotte

import (
	"strconv"
	"sync"
	"time"
)

// Configuration en temps réel (demande du 12/11, SPEC § 22). L'agent
// n'ouvre aucun port : c'est lui qui garde une demande ouverte vers le
// serveur (GET de la configuration avec « X-Mirador-Attendre »). Le serveur
// ne répond que lorsque la configuration de la machine change, qu'une
// actualisation est demandée dans l'interface, ou au bout de l'attente
// (45 s au plus, sous le délai d'écriture du serveur et de nginx).
//
// Coût : une goroutine et une connexion TLS ouverte par machine (quelques
// dizaines de Ko), aucune requête en base pendant l'attente.

// AttenteMaxConfig : attente la plus longue accordée à un agent.
const AttenteMaxConfig = 45 * time.Second

// signalConfig : canal fermé (puis remplacé) à chaque changement.
var signalConfig = struct {
	mu sync.Mutex
	ch chan struct{}
}{ch: make(chan struct{})}

// SignalerConfig réveille tous les agents en attente : ils recalculent leur
// configuration et reçoivent la nouvelle si elle les concerne.
func SignalerConfig() {
	signalConfig.mu.Lock()
	close(signalConfig.ch)
	signalConfig.ch = make(chan struct{})
	signalConfig.mu.Unlock()
}

func attenteSignal() <-chan struct{} {
	signalConfig.mu.Lock()
	defer signalConfig.mu.Unlock()
	return signalConfig.ch
}

// actualisations : machine → numéro de la dernière demande « Actualiser »
// (horodatage en nanosecondes). En mémoire : perdu au redémarrage du
// serveur, sans conséquence (l'agent relit tout de même sa configuration).
var actualisations = struct {
	mu sync.Mutex
	m  map[int64]int64
}{m: map[int64]int64{}}

// DemanderActualisation : bouton « Actualiser » d'une machine. L'agent relit
// sa configuration et relance l'assistant root, dans la seconde s'il est
// en attente.
func DemanderActualisation(machine int64, maintenant time.Time) {
	actualisations.mu.Lock()
	actualisations.m[machine] = maintenant.UnixNano()
	actualisations.mu.Unlock()
	SignalerConfig()
}

func actualisationDe(machine int64) int64 {
	actualisations.mu.Lock()
	defer actualisations.mu.Unlock()
	return actualisations.m[machine]
}

// lireAttente : durée demandée par l'agent (0 = réponse immédiate).
func lireAttente(v string) time.Duration {
	s, err := strconv.Atoi(v)
	if err != nil || s <= 0 {
		return 0
	}
	return min(time.Duration(s)*time.Second, AttenteMaxConfig)
}
