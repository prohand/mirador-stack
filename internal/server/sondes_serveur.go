// SPDX-License-Identifier: GPL-3.0-or-later

package server

import (
	"context"
	"encoding/json"
	"net"
	"sync"
	"time"

	"mirador-stack/internal/server/flotte"
	"mirador-stack/internal/server/stockage"
	essais "mirador-stack/internal/sondes"
)

// Sondes faites depuis le serveur (SPEC § 48) : « vue de l'extérieur ».
// La liste est relue toutes les 30 s ; une tâche par sonde. Résultats
// écrits dans la base des métriques comme ceux du collecteur (job="sondes",
// instance="mirador-server").
//
// Adresses refusées depuis le serveur : boucle locale, réseaux privés,
// lien local, multicast, non spécifiée. Une sonde ne peut donc pas viser
// le serveur lui-même ni les conteneurs voisins (Docker).

type sondeur struct {
	s         *Serveur
	mu        sync.Mutex
	taches    map[string]context.CancelFunc // réglage (JSON) → arrêt
	signaux   map[string]chan struct{}      // nom → « essayer maintenant »
	resultats map[string]essais.Resultat    // nom → dernier essai (page Sondes)
}

// refuserDepuisServeur : adresses interdites aux sondes du serveur.
func refuserDepuisServeur(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified()
}

func (x *sondeur) boucle(ctx context.Context) {
	x.taches = map[string]context.CancelFunc{}
	x.signaux = map[string]chan struct{}{}
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		x.ajuster(ctx)
		if !x.attendre(ctx, t.C) {
			return
		}
	}
}

// attendre : jusqu'au prochain tour (faux : arrêt). Pendant l'attente, les
// demandes « Vérifier maintenant » (SPEC § 108) sont passées aux sondes.
func (x *sondeur) attendre(ctx context.Context, tour <-chan time.Time) bool {
	for {
		select {
		case <-ctx.Done():
			return false
		case <-tour:
			return true
		case nom := <-flotte.EssaisServeur():
			x.mu.Lock()
			if ch := x.signaux[nom]; ch != nil {
				select {
				case ch <- struct{}{}:
				default: // un essai est déjà demandé
				}
			}
			x.mu.Unlock()
		}
	}
}

// ajuster : lance les sondes nouvelles, arrête celles retirées ou changées.
func (x *sondeur) ajuster(ctx context.Context) {
	liste, err := flotte.LireSondes(ctx, x.s.db)
	if err != nil {
		x.s.journal.Warn("sondes du serveur", "erreur", err)
		return
	}
	voulues := map[string]essais.Sonde{}
	for _, s := range liste {
		if s.Depuis != essais.ServeurLocal {
			continue
		}
		cle, _ := json.Marshal(s)
		voulues[string(cle)] = s
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	for cle, arreter := range x.taches {
		if _, ok := voulues[cle]; !ok {
			arreter()
			delete(x.taches, cle)
		}
	}
	for nom := range x.signaux {
		garder := false
		for _, s := range voulues {
			garder = garder || s.Nom == nom
		}
		if !garder {
			delete(x.signaux, nom)
		}
	}
	for cle, s := range voulues {
		if _, ok := x.taches[cle]; ok {
			continue
		}
		c, arreter := context.WithCancel(ctx)
		x.taches[cle] = arreter
		ch := make(chan struct{}, 1)
		x.signaux[s.Nom] = ch
		go essais.Tourner(c, s, essais.Options{Refuser: refuserDepuisServeur, Maintenant: ch}, func(s essais.Sonde, r essais.Resultat) {
			x.rendre(c, s, r)
		})
	}
}

// rendre : séries d'un essai, comme celles du collecteur.
func (x *sondeur) rendre(ctx context.Context, s essais.Sonde, r essais.Resultat) {
	x.mu.Lock()
	if x.resultats == nil {
		x.resultats = map[string]essais.Resultat{}
	}
	x.resultats[s.Nom] = r
	x.mu.Unlock()
	ms := time.Now().UnixMilli()
	var series []stockage.Serie
	ajout := func(nom string, v float64, extra ...string) {
		var se stockage.Serie
		se.Remplacer("__name__", nom)
		se.Remplacer("instance", InstanceServeur)
		se.Remplacer("job", "sondes")
		for _, e := range essais.Etiquettes(s) {
			se.Remplacer(e[0], e[1])
		}
		for k := 0; k+1 < len(extra); k += 2 {
			se.Remplacer(extra[k], extra[k+1])
		}
		se.Points = []stockage.Point{{T: ms, V: v}}
		series = append(series, se)
	}
	for nom, v := range essais.Valeurs(s, r) {
		ajout(nom, v)
	}
	if !r.OK {
		ajout("mirador_sonde_erreur", 1, "erreur", r.Erreur)
	}
	if err := x.s.met.Ecrire(ctx, series); err != nil && ctx.Err() == nil {
		x.s.journal.Warn("sonde du serveur : écriture", "sonde", s.Nom, "erreur", err)
	}
}
