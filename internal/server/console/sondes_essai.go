// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"errors"
	"net/url"
	"strconv"
	"sync"
	"time"

	"mirador-stack/internal/protocole"
	"mirador-stack/internal/server/flotte"
	essais "mirador-stack/internal/sondes"
)

// Bouton « Vérifier maintenant » d'une sonde (demande du 21/11, SPEC
// § 108). Sonde du serveur : essai tout de suite. Sonde d'un collecteur :
// demande donnée au collecteur dans sa prochaine réponse de configuration
// (dans la seconde, sa demande étant toujours ouverte) ; il faut un
// collecteur qui sait le faire (capacité « sonder »).

// intervalleEssai : un essai demandé par sonde toutes les 10 s au plus.
const intervalleEssai = 10 * time.Second

// fenetreEssai : après une demande, la page se met à jour toute seule
// pendant ce temps (résultat attendu en quelques secondes).
const fenetreEssai = 30 * time.Second

var essaisRecents = struct {
	mu sync.Mutex
	m  map[string]time.Time
}{m: map[string]time.Time{}}

// essaiPossible : le bouton est-il utilisable, et sinon pourquoi.
func essaiPossible(s essais.Sonde, collecteurs map[string]flotte.InfoMachine) (bool, string) {
	if s.Depuis == essais.ServeurLocal {
		return true, ""
	}
	m, ok := collecteurs[s.Depuis]
	if !ok {
		return false, "collecteur « " + s.Depuis + " » inconnu"
	}
	if !flotte.ACapacite(m.Capacites, protocole.CapaciteSonder) {
		return false, "collecteur « " + s.Depuis + " » à mettre à jour pour ce bouton"
	}
	return true, ""
}

// collecteursParNom : machines de type collecteur, par nom.
func (c *Console) collecteursParNom(ctx context.Context) map[string]flotte.InfoMachine {
	r := map[string]flotte.InfoMachine{}
	if ms, err := flotte.ListerMachines(ctx, c.db); err == nil {
		for _, m := range ms {
			if m.Type == "collecteur" {
				r[m.Nom] = m
			}
		}
	}
	return r
}

// enEssai : la page a été ouverte juste après une demande (?essai=heure).
func enEssai(q *req) bool {
	t, err := strconv.ParseInt(q.r.URL.Query().Get("essai"), 10, 64)
	return err == nil && q.t.Sub(time.Unix(t, 0)) < fenetreEssai && q.t.Unix() >= t
}

func (c *Console) postEssaiSonde(q *req) {
	ctx := q.r.Context()
	nom := q.r.PathValue("nom")
	retour := retourLocal(champs{q}.texte("retour"))
	if retour == "/" {
		retour = "/sondes/" + url.PathEscape(nom)
	}
	err := func() error {
		liste, err := flotte.LireSondes(ctx, c.db)
		if err != nil {
			return err
		}
		var s *essais.Sonde
		for i := range liste {
			if liste[i].Nom == nom {
				s = &liste[i]
			}
		}
		if s == nil {
			return errors.New("sonde introuvable")
		}
		cols := c.collecteursParNom(ctx)
		if ok, pourquoi := essaiPossible(*s, cols); !ok {
			return errors.New(pourquoi)
		}
		essaisRecents.mu.Lock()
		dernier := essaisRecents.m[nom]
		if q.t.Sub(dernier) < intervalleEssai {
			essaisRecents.mu.Unlock()
			return errors.New("essai déjà demandé il y a moins de 10 s")
		}
		essaisRecents.m[nom] = q.t
		essaisRecents.mu.Unlock()
		machine := flotte.MachineServeur
		if s.Depuis != essais.ServeurLocal {
			machine = cols[s.Depuis].ID
		}
		flotte.DemanderEssai(machine, nom, q.t)
		return nil
	}()
	if err == nil {
		retour = avecEssai(retour, q.t)
	}
	c.retour(q, retour, err, "essai de « "+nom+" » demandé : résultat dans quelques secondes")
}

// avecEssai : ajoute ?essai=heure à l'adresse de retour (mise à jour
// automatique de la page pendant fenetreEssai).
func avecEssai(retour string, t time.Time) string {
	u, err := url.Parse(retour)
	if err != nil {
		return retour
	}
	v := u.Query()
	v.Set("essai", strconv.FormatInt(t.Unix(), 10))
	u.RawQuery = v.Encode()
	return u.String()
}
