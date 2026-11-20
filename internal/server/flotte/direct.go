// SPDX-License-Identifier: GPL-3.0-or-later

package flotte

import (
	"net/http"
	"strconv"
	"sync"
	"time"

	"mirador-stack/internal/protocole"
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

// recevoirDirect : un relevé « en direct » de l'agent (SPEC § 97). La
// réponse donne les secondes restantes (0 : l'agent s'arrête). Aucun relevé
// n'est accepté sans page qui regarde la machine.
func (f *Flotte) recevoirDirect(w http.ResponseWriter, r *http.Request) {
	m, liberer, ok := f.opt.Ingestion.Accueillir(w, r)
	if !ok {
		return
	}
	defer liberer()
	if f.opt.Direct == nil {
		w.Header().Set(protocole.EnteteDirect, "0")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var d protocole.Direct
	if err := lireJSON(r, &d); err != nil {
		erreur(w, http.StatusBadRequest, "relevé illisible")
		return
	}
	if err := d.Valider(); err != nil {
		erreur(w, http.StatusBadRequest, "relevé invalide")
		return
	}
	d.T = f.opt.Maintenant().UnixMilli() // heure du serveur : les courbes ne dépendent pas de l'horloge de l'agent
	w.Header().Set(protocole.EnteteDirect, strconv.Itoa(f.opt.Direct.Recevoir(m.ID, d)))
	w.WriteHeader(http.StatusNoContent)
}
