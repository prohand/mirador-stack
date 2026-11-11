// SPDX-License-Identifier: GPL-3.0-or-later

package envoi

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"mirador-stack/internal/protocole"
	"mirador-stack/internal/version"
)

// Options d'un expéditeur.
type Options struct {
	URL   string
	Token string
	// En-têtes propres au format (ex. Content-Type, Content-Encoding).
	Entetes map[string]string
	// Délai maximal d'une requête.
	Delai time.Duration
	// Délai d'attente entre deux essais : de Min à Max, doublé à chaque échec.
	AttenteMin, AttenteMax time.Duration
	Journal                *slog.Logger
	// Client HTTP (remplaçable dans les tests).
	Client *http.Client
}

// Expediteur vide une File vers une adresse HTTP, dans l'ordre, un lot à la fois.
type Expediteur struct {
	opt  Options
	file *File

	// Compteurs pour les métriques internes de l'agent.
	Envoyes     atomic.Uint64
	Echecs      atomic.Uint64
	Rejetes     atomic.Uint64 // lots refusés définitivement par le serveur (400)
	OctetsEnvoy atomic.Uint64
}

// NewExpediteur prépare un expéditeur. Le client HTTP par défaut impose TLS 1.2
// minimum et vérifie le certificat avec les autorités du système.
func NewExpediteur(file *File, opt Options) *Expediteur {
	if opt.Delai == 0 {
		opt.Delai = 30 * time.Second
	}
	if opt.AttenteMin == 0 {
		opt.AttenteMin = time.Second
	}
	if opt.AttenteMax == 0 {
		opt.AttenteMax = 5 * time.Minute
	}
	if opt.Journal == nil {
		opt.Journal = slog.Default()
	}
	if opt.Client == nil {
		opt.Client = &http.Client{
			Timeout: opt.Delai,
			Transport: &http.Transport{
				TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
				MaxIdleConns:        2,
				IdleConnTimeout:     90 * time.Second,
				TLSHandshakeTimeout: 10 * time.Second,
				ForceAttemptHTTP2:   true,
				Proxy:               http.ProxyFromEnvironment,
			},
		}
	}
	return &Expediteur{opt: opt, file: file}
}

// erreurDefinitive : le serveur refuse ce lot pour toujours (inutile de réessayer).
var erreurDefinitive = errors.New("lot refusé par le serveur")

// Tourner envoie les lots jusqu'à l'arrêt du contexte.
func (e *Expediteur) Tourner(ctx context.Context) {
	attente := e.opt.AttenteMin
	for {
		lot := e.file.Premier()
		if lot == nil {
			select {
			case <-ctx.Done():
				return
			case <-e.file.Signal():
				continue
			}
		}
		err := e.envoyer(ctx, lot)
		switch {
		case err == nil:
			e.Envoyes.Add(1)
			e.OctetsEnvoy.Add(uint64(len(lot)))
			e.file.Retirer(lot)
			attente = e.opt.AttenteMin
			continue
		case errors.Is(err, erreurDefinitive):
			// Format refusé : le garder bloquerait toute la file.
			e.Rejetes.Add(1)
			e.file.Retirer(lot)
			e.opt.Journal.Error("lot rejeté par le serveur, abandonné", "url", e.opt.URL, "erreur", err)
			continue
		}
		e.Echecs.Add(1)
		e.opt.Journal.Warn("envoi en échec, nouvel essai plus tard",
			"url", e.opt.URL, "attente", attente.String(), "erreur", err)
		// Attente avec aléa (± 20 %) pour que 25 agents ne reviennent pas tous
		// en même temps après une coupure du serveur.
		alea := time.Duration(float64(attente) * (0.8 + 0.4*rand.Float64())) // #nosec G404 -- aléa de délai, pas de sécurité
		select {
		case <-ctx.Done():
			return
		case <-time.After(alea):
		}
		attente = min(attente*2, e.opt.AttenteMax)
	}
}

// envoyer fait une requête POST avec le lot.
func (e *Expediteur) envoyer(ctx context.Context, lot []byte) error {
	ctx, annuler := context.WithTimeout(ctx, e.opt.Delai)
	defer annuler()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.opt.URL, bytes.NewReader(lot))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+e.opt.Token)
	req.Header.Set("User-Agent", "mirador-agent/"+version.Version)
	// Heure d'envoi : le serveur en déduit le décalage d'horloge (§ 16.4).
	req.Header.Set(protocole.EnteteHeure, strconv.FormatInt(time.Now().UnixMilli(), 10))
	for k, v := range e.opt.Entetes {
		req.Header.Set(k, v)
	}
	resp, err := e.opt.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// Lit au plus 1 Kio de la réponse (message d'erreur) puis jette le reste,
	// pour pouvoir réutiliser la connexion.
	extrait, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusBadRequest, resp.StatusCode == http.StatusRequestEntityTooLarge:
		// Lot invalide ou trop gros : le renvoyer ne changerait rien.
		return fmt.Errorf("%w : HTTP %d : %s", erreurDefinitive, resp.StatusCode, bytes.TrimSpace(extrait))
	default:
		// 401/403 (token refusé), 429 (limite ou quota), 5xx : on réessaie plus tard.
		return fmt.Errorf("HTTP %d : %s", resp.StatusCode, bytes.TrimSpace(extrait))
	}
}
