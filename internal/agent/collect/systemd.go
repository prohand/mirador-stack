// SPDX-License-Identifier: GPL-3.0-or-later

package collect

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"mirador-stack/internal/agent/metrics"
)

// Systemd produit (SPEC.md § 4.2) :
//   - node_systemd_unit_state{name,state} pour les unités .service, .timer,
//     .mount, états « active » et « failed » seulement (comme Alloy) ;
//   - node_systemd_service_restart_total{name} (règle ServiceRedemarreEnBoucle) ;
//   - node_systemd_unit_enabled_active{name}, node_systemd_enabled_units_total
//     et node_systemd_enabled_units_down (remplace obs-systemd.sh).
//
// Coût : l'état des unités vient d'UN appel D-Bus (ListUnits) à chaque
// passage (60 s). Les redémarrages et l'analyse « activé mais arrêté »
// demandent un appel par service : ils sont refaits toutes les 5 minutes
// seulement, et la dernière valeur est renvoyée entre-temps.
//
// Aucune commande n'est lancée : pas de systemctl, tout passe par D-Bus en
// lecture seule (autorisé à un utilisateur sans droits).
type Systemd struct {
	source     sourceSystemd
	ignorer    map[string]bool // services à ne pas surveiller (activé mais arrêté)
	periode    time.Duration   // rafraîchissement des données coûteuses
	delai      time.Duration   // délai maximal d'un passage
	maintenant func() time.Time

	chargees []uniteEtat // tampon : unités chargées et suivies

	mu          sync.Mutex
	rafraichi   time.Time
	redemarrage map[string]float64 // nom → NRestarts
	actives     []serviceActive    // services activés au démarrage, retenus
}

// uniteEtat : une unité chargée et son état (ActiveState).
type uniteEtat struct {
	Nom, Etat string
	Charge    string // LoadState : « loaded », « not-found », « masked »…
	Chemin    string // chemin D-Bus de l'unité (évite un appel LoadUnit)
}

// proprietesService : ce qu'il faut savoir d'un service pour décider s'il
// « devrait tourner » (mêmes règles qu'obs-systemd.sh, SPEC.md § 4.2).
type proprietesService struct {
	Type          string // simple, forking, oneshot…
	ConditionOK   bool   // ConditionResult
	DeclencheePar int    // nombre d'éléments de TriggeredBy (socket, timer, path)
	Etat          string // ActiveState
	CodeSortie    int32  // ExecMainCode (1 = sorti de lui-même)
	StatutSortie  int32  // ExecMainStatus (code de retour)
	DebutMono     uint64 // ExecMainStartTimestampMonotonic (µs)
	FinMono       uint64 // ExecMainExitTimestampMonotonic (µs)
}

// sourceSystemd isole D-Bus : la vraie source parle à systemd, celle des
// tests renvoie des valeurs fixes.
type sourceSystemd interface {
	Unites(ctx context.Context) ([]uniteEtat, error)
	Redemarrages(ctx context.Context, u uniteEtat) (uint32, error)
	ServicesActivesAuDemarrage(ctx context.Context) ([]string, error)
	Proprietes(ctx context.Context, service string) (proprietesService, error)
}

type serviceActive struct {
	nom    string
	tourne bool
}

// NewSystemd crée le collecteur. ignorer : services exclus de la surveillance
// « activé mais arrêté » (ancien /etc/obs-systemd-ignore).
func NewSystemd(ignorer []string) *Systemd {
	s := newSystemdAvecSource(&sourceDBus{}, ignorer)
	return s
}

func newSystemdAvecSource(src sourceSystemd, ignorer []string) *Systemd {
	ign := make(map[string]bool, len(ignorer))
	for _, n := range ignorer {
		ign[n] = true
	}
	return &Systemd{
		source:      src,
		ignorer:     ign,
		periode:     5 * time.Minute,
		delai:       10 * time.Second,
		maintenant:  time.Now,
		redemarrage: make(map[string]float64),
	}
}

func (s *Systemd) Nom() string { return "systemd" }
func (s *Systemd) Lent() bool  { return true }

// Types d'unités suivis (comme unit_include d'Alloy).
func uniteSuivie(nom string) bool {
	return strings.HasSuffix(nom, ".service") || strings.HasSuffix(nom, ".timer") ||
		strings.HasSuffix(nom, ".mount")
}

