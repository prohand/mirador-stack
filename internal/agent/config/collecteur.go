// SPDX-License-Identifier: GPL-3.0-or-later

package config

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"strconv"
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
	// Syslog : réception syslog TLS sur le LAN (partie 4, SPEC § 23.14).
	Syslog *Syslog `json:"syslog,omitempty"`
}

// Syslog : écoute syslog TLS du collecteur. Seuls les émetteurs listés sont
// acceptés, chacun rangé sous le nom d'un équipement rattaché.
type Syslog struct {
	// Ecoute : adresse IP PRIVÉE du collecteur et port (ex. 192.168.1.10:6514) ;
	// jamais 0.0.0.0 ni une adresse publique.
	Ecoute string `json:"ecoute"`
	// NomCertificat : nom DNS du certificat (celui que les émetteurs
	// utilisent pour joindre le collecteur), obtenu par le serveur (ACME).
	NomCertificat string `json:"nom_certificat"`
	// Emetteurs : adresse (ou réseau CIDR) → équipement.
	Emetteurs []Emetteur `json:"emetteurs,omitempty"`
	// GarderFilterlog : garder les lignes « filterlog » du pare-feu d'OPNsense
	// (très nombreuses ; jetées par défaut, comptées).
	GarderFilterlog bool `json:"garder_filterlog,omitempty"`
	// Debit, Rafale : lignes par seconde et par émetteur (défaut 500, 1000).
	Debit  float64 `json:"debit,omitempty"`
	Rafale float64 `json:"rafale,omitempty"`
	// Certificat : chaîne PEM obtenue par le serveur pour la clé de ce
	// collecteur (posée par le serveur, jamais saisie à la main).
	Certificat string `json:"certificat,omitempty"`
}

// Emetteur : machine autorisée à envoyer du syslog.
type Emetteur struct {
	Adresse string `json:"adresse"` // IP ou réseau CIDR
	Machine string `json:"machine"` // équipement rattaché au collecteur
}

// Bornes du syslog.
const (
	MaxEmetteurs       = 200
	SyslogDebitDefaut  = 500
	SyslogRafaleDefaut = 1000
	tailleMaxCertPEM   = 32 << 10
)

var reNomDNS = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)

// AdressePrivee : adresse du LAN (RFC 1918, RFC 4193), jamais publique,
// jamais « toutes les adresses ».
func AdressePrivee(a netip.Addr) bool {
	return a.IsValid() && a.IsPrivate() && !a.IsUnspecified()
}

// Valider : adresse d'écoute privée, port non privilégié, émetteurs connus.
func (s *Syslog) Valider(equipements []string) error {
	hote, port, err := net.SplitHostPort(s.Ecoute)
	if err != nil {
		return errors.New("syslog : écoute « adresse:port » attendue (ex. 192.168.1.10:6514)")
	}
	a, err := netip.ParseAddr(hote)
	if err != nil || !AdressePrivee(a) {
		return errors.New("syslog : l'adresse d'écoute doit être une adresse IP privée du LAN (jamais 0.0.0.0 ni une adresse publique)")
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1024 || n > 65535 {
		return errors.New("syslog : port de 1024 à 65535 (6514 conseillé)")
	}
	if len(s.NomCertificat) > 253 || !reNomDNS.MatchString(s.NomCertificat) {
		return errors.New("syslog : nom du certificat invalide (nom DNS en minuscules, ex. syslog.lan.exemple.org)")
	}
	if len(s.Emetteurs) > MaxEmetteurs {
		return fmt.Errorf("syslog : %d émetteurs au plus", MaxEmetteurs)
	}
	connus := map[string]bool{}
	for _, e := range equipements {
		connus[e] = true
	}
	for _, e := range s.Emetteurs {
		if !reNomEquipement.MatchString(e.Machine) {
			return fmt.Errorf("syslog : émetteur %s : nom d'équipement invalide", e.Adresse)
		}
		if _, err := LireEmetteur(e.Adresse); err != nil {
			return fmt.Errorf("syslog : émetteur %q : %w", e.Adresse, err)
		}
		// Équipements connus (configuration complète du collecteur) : chaque
		// émetteur doit en être un. Réglage d'une portée seule (sans la liste,
		// posée par le serveur) : le serveur retire lui-même les émetteurs
		// d'équipements non rattachés.
		if len(connus) > 0 && !connus[e.Machine] {
			return fmt.Errorf("syslog : émetteur %s : %q n'est pas un équipement rattaché à ce collecteur", e.Adresse, e.Machine)
		}
	}
	if s.Debit < 0 || s.Debit > 10000 || s.Rafale < 0 || s.Rafale > 50000 {
		return errors.New("syslog : débit 0 à 10 000 lignes/s, rafale 0 à 50 000")
	}
	if len(s.Certificat) > tailleMaxCertPEM {
		return errors.New("syslog : certificat trop grand")
	}
	return nil
}

// LireEmetteur : adresse IP ou réseau CIDR d'un émetteur (privé, comme lui).
func LireEmetteur(v string) (netip.Prefix, error) {
	if p, err := netip.ParsePrefix(v); err == nil {
		if !AdressePrivee(p.Addr()) {
			return netip.Prefix{}, errors.New("réseau du LAN attendu")
		}
		return p.Masked(), nil
	}
	a, err := netip.ParseAddr(v)
	if err != nil {
		return netip.Prefix{}, errors.New("adresse IP ou réseau CIDR attendu")
	}
	if !AdressePrivee(a) {
		return netip.Prefix{}, errors.New("adresse du LAN attendue")
	}
	return netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen()), nil
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
	if c.Syslog != nil {
		if err := c.Syslog.Valider(c.Equipements); err != nil {
			return err
		}
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
