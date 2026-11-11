// SPDX-License-Identifier: GPL-3.0-or-later

package sauvegarde

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
)

// Contenu d'une archive (chemins dans le tar) :
//
//	base/mirador.db          copie cohérente de la base (VACUUM INTO)
//	demarrage/server.conf    fichier de démarrage (sans secret), pour mémoire
//	versions/…               binaires de l'agent publiés
//	metriques/brut/…         instantané de la TSDB (option)
//	metriques/agrege/…       idem, agrégats (option)
//	logs/…                   segments de logs (option)
//	MANIFESTE.json           en dernier : liste, tailles, empreintes SHA-256
//
// La clé des secrets n'est JAMAIS dans l'archive (elle la chiffre).
const nomManifeste = "MANIFESTE.json"

// Manifeste : ce que contient l'archive.
type Manifeste struct {
	Format     int       `json:"format"`
	Version    string    `json:"version_serveur"`
	Schema     int       `json:"schema_base"`
	Date       time.Time `json:"date"`
	Metriques  bool      `json:"metriques"`
	Logs       bool      `json:"logs"`
	Fichiers   []Entree  `json:"fichiers"`
	TailleBrut int64     `json:"taille_totale"`
}

// Entree : un fichier de l'archive.
type Entree struct {
	Chemin string `json:"chemin"`
	Taille int64  `json:"taille"`
	SHA256 string `json:"sha256"`
}

// prefixesAutorises : rien d'autre n'est accepté à la restauration.
var prefixesAutorises = []string{"base/", "demarrage/", "versions/", "metriques/brut/", "metriques/agrege/", "logs/"}

// ecrivainArchive : tar → zstd → chiffrement → destination.
type ecrivainArchive struct {
	chiffre io.WriteCloser
	zst     *zstd.Encoder
	tw      *tar.Writer
	man     Manifeste
}

func nouvelleArchive(w io.Writer, cle []byte, man Manifeste) (*ecrivainArchive, error) {
	c, err := NouvelEcrivain(w, cle)
	if err != nil {
		return nil, err
	}
	z, err := zstd.NewWriter(c, zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithEncoderConcurrency(1))
	if err != nil {
		return nil, err
	}
	man.Format = 1
	return &ecrivainArchive{chiffre: c, zst: z, tw: tar.NewWriter(z), man: man}, nil
}

