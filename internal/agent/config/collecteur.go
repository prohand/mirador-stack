// SPDX-License-Identifier: GPL-3.0-or-later

package config

import (
	"errors"
	"fmt"
	"regexp"
)

// Collecteur : réglages propres à mirador-collector (SPEC § 23.13), reçus
// dans la clé « collecteur » de la configuration distante (envoyée
// seulement à un binaire qui annonce la capacité « collecteur »). Les
// parties suivantes de l'étape 6 y ajoutent le syslog, le SNMP, les sondes
// et UniFi. Que des valeurs typées : jamais de commande.
type Collecteur struct {
	// Equipements : équipements rattachés à ce collecteur dans l'interface
	// (posé par le serveur, jamais saisi à la main). Le serveur refuse de
	// toute façon les séries d'un équipement non rattaché.
	Equipements []string `json:"equipements,omitempty"`
}

// MaxEquipements : équipements par collecteur.
const MaxEquipements = 500

var reNomEquipement = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

// Valider : noms d'équipements au format des noms de machines, sans doublon.
func (c *Collecteur) Valider() error {
	if len(c.Equipements) > MaxEquipements {
		return fmt.Errorf("%d équipements au plus", MaxEquipements)
	}
	vus := map[string]bool{}
	for _, e := range c.Equipements {
		if !reNomEquipement.MatchString(e) {
			return fmt.Errorf("équipement %q : nom invalide", e)
		}
		if vus[e] {
			return errors.New("équipement " + e + " en double")
		}
		vus[e] = true
	}
	return nil
}

// DefautCollecteur : valeurs par défaut de mirador-collector. Il ne relève
// pas la machine où il tourne (un mirador-agent s'en charge si besoin) :
// pas de systemd, de journal, de Docker ni d'assistant root ; ses fichiers
// sont rangés sous /etc/mirador-collector et /var/lib/mirador-collector.
func DefautCollecteur() Agent {
	a := Defaut()
	a.Systemd, a.MisesAJour, a.Temperatures, a.DockerMetriques = false, false, false, false
	a.EnvoyerPareFeu, a.EnvoyerSmart, a.EnvoyerCertificats, a.CertificatsDistants = false, false, false, false
	a.Journal, a.LogsDocker, a.MajRacine, a.ChecksActifs = false, false, false, false
	a.Certificats = nil
	a.FichierRoot = "/run/mirador-collector/root.json" // pas d'assistant root : jamais lu
	a.FichierCurseur = "/var/lib/mirador-collector/journal.cursor"
	a.FichierToken = "/var/lib/mirador-collector/token" // #nosec G101 -- chemin du fichier (écrit par le collecteur à l'enrôlement), pas un secret
	a.FichierCles = "/etc/mirador-collector/cles-publiques.txt"
	a.FichierJeton = "/var/lib/mirador-collector/jeton"
	a.FichierConfigDistante = "/var/lib/mirador-collector/config-distante.json"
	a.FichierActualiser = ""
	a.DossierBin = "/var/lib/mirador-collector/bin"
	return a
}
