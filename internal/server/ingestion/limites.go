// SPDX-License-Identifier: GPL-3.0-or-later

package ingestion

import (
	"hash/fnv"
	"sync"
	"time"

	"mirador-stack/internal/server/stockage"
)

// --- Limite de débit (seau à jetons) ----------------------------------------------

// Limiteur : un seau à jetons par clé (IP, machine…). Le nombre de clés suivies
// est borné : une avalanche d'adresses ne peut pas épuiser la mémoire.
type Limiteur struct {
	debit, rafale float64 // jetons par seconde, taille du seau
	maxCles       int

	mu    sync.Mutex
	seaux map[string]*seau
}

type seau struct {
	jetons  float64
	dernier int64 // nanosecondes Unix
}

// NouveauLimiteur : debit jetons par seconde, rafale jetons au plus.
func NouveauLimiteur(debit, rafale float64, maxCles int) *Limiteur {
	return &Limiteur{debit: debit, rafale: rafale, maxCles: maxCles, seaux: make(map[string]*seau)}
}

// Prendre retire n jetons pour cette clé. Faux si le seau est vide.
// Le seau peut passer en négatif (lot plus gros que le reste) : la clé
// attend alors d'avoir remboursé avant la requête suivante.
func (l *Limiteur) Prendre(cle string, n float64, maintenant time.Time) bool {
	t := maintenant.UnixNano()
	l.mu.Lock()
	defer l.mu.Unlock()
	s, ok := l.seaux[cle]
	if !ok && n == 0 {
		// Simple vérification d'une clé inconnue : seau plein, rien n'est
		// créé (une avalanche d'adresses ne remplit pas la table et ne fait
		// pas refuser les vraies machines, revue du 11/11).
		return true
	}
	if !ok {
		if len(l.seaux) >= l.maxCles {
			l.nettoyer(t)
			if len(l.seaux) >= l.maxCles {
				return false // trop de clés actives : on refuse plutôt que grossir
			}
		}
		s = &seau{jetons: l.rafale, dernier: t}
		l.seaux[cle] = s
	}
	s.jetons = min(l.rafale, s.jetons+float64(t-s.dernier)/1e9*l.debit)
	s.dernier = t
	if s.jetons <= 0 {
		return false
	}
	s.jetons -= n
	return true
}

// Attente : secondes conseillées avant de réessayer (en-tête Retry-After).
func (l *Limiteur) Attente(cle string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	s, ok := l.seaux[cle]
	if !ok || s.jetons > 0 || l.debit <= 0 {
		return 1
	}
	return max(1, int(-s.jetons/l.debit)+1)
}

// nettoyer retire les seaux pleins : les recréer donne le même résultat.
func (l *Limiteur) nettoyer(t int64) {
	for cle, s := range l.seaux {
		if s.jetons+float64(t-s.dernier)/1e9*l.debit >= l.rafale {
			delete(l.seaux, cle)
		}
	}
}

// Nettoyer : à appeler de temps en temps pour rendre la mémoire.
func (l *Limiteur) Nettoyer(maintenant time.Time) {
	l.mu.Lock()
	l.nettoyer(maintenant.UnixNano())
	l.mu.Unlock()
}

// --- Quotas et cardinalité des séries --------------------------------------------

// LimitesSeries : nombre de séries actives (vues depuis moins d'Inactivite).
type LimitesSeries struct {
	ParMachine int           // séries par machine
	ParNom     int           // séries d'un même nom de métrique, par machine (cardinalité)
	Total      int           // toutes machines : protège la mémoire du stockage
	Inactivite time.Duration // une série muette depuis plus longtemps ne compte plus
}

// LimitesSeriesDefaut : un agent produit ~600 à 1 000 séries ; 40 000 au
// total tiennent dans le budget mémoire mesuré (~100 Mo pour 30 000, § 16.1).
func LimitesSeriesDefaut() LimitesSeries {
	return LimitesSeries{ParMachine: 5000, ParNom: 2000, Total: 40_000, Inactivite: time.Hour}
}

// Quotas suit les séries actives de chaque machine. Une nouvelle série
// au-delà d'une limite est refusée ; les séries déjà connues passent
// toujours (une étiquette qui explose ne coupe pas les métriques normales).
type Quotas struct {
	lim LimitesSeries

	mu       sync.Mutex
	total    int
	machines map[int64]*seriesMachine
}

type seriesMachine struct {
	vues   map[uint64]vue // empreinte de la série → dernier passage
	parNom map[string]int
}

