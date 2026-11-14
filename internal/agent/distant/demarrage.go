// SPDX-License-Identifier: GPL-3.0-or-later

package distant

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"mirador-stack/internal/agent/config"
	"mirador-stack/internal/protocole"
)

// Démarrage commun à mirador-agent et mirador-collector : premier démarrage
// avec un jeton d'enrôlement, attente de l'approbation, token écrit.

// AEnroler : serveur mirador configuré, pas encore de token, jeton présent.
func AEnroler(cfg config.Agent) bool {
	if cfg.Serveur == "" {
		return false
	}
	if _, err := os.Stat(cfg.FichierToken); err == nil {
		return false
	}
	_, err := os.Stat(cfg.FichierJeton)
	return err == nil
}

// EnrolerAvecJeton envoie la demande, attend l'approbation dans mirador, écrit
// le token puis retire le jeton (usage unique). context.Canceled si l'arrêt
// est demandé pendant l'attente.
func EnrolerAvecJeton(ctx context.Context, cfg config.Agent, client *http.Client, journal *slog.Logger) error {
	brut, err := os.ReadFile(cfg.FichierJeton)
	if err != nil {
		return err
	}
	e := Enrolement{URL: cfg.Serveur + protocole.CheminEnrolement, Jeton: strings.TrimSpace(string(brut)),
		NomPropose: cfg.Nom, SitePropose: cfg.Site, EnvPropose: cfg.Env, FichierToken: cfg.FichierToken, Client: client}
	journal.Info("enrôlement : demande envoyée, en attente d'approbation dans mirador", "machine", cfg.Nom, "serveur", cfg.Serveur)
	if err := e.Executer(ctx); err != nil {
		return err
	}
	if err := os.Remove(cfg.FichierJeton); err != nil {
		journal.Warn("jeton d'enrôlement non supprimé", "erreur", err)
	}
	journal.Info("machine approuvée : token reçu et enregistré")
	return nil
}
