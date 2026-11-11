// SPDX-License-Identifier: GPL-3.0-or-later

// Package sauvegarde : sauvegarde à chaud et restauration de mirador-server
// (SPEC.md § 16.9).
//
// Une archive = un fichier chiffré (clé dérivée de la clé des secrets du
// serveur) contenant toujours la configuration (base SQLite, binaires de
// l'agent publiés, fichier de démarrage pour mémoire), et en option les
// métriques et les logs (volumineux).
package sauvegarde

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"time"

	"mirador-stack/internal/server/base"
)

// Instantane : copie cohérente des métriques dans un dossier (TSDB).
type Instantane interface {
	Instantane(dossier string) error
}

// Segment : un fichier de logs et la taille figée à copier.
type Segment struct {
	Nom, Chemin string
	Taille      int64
}

// Segments : liste figée des segments de logs (bloc en cours écrit avant).
type Segments interface {
	Segments() ([]Segment, error)
}

// Sources : ce qui est sauvegardé.
type Sources struct {
	DB               *sql.DB
	Dossier          string // dossier des données du serveur
	FichierDemarrage string // pour mémoire (aucun secret dedans)
	Metriques        Instantane
	Logs             Segments
}

// Options d'une sauvegarde.
type Options struct {
	Cle        []byte // clé des secrets du serveur (32 octets)
	Metriques  bool   // inclure les métriques (volumineux)
	Logs       bool   // inclure les logs (volumineux)
	Version    string // version du serveur
	Maintenant func() time.Time
}

// DossierArchives : où les archives sont écrites (destination « locale »).
func DossierArchives(donnees string) string { return filepath.Join(donnees, "archives") }

// reNom : nom des archives (sert aussi à la rétention : rien d'autre n'est supprimé).
// Deux sauvegardes dans la même seconde : « -2 », « -3 »… ajoutés au nom.
var reNom = regexp.MustCompile(`^mirador-(\d{8}-\d{6})(-\d{1,3})?\.sauvegarde$`)

// Sauvegarder écrit une archive dans DossierArchives et renvoie son chemin.
// La base est copiée par VACUUM INTO (cohérent, sans bloquer les lecteurs),
// la TSDB par instantané, les logs jusqu'à une taille figée.
func Sauvegarder(ctx context.Context, src Sources, opt Options) (string, *Manifeste, error) {
	if opt.Maintenant == nil {
		opt.Maintenant = time.Now
	}
	date := opt.Maintenant().UTC()
	archives := DossierArchives(src.Dossier)
	if err := os.MkdirAll(archives, 0o700); err != nil {
		return "", nil, err
	}
	nettoyer(src.Dossier, archives)
	travail, err := os.MkdirTemp(src.Dossier, ".sauvegarde-")
	if err != nil {
		return "", nil, err
	}
	defer os.RemoveAll(travail)

	schema, err := base.Version(ctx, src.DB)
	if err != nil {
		return "", nil, err
	}
	copieBase, err := base.Copier(ctx, src.DB, travail, "mirador.db")
	if err != nil {
		return "", nil, fmt.Errorf("copie de la base : %w", err)
	}

	racine := "mirador-" + date.Format("20060102-150405")
	nom := racine + ".sauvegarde"
	for i := 2; ; i++ {
		if _, err := os.Stat(filepath.Join(archives, nom)); errors.Is(err, os.ErrNotExist) {
			break
		}
		if i > 999 {
			return "", nil, errors.New("trop de sauvegardes dans la même seconde")
		}
		nom = racine + "-" + strconv.Itoa(i) + ".sauvegarde"
	}
	final := filepath.Join(archives, nom)
	tmp := final + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- nom fabriqué ici
	if err != nil {
		return "", nil, err
	}
	reussi := false
	defer func() {
		if !reussi {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()
	a, err := nouvelleArchive(f, opt.Cle, Manifeste{Version: opt.Version, Schema: schema, Date: date,
		Metriques: opt.Metriques && src.Metriques != nil, Logs: opt.Logs && src.Logs != nil})
	if err != nil {
		return "", nil, err
	}
	if err := a.ajouterFichier("base/mirador.db", copieBase); err != nil {
		return "", nil, err
	}
	if src.FichierDemarrage != "" {
		if _, err := os.Stat(src.FichierDemarrage); err == nil {
			if err := a.ajouterFichier("demarrage/server.conf", src.FichierDemarrage); err != nil {
				return "", nil, err
			}
		}
	}
	if info, err := os.Stat(filepath.Join(src.Dossier, "versions")); err == nil && info.IsDir() {
		if err := a.ajouterDossier("versions/", filepath.Join(src.Dossier, "versions")); err != nil {
			return "", nil, err
		}
	}
	if a.man.Metriques {
		d := filepath.Join(travail, "metriques")
		if err := src.Metriques.Instantane(d); err != nil {
			return "", nil, fmt.Errorf("instantané des métriques : %w", err)
		}
		if err := a.ajouterDossier("metriques/", d); err != nil {
			return "", nil, err
		}
	}
	if a.man.Logs {
		segs, err := src.Logs.Segments()
		if err != nil {
			return "", nil, err
		}
		for _, s := range segs {
			if err := ajouterSegment(a, s); err != nil {
				return "", nil, err
			}
		}
	}
	if err := a.fermer(); err != nil {
		return "", nil, err
	}
	if err := f.Sync(); err != nil {
		return "", nil, err
	}
	if err := f.Close(); err != nil {
		return "", nil, err
	}
	if err := os.Rename(tmp, final); err != nil {
		return "", nil, err
	}
	reussi = true
	return final, &a.man, nil
}

// nettoyer retire les restes d'une sauvegarde interrompue (arrêt brutal).
func nettoyer(donnees, archives string) {
	if l, err := filepath.Glob(filepath.Join(donnees, ".sauvegarde-*")); err == nil {
		for _, d := range l {
			_ = os.RemoveAll(d)
		}
	}
	if l, err := filepath.Glob(filepath.Join(archives, "mirador-*.sauvegarde.tmp")); err == nil {
		for _, f := range l {
			_ = os.Remove(f)
		}
	}
}

func ajouterSegment(a *ecrivainArchive, s Segment) error {
	f, err := os.Open(s.Chemin) // #nosec G304 -- segment listé par le moteur de logs
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil // purgé entre-temps
		}
		return err
	}
	defer f.Close()
	return a.ajouter("logs/"+s.Nom, s.Taille, f, time.Now())
}

