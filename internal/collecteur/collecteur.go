// SPDX-License-Identifier: GPL-3.0-or-later

// Package collecteur : mirador-collector (SPEC § 23.13), le collecteur du
// LAN. Il relève les équipements sans agent (syslog, SNMP, sondes, UniFi :
// parties 4 à 6 de l'étape 6) et envoie au serveur avec le même code que
// mirador-agent (file, remote write, configuration distante, mise à jour
// signée). C'est le seul programme de mirador qui écoute, et seulement sur
// une adresse privée du LAN.
//
// Partie 3 : le squelette (enrôlement, configuration, mise à jour, état).
package collecteur

import (
	"strconv"

	"mirador-stack/internal/agent/collect"
	"mirador-stack/internal/agent/config"
	"mirador-stack/internal/agent/metrics"
	"mirador-stack/internal/collecteur/compagnon"
	"mirador-stack/internal/version"
)

// Job : étiquette « job » des mesures propres du collecteur.
const Job = "mirador-collector"

// Collecteurs : ce que le collecteur mesure à chaque cycle.
//
// CPU, charge, mémoire, disques, réseau et systèmes de fichiers de sa
// machine (retour du 22/11 : sa tuile et sa page n'avaient pas les jauges et
// courbes des machines Linux) : les collecteurs de l'agent, mêmes séries
// node_*. Coût mesuré : voir SPEC § 120.1.
func Collecteurs(cfg config.Agent) []collect.Collecteur {
	lecteur := compagnon.NouveauLecteur()
	return []collect.Collecteur{&etat{cfg: cfg, compagnon: lecteur}, &adresses{proc: cfg.RacineProc},
		collect.NewCPU(cfg.RacineProc),
		collect.NewCharge(cfg.RacineProc),
		collect.NewMemoire(cfg.RacineProc),
		collect.NewDisques(cfg.RacineProc, cfg.ExclureDisques, cfg.IgnorerDisques...),
		collect.NewReseau(cfg.RacineProc, cfg.ExclureInterfaces, cfg.IgnorerInterfaces...),
		collect.NewSystemesFichiers(cfg.RacineProc, cfg.ExclureMontages, cfg.ExclureTypesFS, cfg.ExclureTypesFSDistant, cfg.IgnorerMontages...),
		&processus{l: lecteur},
	}
}

// processus : les 5 processus les plus gourmands de la machine, relevés
// chaque minute par le compagnon sans réseau (« cause probable » des
// alertes, SPEC § 98 et § 120.1). Mêmes séries que l'agent. Sans
// compagnon (pas encore installé) : rien, plutôt qu'une liste réduite au
// collecteur lui-même.
type processus struct{ l *compagnon.Lecteur }

func (p *processus) Nom() string { return "processus" }
func (p *processus) Lent() bool  { return true }

func (p *processus) Collecter(b *metrics.Batch) error {
	cpu, mem, ok := p.l.Minute()
	if !ok {
		return nil
	}
	for i, x := range cpu {
		b.Add("mirador_processus_cpu_pourcent", x.CPU, metrics.L("rang", strconv.Itoa(i+1)), metrics.L("nom", x.Nom), metrics.L("groupe", x.Groupe))
	}
	for i, x := range mem {
		b.Add("mirador_processus_memoire_octets", x.Memoire, metrics.L("rang", strconv.Itoa(i+1)), metrics.L("nom", x.Nom), metrics.L("groupe", x.Groupe))
	}
	return nil
}

// adresses : adresses des interfaces du collecteur (même série que l'agent,
// mirador_adresse_info), pour proposer l'écoute syslog dans l'interface
// (retour du 22/11 : choisir l'interface au lieu de taper l'adresse).
type adresses struct{ proc string }

func (a *adresses) Nom() string { return "adresses" }
func (a *adresses) Lent() bool  { return true }

func (a *adresses) Collecter(b *metrics.Batch) error {
	for _, x := range collect.AdressesLocales(a.proc) {
		p := "0"
		if x.Principale {
			p = "1"
		}
		b.Add("mirador_adresse_info", 1, metrics.L("interface", x.Interface), metrics.L("adresse", x.IP), metrics.L("principale", p))
	}
	return nil
}

// etat : état du collecteur lui-même (équipements rattachés, version).
type etat struct {
	cfg       config.Agent
	compagnon *compagnon.Lecteur
}

func (e *etat) Nom() string { return "collecteur" }
func (e *etat) Lent() bool  { return false }

func (e *etat) Collecter(b *metrics.Batch) error {
	b.Add("mirador_collecteur_info", 1, metrics.L("version", version.Version))
	b.Add("mirador_collecteur_equipements", float64(len(e.cfg.Collecteur.Equipements)))
	// Compagnon des processus en marche (page « Santé de mirador »).
	if e.compagnon != nil {
		b.Add("mirador_collecteur_compagnon", bool01(e.compagnon.Present()))
	}
	return nil
}

func bool01(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
