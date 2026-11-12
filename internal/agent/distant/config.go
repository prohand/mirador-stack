// SPDX-License-Identifier: GPL-3.0-or-later

// Package distant : ce que l'agent demande au serveur (configuration,
// enrôlement). DÉSACTIVÉ tant que le serveur n'existe pas (SPEC.md § 6.7).
//
// Principe : l'agent INTERROGE le serveur, rarement (toutes les 5 à 10 min,
// avec un délai aléatoire), avec une requête conditionnelle (ETag) qui ne
// transfère rien si rien n'a changé. Le serveur ne contacte jamais l'agent.
package distant

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"mirador-stack/internal/agent/config"
	"mirador-stack/internal/protocole"
	"mirador-stack/internal/version"
)

// ConfigDistante : réglages que le serveur PEUT changer. Tous les champs sont
// facultatifs (nil = garder la valeur locale). Ne figurent ici que des
// valeurs typées : aucune adresse de serveur, aucun chemin de fichier, aucun
// programme, aucune commande. Un champ inconnu fait refuser tout le document.
type ConfigDistante struct {
	Intervalle     *config.Duree `json:"intervalle,omitempty"`
	IntervalleLent *config.Duree `json:"intervalle_lent,omitempty"`

	TamponMetriques *int `json:"tampon_metriques,omitempty"`
	TamponLogs      *int `json:"tampon_logs,omitempty"`

	ExclureMontages   *string `json:"exclure_montages,omitempty"`
	ExclureTypesFS    *string `json:"exclure_types_fs,omitempty"`
	ExclureDisques    *string `json:"exclure_disques,omitempty"`
	ExclureInterfaces *string `json:"exclure_interfaces,omitempty"`

	Systemd        *bool     `json:"systemd,omitempty"`
	SystemdIgnorer *[]string `json:"systemd_ignorer,omitempty"`
	MisesAJour     *bool     `json:"mises_a_jour,omitempty"`

	// Surveillance choisie dans l'interface (SPEC.md § 19). Champs ajoutés
	// après la 0.2.11 : le serveur ne les envoie qu'aux agents qui annoncent
	// la capacité « surveillance » (un ancien agent refuserait le document).
	Temperatures       *bool     `json:"temperatures,omitempty"`
	DockerMetriques    *bool     `json:"docker_metriques,omitempty"`
	Ventilateurs       *bool     `json:"ventilateurs,omitempty"`
	ZFS                *bool     `json:"zfs,omitempty"`
	Mdraid             *bool     `json:"mdraid,omitempty"`
	SystemdValidation  *bool     `json:"systemd_validation,omitempty"`
	SystemdSurveiller  *[]string `json:"systemd_surveiller,omitempty"`
	EnvoyerPareFeu     *bool     `json:"envoyer_pare_feu,omitempty"`
	EnvoyerSmart       *bool     `json:"envoyer_smart,omitempty"`
	EnvoyerCertificats *bool     `json:"envoyer_certificats,omitempty"`

	// Éléments choisis un par un (SPEC.md § 21). Ajoutés après la 0.2.12 :
	// envoyés seulement aux agents qui annoncent la capacité « elements ».
	IgnorerDisques     *[]string `json:"ignorer_disques,omitempty"`
	IgnorerInterfaces  *[]string `json:"ignorer_interfaces,omitempty"`
	IgnorerMontages    *[]string `json:"ignorer_montages,omitempty"`
	CertificatsAjout   *[]string `json:"certificats_ajout,omitempty"`
	CertificatsIgnorer *[]string `json:"certificats_ignorer,omitempty"`

	Metriques     *bool     `json:"metriques,omitempty"` // non = logs seulement
	Journal       *bool     `json:"journal,omitempty"`
	LogsDocker    *bool     `json:"logs_docker,omitempty"`
	LogsDebit     *float64  `json:"logs_debit,omitempty"`
	LogsRafale    *float64  `json:"logs_rafale,omitempty"`
	LogsBruit     *string   `json:"logs_bruit,omitempty"`
	Masquage      *[]string `json:"masquage,omitempty"`
	MasquagePerso *[]string `json:"masquage_perso,omitempty"`

	NiveauLog *string `json:"niveau_log,omitempty"`

	// Intervalle de récupération de cette configuration (5 à 60 min).
	Recuperation *config.Duree `json:"recuperation,omitempty"`
}

// tailleMaxDocument : une configuration plus grosse est refusée.
const tailleMaxDocument = 256 << 10

// Decoder lit le document JSON en refusant tout champ inconnu.
func Decoder(r io.Reader) (*ConfigDistante, error) {
	var c ConfigDistante
	dec := json.NewDecoder(io.LimitReader(r, tailleMaxDocument))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("configuration distante illisible : %w", err)
	}
	if dec.More() {
		return nil, errors.New("configuration distante : contenu en trop après le document")
	}
	return &c, nil
}

