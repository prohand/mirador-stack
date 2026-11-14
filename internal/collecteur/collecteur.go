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
	return []collect.Collecteur{&etat{cfg: cfg}}
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