type vue struct {
	nom string // __name__ (chaîne partagée avec parNom)
	t   int64  // secondes Unix
}

// NouveauxQuotas prépare le suivi des séries.
func NouveauxQuotas(lim LimitesSeries) *Quotas {
	return &Quotas{lim: lim, machines: make(map[int64]*seriesMachine)}
}

// Filtrer garde les séries autorisées (dans le même tableau) et renvoie le
// nombre de séries refusées.
func (q *Quotas) Filtrer(machine int64, series []stockage.Serie, maintenant time.Time) ([]stockage.Serie, int) {
	t := maintenant.Unix()
	q.mu.Lock()
	defer q.mu.Unlock()
	m := q.machines[machine]
	if m == nil {
		m = &seriesMachine{vues: make(map[uint64]vue), parNom: make(map[string]int)}
		q.machines[machine] = m
	}
	gardees, refusees := series[:0], 0
	for _, s := range series {
		e := empreinteSerie(&s)
		if v, ok := m.vues[e]; ok {
			v.t = t
			m.vues[e] = v
			gardees = append(gardees, s)
			continue
		}
		nom := s.Valeur("__name__")
		if len(m.vues) >= q.lim.ParMachine || m.parNom[nom] >= q.lim.ParNom || q.total >= q.lim.Total {
			refusees++
			continue
		}
		m.vues[e] = vue{nom: nom, t: t}
		m.parNom[nom]++
		q.total++
		gardees = append(gardees, s)
	}
	return gardees, refusees
}

// Nettoyer oublie les séries muettes depuis plus que Inactivite.
func (q *Quotas) Nettoyer(maintenant time.Time) {
	limite := maintenant.Add(-q.lim.Inactivite).Unix()
	q.mu.Lock()
	defer q.mu.Unlock()
	for id, m := range q.machines {
		for e, v := range m.vues {
			if v.t < limite {
				delete(m.vues, e)
				if m.parNom[v.nom]--; m.parNom[v.nom] <= 0 {
					delete(m.parNom, v.nom)
				}
				q.total--
			}
		}
		if len(m.vues) == 0 {
			delete(q.machines, id)
		}
	}
}

// Actives renvoie le nombre de séries actives d'une machine (0 = inconnue).
func (q *Quotas) Actives(machine int64) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	if m := q.machines[machine]; m != nil {
		return len(m.vues)
	}
	return 0
}

// empreinteSerie : FNV-1a 64 bits des étiquettes (triées). Une collision ne
// ferait que compter deux séries pour une : sans effet sur les données.
func empreinteSerie(s *stockage.Serie) uint64 {
	h := fnv.New64a()
	for _, l := range s.Labels {
		_, _ = h.Write([]byte(l.Nom))
		_, _ = h.Write([]byte{0xff})
		_, _ = h.Write([]byte(l.Valeur))
		_, _ = h.Write([]byte{0xfe})
	}
	return h.Sum64()
}

// --- Décalage d'horloge -----------------------------------------------------------

// LimitesHorloge : au-delà de Tolerance, l'horodatage est corrigé ; au-delà
// de Maximum, la requête est refusée (HTTP 400 : l'agent abandonne le lot,
// car des données à une heure fausse fausseraient les alertes).
type LimitesHorloge struct {
	Tolerance, Maximum time.Duration
}

// LimitesHorlogeDefaut : 30 s tolérées (NTP absent quelques heures),
// 10 min au plus corrigées.
func LimitesHorlogeDefaut() LimitesHorloge {
	return LimitesHorloge{Tolerance: 30 * time.Second, Maximum: 10 * time.Minute}
}

// Decision sur l'horloge d'une requête.
type Decision int

// Décisions possibles.
const (
	HeureBonne   Decision = iota // rien à faire
	HeureCorrige                 // horodatages décalés de -Decalage
	HeureRefuse                  // requête refusée
)

// Juger compare l'heure de l'agent au moment de l'envoi (en-tête
// X-Mirador-Heure) à celle du serveur. Le temps de transport (< 1 s
// d'habitude, compté dans la tolérance) n'est pas retranché.
func (h LimitesHorloge) Juger(heureAgent, maintenant time.Time) (Decision, time.Duration) {
	d := heureAgent.Sub(maintenant)
	a := d
	if a < 0 {
		a = -a
	}
	switch {
	case a <= h.Tolerance:
		return HeureBonne, d
	case a <= h.Maximum:
		return HeureCorrige, d
	default:
		return HeureRefuse, d
	}
}
