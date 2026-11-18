// SPDX-License-Identifier: GPL-3.0-or-later

package config

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"time"

	essais "mirador-stack/internal/sondes"
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
	// SNMP : équipements relevés en SNMP v3 (partie 5, SPEC § 23.15).
	SNMP []CibleSNMP `json:"snmp,omitempty"`
	// Sondes : certificats TLS distants lus par le collecteur (partie 5).
	Sondes []Sonde `json:"sondes,omitempty"`
	// UniFi : contrôleur UniFi lu en lecture seule (partie 6, SPEC § 23.16).
	UniFi *UniFi `json:"unifi,omitempty"`
	// Services : sondes HTTP, TCP, ping, DNS faites depuis le LAN (SPEC
	// § 48), posées par le serveur (page Sondes), jamais saisies ici.
	Services []essais.Sonde `json:"services,omitempty"`
}

// UniFi : contrôleur UniFi Network (UniFi OS ou logiciel), lu par son API en
// lecture seule (compte local en lecture seule, ou clé API).
type UniFi struct {
	Machine     string `json:"machine"`                // équipement rattaché (instance)
	Adresse     string `json:"adresse"`                // https://IP-ou-nom-du-LAN[:port]
	Utilisateur string `json:"utilisateur,omitempty"`  // compte local en lecture seule…
	MotDePasse  string `json:"mot_de_passe,omitempty"` // … et son mot de passe (chiffré sur le serveur)
	CleAPI      string `json:"cle_api,omitempty"`      // ou clé API (UniFi OS ; chiffrée sur le serveur)
	// Empreinte : SHA-256 (hexadécimal) du certificat du contrôleur, pour un
	// certificat fait maison ; vide = certificat vérifié normalement.
	Empreinte  string   `json:"empreinte,omitempty"`
	Sites      []string `json:"sites,omitempty"`      // vide = tous
	Intervalle Duree    `json:"intervalle,omitempty"` // 30 s à 10 min (défaut 60 s)
}

var (
	reEmpreinte = regexp.MustCompile(`^[0-9a-f]{64}$`)
	reSiteUniFi = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
)

// validerUniFi : adresse https du LAN, un moyen d'authentification.
func validerUniFi(u *UniFi, connus map[string]bool) error {
	if u == nil {
		return nil
	}
	if !reNomEquipement.MatchString(u.Machine) || (len(connus) > 0 && !connus[u.Machine]) {
		return fmt.Errorf("unifi : %q n'est pas un équipement rattaché à ce collecteur", u.Machine)
	}
	reste, ok := strings.CutPrefix(u.Adresse, "https://")
	if !ok || strings.ContainsAny(reste, "/?#@") {
		return errors.New("unifi : adresse https://hôte[:port] attendue (sans chemin)")
	}
	if _, err := hoteDuLAN(reste, true); err != nil {
		return fmt.Errorf("unifi : %w", err)
	}
	switch {
	case u.CleAPI != "" && (u.Utilisateur != "" || u.MotDePasse != ""):
		return errors.New("unifi : clé API ou compte, pas les deux")
	case u.CleAPI != "" && !(strings.HasPrefix(u.CleAPI, PrefixeSecretChiffre) && len(u.CleAPI) <= tailleMaxSecretChiffre ||
		len(u.CleAPI) >= 16 && len(u.CleAPI) <= 128 && !strings.ContainsAny(u.CleAPI, " \r\n")):
		return errors.New("unifi : clé API invalide")
	case u.CleAPI == "" && (!reUtilisateurSNMP.MatchString(u.Utilisateur) || !(strings.HasPrefix(u.MotDePasse, PrefixeSecretChiffre) &&
		len(u.MotDePasse) <= tailleMaxSecretChiffre || len(u.MotDePasse) >= 1 && len(u.MotDePasse) <= 128)):
		return errors.New("unifi : compte local (utilisateur et mot de passe) ou clé API obligatoire")
	case u.Empreinte != "" && !reEmpreinte.MatchString(u.Empreinte):
		return errors.New("unifi : empreinte SHA-256 en hexadécimal (64 caractères) attendue")
	case u.Intervalle != 0 && (time.Duration(u.Intervalle) < 30*time.Second || time.Duration(u.Intervalle) > 10*time.Minute):
		return errors.New("unifi : intervalle de 30 s à 10 min")
	case len(u.Sites) > 50:
		return errors.New("unifi : 50 sites au plus")
	}
	for _, s := range u.Sites {
		if !reSiteUniFi.MatchString(s) {
			return fmt.Errorf("unifi : site %q invalide", s)
		}
	}
	return nil
}

