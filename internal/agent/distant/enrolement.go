// SPDX-License-Identifier: GPL-3.0-or-later

package distant

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"mirador-stack/internal/protocole"
	"mirador-stack/internal/version"
)

// Enrolement : première connexion d'une machine (SPEC.md, « Enrôlement ») :
//  1. le script d'installation reçoit un jeton à usage unique, généré dans
//     l'interface ;
//  2. l'agent l'envoie ; la machine apparaît « en attente d'approbation » ;
//  3. l'agent redemande de temps en temps ; une fois approuvée, il reçoit son
//     token définitif, l'écrit dans son fichier (0600) et n'a plus besoin du
//     jeton d'enrôlement.
type Enrolement struct {
	URL          string // https://serveur/api/agent/v1/enroll
	Jeton        string
	NomPropose   string
	SitePropose  string // site et type proposés (le serveur garde ceux du jeton s'il y en a)
	EnvPropose   string
	FichierToken string
	Client       *http.Client
	AttenteMin   time.Duration // entre deux demandes (défaut 30 s)
	AttenteMax   time.Duration // plafond du délai demandé par le serveur (défaut 5 min)
}

// NouveauClient : client HTTPS des échanges avec le serveur (TLS 1.2 minimum,
// autorités du système, proxy de l'environnement).
func NouveauClient() *http.Client {
	return &http.Client{
		Timeout: 5 * time.Minute, // un binaire de mise à jour peut être long à venir
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

// ErrRefuse : jeton invalide, expiré, ou demande rejetée dans l'interface.
var ErrRefuse = errors.New("enrôlement refusé")

// Executer demande l'enrôlement jusqu'à l'approbation (ou l'arrêt du contexte).
func (e *Enrolement) Executer(ctx context.Context) error {
	if e.AttenteMin == 0 {
		e.AttenteMin = 30 * time.Second
	}
	if e.AttenteMax == 0 {
		e.AttenteMax = 5 * time.Minute
	}
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return err
	}
	demande := protocole.DemandeEnrolement{
		Jeton: e.Jeton, NomPropose: e.NomPropose, SitePropose: e.SitePropose, EnvPropose: e.EnvPropose, Version: version.Version,
		Architecture: runtime.GOARCH, Identifiant: hex.EncodeToString(id),
	}
	for {
		rep, err := e.demander(ctx, demande)
		attente := e.AttenteMin
		switch {
		case err != nil:
			// serveur injoignable : on réessaie
		case rep.Statut == protocole.Refuse:
			return fmt.Errorf("%w : %s", ErrRefuse, rep.Message)
		case rep.Statut == protocole.Approuve:
			if len(rep.Token) < 32 {
				return errors.New("token définitif trop court")
			}
			return ecrireToken(e.FichierToken, rep.Token)
		case rep.Attente > 0:
			attente = max(attente, time.Duration(rep.Attente)*time.Second)
		}
		attente = min(attente, e.AttenteMax)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(attente):
		}
	}
}

func (e *Enrolement) demander(ctx context.Context, d protocole.DemandeEnrolement) (*protocole.ReponseEnrolement, error) {
	corps, err := json.Marshal(d)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.URL, bytes.NewReader(corps))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(protocole.EnteteProtocole, "1")
	resp, err := e.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return nil, fmt.Errorf("enrôlement : HTTP %d", resp.StatusCode)
	}
	var rep protocole.ReponseEnrolement
	dec := json.NewDecoder(io.LimitReader(resp.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rep); err != nil {
		return nil, err
	}
	return &rep, nil
}

// ecrireToken enregistre le token définitif (0600, écriture atomique).
func ecrireToken(chemin, token string) error {
	tmp, err := os.CreateTemp(filepath.Dir(chemin), ".token-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		return errors.Join(err, tmp.Close())
	}
	if _, err := tmp.WriteString(token + "\n"); err != nil {
		return errors.Join(err, tmp.Close())
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), chemin)
}
