// SPDX-License-Identifier: GPL-3.0-or-later

// Package config : fichier de démarrage de mirador-server.
//
// Règle du brief : ce fichier ne contient QUE ce qui est nécessaire au
// démarrage. Tout le reste (machines, alertes, rétention, notifications…) se
// règle dans l'interface et vit dans la base.
//
//	MIRADOR_ECOUTE_AGENTS      adresse d'écoute des agents     (ex. 0.0.0.0:9440)
//	MIRADOR_ECOUTE_INTERFACE   adresse d'écoute de l'interface (ex. 0.0.0.0:9441)
//	MIRADOR_TLS_CERTIFICAT     certificat TLS (facultatif : nginx fait le TLS)
//	MIRADOR_TLS_CLE            clé TLS (avec le certificat)
//	MIRADOR_DONNEES            dossier des données (base, métriques, logs, sauvegardes)
//	MIRADOR_FICHIER_CLE        fichier de la clé de chiffrement des secrets (32 octets, base64)
//	MIRADOR_PROXY              adresses de nginx, seules autorisées à donner
//	                           l'IP du client (X-Forwarded-For) — voir SPEC § 5
package config

import (
	"bufio"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"strings"
)

// Serveur : réglages de démarrage.
type Serveur struct {
	EcouteAgents    string
	EcouteInterface string
	TLSCertificat   string
	TLSCle          string
	Donnees         string
	FichierCle      string
	Proxys          []netip.Prefix
	// Fichier : chemin du fichier de démarrage lu (pas une clé ; copié dans
	// les sauvegardes pour mémoire, il ne contient aucun secret).
	Fichier string
}

// Defaut : valeurs par défaut (conteneur Docker, nginx devant).
func Defaut() Serveur {
	return Serveur{
		EcouteAgents:    "0.0.0.0:9440",
		EcouteInterface: "0.0.0.0:9441",
		Donnees:         "/var/lib/mirador",
		FichierCle:      "/run/secrets/mirador_cle",
	}
}

// Lire analyse le fichier CLE=valeur (même format que l'agent).
func Lire(r io.Reader) (Serveur, error) {
	s := Defaut()
	vues := map[string]bool{}
	sc := bufio.NewScanner(r)
	n := 0
	for sc.Scan() {
		n++
		ligne := strings.TrimSpace(sc.Text())
		if ligne == "" || strings.HasPrefix(ligne, "#") {
			continue
		}
		cle, val, ok := strings.Cut(ligne, "=")
		if !ok {
			return Serveur{}, fmt.Errorf("ligne %d : « CLE=valeur » attendu", n)
		}
		cle, val = strings.TrimSpace(cle), strings.Trim(strings.TrimSpace(val), `"'`)
		if vues[cle] {
			return Serveur{}, fmt.Errorf("ligne %d : %s en double", n, cle)
		}
		vues[cle] = true
		switch cle {
		case "MIRADOR_ECOUTE_AGENTS":
			s.EcouteAgents = val
		case "MIRADOR_ECOUTE_INTERFACE":
			s.EcouteInterface = val
		case "MIRADOR_TLS_CERTIFICAT":
			s.TLSCertificat = val
		case "MIRADOR_TLS_CLE":
			s.TLSCle = val
		case "MIRADOR_DONNEES":
			s.Donnees = val
		case "MIRADOR_FICHIER_CLE":
			s.FichierCle = val
		case "MIRADOR_PROXY":
			for _, p := range strings.Split(val, ",") {
				if p = strings.TrimSpace(p); p == "" {
					continue
				}
				pref, err := prefixe(p)
				if err != nil {
					return Serveur{}, fmt.Errorf("ligne %d : MIRADOR_PROXY : %w", n, err)
				}
				s.Proxys = append(s.Proxys, pref)
			}
		default:
			return Serveur{}, fmt.Errorf("ligne %d : clé inconnue %q (le reste se règle dans l'interface)", n, cle)
		}
	}
	if err := sc.Err(); err != nil {
		return Serveur{}, err
	}
	return s, s.Valider()
}

// prefixe accepte une adresse (10.0.0.1) ou un réseau (172.30.0.0/24).
func prefixe(p string) (netip.Prefix, error) {
	if strings.Contains(p, "/") {
		return netip.ParsePrefix(p)
	}
	a, err := netip.ParseAddr(p)
	if err != nil {
		return netip.Prefix{}, err
	}
	return netip.PrefixFrom(a, a.BitLen()), nil
}

// Valider vérifie les réglages.
func (s *Serveur) Valider() error {
	var errs []error
	for nom, adr := range map[string]string{"MIRADOR_ECOUTE_AGENTS": s.EcouteAgents, "MIRADOR_ECOUTE_INTERFACE": s.EcouteInterface} {
		if _, _, err := net.SplitHostPort(adr); err != nil {
			errs = append(errs, fmt.Errorf("%s invalide %q", nom, adr))
		}
	}
	if s.EcouteAgents == s.EcouteInterface {
		errs = append(errs, errors.New("agents et interface doivent écouter sur deux adresses différentes (SPEC § 5)"))
	}
	if (s.TLSCertificat == "") != (s.TLSCle == "") {
		errs = append(errs, errors.New("MIRADOR_TLS_CERTIFICAT et MIRADOR_TLS_CLE vont ensemble"))
	}
	if !strings.HasPrefix(s.Donnees, "/") {
		errs = append(errs, errors.New("MIRADOR_DONNEES doit être un chemin absolu"))
	}
	if !strings.HasPrefix(s.FichierCle, "/") {
		errs = append(errs, errors.New("MIRADOR_FICHIER_CLE doit être un chemin absolu"))
	}
	return errors.Join(errs...)
}

// LireCle charge la clé de chiffrement des secrets (32 octets en base64).
// Refusée si le fichier est lisible par tous.
func LireCle(chemin string) ([]byte, error) {
	info, err := os.Stat(chemin)
	if err != nil {
		return nil, err
	}
	if info.Mode().Perm()&0o004 != 0 {
		return nil, fmt.Errorf("%s est lisible par tous : chmod 0400", chemin)
	}
	brut, err := os.ReadFile(chemin) // #nosec G304 -- chemin du fichier de démarrage
	if err != nil {
		return nil, err
	}
	cle, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(brut)))
	if err != nil || len(cle) != 32 {
		return nil, fmt.Errorf("%s : 32 octets en base64 attendus (openssl rand -base64 32)", chemin)
	}
	return cle, nil
}
