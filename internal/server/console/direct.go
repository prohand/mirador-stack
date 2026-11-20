// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"encoding/json"
	"io"
	"net/http"
	"time"

	"mirador-stack/internal/protocole"
	"mirador-stack/internal/server/flotte"
)

// « En direct » sur la page d'une machine (SPEC § 97, maquette validée le
// 20/11) : la page ouvre un flux (EventSource, GET /machines/{nom}/direct)
// et reçoit un relevé par seconde. Tant que ce flux est ouvert, le serveur
// demande à l'agent ses relevés ; à la fermeture de la page (ou quand elle
// est cachée 30 s), plus rien n'est demandé et l'agent s'arrête dans les 30 s.
// Un flux dure 15 min au plus : la page propose alors « Continuer en direct ».

const (
	dureeMaxDirect = 15 * time.Minute
	battementFlux  = 15 * time.Second // ligne vide : garde la connexion (proxy, nginx)
	delaiEcritFlux = 10 * time.Second // une page qui ne lit plus est coupée
)

func (c *Console) fluxDirect(q *req) {
	m, ok := c.machine(q)
	if !ok || c.opt.Direct == nil {
		http.Error(q.w, "introuvable", http.StatusNotFound)
		return
	}
	if !flotte.ACapacite(m.Capacites, protocole.CapaciteDirect) {
		// Ancien agent : la page affiche « agent trop ancien ».
		http.Error(q.w, "agent trop ancien", http.StatusConflict)
		return
	}
	a, avant, ok := c.opt.Direct.Abonner(m.ID)
	if !ok {
		http.Error(q.w, "trop de pages en direct", http.StatusTooManyRequests)
		return
	}
	defer c.opt.Direct.Desabonner(a)
	h := q.w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no") // nginx : pas de mise en tampon
	rc := http.NewResponseController(q.w)
	envoyer := func(evenement string, v any) bool {
		_ = rc.SetWriteDeadline(time.Now().Add(delaiEcritFlux))
		b, _ := json.Marshal(v)
		if _, err := io.WriteString(q.w, "event: "+evenement+"\ndata: "+string(b)+"\n\n"); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	q.w.WriteHeader(http.StatusOK)
	// Relevés déjà en mémoire (moins de 5 min) : les courbes se remplissent tout de suite.
	if !envoyer("debut", map[string]any{"points": avant, "max": int(dureeMaxDirect / time.Second)}) {
		return
	}
	fin := time.NewTimer(dureeMaxDirect)
	defer fin.Stop()
	bat := time.NewTicker(battementFlux)
	defer bat.Stop()
	for {
		select {
		case <-q.r.Context().Done():
			return
		case d := <-a.C:
			if !envoyer("point", d) {
				return
			}
		case <-bat.C:
			_ = rc.SetWriteDeadline(time.Now().Add(delaiEcritFlux))
			if _, err := io.WriteString(q.w, ": \n\n"); err != nil || rc.Flush() != nil {
				return
			}
		case <-fin.C:
			envoyer("fin", map[string]string{"raison": "15 min"})
			return
		}
	}
}
