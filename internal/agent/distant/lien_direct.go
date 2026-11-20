// SPDX-License-Identifier: GPL-3.0-or-later

package distant

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"mirador-stack/internal/agent/direct"
	"mirador-stack/internal/protocole"
)

// Direct (SPEC § 97) : tant que le serveur le demande (une page regarde la
// machine), un relevé par seconde. Chaque réponse redonne les secondes
// restantes ; 0, une erreur de forme ou 3 échecs d'affilée : arrêt. Bornes
// de sécurité côté agent : 16 min au plus d'affilée (le serveur coupe à 15),
// jamais plus d'un relevé par seconde.
const (
	rythmeDirect = time.Second
	maxDirect    = 16 * time.Minute
)

func (l *Lien) boucleDirect(ctx context.Context) {
	defer l.enDirect.Store(false)
	relever := l.releveDirect
	if relever == nil {
		relever = direct.Nouveau().Relever
	}
	relever(time.Now()) // compteurs de départ : le premier envoi a déjà des débits
	debut := time.Now()
	t := time.NewTicker(rythmeDirect)
	defer t.Stop()
	echecs := 0
	l.Journal.Debug("direct : début")
	for time.Since(debut) < maxDirect {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		corps, err := json.Marshal(relever(time.Now()))
		if err != nil || len(corps) > protocole.TailleMaxDirect {
			return
		}
		ctxReq, annuler := context.WithTimeout(ctx, 5*time.Second)
		resp, _, err := l.requete(ctxReq, http.MethodPost, protocole.CheminDirect, corps,
			map[string]string{"Content-Type": "application/json"}, 1<<10)
		annuler()
		if err != nil || resp.StatusCode/100 != 2 {
			if echecs++; echecs >= 3 {
				return
			}
			continue
		}
		echecs = 0
		reste, err := strconv.Atoi(resp.Header.Get(protocole.EnteteDirect))
		if err != nil || reste <= 0 {
			l.Journal.Debug("direct : fin")
			return
		}
	}
}

// Signaler : une panne vue par l'agent (SPEC § 98) est envoyée au serveur,
// qui la note et évalue les alertes tout de suite. Serveur trop ancien
// (404) : plus rien n'est envoyé jusqu'au prochain démarrage.
func (l *Lien) Signaler(ctx context.Context, e protocole.EvenementAgent) {
	if l.sansEvenements.Load() {
		return
	}
	corps, err := json.Marshal(e)
	if err != nil {
		return
	}
	go func() {
		ctxReq, annuler := context.WithTimeout(ctx, 10*time.Second)
		defer annuler()
		resp, _, err := l.requete(ctxReq, http.MethodPost, protocole.CheminEvenement, corps,
			map[string]string{"Content-Type": "application/json"}, 1<<10)
		switch {
		case err != nil:
			l.Journal.Warn("panne non signalée au serveur", "erreur", err)
		case resp.StatusCode == http.StatusNotFound:
			l.sansEvenements.Store(true)
		case resp.StatusCode/100 != 2:
			l.Journal.Warn("panne non signalée au serveur", "statut", resp.StatusCode)
		}
	}()
}
