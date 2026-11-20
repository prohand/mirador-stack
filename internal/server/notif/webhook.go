// SPDX-License-Identifier: GPL-3.0-or-later

package notif

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
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/prometheus/prometheus/model/labels"

	"mirador-stack/internal/server/alertes"
	"mirador-stack/internal/server/secrets"
)

// Canal « Webhook » (SPEC § 81, idée reprise de Checkmate, idée seulement) :
// un appel HTTP par message, sous l'un des formats ci-dessous. Sert de canal
// principal, de secours, ou pour Home Assistant.

// NomWebhook : nom du canal.
const NomWebhook = "webhook"

// Formats du webhook.
const (
	FormatJSON    = "json"    // JSON mirador (alertes, gravité, liens)
	FormatDiscord = "discord" // {"content": …}
	FormatGotify  = "gotify"  // /message, en-tête X-Gotify-Key
	FormatMatrix  = "matrix"  // PUT …/send/m.room.message/<id>, jeton d'accès
	FormatSlack   = "slack"   // Slack et Mattermost : {"text": …}
)

// FormatsWebhook : formats proposés dans l'interface (code, nom).
var FormatsWebhook = [][2]string{
	{FormatJSON, "JSON mirador (alertes, gravité, liens)"},
	{FormatDiscord, "Discord"},
	{FormatGotify, "Gotify"},
	{FormatMatrix, "Matrix"},
	{FormatSlack, "Slack / Mattermost"},
}

// Webhook : réglage du canal. L'adresse compte comme un secret (Discord,
// Slack et Home Assistant y mettent leur jeton) : chiffrée, jamais montrée.
type Webhook struct {
	Actif  bool   `json:"actif"`
	URL    string `json:"url"`    // secret
	Format string `json:"format"` // voir FormatsWebhook
	// Secret : jeton envoyé dans un en-tête (facultatif) : « Authorization:
	// Bearer … » (JSON, Matrix) ou « X-Gotify-Key » (Gotify).
	Secret string `json:"secret,omitempty"`
	// ReseauPrive : adresse du LAN permise (Home Assistant, Gotify maison) ;
	// http:// n'est accepté qu'avec elle.
	ReseauPrive bool `json:"reseau_prive,omitempty"`
}

const (
	usageWebhookURL    = "notif.webhook.url"
	usageWebhookSecret = "notif.webhook.secret"
)

// valider : format connu ; adresse lisible si elle est donnée en clair.
func (w *Webhook) valider() error {
	connu := false
	for _, f := range FormatsWebhook {
		connu = connu || f[0] == w.Format
	}
	if !connu {
		return fmt.Errorf("webhook.format %q inconnu", w.Format)
	}
	if secrets.EstChiffre(w.URL) || w.URL == "***" {
		return nil
	}
	return w.validerURL(w.URL)
}

func (w *Webhook) validerURL(adresse string) error {
	u, err := url.Parse(adresse)
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" || (u.Scheme != "https" && u.Scheme != "http") {
		return errors.New("webhook.url : adresse https://… attendue (sans identifiant dans l'adresse)")
	}
	if u.Scheme == "http" && !w.ReseauPrive {
		return errors.New("webhook.url : http:// seulement avec « réseau local permis » (sinon https://)")
	}
	if w.Format == FormatMatrix && !strings.HasSuffix(strings.TrimRight(u.Path, "/"), "/send/m.room.message") {
		return errors.New("webhook.url (Matrix) : …/_matrix/client/v3/rooms/<salon>/send/m.room.message attendu")
	}
	return nil
}

type webhook struct {
	url, format, secret string
	client              *http.Client
	prive               bool
}

func (w *webhook) Nom() string { return NomWebhook }