// ajouter copie taille octets de r sous le nom donné, et note son empreinte.
func (a *ecrivainArchive) ajouter(nom string, taille int64, r io.Reader, modif time.Time) error {
	if err := a.tw.WriteHeader(&tar.Header{Name: nom, Mode: 0o600, Size: taille, ModTime: modif, Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	h := sha256.New()
	n, err := io.Copy(a.tw, io.TeeReader(io.LimitReader(r, taille), h))
	if err != nil {
		return err
	}
	if n != taille {
		return fmt.Errorf("%s : %d octets lus au lieu de %d", nom, n, taille)
	}
	a.man.Fichiers = append(a.man.Fichiers, Entree{Chemin: nom, Taille: taille, SHA256: hex.EncodeToString(h.Sum(nil))})
	a.man.TailleBrut += taille
	return nil
}

// ajouterFichier ajoute un fichier du disque.
func (a *ecrivainArchive) ajouterFichier(nom, chemin string) error {
	f, err := os.Open(chemin) // #nosec G304 -- fichier du dossier des données
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	return a.ajouter(nom, info.Size(), f, info.ModTime())
}

// ajouterDossier ajoute récursivement un dossier sous le préfixe donné.
func (a *ecrivainArchive) ajouterDossier(prefixe, dossier string) error {
	return filepath.WalkDir(dossier, func(p string, e os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() || !e.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(dossier, p)
		if err != nil {
			return err
		}
		return a.ajouterFichier(prefixe+filepath.ToSlash(rel), p)
	})
}

// fermer écrit le manifeste et ferme toutes les couches.
func (a *ecrivainArchive) fermer() error {
	brut, err := json.MarshalIndent(a.man, "", "  ")
	if err != nil {
		return err
	}
	if err := a.tw.WriteHeader(&tar.Header{Name: nomManifeste, Mode: 0o600, Size: int64(len(brut)), ModTime: a.man.Date, Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	if _, err := a.tw.Write(brut); err != nil {
		return err
	}
	return errors.Join(a.tw.Close(), a.zst.Close(), a.chiffre.Close())
}

// cheminSur : un nom d'entrée ne doit jamais sortir du dossier de restauration.
func cheminSur(nom string) bool {
	if nom == nomManifeste {
		return true
	}
	propre := path.Clean(nom)
	if propre != nom || strings.HasPrefix(nom, "/") || strings.Contains(nom, "..") || strings.Contains(nom, "\\") {
		return false
	}
	for _, p := range prefixesAutorises {
		if strings.HasPrefix(nom, p) && len(nom) > len(p) {
			return true
		}
	}
	return false
}

// lireArchive déchiffre et extrait dans dossier, vérifie chaque empreinte
// contre le manifeste et renvoie celui-ci. maxTotal borne la taille extraite.
func lireArchive(r io.Reader, cle []byte, dossier string, maxTotal int64) (*Manifeste, error) {
	clair, err := NouveauLecteur(r, cle)
	if err != nil {
		return nil, err
	}
	z, err := zstd.NewReader(clair, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(64<<20))
	if err != nil {
		return nil, err
	}
	defer z.Close()
	tr := tar.NewReader(z)
	vues := map[string]string{}
	var man *Manifeste
	var total int64
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if errors.Is(err, ErrArchive) {
				return nil, ErrArchive
			}
			return nil, fmt.Errorf("archive : %w", err)
		}
		if h.Typeflag != tar.TypeReg || !cheminSur(h.Name) || h.Size < 0 {
			return nil, fmt.Errorf("entrée refusée dans l'archive : %q", h.Name)
		}
		if man != nil {
			return nil, errors.New("contenu après le manifeste")
		}
		if total += h.Size; total > maxTotal {
			return nil, errors.New("archive plus grosse que la limite de restauration")
		}
		if h.Name == nomManifeste {
			var m Manifeste
			dec := json.NewDecoder(io.LimitReader(tr, 16<<20))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&m); err != nil {
				return nil, fmt.Errorf("manifeste illisible : %w", err)
			}
			man = &m
			continue
		}
		if _, deja := vues[h.Name]; deja {
			return nil, fmt.Errorf("entrée en double : %q", h.Name)
		}
		cible := filepath.Join(dossier, filepath.FromSlash(h.Name))
		if err := os.MkdirAll(filepath.Dir(cible), 0o750); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(cible, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- chemin vérifié (cheminSur)
		if err != nil {
			return nil, err
		}
		hs := sha256.New()
		_, err = io.Copy(io.MultiWriter(f, hs), tr) // #nosec G110 -- taille bornée : en-tête tar compté dans maxTotal, lecture limitée à h.Size
		if e := f.Close(); err == nil {
			err = e
		}
		if err != nil {
			if errors.Is(err, ErrArchive) {
				return nil, ErrArchive
			}
			return nil, err
		}
		vues[h.Name] = hex.EncodeToString(hs.Sum(nil))
	}
	// Lire jusqu'au bout : chaque segment chiffré, y compris le dernier, est
	// ainsi vérifié (une fin coupée est détectée).
	if _, err := io.Copy(io.Discard, z); err != nil {
		return nil, ErrArchive
	}
	if man == nil {
		return nil, ErrArchive // pas de manifeste : archive incomplète
	}
	if len(man.Fichiers) != len(vues) {
		return nil, fmt.Errorf("le manifeste annonce %d fichiers, l'archive en contient %d", len(man.Fichiers), len(vues))
	}
	for _, e := range man.Fichiers {
		if vues[e.Chemin] != e.SHA256 {
			return nil, fmt.Errorf("empreinte différente pour %s", e.Chemin)
		}
	}
	return man, nil
}
