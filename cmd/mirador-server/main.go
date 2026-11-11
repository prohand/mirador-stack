// SPDX-License-Identifier: GPL-3.0-or-later

// mirador-server : serveur de mirador-stack.
//
// Utilisation :
//
//	mirador-server                      démarre (config /etc/mirador/server.conf)
//	mirador-server -config FICHIER      autre fichier de démarrage
//	mirador-server -verifier            vérifie le fichier et la clé, puis s'arrête
//	mirador-server -sante               interroge /sante du serveur local (healthcheck Docker)
//	mirador-server -version             affiche la version
//	mirador-server COMMANDE …           administration (machines, jetons, tokens,
//	                                    configuration des agents, versions) ; « aide »
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"mirador-stack/internal/server"
	"mirador-stack/internal/server/config"
	"mirador-stack/internal/version"
)

func main() {
	os.Exit(lancer(os.Args[1:], os.Stdout, os.Stderr))
}

func lancer(args []string, sortie, erreurs io.Writer) int {
	fs := flag.NewFlagSet("mirador-server", flag.ContinueOnError)
	fs.SetOutput(erreurs)
	chemin := fs.String("config", "/etc/mirador/server.conf", "fichier de démarrage")
	verifier := fs.Bool("verifier", false, "vérifier le fichier de démarrage et la clé, puis s'arrêter")
	afficherVersion := fs.Bool("version", false, "afficher la version")
	sante := fs.Bool("sante", false, "interroger /sante du serveur local (code 0 si ok)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *afficherVersion {
		fmt.Fprintln(sortie, "mirador-server", version.Complete())
		return 0
	}
	// Plafond mémoire souple du ramasse-miettes (SPEC § 16.1), si le
	// conteneur ne le fixe pas déjà par GOMEMLIMIT.
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(200 << 20)
	}

	f, err := os.Open(*chemin) // #nosec G304 -- chemin donné par l'administrateur
	if err != nil {
		fmt.Fprintf(erreurs, "fichier de démarrage : %v\n", err)
		return 1
	}
	cfg, err := config.Lire(f)
	_ = f.Close() // lecture seule : une erreur de fermeture est sans effet
	if err != nil {
		fmt.Fprintf(erreurs, "fichier de démarrage %s invalide :\n%v\n", *chemin, err)
		return 1
	}
	cfg.Fichier = *chemin
	if *sante {
		return verifierSante(cfg.EcouteInterface, erreurs)
	}
	if fs.NArg() > 0 {
		return server.Commande(context.Background(), cfg, fs.Args(), sortie, erreurs)
	}
	if *verifier {
		if _, err := config.LireCle(cfg.FichierCle); err != nil {
			fmt.Fprintf(erreurs, "clé des secrets : %v\n", err)
			return 1
		}
		fmt.Fprintln(sortie, "fichier de démarrage et clé valides")
		return 0
	}

	// Niveau réglable à chaud : mode debug temporaire depuis l'interface.
	journal := slog.New(slog.NewJSONHandler(erreurs, &slog.HandlerOptions{Level: &server.NiveauJournal}))
	ctx, arreter := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer arreter()
	s, err := server.Demarrer(ctx, cfg, journal)
	if err != nil {
		journal.Error("démarrage impossible", "erreur", err)
		return 1
	}
	<-ctx.Done()
	journal.Info("arrêt demandé")
	s.Arreter()
	return 0
}

// verifierSante appelle /sante sur l'écoute de l'interface, en local. Sert au
// healthcheck Docker (l'image ne contient ni curl ni shell).
func verifierSante(ecoute string, erreurs io.Writer) int {
	_, port, err := net.SplitHostPort(ecoute)
	if err != nil {
		return 1
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + port + "/sante")
	if err != nil {
		fmt.Fprintln(erreurs, err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(erreurs, "état :", resp.Status)
		return 1
	}
	return 0
}