// corpsWebhook : corps JSON selon le format (texte brut pour les formats de
// messagerie, détail structuré pour le JSON mirador).
func corpsWebhook(format string, m *Message, o Mise) ([]byte, error) {
	o.HTML = false
	switch format {
	case FormatDiscord:
		o.Max = 2000
		return json.Marshal(map[string]string{"content": Texte(m, o)})
	case FormatSlack:
		o.Max = 4000
		return json.Marshal(map[string]string{"text": Texte(m, o)})
	case FormatGotify:
		o.Max = 4000
		prio := map[string]int{alertes.Critique: 8, alertes.Avertissement: 5}[m.Gravite]
		if prio == 0 {
			prio = 2
		}
		return json.Marshal(map[string]any{"title": titreCourt(m), "message": Texte(m, o), "priority": prio})
	case FormatMatrix:
		o.Max = 8000
		txt := Texte(m, o)
		o.HTML = true
		return json.Marshal(map[string]string{"msgtype": "m.text", "body": txt,
			"format": "org.matrix.custom.html", "formatted_body": strings.ReplaceAll(Texte(m, o), "\n", "<br>")})
	}
	// JSON mirador.
	type alerteJSON struct {
		Titre      string            `json:"titre"`
		Regle      string            `json:"regle"`
		Machine    string            `json:"machine,omitempty"`
		Gravite    string            `json:"gravite"`
		Etat       string            `json:"etat"` // en_cours ou resolue
		Resume     string            `json:"resume,omitempty"`
		Valeur     any               `json:"valeur,omitempty"`
		Depuis     string            `json:"depuis,omitempty"`
		Fin        string            `json:"fin,omitempty"`
		Lien       string            `json:"lien,omitempty"`
		Acquittee  string            `json:"acquittee_par,omitempty"`
		Etiquettes map[string]string `json:"etiquettes"`
	}
	type corpsJSON struct {
		Type    string       `json:"type"` // alerte, rappel, resolution, systeme
		Gravite string       `json:"gravite,omitempty"`
		Nb      int          `json:"nb"`
		Site    string       `json:"site,omitempty"`
		Le      string       `json:"le"`
		Titre   string       `json:"titre"`
		Texte   string       `json:"texte"`
		Alertes []alerteJSON `json:"alertes"`
	}
	c := corpsJSON{Type: m.Type, Gravite: m.Gravite, Nb: m.Nb(), Site: m.Site, Le: m.Le.UTC().Format(time.RFC3339),
		Titre: titreCourt(m), Alertes: []alerteJSON{}}
	if m.Systeme != "" {
		c.Type = "systeme"
	}
	o.Max = 8000
	c.Texte = Texte(m, o)
	ajout := func(a alertes.Alerte, etat string) {
		x := alerteJSON{Titre: a.Regle, Regle: a.Regle, Machine: a.Machine(), Gravite: a.Gravite, Etat: etat, Resume: a.Resume,
			Acquittee: a.AcquitteePar, Etiquettes: map[string]string{}}
		if d := debutActif(&a); !d.IsZero() {
			x.Depuis = d.UTC().Format(time.RFC3339)
		}
		if etat == "resolue" && !a.Fin.IsZero() {
			x.Fin = a.Fin.UTC().Format(time.RFC3339)
		} else {
			x.Valeur = a.Valeur
		}
		if o.Lien != "" && a.ID > 0 {
			x.Lien = fmt.Sprintf("%s/alertes/%d", o.Lien, a.ID)
		}
		a.Etiquettes.Range(func(l labels.Label) { x.Etiquettes[l.Name] = l.Value })
		c.Alertes = append(c.Alertes, x)
	}
	for _, a := range m.Actives {
		ajout(a, "en_cours")
	}
	for _, a := range m.Resolues {
		ajout(a, "resolue")
	}
	return json.Marshal(c)
}

