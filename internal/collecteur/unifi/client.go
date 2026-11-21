// SPDX-License-Identifier: GPL-3.0-or-later

// Package unifi : lecture du contrôleur UniFi Network par le collecteur
// (SPEC § 23.16), à la place d'unpoller. Lecture seule (GET de l'API
// « stat »), métriques aux noms d'unpoller : règles et tableau « UniFi »
// inchangés.
package unifi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"reflect"
	"strconv"
	"time"

	"mirador-stack/internal/agent/config"
)

// tailleMaxReponse : réponse JSON du contrôleur lue au plus.
const tailleMaxReponse = 16 << 20

// Client : session vers un contrôleur (UniFi OS ou logiciel classique).
type Client struct {
	cfg     config.UniFi
	http    *http.Client
	prefixe string // « /proxy/network » (UniFi OS) ou « »
	csrf    string // UniFi OS : jeton à renvoyer
	// connecte : session ouverte, gardée d'un relevé à l'autre (relecture
	// du 21/11 : une connexion au compte par relevé, jamais fermée, soit
	// 1 440 sessions par jour sur le contrôleur).
	connecte bool
}

// errSession : le contrôleur refuse la session (expirée) : se reconnecter.
var errSession = errors.New("contrôleur UniFi : session refusée")

// Nouveau prépare le client : certificat vérifié, ou épinglé par son
// empreinte (certificat fait maison) ; pas de proxy, pas de redirection.
func Nouveau(cfg config.UniFi) *Client {
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if cfg.Empreinte != "" {
		attendue := cfg.Empreinte
		// #nosec G402 -- vérification remplacée par l'empreinte épinglée
		// (VerifyConnection ci-dessous) : plus stricte qu'une CA.
		tlsCfg.InsecureSkipVerify = true
		// VerifyConnection : appelée à chaque connexion, y compris une
		// session TLS reprise (VerifyPeerCertificate ne l'est pas).
		tlsCfg.VerifyConnection = func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("aucun certificat")
			}
			h := sha256.Sum256(cs.PeerCertificates[0].Raw)
			if hex.EncodeToString(h[:]) != attendue {
				return errors.New("certificat du contrôleur différent de l'empreinte épinglée")
			}
			return nil
		}
	}
	jar, _ := cookiejar.New(nil)
	return &Client{cfg: cfg, http: &http.Client{Timeout: 20 * time.Second, Jar: jar,
		Transport:     &http.Transport{Proxy: nil, TLSClientConfig: tlsCfg, MaxIdleConns: 2, IdleConnTimeout: time.Minute},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

// Fermer : connexions rendues.
func (c *Client) Fermer() { c.http.CloseIdleConnections() }

// Deconnecter : ferme la session du compte local (au mieux, sans erreur),
// puis rend les connexions.
func (c *Client) Deconnecter(ctx context.Context) {
	if c.connecte && c.cfg.CleAPI == "" {
		chemin := "/api/logout"
		if c.prefixe != "" {
			chemin = "/api/auth/logout"
		}
		_, _, _ = c.poster(ctx, chemin, []byte("{}"))
	}
	c.connecte = false
	c.Fermer()
}

// Connecter : clé API (UniFi OS), sinon compte local : UniFi OS
// (/api/auth/login) puis, s'il n'existe pas, contrôleur classique (/api/login).
func (c *Client) Connecter(ctx context.Context) error {
	if c.cfg.CleAPI != "" {
		c.prefixe = "/proxy/network"
		return nil
	}
	corps, _ := json.Marshal(map[string]any{"username": c.cfg.Utilisateur, "password": c.cfg.MotDePasse, "remember": false})
	code, entetes, err := c.poster(ctx, "/api/auth/login", corps)
	if err != nil {
		return err
	}
	switch {
	case code == http.StatusOK:
		c.prefixe, c.csrf = "/proxy/network", entetes.Get("X-Csrf-Token")
		return nil
	case code == http.StatusNotFound:
		code, _, err = c.poster(ctx, "/api/login", corps)
		if err != nil {
			return err
		}
		if code == http.StatusOK {
			c.prefixe = ""
			return nil
		}
	}
	if code == http.StatusUnauthorized || code == http.StatusForbidden || code == http.StatusBadRequest {
		return errors.New("contrôleur UniFi : compte refusé")
	}
	return fmt.Errorf("contrôleur UniFi : connexion refusée (HTTP %d)", code)
}

func (c *Client) poster(ctx context.Context, chemin string, corps []byte) (int, http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.Adresse+chemin, bytes.NewReader(corps))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.csrf != "" {
		req.Header.Set("X-Csrf-Token", c.csrf)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	return resp.StatusCode, resp.Header, nil
}

// lire : GET d'un point de l'API (« data » de la réponse dans v).
func (c *Client) lire(ctx context.Context, chemin string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.Adresse+c.prefixe+chemin, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if c.cfg.CleAPI != "" {
		req.Header.Set("X-API-KEY", c.cfg.CleAPI)
	}
	if c.csrf != "" {
		req.Header.Set("X-Csrf-Token", c.csrf)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) && c.cfg.CleAPI == "" {
		return errSession
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("contrôleur UniFi : %s → HTTP %d", chemin, resp.StatusCode)
	}
	lim := &io.LimitedReader{R: resp.Body, N: tailleMaxReponse + 1}
	if err := lireData(json.NewDecoder(lim), v); err != nil {
		if lim.N <= 0 {
			return errors.New("contrôleur UniFi : réponse trop grande")
		}
		return fmt.Errorf("contrôleur UniFi : réponse illisible : %w", err)
	}
	return nil
}

// lireData : liste « data » de la réponse, lue élément par élément dans v
// (pointeur vers une tranche). Relecture du 21/11 : la réponse entière était
// lue, copiée puis décodée (~4 fois sa taille en mémoire) ; maintenant, un
// élément à la fois, et les champs inutiles ne sont pas gardés.
func lireData(dec *json.Decoder, v any) error {
	liste := reflect.ValueOf(v).Elem()
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return fmt.Errorf("objet attendu (%v)", err)
	}
	for dec.More() {
		cle, err := dec.Token()
		if err != nil {
			return err
		}
		if cle != "data" {
			var autre json.RawMessage // « meta » : petit
			if err := dec.Decode(&autre); err != nil {
				return err
			}
			continue
		}
		t, err := dec.Token()
		if err != nil {
			return err
		}
		if t == nil {
			continue // « data »: null
		}
		if t != json.Delim('[') {
			return errors.New("« data » : liste attendue")
		}
		for dec.More() {
			e := reflect.New(liste.Type().Elem())
			if err := dec.Decode(e.Interface()); err != nil {
				return err
			}
			liste.Set(reflect.Append(liste, e.Elem()))
		}
		if _, err := dec.Token(); err != nil { // « ] »
			return err
		}
	}
	return nil
}

// Site : un site du contrôleur.
type Site struct {
	Nom  string `json:"name"`
	Desc string `json:"desc"`
}

// Sites : sites du contrôleur.
func (c *Client) Sites(ctx context.Context) ([]Site, error) {
	var s []Site
	return s, c.lire(ctx, "/api/self/sites", &s)
}

// LireSite : appareils, clients et santé d'un site.
func (c *Client) LireSite(ctx context.Context, site string) (Donnees, error) {
	var d Donnees
	base := "/api/s/" + url.PathEscape(site)
	if err := c.lire(ctx, base+"/stat/device", &d.Appareils); err != nil {
		return d, err
	}
	if err := c.lire(ctx, base+"/stat/sta", &d.Clients); err != nil {
		return d, err
	}
	return d, c.lire(ctx, base+"/stat/health", &d.Sante)
}

// --- Données de l'API (champs utiles seulement) ---------------------------------------

// Nombre : nombre JSON, ou texte qui contient un nombre (l'API UniFi mélange).
type Nombre float64

func (n *Nombre) UnmarshalJSON(b []byte) error {
	b = bytes.Trim(b, `"`)
	if len(b) == 0 || string(b) == "null" {
		*n = 0
		return nil
	}
	v, err := strconv.ParseFloat(string(b), 64)
	if err != nil {
		*n = 0     // valeur illisible : ignorée plutôt que tout refuser
		return nil //nolint:nilerr
	}
	*n = Nombre(v)
	return nil
}

// Donnees : un site lu.
type Donnees struct {
	Appareils []Appareil
	Clients   []ClientUniFi
	Sante     []Sous
}

// Appareil : borne, switch, passerelle.
type Appareil struct {
	Nom          string `json:"name"`
	Mac          string `json:"mac"`
	Type         string `json:"type"`
	Modele       string `json:"model"`
	Version      string `json:"version"`
	IP           string `json:"ip"`
	Etat         Nombre `json:"state"`
	Uptime       Nombre `json:"uptime"`
	Upgradable   bool   `json:"upgradable"`
	Stations     Nombre `json:"user-num_sta"`
	Invites      Nombre `json:"guest-num_sta"`
	RxOctets     Nombre `json:"rx_bytes"`
	TxOctets     Nombre `json:"tx_bytes"`
	SystemeStats struct {
		CPU Nombre `json:"cpu"`
		Mem Nombre `json:"mem"`
	} `json:"system-stats"`
	Temperatures []struct {
		Nom    string `json:"name"`
		Type   string `json:"type"`
		Valeur Nombre `json:"value"`
	} `json:"temperatures"`
	TemperatureGenerale Nombre `json:"general_temperature"`
	Ports               []struct {
		Numero    Nombre `json:"port_idx"`
		Nom       string `json:"name"`
		Rx        Nombre `json:"rx_bytes"`
		Tx        Nombre `json:"tx_bytes"`
		RxErreurs Nombre `json:"rx_errors"`
		TxErreurs Nombre `json:"tx_errors"`
		RxRejets  Nombre `json:"rx_dropped"`
		TxRejets  Nombre `json:"tx_dropped"`
		Vitesse   Nombre `json:"speed"`
		PoE       Nombre `json:"poe_power"`
	} `json:"port_table"`
	Radios []struct {
		Nom   string `json:"name"`
		Radio string `json:"radio"`
		CU    Nombre `json:"cu_total"`
	} `json:"radio_table_stats"`
}

// ClientUniFi : appareil connecté (téléphone, PC…).
type ClientUniFi struct {
	Nom          string `json:"name"`
	NomHote      string `json:"hostname"`
	Mac          string `json:"mac"`
	Filaire      bool   `json:"is_wired"`
	ESSID        string `json:"essid"`
	APMac        string `json:"ap_mac"`
	Radio        string `json:"radio"`
	Signal       Nombre `json:"signal"`
	Satisfaction Nombre `json:"satisfaction"`
	Uptime       Nombre `json:"uptime"`
	Rx           Nombre `json:"rx_bytes"`
	Tx           Nombre `json:"tx_bytes"`
}

// Sous : santé d'un sous-système (wlan, lan, wan…).
type Sous struct {
	Sous        string `json:"subsystem"`
	Deconnectes Nombre `json:"num_disconnected"`
}
