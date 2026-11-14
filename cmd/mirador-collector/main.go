// SPDX-License-Identifier: GPL-3.0-or-later

// mirador-collector : collecteur du LAN de mirador-stack (SPEC § 23.13).
//
// Il relève les équipements sans agent (Synology, UniFi, imprimante…) et
// envoie au serveur en HTTPS, comme mirador-agent. C'est le seul programme
// de mirador qui écoute (syslog, partie 4), et seulement sur une adresse
// privée du LAN. Il n'exécute aucun ordre reçu.
//
// Utilisation :
//
//	mirador-collector                  démarre (config /etc/mirador-collector/collector.conf)
//	mirador-collector -config FICHIER  autre fichier de configuration
//	mirador-collector -verifier        vérifie la configuration et le token, puis s'arrête
//	mirador-collector -self-test       contrôle à vide (mise à jour) ; code 0 si tout va bien
//	mirador-collector -retour-arriere  remet la version précédente (mise à jour ratée)
//	mirador-collector -version         affiche la version
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"mirador-stack/internal/agent"
	"mirador-stack/internal/agent/config"
	"mirador-stack/internal/agent/distant"
	"mirador-stack/internal/agent/majagent"
	"mirador-stack/internal/agent/remotewrite"
	"mirador-stack/internal/collecteur"
	"mirador-stack/internal/protocole"
	"mirador-stack/internal/version"
)

func main() {
	os.Exit(lancer(os.Args[1:], os.Stdout, os.Stderr))
}

func lancer(args []string, sortie, erreurs io.Writer) int {
	// Ce binaire est mirador-collector : manifestes, noms des binaires
	// installés et capacités annoncées au serveur.
	majagent.ProduitCourant = protocole.ProduitCollecteur
	distant.Capacites = protocole.CapaciteCollecteur

	fs := flag.NewFlagSet("mirador-collector", flag.ContinueOnError)
	fs.SetOutput(erreurs)
	chemin := fs.String("config", "/etc/mirador-collector/collector.conf", "fichier de configuration")
	verifier := fs.Bool("verifier", false, "vérifier la configuration et le token, puis s'arrêter")
	selfTest := fs.Bool("self-test", false, "contrôle à vide, sans envoi")
	retourArriere := fs.Bool("retour-arriere", false, "remettre la version précédente (mise à jour en échec)")
	dossierBin := fs.String("dossier-bin", "/var/lib/mirador-collector/bin", "dossier des binaires (mise à jour)")
	afficherVersion := fs.Bool("version", false, "afficher la version")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *afficherVersion {
		fmt.Fprintln(sortie, "mirador-collector", version.Complete())
		return 0
	}
	if *retourArriere {
		if err := majagent.NewInstallateur(*dossierBin, *chemin).RetourArriere("le nouveau collecteur ne démarre pas"); err != nil {
			fmt.Fprintf(erreurs, "retour arrière : %v\n", err)
			return 1
		}
		fmt.Fprintln(sortie, "version précédente remise en service")
		return 0
	}
	// Plafond mémoire souple (objectif < 40 Mio, SPEC § 23.3).
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(35 << 20)
	}

	locale, err := config.LireFichierDepuis(*chemin, config.DefautCollecteur())
	if err != nil {
		fmt.Fprintf(erreurs, "configuration %s invalide :\n%v\n", *chemin, err)
		return 1
	}
	journal := slog.New(slog.NewTextHandler(erreurs, &slog.HandlerOptions{Level: niveau(locale.NiveauLog)}))
	cfg := distant.ConfigAuDemarrage(locale, journal)

	if *selfTest {
		return controle(cfg, journal, sortie)
	}

	ctx, arreter := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer arreter()
	client := distant.NouveauClient()
	if distant.AEnroler(cfg) {
		if *verifier {
			fmt.Fprintln(sortie, "configuration valide ; enrôlement à faire au démarrage (jeton présent)")
			return 0
		}
		if err := distant.EnrolerAvecJeton(ctx, cfg, client, journal); err != nil {
			if errors.Is(err, context.Canceled) {
				return 0
			}
			journal.Error("enrôlement impossible", "erreur", err)
			return 1
		}
	}
	token, err := config.LireToken(cfg.FichierToken)
	if err != nil {
		fmt.Fprintf(erreurs, "token : %v\n", err)
		return 1
	}
	if *verifier {
		fmt.Fprintf(sortie, "configuration et token valides (collecteur %s, %d équipement(s))\n", cfg.Nom, len(cfg.Collecteur.Equipements))
		return 0
	}

	journal.Info("démarrage", "version", version.Complete(), "collecteur", cfg.Nom,
		"destination", cfg.URLMetriques, "equipements", len(cfg.Collecteur.Equipements))
	// Syslog TLS (partie 4) : clé locale, demande de certificat jointe au
	// battement, écoute dès que le certificat est reçu.
	sys, err := collecteur.NouveauSyslog(cfg, token, journal)
	if err != nil {
		journal.Error("syslog désactivé", "erreur", err)
	}
	mesures := collecteur.Collecteurs(cfg)
	csr := ""
	if sys != nil {
		mesures, csr = append(mesures, sys), sys.CSR
	}
	a := agent.NouveauAvec(cfg, token, journal, agent.Options{Collecteurs: mesures, Job: collecteur.Job})
	if cfg.Serveur != "" {
		lien := &distant.Lien{Cfg: cfg, Locale: locale, Token: token, Client: client, Journal: journal, CSRSyslog: csr,
			Empreinte:    config.EmpreinteMachine(""),
			Installateur: majagent.NewInstallateur(*dossierBin, *chemin),
			Redemarrer: func(raison string) {
				journal.Info("redémarrage demandé", "raison", raison)
				arreter() // arrêt propre ; systemd relance le collecteur
			}}
		go lien.Tourner(ctx)
	}
	ctxEnvoi, arreterEnvoi := context.WithCancel(context.Background())
	defer arreterEnvoi()
	go a.Envoyer(ctxEnvoi)
	sysFini := make(chan struct{})
	go func() {
		if sys != nil {
			sys.Tourner(ctx, ctxEnvoi)
		}
		close(sysFini)
	}()
	a.Tourner(ctx)
	<-sysFini // dernier lot syslog mis en file

	journal.Info("arrêt demandé, envoi des derniers lots")
	a.Vider(5 * time.Second)
	arreterEnvoi()
	return 0
}

// controle : configuration lue et un cycle encodé, sans envoi (lancé par la
// mise à jour sur le nouveau binaire avant de l'activer).
func controle(cfg config.Agent, journal *slog.Logger, sortie io.Writer) int {
	a := agent.NouveauAvec(cfg, "", journal, agent.Options{Collecteurs: collecteur.Collecteurs(cfg), Job: collecteur.Job})
	lot := a.Remplir(time.Now(), true)
	var enc remotewrite.Encoder
	donnees := enc.Encode(nil, lot)
	if lot.Len() == 0 || len(donnees) == 0 || lot.Ignored > 0 {
		fmt.Fprintf(sortie, "self-test ÉCHEC : %d points, %d octets, %d ignorés\n", lot.Len(), len(donnees), lot.Ignored)
		return 1
	}
	fmt.Fprintf(sortie, "self-test OK : %d points, %d octets compressés\n", lot.Len(), len(donnees))
	return 0
}

func niveau(n string) slog.Level {
	var l slog.Level
	if err := l.UnmarshalText([]byte(n)); err != nil {
		return slog.LevelInfo
	}
	return l
}