// titreCourt : « mirador : CpuEleve pve1 », « mirador : résolu CpuEleve ».
func titreCourt(m *Message) string {
	titre := "mirador"
	switch {
	case m.Systeme != "":
	case m.TitreSite() != "":
		titre += " : " + m.TitreSite()
	case len(m.Actives) > 0:
		titre += " : " + strings.TrimSpace(m.Actives[0].Regle+" "+m.Actives[0].Machine())
	case len(m.Resolues) > 0:
		titre += " : résolu " + m.Resolues[0].Regle
	}
	return titre
}

func (w *webhook) Envoyer(ctx context.Context, m *Message, o Mise) error {
	corps, err := corpsWebhook(w.format, m, o)
	if err != nil {
		return err
	}
	methode, adresse := http.MethodPost, w.url
	switch w.format {
	case FormatGotify:
		u, _ := url.Parse(w.url)
		if p := strings.TrimRight(u.Path, "/"); !strings.HasSuffix(p, "/message") {
			u.Path = p + "/message"
		}
		adresse = u.String()
	case FormatMatrix:
		// Identifiant de transaction aléatoire : un nouvel essai n'envoie pas deux fois.
		b := make([]byte, 12)
		_, _ = rand.Read(b)
		methode, adresse = http.MethodPut, strings.TrimRight(w.url, "/")+"/mirador"+hex.EncodeToString(b)
	}
	req, err := http.NewRequestWithContext(ctx, methode, adresse, bytes.NewReader(corps))
	if err != nil {
		return errors.New("webhook : requête impossible")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "mirador-server")
	if w.secret != "" {
		if w.format == FormatGotify {
			req.Header.Set("X-Gotify-Key", w.secret)
		} else {
			req.Header.Set("Authorization", "Bearer "+w.secret)
		}
	}
	r, err := w.client.Do(req)
	if err != nil {
		return fmt.Errorf("webhook injoignable : %w", sansSecret(err))
	}
	defer r.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 64<<10))
	if r.StatusCode/100 != 2 {
		return fmt.Errorf("webhook : HTTP %d", r.StatusCode)
	}
	return nil
}

// Verifier : connexion (et chiffrement en https) au serveur, sans message ni
// requête : un webhook n'a pas d'adresse de santé commune à tous.
func (w *webhook) Verifier(ctx context.Context) error {
	u, err := url.Parse(w.url)
	if err != nil {
		return errors.New("webhook : adresse illisible")
	}
	port := u.Port()
	if port == "" {
		port = map[string]string{"https": "443", "http": "80"}[u.Scheme]
	}
	ctx, fin := context.WithTimeout(ctx, 10*time.Second)
	defer fin()
	conn, err := numeroteur(w.prive).DialContext(ctx, "tcp", net.JoinHostPort(u.Hostname(), port))
	if err != nil {
		return fmt.Errorf("webhook injoignable : %w", err)
	}
	defer conn.Close()
	if u.Scheme == "https" {
		tc := tls.Client(conn, &tls.Config{ServerName: u.Hostname(), MinVersion: tls.VersionTLS12})
		if err := tc.HandshakeContext(ctx); err != nil {
			return fmt.Errorf("webhook : certificat ou TLS refusé : %w", err)
		}
	}
	return nil
}

// fabriquerWebhook : canal prêt, secrets déchiffrés.
func fabriquerWebhook(c *Canaux, cle []byte) (Canal, error) {
	w := c.Webhook
	adresse, err := secrets.Dechiffrer(cle, usageWebhookURL, w.URL)
	if err != nil {
		return nil, fmt.Errorf("webhook.url : %w", err)
	}
	if err := w.validerURL(adresse); err != nil {
		return nil, err
	}
	s := ""
	if w.Secret != "" {
		if s, err = secrets.Dechiffrer(cle, usageWebhookSecret, w.Secret); err != nil {
			return nil, fmt.Errorf("webhook.secret : %w", err)
		}
	}
	prive := c.ReseauPrive || w.ReseauPrive
	return &webhook{url: adresse, format: w.Format, secret: s, client: clientHTTP(prive), prive: prive}, nil
}