// CibleSNMP : un équipement relevé en SNMP v3, en lecture seule, avec une
// liste d'OID figée dans le code (préréglage). Authentification et
// chiffrement obligatoires (authPriv).
type CibleSNMP struct {
	Machine     string `json:"machine"`              // équipement rattaché (instance)
	Adresse     string `json:"adresse"`              // IP du LAN[:port] (161 par défaut)
	Prereglage  string `json:"prereglage"`           // « synology » ou « standard »
	Utilisateur string `json:"utilisateur"`          // utilisateur SNMP v3 (lecture seule)
	Auth        string `json:"auth"`                 // SHA, SHA256, SHA512
	MotDePasse  string `json:"mot_de_passe"`         // authentification (chiffré sur le serveur)
	Chiffrement string `json:"chiffrement"`          // AES, AES256
	CleChiffre  string `json:"cle_chiffrement"`      // chiffrement (chiffré sur le serveur)
	Intervalle  Duree  `json:"intervalle,omitempty"` // 30 s à 10 min (défaut 60 s)
}

// Sonde : certificat TLS d'un service du LAN (lu sans être validé : seule
// son échéance compte).
type Sonde struct {
	Machine string `json:"machine"`       // équipement rattaché (instance)
	Adresse string `json:"adresse"`       // hôte:port (IP ou nom du LAN)
	Nom     string `json:"nom,omitempty"` // nom présenté (SNI) ; défaut : l'hôte
}

// Bornes SNMP et sondes.
const (
	MaxCiblesSNMP          = 50
	MaxSondes              = 100
	IntervalleSNMPDefaut   = 60 * time.Second
	PrefixeSecretChiffre   = "chiffre:v1:" // #nosec G101 -- marque d'un secret encore chiffré, pas un secret
	tailleMaxSecretChiffre = 512
)

// Prereglages : listes d'OID connues du collecteur.
var Prereglages = map[string]bool{"synology": true, "standard": true}

var (
	reUtilisateurSNMP = regexp.MustCompile(`^[A-Za-z0-9._@-]{1,32}$`)
	authSNMP          = map[string]bool{"SHA": true, "SHA256": true, "SHA512": true}
	chiffrementSNMP   = map[string]bool{"AES": true, "AES256": true}
)

// secretValide : 8 à 64 caractères (SNMP v3), ou valeur encore chiffrée
// (configuration telle que rangée sur le serveur).
func secretValide(s string) bool {
	if strings.HasPrefix(s, PrefixeSecretChiffre) {
		return len(s) <= tailleMaxSecretChiffre
	}
	return len(s) >= 8 && len(s) <= 64 && !strings.ContainsAny(s, "\x00\r\n")
}

// hoteDuLAN : adresse IP privée (ou nom d'hôte simple) avec port facultatif.
func hoteDuLAN(adresse string, portDefaut bool) (string, error) {
	hote, port, err := net.SplitHostPort(adresse)
	if err != nil {
		if !portDefaut {
			return "", errors.New("« hôte:port » attendu")
		}
		hote, port = adresse, ""
	}
	if port != "" {
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return "", errors.New("port invalide")
		}
	}
	if a, err := netip.ParseAddr(hote); err == nil {
		if !AdressePrivee(a) {
			return "", errors.New("adresse du LAN attendue")
		}
		return hote, nil
	}
	if !reHoteLAN.MatchString(hote) {
		return "", errors.New("adresse IP ou nom d'hôte attendu")
	}
	return hote, nil
}