// Restaurer extrait une archive dans un dossier de données VIDE (instance
// neuve). Tout est vérifié (clé, intégrité, empreintes, version de la base)
// avant que quoi que ce soit ne soit mis en place.
func Restaurer(r io.Reader, cle []byte, dossier string, maxTotal int64) (*Manifeste, error) {
	if err := os.MkdirAll(dossier, 0o750); err != nil {
		return nil, err
	}
	entrees, err := os.ReadDir(dossier)
	if err != nil {
		return nil, err
	}
	for _, e := range entrees {
		if e.Name() != "lost+found" {
			return nil, fmt.Errorf("le dossier %s n'est pas vide : restauration refusée (elle ne remplace jamais des données existantes)", dossier)
		}
	}
	travail, err := os.MkdirTemp(dossier, ".restauration-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(travail)
	man, err := lireArchive(r, cle, travail, maxTotal)
	if err != nil {
		return nil, err
	}
	if man.Schema > base.VersionAttendue() {
		return nil, fmt.Errorf("archive d'un serveur plus récent (base en version %d, ce binaire gère %d) : mettez le serveur à jour",
			man.Schema, base.VersionAttendue())
	}
	// Mise en place : simples renommages dans le même dossier.
	deplacements := [][2]string{
		{"base/mirador.db", "mirador.db"},
		{"versions", "versions"},
		{"metriques", "metriques"},
		{"logs", "logs"},
		{"demarrage/server.conf", filepath.Join("restauration", "server.conf")},
	}
	for _, d := range deplacements {
		src := filepath.Join(travail, filepath.FromSlash(d[0]))
		if _, err := os.Stat(src); errors.Is(err, os.ErrNotExist) {
			continue
		}
		dst := filepath.Join(dossier, d[1])
		if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
			return nil, err
		}
		if err := os.Rename(src, dst); err != nil {
			return nil, err
		}
	}
	return man, nil
}

// Archive : une sauvegarde présente dans le dossier local.
type Archive struct {
	Nom    string
	Chemin string
	Taille int64
	Date   time.Time
}

// Lister renvoie les archives du dossier, la plus récente d'abord.
func Lister(dossier string) ([]Archive, error) {
	entrees, err := os.ReadDir(dossier)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Archive
	for _, e := range entrees {
		m := reNom.FindStringSubmatch(e.Name())
		if m == nil || !e.Type().IsRegular() {
			continue
		}
		d, err := time.Parse("20060102-150405", m[1])
		if err != nil {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, Archive{Nom: e.Name(), Chemin: filepath.Join(dossier, e.Name()), Taille: info.Size(), Date: d})
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Date.Equal(out[j].Date) {
			return out[i].Date.After(out[j].Date)
		}
		return numero(out[i].Nom) > numero(out[j].Nom)
	})
	return out, nil
}

// Retention garde les « conserver » archives les plus récentes du dossier et
// supprime les autres (seulement des fichiers nommés par mirador).
func Retention(dossier string, conserver int) (supprimees []string, err error) {
	if conserver < 1 {
		return nil, errors.New("il faut garder au moins une sauvegarde")
	}
	liste, err := Lister(dossier)
	if err != nil {
		return nil, err
	}
	var errs []error
	for _, a := range liste[min(conserver, len(liste)):] {
		if err := os.Remove(a.Chemin); err != nil {
			errs = append(errs, err)
			continue
		}
		supprimees = append(supprimees, a.Nom)
	}
	return supprimees, errors.Join(errs...)
}

// TailleMaxRestauration : borne par défaut des données extraites.
const TailleMaxRestauration = 64 << 30

// FormatTaille : écriture courte d'une taille (pour les commandes).
func FormatTaille(n int64) string {
	switch {
	case n >= 1<<30:
		return strconv.FormatFloat(float64(n)/(1<<30), 'f', 1, 64) + " Go"
	case n >= 1<<20:
		return strconv.FormatFloat(float64(n)/(1<<20), 'f', 1, 64) + " Mo"
	default:
		return strconv.FormatFloat(float64(n)/(1<<10), 'f', 0, 64) + " Ko"
	}
}

// Verifier déchiffre et contrôle une archive entière (clé, intégrité,
// empreintes) dans un dossier de travail supprimé ensuite, sans rien
// restaurer. Sert à tester les sauvegardes.
func Verifier(r io.Reader, cle []byte, dossierTravail string, maxTotal int64) (*Manifeste, error) {
	if err := os.MkdirAll(dossierTravail, 0o700); err != nil {
		return nil, err
	}
	travail, err := os.MkdirTemp(dossierTravail, ".verification-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(travail)
	return lireArchive(r, cle, travail, maxTotal)
}

// numero : 1 pour « mirador-….sauvegarde », n pour « mirador-…-n.sauvegarde ».
func numero(nom string) int {
	m := reNom.FindStringSubmatch(nom)
	if m == nil || m[2] == "" {
		return 1
	}
	n, _ := strconv.Atoi(m[2][1:])
	return n
}
