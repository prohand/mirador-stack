// SPDX-License-Identifier: GPL-3.0-or-later

// mirador-agent : agent de supervision de mirador-stack.
//
// Il lit /proc et /sys, et envoie ses métriques en HTTPS (sortant uniquement).
// Il n'ouvre aucun port et n'exécute aucun ordre reçu (SPEC.md § 6).
//
// Utilisation :
//
//	mirador-agent                      démarre (config /etc/mirador-agent/agent.conf)
//	mirador-agent -config FICHIER      autre fichier de configuration
//	mirador-agent -verifier            vérifie la configuration et le token, puis s'arrête
//	mirador-agent -une-fois            fait une collecte, l'affiche, n'envoie rien
//	mirador-agent -self-test           collecte + encodage à vide ; code 0 si tout va bien
//	mirador-agent -retour-arriere      remet la version précédente (mise à jour ratée ;
//	                                   lancé par l'unité mirador-agent-retour, partie 7)
//	mirador-agent -collecte-root       assistant root : pare-feu, SMART, certificats
//	                                   (lancé par le timer mirador-agent-root, voir SPEC § 6.4)
//	mirador-agent -version             affiche la version
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"mirador-stack/internal/agent"
	"mirador-stack/internal/agent/config"
	"mirador-stack/internal/agent/distant"
	"mirador-stack/internal/agent/majagent"
	"mirador-stack/internal/agent/metrics"
	"mirador-stack/internal/agent/racine"
	"mirador-stack/internal/agent/remotewrite"
	"mirador-stack/internal/protocole"
	"mirador-stack/internal/version"
)

func main() {
	os.Exit(lancer(os.Args[1:], os.Stdout, os.Stderr))
}

func lancer(args []string, sortie, erreurs io.Writer) int {
	fs := flag.NewFlagSet("mirador-agent", flag.ContinueOnError)
	fs.SetOutput(erreurs)
	chemin := fs.String("config", "/etc/mirador-agent/agent.conf", "fichier de configuration")
	verifier := fs.Bool("verifier", false, "vérifier la configuration et le token, puis s'arrêter")
	uneFois := fs.Bool("une-fois", false, "faire une collecte et l'afficher, sans rien envoyer")
	selfTest := fs.Bool("self-test", false, "collecte et encodage de contrôle, sans envoi")
	collecteRoot := fs.Bool("collecte-root", false, "assistant root : mesures réservées à root, écrites dans /run/mirador-agent")
	retourArriere := fs.Bool("retour-arriere", false, "remettre la version précédente de l'agent (mise à jour en échec)")
	dossierBin := fs.String("dossier-bin", "/var/lib/mirador-agent/bin", "dossier des binaires de l'agent (mise à jour)")
	afficherVersion := fs.Bool("version", false, "afficher la version")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *afficherVersion {
		fmt.Fprintln(sortie, "mirador-agent", version.Complete())
		return 0
	}
	if *retourArriere {
		// Action locale uniquement (déclenchée par systemd sur cette machine),
		// jamais par le serveur.
		if err := majagent.NewInstallateur(*dossierBin, *chemin).RetourArriere("le nouvel agent ne démarre pas"); err != nil {
			fmt.Fprintf(erreurs, "retour arrière : %v\n", err)
			return 1
		}
		fmt.Fprintln(sortie, "version précédente remise en service")
		return 0
	}

	// Plafond mémoire souple du ramasse-miettes, si systemd ne l'a pas déjà
	// fixé par GOMEMLIMIT (SPEC.md § 6.1).
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(25 << 20)
	}

	locale, err := config.LireFichier(*chemin)
	if err != nil {
		fmt.Fprintf(erreurs, "configuration %s invalide :\n%v\n", *chemin, err)
		return 1
	}
	journal := slog.New(slog.NewTextHandler(erreurs, &slog.HandlerOptions{Level: niveau(locale.NiveauLog)}))
	// Avec mirador-server : dernière configuration distante reçue (sur disque).
	// Pas pour l'assistant root : ce fichier est modifiable par l'agent (revue
	// de sécurité du 11/11). Root n'en lit qu'une clé, les chemins de
	// certificats, revérifiés avec des règles strictes (SPEC.md § 21.3).
	cfg := locale
	if !*collecteRoot {
		cfg = distant.ConfigAuDemarrage(locale, journal)
	}

	if *collecteRoot {
		// Pas de token ni de réseau : l'assistant écrit seulement un fichier.
		if err := racine.Executer(cfg, time.Now()); err != nil {
			fmt.Fprintf(erreurs, "assistant root : %v\n", err)
			return 1
		}
		return 0
	}

	if *uneFois || *selfTest {
		return collecteDeControle(cfg, journal, sortie, *uneFois)
	}

	ctxCollecte, arreterCollecte := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer arreterCollecte()
	client := distant.NouveauClient()

	// Premier démarrage avec un jeton d'enrôlement : on attend l'approbation
	// dans l'interface, puis le token définitif est écrit.
	if aEnroler(cfg) {
		if *verifier {
			fmt.Fprintln(sortie, "configuration valide ; enrôlement à faire au démarrage (jeton présent)")
			return 0
		}
		if code := enroler(ctxCollecte, cfg, client, journal, erreurs); code != 0 {
			return code
		}
	}

	token, err := config.LireToken(cfg.FichierToken)
	if err != nil {
		fmt.Fprintf(erreurs, "token : %v\n", err)
		return 1
	}
	if *verifier {
		fmt.Fprintf(sortie, "configuration et token valides (machine %s, site %s, env %s)\n", cfg.Nom, cfg.Site, cfg.Env)
		return 0
	}

	journal.Info("démarrage", "version", version.Complete(), "machine", cfg.Nom,
		"destination", cfg.URLMetriques, "intervalle", cfg.Intervalle.String())

	a := agent.Nouveau(cfg, token, journal)
	if cfg.Serveur != "" {
		lien := &distant.Lien{Cfg: cfg, Locale: locale, Token: token, Client: client, Journal: journal,
			Empreinte:    config.EmpreinteMachine(""),
			Installateur: majagent.NewInstallateur(*dossierBin, *chemin),
			Redemarrer: func(raison string) {
				journal.Info("redémarrage demandé", "raison", raison)
				arreterCollecte() // arrêt propre ; systemd relance l'agent
			}}
		go lien.Tourner(ctxCollecte)
	}
	ctxEnvoi, arreterEnvoi := context.WithCancel(context.Background())
	defer arreterEnvoi()

	go a.Envoyer(ctxEnvoi)
	go agent.TournerDisquesPVE(ctxCollecte, cfg, journal) // nœud Proxmox seulement
	logsFinis := make(chan struct{})
	go func() {
		a.TournerLogs(ctxCollecte, ctxEnvoi)
		close(logsFinis)
	}()
	a.Tourner(ctxCollecte)
	<-logsFinis // dernier lot de logs mis en file

	// Arrêt demandé : on laisse 5 s pour envoyer ce qui reste.
	journal.Info("arrêt demandé, envoi des derniers lots")
	a.Vider(5 * time.Second)
	arreterEnvoi()
	if lots, _, _ := a.File().Etat(); lots > 0 {
		journal.Warn("arrêt avec des lots non envoyés", "lots", lots)
	}
	return 0
}

