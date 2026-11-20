// SPDX-License-Identifier: GPL-3.0-or-later

// Package majserveur : mise à jour du serveur depuis l'interface (SPEC § 102,
// demande du 20/11, option B).
//
// Le serveur tourne dans Docker, sans root ni accès à Docker : il ne peut pas
// changer sa propre image (et ne doit pas pouvoir). Il dépose seulement une
// demande ; un petit service du VPS (mirador-maj-serveur, root, hors Docker,
// sans port ouvert) la traite :
//
//	serveur (conteneur)                       VPS (mirador-maj-serveur)
//	/var/lib/mirador-maj/demande.json  ──►   lit la version demandée, rien d'autre
//	/var/lib/mirador-maj/serveur.json  ──►   « je tourne en version X » (au démarrage)
//	/var/lib/mirador-maj-etat/etat.json ◄──   où en est la mise à jour (lecture seule
//	                                          pour le serveur)
//
// Ce fichier : formats d'échange, lecture et écriture prudentes, versions.
// Rien de ce qui est lu n'est exécuté : la demande ne porte qu'un numéro de
// version, vérifié (format, plus récent que l'actuelle, image signée).
package majserveur

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
)

// Noms des fichiers d'échange.
const (
	NomDemande  = "demande.json"
	NomPresence = "serveur.json"
	NomEtat     = "etat.json"
	// TailleMax : un fichier d'échange plus gros est refusé.
	TailleMax = 16 << 10
)

// Dossiers d'échange par défaut, vus du conteneur du serveur.
const (
	DossierDemandeDefaut = "/var/lib/mirador-maj"
	DossierEtatDefaut    = "/var/lib/mirador-maj-etat"
)

// Demande : déposée par le serveur (bouton, administrateur, code 2FA).
type Demande struct {
	Version string `json:"version"` // X.Y.Z
	Le      int64  `json:"le"`      // secondes Unix
	Par     string `json:"par"`     // compte qui l'a demandée (affichage seulement)
}

// Presence : écrite par le serveur à chaque démarrage. Sert au service du VPS
// à savoir que la nouvelle version a bien démarré.
type Presence struct {
	Version   string `json:"version"`
	DemarreLe int64  `json:"demarre_le"`
}

// États d'une mise à jour (Etat.Etat).
const (
	EtatEnCours = "en_cours"
	EtatOK      = "ok"
	EtatEchec   = "echec"  // la nouvelle version n'a pas démarré : retour à l'ancienne
	EtatRefuse  = "refuse" // demande refusée avant toute action (version, signature…)
)

// Etape : une ligne du déroulé (affichée dans l'interface).
type Etape struct {
	Le    int64  `json:"le"`
	Texte string `json:"texte"`
}

// Etat : écrit par le service du VPS, lu par le serveur.
type Etat struct {
	Etat    string  `json:"etat"`
	Version string  `json:"version"` // version demandée
	Avant   string  `json:"avant"`   // version d'avant
	Message string  `json:"message"`
	Debut   int64   `json:"debut"`
	Le      int64   `json:"le"` // dernière mise à jour du fichier
	Etapes  []Etape `json:"etapes"`
}

var formatVersion = regexp.MustCompile(`^[0-9]{1,5}\.[0-9]{1,5}\.[0-9]{1,5}$`)

// ValiderVersion : X.Y.Z seulement (pas de préversion : une mise à jour depuis
// l'interface installe une version finale).
func ValiderVersion(v string) error {
	if !formatVersion.MatchString(v) {
		return fmt.Errorf("version %q invalide (attendu X.Y.Z)", v)
	}
	return nil
}

// Comparer : -1 si a < b, 0 si égales, 1 si a > b (versions valides).
func Comparer(a, b string) int {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < 3 && i < len(pa) && i < len(pb); i++ {
		x, _ := strconv.Atoi(pa[i])
		y, _ := strconv.Atoi(pb[i])
		switch {
		case x < y:
			return -1
		case x > y:
			return 1
		}
	}
	return 0
}

// ErrAbsent : le fichier n'existe pas.
var ErrAbsent = errors.New("fichier absent")

// LireJSON lit un fichier d'échange avec prudence : lien symbolique refusé
// (le conteneur pourrait en poser un vers un fichier du VPS), fichier
// ordinaire seulement, TailleMax au plus, champs inconnus refusés.
func LireJSON(chemin string, v any) error {
	f, err := os.OpenFile(chemin, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_NOCTTY, 0) // #nosec G304 -- dossier d'échange fixé par la configuration
	if errors.Is(err, os.ErrNotExist) {
		return ErrAbsent
	}
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > TailleMax {
		return errors.New(filepath.Base(chemin) + " : pas un fichier ordinaire, ou trop gros")
	}
	brut, err := io.ReadAll(io.LimitReader(f, TailleMax+1))
	if err != nil {
		return err
	}
	if len(brut) > TailleMax {
		return errors.New(filepath.Base(chemin) + " : trop gros")
	}
	dec := json.NewDecoder(bytes.NewReader(brut))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%s illisible : %w", filepath.Base(chemin), err)
	}
	return nil
}

// EcrireJSON écrit un fichier d'échange d'un coup : fichier temporaire au nom
// tiré au sort (jamais un fichier existant ni un lien), puis renommage. Un
// lecteur voit l'ancien fichier ou le nouveau, jamais un fichier coupé.
func EcrireJSON(dossier, nom string, v any, mode os.FileMode) error {
	brut, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return EcrireFichier(dossier, nom, append(brut, '\n'), mode)
}

// EcrireFichier : comme EcrireJSON, pour un contenu déjà prêt.
func EcrireFichier(dossier, nom string, brut []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(dossier, "."+nom+".*")
	if err != nil {
		return err
	}
	nomTmp := tmp.Name()
	defer func() { _ = os.Remove(nomTmp) }() // sans effet après le renommage
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(brut); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(nomTmp, filepath.Join(dossier, nom))
}