var reHoteLAN = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$`)

// validerSNMP : cibles SNMP (équipements connus, authPriv obligatoire).
func validerSNMP(cibles []CibleSNMP, connus map[string]bool) error {
	if len(cibles) > MaxCiblesSNMP {
		return fmt.Errorf("snmp : %d équipements au plus", MaxCiblesSNMP)
	}
	vus := map[string]bool{}
	for _, c := range cibles {
		quoi := "snmp " + c.Machine
		switch {
		case !reNomEquipement.MatchString(c.Machine) || (len(connus) > 0 && !connus[c.Machine]):
			return fmt.Errorf("%s : n'est pas un équipement rattaché à ce collecteur", quoi)
		case vus[c.Machine]:
			return fmt.Errorf("%s : en double", quoi)
		case !Prereglages[c.Prereglage]:
			return fmt.Errorf("%s : préréglage inconnu (synology, standard)", quoi)
		case !reUtilisateurSNMP.MatchString(c.Utilisateur):
			return fmt.Errorf("%s : utilisateur SNMP invalide", quoi)
		case !authSNMP[c.Auth] || !chiffrementSNMP[c.Chiffrement]:
			return fmt.Errorf("%s : authentification (SHA, SHA256, SHA512) et chiffrement (AES, AES256) obligatoires", quoi)
		case !secretValide(c.MotDePasse) || !secretValide(c.CleChiffre):
			return fmt.Errorf("%s : mots de passe SNMP de 8 à 64 caractères", quoi)
		case c.Intervalle != 0 && (time.Duration(c.Intervalle) < 30*time.Second || time.Duration(c.Intervalle) > 10*time.Minute):
			return fmt.Errorf("%s : intervalle de 30 s à 10 min", quoi)
		}
		if _, err := hoteDuLAN(c.Adresse, true); err != nil {
			return fmt.Errorf("%s : %w", quoi, err)
		}
		vus[c.Machine] = true
	}
	return nil
}

// validerSondes : certificats distants (équipements connus, adresse du LAN).
func validerSondes(sondes []Sonde, connus map[string]bool) error {
	if len(sondes) > MaxSondes {
		return fmt.Errorf("sondes : %d au plus", MaxSondes)
	}
	vus := map[string]bool{}
	for _, s := range sondes {
		if !reNomEquipement.MatchString(s.Machine) || (len(connus) > 0 && !connus[s.Machine]) {
			return fmt.Errorf("sonde %s : %q n'est pas un équipement rattaché", s.Adresse, s.Machine)
		}
		if _, err := hoteDuLAN(s.Adresse, false); err != nil {
			return fmt.Errorf("sonde %s : %w", s.Adresse, err)
		}
		if s.Nom != "" && !reNomDNS.MatchString(s.Nom) {
			return fmt.Errorf("sonde %s : nom présenté invalide", s.Adresse)
		}
		cle := s.Machine + " " + s.Adresse
		if vus[cle] {
			return fmt.Errorf("sonde %s en double", s.Adresse)
		}
		vus[cle] = true
	}
	return nil
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
	connus := map[string]bool{}
	for _, e := range c.Equipements {
		connus[e] = true
	}
	if err := validerSNMP(c.SNMP, connus); err != nil {
		return err
	}
	if err := validerUniFi(c.UniFi, connus); err != nil {
		return err
	}
	if err := validerSondes(c.Sondes, connus); err != nil {
		return err
	}
	if len(c.Services) > essais.MaxSondes {
		return fmt.Errorf("sondes de services : %d au plus", essais.MaxSondes)
	}
	nomsServices := map[string]bool{}
	for i := range c.Services {
		if err := c.Services[i].Valider(); err != nil {
			return err
		}
		if nomsServices[c.Services[i].Nom] {
			return errors.New("sonde " + c.Services[i].Nom + " en double")
		}
		nomsServices[c.Services[i].Nom] = true
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