// aEnroler : serveur mirador configuré, pas encore de token, jeton présent.
func aEnroler(cfg config.Agent) bool {
	if cfg.Serveur == "" {
		return false
	}
	if _, err := os.Stat(cfg.FichierToken); err == nil {
		return false
	}
	_, err := os.Stat(cfg.FichierJeton)
	return err == nil
}

func enroler(ctx context.Context, cfg config.Agent, client *http.Client, journal *slog.Logger, erreurs io.Writer) int {
	brut, err := os.ReadFile(cfg.FichierJeton)
	if err != nil {
		fmt.Fprintf(erreurs, "jeton d'enrôlement : %v\n", err)
		return 1
	}
	e := distant.Enrolement{URL: cfg.Serveur + protocole.CheminEnrolement, Jeton: strings.TrimSpace(string(brut)),
		NomPropose: cfg.Nom, SitePropose: cfg.Site, EnvPropose: cfg.Env, FichierToken: cfg.FichierToken, Client: client}
	journal.Info("enrôlement : demande envoyée, en attente d'approbation dans mirador", "machine", cfg.Nom, "serveur", cfg.Serveur)
	if err := e.Executer(ctx); err != nil {
		if errors.Is(err, context.Canceled) {
			return 0
		}
		journal.Error("enrôlement impossible", "erreur", err)
		return 1
	}
	// Usage unique : le jeton ne sert plus à rien, on le retire.
	if err := os.Remove(cfg.FichierJeton); err != nil {
		journal.Warn("jeton d'enrôlement non supprimé", "erreur", err)
	}
	journal.Info("machine approuvée : token reçu et enregistré")
	return 0
}

// collecteDeControle fait une collecte sans envoi. Avec afficher, elle écrit
// les points au format texte Prometheus (pour comparer avec node_exporter).
func collecteDeControle(cfg config.Agent, journal *slog.Logger, sortie io.Writer, afficher bool) int {
	a := agent.Nouveau(cfg, "", journal)
	lot := a.Remplir(time.Now(), true)
	var enc remotewrite.Encoder
	donnees := enc.Encode(nil, lot)
	if afficher {
		if err := ecrireTexte(sortie, lot); err != nil {
			return 1
		}
		return 0
	}
	if lot.Len() < 10 || len(donnees) == 0 || lot.Ignored > 0 {
		fmt.Fprintf(sortie, "self-test ÉCHEC : %d points, %d octets, %d ignorés\n", lot.Len(), len(donnees), lot.Ignored)
		return 1
	}
	fmt.Fprintf(sortie, "self-test OK : %d points, %d octets compressés\n", lot.Len(), len(donnees))
	return 0
}

// ecrireTexte affiche un lot au format texte Prometheus, trié.
func ecrireTexte(w io.Writer, b *metrics.Batch) error {
	lignes := make([]string, 0, b.Len())
	for i := range b.Samples {
		s := &b.Samples[i]
		var sb strings.Builder
		sb.WriteString(s.Name)
		sep := "{"
		for _, l := range append(append([]metrics.Label{}, b.Common...), s.Labels[:s.N]...) {
			if l.Value == "" {
				continue
			}
			sb.WriteString(sep + l.Name + `="` + strings.ReplaceAll(l.Value, `"`, `\"`) + `"`)
			sep = ","
		}
		if sep == "," {
			sb.WriteString("}")
		}
		sb.WriteString(" " + strconv.FormatFloat(s.Value, 'g', -1, 64))
		lignes = append(lignes, sb.String())
	}
	sort.Strings(lignes)
	bw := bufio.NewWriter(w)
	for _, l := range lignes {
		if _, err := bw.WriteString(l + "\n"); err != nil {
			return err
		}
	}
	return bw.Flush()
}

func niveau(n string) slog.Level {
	var l slog.Level
	if err := l.UnmarshalText([]byte(n)); err != nil {
		return slog.LevelInfo
	}
	return l
}