// Appliquer renvoie la configuration locale complétée par les valeurs
// distantes, puis validée avec les MÊMES bornes que le fichier local. En cas
// d'erreur, la configuration actuelle est gardée telle quelle.
func Appliquer(base config.Agent, d *ConfigDistante) (config.Agent, error) {
	a := base
	if d.Intervalle != nil {
		a.Intervalle = time.Duration(*d.Intervalle)
	}
	if d.IntervalleLent != nil {
		a.IntervalleLent = time.Duration(*d.IntervalleLent)
	}
	affecter(&a.TamponMetriques, d.TamponMetriques)
	affecter(&a.TamponLogs, d.TamponLogs)
	affecter(&a.ExclureMontages, d.ExclureMontages)
	affecter(&a.ExclureTypesFS, d.ExclureTypesFS)
	affecter(&a.ExclureDisques, d.ExclureDisques)
	affecter(&a.ExclureInterfaces, d.ExclureInterfaces)
	affecter(&a.Systemd, d.Systemd)
	affecter(&a.SystemdIgnorer, d.SystemdIgnorer)
	affecter(&a.MisesAJour, d.MisesAJour)
	affecter(&a.Temperatures, d.Temperatures)
	affecter(&a.DockerMetriques, d.DockerMetriques)
	affecter(&a.Ventilateurs, d.Ventilateurs)
	affecter(&a.ZFS, d.ZFS)
	affecter(&a.Mdraid, d.Mdraid)
	affecter(&a.SystemdValidation, d.SystemdValidation)
	affecter(&a.SystemdSurveiller, d.SystemdSurveiller)
	affecter(&a.EnvoyerPareFeu, d.EnvoyerPareFeu)
	affecter(&a.EnvoyerSmart, d.EnvoyerSmart)
	affecter(&a.EnvoyerCertificats, d.EnvoyerCertificats)
	affecter(&a.IgnorerDisques, d.IgnorerDisques)
	affecter(&a.IgnorerInterfaces, d.IgnorerInterfaces)
	affecter(&a.IgnorerMontages, d.IgnorerMontages)
	affecter(&a.CertificatsAjout, d.CertificatsAjout)
	affecter(&a.CertificatsIgnorer, d.CertificatsIgnorer)
	affecter(&a.Metriques, d.Metriques)
	affecter(&a.Journal, d.Journal)
	affecter(&a.LogsDocker, d.LogsDocker)
	affecter(&a.LogsDebit, d.LogsDebit)
	affecter(&a.LogsRafale, d.LogsRafale)
	affecter(&a.LogsBruit, d.LogsBruit)
	affecter(&a.Masquage, d.Masquage)
	affecter(&a.MasquagePerso, d.MasquagePerso)
	affecter(&a.NiveauLog, d.NiveauLog)
	if d.Recuperation != nil {
		r := time.Duration(*d.Recuperation)
		if r < 5*time.Minute || r > time.Hour {
			return base, fmt.Errorf("recuperation %s hors bornes (5 min à 1 h)", r)
		}
	}
	if err := a.Valider(); err != nil {
		return base, fmt.Errorf("configuration distante refusée : %w", err)
	}
	return a, nil
}

func affecter[T any](champ *T, valeur *T) {
	if valeur != nil {
		*champ = *valeur
	}
}

// Recuperateur interroge le serveur avec l'ETag de la dernière réponse.
type Recuperateur struct {
	URL    string // https://serveur/api/agent/v1/config
	Token  string
	Client *http.Client
	// Empreinte de la machine ('' : pas envoyée).
	Empreinte string
	etag      string
}

// ErrInchange : le serveur a répondu 304 (rien n'a changé, rien transféré).
var ErrInchange = errors.New("configuration inchangée")

// Recuperer demande la configuration. Renvoie ErrInchange si l'ETag correspond.
func (r *Recuperateur) Recuperer(ctx context.Context) (*ConfigDistante, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.URL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+r.Token)
	req.Header.Set(protocole.EnteteProtocole, "1")
	req.Header.Set(protocole.EnteteVersion, version.Version)
	req.Header.Set(protocole.EnteteCapacites, protocole.Capacites)
	if r.Empreinte != "" {
		req.Header.Set(protocole.EnteteEmpreinte, r.Empreinte)
	}
	if r.etag != "" {
		req.Header.Set("If-None-Match", r.etag)
	}
	resp, err := r.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNotModified:
		return nil, ErrInchange
	case http.StatusOK:
	default:
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return nil, fmt.Errorf("configuration distante : HTTP %d", resp.StatusCode)
	}
	brut, err := io.ReadAll(io.LimitReader(resp.Body, tailleMaxDocument+1))
	if err != nil {
		return nil, err
	}
	if len(brut) > tailleMaxDocument {
		return nil, errors.New("configuration distante trop grosse")
	}
	c, err := Decoder(bytes.NewReader(brut))
	if err != nil {
		return nil, err
	}
	r.etag = resp.Header.Get("ETag")
	return c, nil
}
