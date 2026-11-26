// SPDX-License-Identifier: GPL-3.0-or-later

// Package charge compte le temps de calcul du serveur par partie (retour du
// 26/11 : 4,9 % d'un cœur en moyenne pour un objectif de 5 %, sans savoir
// qui consomme). Des compteurs simples, sans outil de profilage exposé :
// temps passé dans chaque partie (horloge murale, proche du CPU pour ces
// traitements sans attente), temps CPU réel du processus (getrusage) et
// part du ramasse-miettes (runtime/metrics). Coût : deux lectures d'horloge
// par requête traitée.
package charge

import (
	"runtime/metrics"
	"sync/atomic"
	"syscall"
	"time"
)

// Partie : un morceau du serveur dont on compte le temps.
type Partie int

// Parties comptées.
const (
	Mesures   Partie = iota // réception des métriques (agents, collecteurs, Telegraf)
	Logs                    // réception des logs
	Interface               // pages de l'interface (hors flux en direct)
	Fond                    // tâches de fond : agrégation, métriques internes
	nbParties
)

// Noms : étiquette « partie » des séries, dans l'ordre des constantes.
var Noms = [nbParties]string{"mesures", "logs", "interface", "fond"}

var cumuls [nbParties]atomic.Int64

// Noter ajoute le temps écoulé depuis debut à la partie p :
// defer charge.Noter(charge.Mesures, time.Now()).
func Noter(p Partie, debut time.Time) {
	if p >= 0 && p < nbParties {
		cumuls[p].Add(int64(time.Since(debut)))
	}
}

// Cumul : temps total passé dans la partie p depuis le démarrage.
func Cumul(p Partie) time.Duration { return time.Duration(cumuls[p].Load()) }

// CPUProcessus : temps CPU (utilisateur + système) du processus.
func CPUProcessus() time.Duration {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

const metriqueGC = "/cpu/classes/gc/total:cpu-seconds"

// CPUGC : temps CPU estimé du ramasse-miettes (runtime/metrics).
func CPUGC() time.Duration {
	s := []metrics.Sample{{Name: metriqueGC}}
	metrics.Read(s)
	if s[0].Value.Kind() != metrics.KindFloat64 {
		return 0
	}
	return time.Duration(s[0].Value.Float64() * float64(time.Second))
}