func (s *Systemd) Collecter(b *metrics.Batch) error {
	ctx, annuler := context.WithTimeout(context.Background(), s.delai)
	defer annuler()

	toutes, err := s.source.Unites(ctx)
	if err != nil {
		return err
	}
	// Seules les unités réellement chargées comptent (comme node_exporter) :
	// une unité « not-found » est seulement référencée par une autre.
	// Filtrage dans un tampon du collecteur (réutilisé d'un passage à
	// l'autre) : la liste reçue de la source n'est jamais modifiée.
	unites := s.chargees[:0]
	for _, u := range toutes {
		if u.Charge == "loaded" && uniteSuivie(u.Nom) {
			unites = append(unites, u)
		}
	}
	s.chargees = unites
	for _, u := range unites {
		// Une seule série par unité : state="failed" (0 ou 1). C'est la seule
		// que les règles et dashboards utilisent ; la série state="active"
		// d'Alloy doublait le nombre de séries sans servir (mesure du 09/11).
		b.Add("node_systemd_unit_state", bool01(u.Etat == "failed"),
			metrics.L("name", u.Nom), metrics.L("state", "failed"))
	}

	if s.maintenant().Sub(s.rafraichi) >= s.periode {
		s.rafraichir(ctx, unites)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for nom, v := range s.redemarrage {
		b.Add("node_systemd_service_restart_total", v, metrics.L("name", nom))
	}
	arretes := 0
	for _, sa := range s.actives {
		b.Add("node_systemd_unit_enabled_active", bool01(sa.tourne), metrics.L("name", sa.nom))
		if !sa.tourne {
			arretes++
		}
	}
	if !s.rafraichi.IsZero() {
		b.Add("node_systemd_enabled_units_total", float64(len(s.actives)))
		b.Add("node_systemd_enabled_units_down", float64(arretes))
	}
	return nil
}

// rafraichir relit les données coûteuses (un appel D-Bus par service).
func (s *Systemd) rafraichir(ctx context.Context, unites []uniteEtat) {
	redem := make(map[string]float64)
	for _, u := range unites {
		if !strings.HasSuffix(u.Nom, ".service") {
			continue
		}
		if n, err := s.source.Redemarrages(ctx, u); err == nil {
			redem[u.Nom] = float64(n)
		}
	}

	var actives []serviceActive
	if noms, err := s.source.ServicesActivesAuDemarrage(ctx); err == nil {
		sort.Strings(noms)
		for _, nom := range noms {
			if strings.HasSuffix(nom, "@.service") || s.ignorer[nom] {
				continue // 1. unité modèle, 6. liste d'exclusion
			}
			p, err := s.source.Proprietes(ctx, nom)
			if err != nil || p.Etat == "" {
				continue // 0. on ne sait rien : ne pas inventer
			}
			if !devraitTourner(p) {
				continue
			}
			actives = append(actives, serviceActive{
				nom:    nom,
				tourne: p.Etat == "active" || p.Etat == "activating" || p.Etat == "reloading",
			})
		}
	}

	s.mu.Lock()
	s.redemarrage = redem
	s.actives = actives
	s.rafraichi = s.maintenant()
	s.mu.Unlock()
}

// dureeMaxTacheDemarrage : un service « simple » qui s'arrête seul, code 0,
// en moins de 2 minutes, est une tâche de démarrage (ex. dmesg.service).
const dureeMaxTacheDemarrage = 120 * 1_000_000 // µs

// devraitTourner applique les exclusions d'obs-systemd.sh (SPEC.md § 4.2).
func devraitTourner(p proprietesService) bool {
	switch {
	case p.Type == "oneshot": // 2. conçu pour se terminer
		return false
	case !p.ConditionOK: // 3. conditions non remplies, démarrage sauté
		return false
	case p.DeclencheePar > 0: // 4. activé à la demande (socket, timer, path)
		return false
	case p.Etat == "inactive" && p.CodeSortie == 1 && p.StatutSortie == 0 &&
		p.DebutMono > 0 && p.FinMono >= p.DebutMono && p.FinMono-p.DebutMono < dureeMaxTacheDemarrage:
		return false // 5. tâche de démarrage terminée normalement
	}
	return true
}

func bool01(v bool) float64 {
	if v {
		return 1
	}
	return 0
}
