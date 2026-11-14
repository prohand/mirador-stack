// SPDX-License-Identifier: GPL-3.0-or-later

package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Check : un contrôle fait par l'agent (SPEC.md § 24.10, modèles partie B).
// Que des valeurs typées : jamais de commande, de script ni de programme.
//
//	http      : GET d'une adresse http(s) ; code, texte attendu, durée, fin du certificat
//	tcp       : ouverture d'une connexion vers hôte:port
//	processus : nombre de processus de ce nom (comm, 15 caractères) et leur mémoire
//	fichier   : fichier (ou le plus récent d'un motif « * ») : existe, âge, taille
//	dossier   : taille totale et nombre de fichiers d'un dossier (borné)
//	dns       : résolution d'un nom
//	ping      : écho ICMP sans droits (net.ipv4.ping_group_range)
type Check struct {
	Type   string `json:"type"`
	Nom    string `json:"nom"`
	Cible  string `json:"cible"`
	Modele string `json:"modele,omitempty"` // étiquette « modele » des mesures
	// http : texte attendu dans les 64 premiers Kio de la réponse.
	Contient string `json:"contient,omitempty"`
	// http : vérifier le certificat (défaut : oui). Non : site au certificat
	// maison ; la date de fin est quand même mesurée.
	VerifierTLS *bool `json:"verifier_tls,omitempty"`
	// Delai : 1 s à 30 s (défaut 10 s).
	Delai Duree `json:"delai,omitempty"`
}

// Bornes des checks.
const (
	MaxChecks        = 50
	DelaiCheckDefaut = 10 * time.Second
	DelaiCheckMax    = 30 * time.Second
)

// TypesCheck : types connus.
var TypesCheck = map[string]bool{"http": true, "tcp": true, "processus": true, "fichier": true, "dossier": true, "dns": true, "ping": true}

var (
	reNomCheck       = regexp.MustCompile(`^[A-Za-z0-9._:/@-]{1,64}$`)
	reHoteCheck      = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,253}$`)
	reProcessusCheck = regexp.MustCompile(`^[A-Za-z0-9._@:+-]{1,15}$`)
	reModeleCheck    = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
)

// DelaiEffectif : délai du check (défaut 10 s).
func (c Check) DelaiEffectif() time.Duration {
	if c.Delai <= 0 {
		return DelaiCheckDefaut
	}
	return time.Duration(c.Delai)
}

// ValiderChecks : 50 au plus, (type, nom) uniques, cible au bon format.
func ValiderChecks(l []Check) error {
	if len(l) > MaxChecks {
		return fmt.Errorf("checks : %d au plus", MaxChecks)
	}
	vus := map[string]bool{}
	var errs []error
	for i, c := range l {
		if err := c.Valider(); err != nil {
			errs = append(errs, fmt.Errorf("check %d (%s %s) : %w", i+1, c.Type, c.Nom, err))
			continue
		}
		cle := c.Type + "|" + c.Nom + "|" + c.Modele
		if vus[cle] {
			errs = append(errs, fmt.Errorf("check %s %s en double", c.Type, c.Nom))
		}
		vus[cle] = true
	}
	return errors.Join(errs...)
}

// Valider : un check.
func (c Check) Valider() error {
	if !TypesCheck[c.Type] {
		return fmt.Errorf("type %q inconnu", c.Type)
	}
	if !reNomCheck.MatchString(c.Nom) {
		return errors.New("nom invalide (64 caractères : lettres, chiffres, . _ : / @ -)")
	}
	if c.Modele != "" && !reModeleCheck.MatchString(c.Modele) {
		return errors.New("modèle invalide")
	}
	if c.Delai != 0 && (time.Duration(c.Delai) < time.Second || time.Duration(c.Delai) > DelaiCheckMax) {
		return errors.New("délai : 1 s à 30 s")
	}
	if len(c.Contient) > 200 {
		return errors.New("texte attendu : 200 caractères au plus")
	}
	if c.Type != "http" && (c.Contient != "" || c.VerifierTLS != nil) {
		return errors.New("« contient » et « verifier_tls » : seulement pour http")
	}
	return ValiderCible(c.Type, c.Cible)
}

// ValiderCible : format de la cible selon le type (vérifié aussi côté
// serveur, quand l'admin saisit une liste dans un modèle).
func ValiderCible(typ, cible string) error {
	switch typ {
	case "http":
		if len(cible) > 2048 {
			return errors.New("adresse : 2048 caractères au plus")
		}
		u, err := url.Parse(cible)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return errors.New("adresse http:// ou https:// attendue")
		}
		if u.User != nil {
			return errors.New("pas d'identifiant ni de mot de passe dans l'adresse")
		}
	case "tcp":
		h, p, err := net.SplitHostPort(cible)
		if err != nil || !reHoteCheck.MatchString(h) {
			return errors.New("hôte:port attendu (ex. 127.0.0.1:5432)")
		}
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			return errors.New("port : 1 à 65535")
		}
	case "dns", "ping":
		if !reHoteCheck.MatchString(cible) {
			return errors.New("nom d'hôte ou adresse attendu")
		}
	case "processus":
		if !reProcessusCheck.MatchString(cible) {
			return errors.New("nom de processus attendu (15 caractères, comme dans « ps -e »)")
		}
	case "fichier", "dossier":
		if len(cible) > 4096 || !strings.HasPrefix(cible, "/") || filepath.Clean(cible) != cible || strings.ContainsRune(cible, 0) {
			return errors.New("chemin absolu attendu (sans « .. »)")
		}
		if typ == "dossier" && strings.ContainsAny(cible, "*?[") {
			return errors.New("dossier : pas de motif « * »")
		}
	default:
		return fmt.Errorf("type %q inconnu", typ)
	}
	return nil
}
