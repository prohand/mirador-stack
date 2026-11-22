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
	"mirador-stack/internal/agent/collect"
	"mirador-stack/internal/agent/config"
	"mirador-stack/internal/agent/metrics"
	"mirador-stack/internal/version"
)

// Job : étiquette « job » des mesures propres du collecteur.
const Job = "mirador-collector"

// Collecteurs : ce que le collecteur mesure à chaque cycle.
func Collecteurs(cfg config.Agent) []collect.Collecteur {
	return []collect.Collecteur{&etat{cfg: cfg}, &adresses{proc: cfg.RacineProc}}
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
type etat struct{ cfg config.Agent }

func (e *etat) Nom() string { return "collecteur" }
func (e *etat) Lent() bool  { return false }

func (e *etat) Collecter(b *metrics.Batch) error {
	b.Add("mirador_collecteur_info", 1, metrics.L("version", version.Version))
	b.Add("mirador_collecteur_equipements", float64(len(e.cfg.Collecteur.Equipements)))
	return nil
}
