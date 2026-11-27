// SPDX-License-Identifier: GPL-3.0-or-later

// mirador-maj-serveur : mise à jour du serveur depuis l'interface (SPEC § 102).
//
// Tourne sur le VPS, en root, hors Docker, sans port ouvert. systemd le
// lance quand le serveur dépose une demande (unité .path) :
//
//	mirador-maj-serveur appliquer   traite la demande en attente
//	mirador-maj-serveur verifier    contrôle l'installation (sans rien changer)
//	mirador-maj-serveur -version
//
// Réglages : /etc/mirador-maj-serveur.conf (ou -config FICHIER).
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"mirador-stack/internal/majserveur"
	"mirador-stack/internal/version"
)

const aide = `mirador-maj-serveur : mise à jour du serveur mirador depuis l'interface

  appliquer [-config FICHIER]   traite la demande déposée par le serveur
  verifier  [-config FICHIER]   contrôle l'installation, sans rien changer
  -version`

func main() {
	os.Exit(lancer(os.Args[1:], os.Stdout, os.Stderr))
}

func lancer(args []string, sortie, erreurs io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(sortie, aide)
		return 0
	}
	if args[0] == "-version" || args[0] == "--version" {
		fmt.Fprintln(sortie, "mirador-maj-serveur", version.Complete())
		return 0
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	fs.SetOutput(erreurs)
	config := fs.String("config", "/etc/mirador-maj-serveur.conf", "fichier de réglages")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	r, err := lireConfig(*config)
	if err != nil {
		fmt.Fprintln(erreurs, "erreur :", err)
		return 1
	}
	switch args[0] {
	case "appliquer":
		err = appliquer(r, sortie)
	case "verifier":
		err = verifier(r, sortie)
	default:
		err = fmt.Errorf("commande inconnue %q\n%s", args[0], aide)
	}
	if err != nil {
		fmt.Fprintln(erreurs, "erreur :", err)
		return 1
	}
	return 0
}

func lireConfig(chemin string) (majserveur.Reglages, error) {
	f, err := os.Open(chemin) // #nosec G304 G703 -- fichier de réglages choisi par l'administrateur
	if err != nil {
		return majserveur.Reglages{}, err
	}
	defer f.Close()
	return majserveur.LireReglages(f)
}

// docker : lance « docker ARGS » dans le dossier du compose (arguments fixés
// par majserveur, jamais par la demande), 10 min au plus par commande.
type docker struct{ dossier string }

func (d docker) Lancer(ctx context.Context, args ...string) (string, error) {
	ctx, fin := context.WithTimeout(ctx, 10*time.Minute)
	defer fin()
	cmd := exec.CommandContext(ctx, "docker", args...) // #nosec G204 -- arguments fixés par majserveur (version validée, empreinte signée)
	cmd.Dir = d.dossier
	var b bytes.Buffer
	cmd.Stdout, cmd.Stderr = &b, &b
	err := cmd.Run()
	out := strings.TrimSpace(b.String())
	if len(out) > 2000 {
		out = out[len(out)-2000:]
	}
	if err != nil {
		return out, fmt.Errorf("docker %s : %v : %s", args[0], err, out)
	}
	return out, nil
}

