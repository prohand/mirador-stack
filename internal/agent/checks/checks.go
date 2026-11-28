// SPDX-License-Identifier: GPL-3.0-or-later

// Package checks : contrôles demandés par les modèles de surveillance
// (SPEC.md § 24.10). Que des mesures en lecture : une requête GET, une
// connexion TCP ouverte puis fermée, une résolution DNS, un écho ICMP, la
// lecture de /proc et de dates de fichiers. Jamais de commande.
//
// Les checks tournent à part, en tâche de fond (toutes les 60 s, dossiers
// toutes les 15 min), au plus 8 à la fois, chacun avec son délai (10 s par
// défaut, 30 s au plus) : un site lent ne retarde pas la collecte. Le
// collecteur renvoie les derniers résultats à chaque passage.
package checks

import (
	"context"
	"sync"
	"time"

	"mirador-stack/internal/agent/config"
	"mirador-stack/internal/agent/metrics"
)

// Résultat d'un check.
type resultat struct {
	ok     bool
	duree  time.Duration
	valeur map[string]float64 // mesures propres au type (code, âge, taille…)
	le     time.Time
}

// Moteur : fait les checks et garde leurs résultats.
type Moteur struct {
	checks []config.Check // tels que reçus (étiquettes des mesures)
	proc   string         // racine de /proc (tests)
	opt    Options

	// Liste effective (garde du « contient », contient.go), lue et changée
	// seulement par la tâche de fond : eff[i] est fait si actifs[i] ;
	// revoir : heure où un changement mis de côté pourra être accepté.
	eff    []config.Check
	actifs []bool
	revoir time.Time

	demarre sync.Once
	mu      sync.Mutex
	res     map[int]resultat

	// Remplaçables dans les tests.
	periode, periodeDossier time.Duration
	maintenant              func() time.Time
	faire                   func(ctx context.Context, c config.Check) resultat
}

// paralleles : checks en même temps au plus.
const paralleles = 8

// Nouveau prépare le moteur sans réglage local (tests, configuration
// locale seule) : « contient » permis, sans délai entre deux changements.
func Nouveau(checks []config.Check, proc string) *Moteur {
	return NouveauAvec(checks, proc, Options{})
}

// NouveauAvec prépare le moteur avec les réglages locaux de l'agent (rien
// ne tourne avant le premier passage du collecteur).
func NouveauAvec(checks []config.Check, proc string, o Options) *Moteur {
	m := &Moteur{checks: checks, proc: proc, opt: o, res: map[int]resultat{},
		periode: time.Minute, periodeDossier: 15 * time.Minute, maintenant: time.Now}
	m.faire = m.executer
	m.appliquerContient(time.Now())
	return m
}

func (m *Moteur) Nom() string { return "checks" }
func (m *Moteur) Lent() bool  { return false }

// Collecter : derniers résultats (le premier appel lance la tâche de fond,
// qui vit autant que l'agent : il redémarre à chaque changement de
// configuration).
func (m *Moteur) Collecter(b *metrics.Batch) error {
	m.demarre.Do(func() { go m.tourner(context.Background()) })
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, c := range m.checks {
		r, ok := m.res[i]
		if !ok {
			continue // pas encore fait
		}
		l := [4]metrics.Label{metrics.L("type", c.Type), metrics.L("nom", c.Nom), metrics.L("modele", c.Modele), metrics.L("cible", c.Cible)}
		b.Add("mirador_check_ok", bool01(r.ok), l[:]...)
		b.Add("mirador_check_duree_secondes", r.duree.Seconds(), l[:]...)
		for nom, v := range r.valeur {
			b.Add(nom, v, l[:]...)
		}
	}
	return nil
}

// tourner : passe sur tous les checks à leur rythme.
func (m *Moteur) tourner(ctx context.Context) {
	prochain := map[int]time.Time{}
	for {
		maintenant := m.maintenant()
		// Un « contient » mis de côté devient permis : liste revue.
		if !m.revoir.IsZero() && !maintenant.Before(m.revoir) {
			m.appliquerContient(maintenant)
		}
		var lot []int
		for i, c := range m.eff {
			if !m.actifs[i] || maintenant.Before(prochain[i]) {
				continue
			}
			p := m.periode
			if c.Type == "dossier" {
				p = m.periodeDossier
			}
			prochain[i] = maintenant.Add(p)
			lot = append(lot, i)
		}
		m.passer(ctx, lot)
		select {
		case <-ctx.Done():
			return
		case <-time.After(m.periode / 6):
		}
	}
}

// passer : fait un lot de checks, 8 à la fois au plus.
func (m *Moteur) passer(ctx context.Context, lot []int) {
	sem := make(chan struct{}, paralleles)
	var wg sync.WaitGroup
	for _, i := range lot {
		sem <- struct{}{}
		wg.Add(1)
		go func(i int) {
			defer func() { <-sem; wg.Done() }()
			c := m.eff[i]
			ctxC, fin := context.WithTimeout(ctx, c.DelaiEffectif())
			r := m.faire(ctxC, c)
			fin()
			r.le = m.maintenant()
			m.mu.Lock()
			m.res[i] = r
			m.mu.Unlock()
		}(i)
	}
	wg.Wait()
}

// executer : un check selon son type.
func (m *Moteur) executer(ctx context.Context, c config.Check) resultat {
	debut := time.Now()
	var r resultat
	switch c.Type {
	case "http":
		r = checkHTTP(ctx, c)
	case "tcp":
		r = checkTCP(ctx, c)
	case "dns":
		r = checkDNS(ctx, c)
	case "ping":
		r = checkPing(ctx, c)
	case "processus":
		r = checkProcessus(m.proc, c)
	case "fichier":
		r = checkFichier(c, time.Now())
	case "dossier":
		r = checkDossier(ctx, c)
	}
	if r.duree == 0 {
		r.duree = time.Since(debut)
	}
	return r
}

func bool01(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