// appliquer : une seule mise à jour à la fois (verrou), 25 min au plus.
func appliquer(r majserveur.Reglages, sortie io.Writer) error {
	if err := os.MkdirAll(r.DossierSauvegardes, 0o700); err != nil {
		return err
	}
	verrou, err := os.OpenFile(filepath.Join(r.DossierSauvegardes, "verrou"), os.O_CREATE|os.O_RDWR, 0o600) // #nosec G304 -- dossier du service
	if err != nil {
		return err
	}
	defer verrou.Close()
	if err := syscall.Flock(int(verrou.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil { // #nosec G115 -- descripteur de fichier
		return errors.New("une mise à jour est déjà en cours")
	}
	// Délai global plus long que l'attente de la nouvelle version (DELAI,
	// jusqu'à 30 min) ; le retour arrière a son propre délai.
	ctx, fin := context.WithTimeout(context.Background(), 25*time.Minute+r.Delai)
	defer fin()
	h := &majserveur.Hote{R: r, Docker: docker{dossier: r.DossierCompose}, Journal: sortie}
	_ = h.EcrireInfo(version.Version, nil) // réglages affichés dans l'interface
	return h.Appliquer(ctx)
}

// verifier : l'installation est-elle prête ? Rien n'est modifié.
func verifier(r majserveur.Reglages, sortie io.Writer) error {
	var pbs []string
	ok := func(c bool, bon, mauvais string) {
		if c {
			fmt.Fprintln(sortie, "  OK   ", bon)
		} else {
			fmt.Fprintln(sortie, "  ÉCHEC", mauvais)
			pbs = append(pbs, mauvais)
		}
	}
	fmt.Fprintln(sortie, "Réglages lus.")
	_, err := docker{dossier: r.DossierCompose}.Lancer(context.Background(), "compose", "version")
	ok(err == nil, "docker compose disponible", "docker compose introuvable ("+fmt.Sprint(err)+")")
	compose, err := lireCompose(r.DossierCompose)
	ok(err == nil && strings.Contains(compose, "${MIRADOR_IMAGE"),
		"docker-compose.yml : image: ${MIRADOR_IMAGE…}", "docker-compose.yml : remplacer la ligne image par « image: ${MIRADOR_IMAGE} » (docs/administration-serveur.md)")
	ok(err == nil && (strings.Contains(compose, r.DossierDemande+":"+majserveur.DossierDemandeDefaut) ||
		strings.Contains(compose, "./"+filepath.Base(r.DossierDemande)+":"+majserveur.DossierDemandeDefaut)),
		"volume de la demande monté", "docker-compose.yml : volume « ./maj:"+majserveur.DossierDemandeDefaut+" » absent")
	ok(err == nil && strings.Contains(compose, majserveur.DossierEtatDefaut+":ro"),
		"volume de l'état monté en lecture seule", "docker-compose.yml : volume « ./maj-etat:"+majserveur.DossierEtatDefaut+":ro » absent")
	h := &majserveur.Hote{R: r}
	v, err := h.VersionInstallee()
	ok(err == nil, "version installée : "+v, "version installée inconnue : ajouter MIRADOR_VERSION=X.Y.Z au .env du compose")
	for _, c := range []struct {
		ch           string
		proprietaire int
		nom          string
	}{{r.DossierDemande, 65532, "dossier de la demande (propriétaire 65532, le serveur y écrit)"}, {r.DossierEtat, 0, "dossier de l'état (propriétaire root)"}} {
		bon := false
		if info, err := os.Lstat(c.ch); err == nil && info.IsDir() {
			st, _ := info.Sys().(*syscall.Stat_t)
			bon = st != nil && int(st.Uid) == c.proprietaire
		}
		ok(bon, c.nom, c.nom+" : absent ou mauvais propriétaire ("+c.ch+")")
	}
	// Réglages et problèmes affichés dans l'interface (Réglages › Mise à jour
	// du serveur), si le dossier de l'état existe.
	if err := h.EcrireInfo(version.Version, pbs); err == nil {
		fmt.Fprintln(sortie, "Réglages envoyés à l'interface (Réglages › Mise à jour du serveur).")
	}
	if len(pbs) > 0 {
		return fmt.Errorf("%d point(s) à corriger", len(pbs))
	}
	fmt.Fprintln(sortie, "Tout est prêt : le bouton de l'interface peut être utilisé.")
	return nil
}

func lireCompose(dossier string) (string, error) {
	for _, n := range []string{"docker-compose.yml", "compose.yml", "docker-compose.yaml", "compose.yaml"} {
		if b, err := os.ReadFile(filepath.Join(dossier, n)); err == nil { // #nosec G304 -- dossier du compose
			return string(b), nil
		}
	}
	return "", errors.New("docker-compose.yml introuvable dans " + dossier)
}
