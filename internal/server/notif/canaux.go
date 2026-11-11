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
	"mime"
	"net"
	"net/http"
	"net/mail"
	"net/smtp"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"mirador-stack/internal/server/alertes"
	"mirador-stack/internal/server/secrets"
)

// Canaux de notification (SPEC.md § 17.7) : modules derrière une interface
// commune. Ajouter un canal = une structure qui implémente Canal + sa
// configuration ci-dessous ; rien d'autre ne change.

// Canal : un moyen d'envoyer un message.
type Canal interface {
	Nom() string
	// Envoyer : un message ; erreur = échec (compte pour la bascule).
	Envoyer(ctx context.Context, m *Message, o Mise) error
	// Verifier : le canal est-il joignable ? Sans envoyer de message.
	Verifier(ctx context.Context) error
}

// Noms des canaux.
const (
	NomTelegram = "telegram"
	NomNtfy     = "ntfy"
	NomEmail    = "email"
)

// --- Configuration (dans Reglages.Canaux) ------------------------------------------

// Canaux : choix du principal et du secours, bascule, chaque canal.
type Canaux struct {
	Principal string `json:"principal"`
	Secours   string `json:"secours"` // "" = pas de secours
	// Gravités envoyées sur TOUS les canaux actifs en même temps (ex. critical).
	Tous []string `json:"tous"`
	// Anti-rebond : panne après Echecs échecs de suite (vérifications ou
	// envois), retour après Retours vérifications réussies de suite.
	Echecs  int `json:"echecs"`
	Retours int `json:"retours"`
	// Vérification de joignabilité (sans message) : Telegram toutes les 5 min,
	// les autres chaque minute ; un canal en panne est revérifié chaque minute.
	VerifTelegram alertes.Duree `json:"verif_telegram"`
	VerifAutres   alertes.Duree `json:"verif_autres"`
	// ReseauPrive : autoriser ntfy et e-mail vers une adresse privée
	// (protection SSRF : refusé par défaut).
	ReseauPrive bool `json:"reseau_prive"`

	Telegram  *Telegram  `json:"telegram,omitempty"`
	Ntfy      *Ntfy      `json:"ntfy,omitempty"`
	Email     *Email     `json:"email,omitempty"`
	Olvid     *Olvid     `json:"olvid,omitempty"`
	Heartbeat *Heartbeat `json:"heartbeat,omitempty"`
}

// Telegram : bot et conversation (groupe).
type Telegram struct {
	Actif bool   `json:"actif"`
	Jeton string `json:"jeton"` // secret
	Chat  string `json:"chat"`  // identifiant du groupe (ex. -100…) ou @canal
	// Acquitter : bouton « Acquitter » sous chaque alerte (identifiant
	// numérique du groupe obligatoire). Acquitteurs : identifiants Telegram
	// des personnes autorisées ; vide = tout membre du groupe.
	Acquitter   bool    `json:"acquitter"`
	Acquitteurs []int64 `json:"acquitteurs"`
}

// Ntfy : sujet sur un serveur ntfy (https).
type Ntfy struct {
	Actif bool   `json:"actif"`
	URL   string `json:"url"`   // https://ntfy.sh/mon-sujet
	Jeton string `json:"jeton"` // secret, facultatif
}

// Email : serveur SMTP (STARTTLS ou TLS obligatoire).
type Email struct {
	Actif       bool     `json:"actif"`
	Serveur     string   `json:"serveur"`  // hôte:port
	Securite    string   `json:"securite"` // starttls (587) ou tls (465)
	Utilisateur string   `json:"utilisateur"`
	MotDePasse  string   `json:"mot_de_passe"` // secret
	De          string   `json:"de"`
	A           []string `json:"a"`
}

// Heartbeat : service de surveillance externe (HetrixTools, Healthchecks.io,
// Uptime Kuma…) appelé à intervalle régulier (GET) tant que le serveur évalue
// ses règles. Indépendant des canaux. Le service ne sert qu'à l'affichage et
// aux exemples : tous reçoivent un simple GET sur l'adresse donnée.
type Heartbeat struct {
	Actif      bool          `json:"actif"`
	Service    string        `json:"service,omitempty"` // voir ServicesHeartbeat ; vide = « autre »
	URL        string        `json:"url"`               // secret (l'URL suffit pour signaler « vivant »)
	Intervalle alertes.Duree `json:"intervalle"`
}

// ServiceHeartbeat : un service connu et la forme de son adresse.
type ServiceHeartbeat struct{ Code, Nom, Exemple string }

// ServicesHeartbeat : services proposés dans l'interface (tous en GET).
var ServicesHeartbeat = []ServiceHeartbeat{
	{"hetrixtools", "HetrixTools", "adresse du moniteur « Heartbeat » donnée par HetrixTools"},
	{"healthchecks", "Healthchecks.io", "https://hc-ping.com/<uuid> (ou votre instance)"},
	{"uptime-kuma", "Uptime Kuma", "https://kuma.exemple.fr/api/push/<jeton>?status=up&msg=OK"},
	{"betterstack", "Better Stack", "https://uptime.betterstack.com/api/v1/heartbeat/<jeton>"},
	{"uptimerobot", "UptimeRobot", "https://heartbeat.uptimerobot.com/<identifiant>"},
	{"cronitor", "Cronitor", "https://cronitor.link/p/<clé>/<moniteur>"},
	{"autre", "Autre", "toute adresse https:// qui répond 2xx à un GET"},
}

// NomService : nom lisible du service (« Autre » si inconnu ou vide).
func (h *Heartbeat) NomService() string {
	for _, s := range ServicesHeartbeat {
		if s.Code == h.Service {
			return s.Nom
		}
	}
	return "Autre"
}

// CanauxDefaut : Telegram principal, pas de secours (SPEC § 10).
func CanauxDefaut() Canaux {
	return Canaux{Principal: NomTelegram, Tous: []string{}, Echecs: 3, Retours: 3,
		VerifTelegram: alertes.Duree(5 * time.Minute), VerifAutres: alertes.Duree(time.Minute)}
}

// Usages des secrets (liés au chiffré : un secret ne peut pas être déplacé
// d'un champ à un autre).
const (
	usageTelegram  = "notif.telegram.jeton"
	usageNtfy      = "notif.ntfy.jeton"
	usageEmail     = "notif.email.mot_de_passe"
	usageHeartbeat = "notif.heartbeat.url"
)

var (
	reJetonTelegram = regexp.MustCompile(`^[0-9]{5,15}:[A-Za-z0-9_-]{30,64}$`)
	reChatTelegram  = regexp.MustCompile(`^(-?[0-9]{1,20}|@[A-Za-z0-9_]{5,32})$`)
)

func nomsCanaux() []string { return []string{NomTelegram, NomNtfy, NomEmail, NomOlvid} }

// configure : le canal nom est-il décrit et actif ?
func (c *Canaux) configure(nom string) bool {
	switch nom {
	case NomTelegram:
		return c.Telegram != nil && c.Telegram.Actif
	case NomNtfy:
		return c.Ntfy != nil && c.Ntfy.Actif
	case NomEmail:
		return c.Email != nil && c.Email.Actif
	case NomOlvid:
		return c.Olvid != nil && c.Olvid.Actif
	}
	return false
}

// Valider vérifie la configuration des canaux (secrets en clair ou chiffrés).
// exigerChoix : le principal (et le secours s'il est donné) doivent être
// configurés (mode actif ou secours choisi) ; sinon seuls les canaux décrits
// sont vérifiés.
func (c *Canaux) Valider(exigerChoix bool) error {
	var errs []error
	if !slices.Contains(nomsCanaux(), c.Principal) {
		errs = append(errs, fmt.Errorf("canal principal %q inconnu (%s)", c.Principal, strings.Join(nomsCanaux(), ", ")))
	}
	if c.Secours != "" && (!slices.Contains(nomsCanaux(), c.Secours) || c.Secours == c.Principal) {
		errs = append(errs, fmt.Errorf("canal de secours %q : inconnu ou identique au principal", c.Secours))
	}
	for _, g := range c.Tous {
		if g != alertes.Critique && g != alertes.Avertissement && g != alertes.Info {
			errs = append(errs, fmt.Errorf("tous : gravité %q inconnue", g))
		}
	}
	if c.Echecs < 1 || c.Echecs > 20 || c.Retours < 1 || c.Retours > 20 {
		errs = append(errs, errors.New("echecs et retours : entre 1 et 20"))
	}
	if d := time.Duration(c.VerifTelegram); d < time.Minute || d > time.Hour {
		errs = append(errs, errors.New("verif_telegram : entre 1 min et 1 h"))
	}
	if d := time.Duration(c.VerifAutres); d < 30*time.Second || d > time.Hour {
		errs = append(errs, errors.New("verif_autres : entre 30 s et 1 h"))
	}
	if t := c.Telegram; t != nil {
		if !secrets.EstChiffre(t.Jeton) && t.Jeton != "***" && !reJetonTelegram.MatchString(t.Jeton) {
			errs = append(errs, errors.New("telegram.jeton : format 123456:ABC… attendu"))
		}
		if !reChatTelegram.MatchString(t.Chat) {
			errs = append(errs, errors.New("telegram.chat : identifiant numérique (ex. -100…) ou @nom"))
		}
		if _, err := strconv.ParseInt(t.Chat, 10, 64); t.Acquitter && err != nil {
			errs = append(errs, errors.New("telegram.acquitter : l'identifiant numérique du groupe est nécessaire (pas @nom)"))
		}
		if len(t.Acquitteurs) > 50 {
			errs = append(errs, errors.New("telegram.acquitteurs : 50 au plus"))
		}
	}
	if n := c.Ntfy; n != nil {
		if err := urlHTTPS(n.URL, true); err != nil {
			errs = append(errs, fmt.Errorf("ntfy.url : %w", err))
		}
	}
	if e := c.Email; e != nil {
		if _, _, err := net.SplitHostPort(e.Serveur); err != nil {
			errs = append(errs, errors.New("email.serveur : hôte:port attendu"))
		}
		if e.Securite != "starttls" && e.Securite != "tls" {
			errs = append(errs, errors.New("email.securite : starttls ou tls (jamais en clair)"))
		}
		if _, err := mail.ParseAddress(e.De); err != nil {
			errs = append(errs, errors.New("email.de : adresse invalide"))
		}
		if len(e.A) == 0 || len(e.A) > 10 {
			errs = append(errs, errors.New("email.a : 1 à 10 destinataires"))
		}
		for _, a := range e.A {
			if _, err := mail.ParseAddress(a); err != nil {
				errs = append(errs, fmt.Errorf("email.a : %q invalide", a))
			}
		}
		if strings.ContainsAny(e.Utilisateur+e.De+strings.Join(e.A, ""), "\r\n") {
			errs = append(errs, errors.New("email : retour à la ligne interdit"))
		}
	}
	if o := c.Olvid; o != nil {
		if err := o.valider(c.Principal == NomOlvid || c.Secours == NomOlvid); err != nil {
			errs = append(errs, err)
		}
	}
	if h := c.Heartbeat; h != nil {
		if !secrets.EstChiffre(h.URL) && h.URL != "***" {
			if err := urlHTTPS(h.URL, false); err != nil {
				errs = append(errs, fmt.Errorf("heartbeat.url : %w", err))
			}
		}
		connu := h.Service == ""
		for _, s := range ServicesHeartbeat {
			connu = connu || s.Code == h.Service
		}
		if !connu {
			errs = append(errs, fmt.Errorf("heartbeat.service %q inconnu", h.Service))
		}
		if d := time.Duration(h.Intervalle); d < 30*time.Second || d > 10*time.Minute {
			errs = append(errs, errors.New("heartbeat.intervalle : entre 30 s et 10 min"))
		}
	}
	for _, nom := range []string{c.Principal, c.Secours} {
		if exigerChoix && nom != "" && !c.configure(nom) {
			errs = append(errs, fmt.Errorf("canal %s choisi mais non configuré ou désactivé", nom))
		}
	}
	return errors.Join(errs...)
}

// urlHTTPS : adresse https://hôte/chemin, sans identifiants dans l'URL.
func urlHTTPS(s string, cheminObligatoire bool) error {
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" {
		return errors.New("adresse https://… attendue (sans identifiant dans l'adresse)")
	}
	if cheminObligatoire && strings.Trim(u.Path, "/") == "" {
		return errors.New("le sujet (chemin) manque")
	}
	return nil
}

// Masques : copie sans aucun secret (affichage, audit).
func (c Canaux) Masques() Canaux {
	m := c
	masque := func(s string) string {
		if s == "" {
			return ""
		}
		return "***"
	}
	if c.Telegram != nil {
		t := *c.Telegram
		t.Jeton = masque(t.Jeton)
		m.Telegram = &t
	}
	if c.Ntfy != nil {
		n := *c.Ntfy
		n.Jeton = masque(n.Jeton)
		m.Ntfy = &n
	}
	if c.Email != nil {
		e := *c.Email
		e.MotDePasse = masque(e.MotDePasse)
		m.Email = &e
	}
	if c.Heartbeat != nil {
		h := *c.Heartbeat
		h.URL = masque(h.URL)
		m.Heartbeat = &h
	}
	if c.Olvid != nil {
		o := *c.Olvid
		o.Cle = masque(o.Cle)
		m.Olvid = &o
	}
	return m
}

// PreparerSecrets chiffre les secrets donnés en clair ; « *** » ou vide =
// garder la valeur déjà enregistrée (on peut renvoyer les réglages affichés).
func PreparerSecrets(nouveaux, anciens Canaux, cle []byte) (Canaux, error) {
	c := nouveaux
	var err error
	prep := func(v, ancien, usage string) string {
		if err != nil {
			return ""
		}
		if v == "" || v == "***" {
			return ancien
		}
		if secrets.EstChiffre(v) {
			return v
		}
		var x string
		x, err = secrets.Chiffrer(cle, usage, v)
		return x
	}
	if c.Telegram != nil {
		t := *c.Telegram
		anc := ""
		if anciens.Telegram != nil {
			anc = anciens.Telegram.Jeton
		}
		t.Jeton = prep(t.Jeton, anc, usageTelegram)
		c.Telegram = &t
	}
	if c.Ntfy != nil {
		n := *c.Ntfy
		anc := ""
		if anciens.Ntfy != nil {
			anc = anciens.Ntfy.Jeton
		}
		n.Jeton = prep(n.Jeton, anc, usageNtfy)
		c.Ntfy = &n
	}
	if c.Email != nil {
		e := *c.Email
		anc := ""
		if anciens.Email != nil {
			anc = anciens.Email.MotDePasse
		}
		e.MotDePasse = prep(e.MotDePasse, anc, usageEmail)
		c.Email = &e
	}
	if c.Heartbeat != nil {
		h := *c.Heartbeat
		anc := ""
		if anciens.Heartbeat != nil {
			anc = anciens.Heartbeat.URL
		}
		h.URL = prep(h.URL, anc, usageHeartbeat)
		c.Heartbeat = &h
	}
	if c.Olvid != nil {
		o := *c.Olvid
		anc := ""
		if anciens.Olvid != nil {
			anc = anciens.Olvid.Cle
		}
		o.Cle = prep(o.Cle, anc, usageOlvid)
		c.Olvid = &o
	}
	return c, err
}

// --- Réseau : délais et protection SSRF ------------------------------------------

// adresseInterdite : adresses internes (SSRF) refusées par défaut.
func adresseInterdite(ip net.IP) bool {
	if ip == nil {
		return true
	}
	cgnat := &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsMulticast() || ip.IsInterfaceLocalMulticast() || cgnat.Contains(ip)
}

// numeroteur : connexions avec délai ; adresses internes refusées sauf si
// prive (vérifié sur l'adresse réellement jointe, après résolution DNS).
func numeroteur(prive bool) *net.Dialer {
	d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	if !prive {
		d.Control = func(_, adresse string, _ syscall.RawConn) error {
			h, _, err := net.SplitHostPort(adresse)
			if err != nil {
				return err
			}
			if adresseInterdite(net.ParseIP(h)) {
				return fmt.Errorf("adresse %s interne refusée (protection SSRF ; voir reseau_prive)", h)
			}
			return nil
		}
	}
	return d
}

// clientHTTP : délais partout, pas de redirection suivie (une redirection
// pourrait viser le réseau interne), TLS 1.2 au moins, sans proxy.
func clientHTTP(prive bool) *http.Client {
	return &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			DialContext:           numeroteur(prive).DialContext,
			TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 15 * time.Second,
			MaxIdleConns:          4,
			IdleConnTimeout:       90 * time.Second,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// sansSecret : erreur réseau sans l'URL (qui contient le jeton pour Telegram
// et vaut un secret pour le heartbeat).
func sansSecret(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("%s : %w", ue.Op, ue.Err)
	}
	return err
}

// --- Telegram ------------------------------------------------------------------------

// APITelegram : adresse de l'API (remplacée dans les tests).
var APITelegram = "https://api.telegram.org"

type telegram struct {
	api, jeton, chat string
	acquitter        bool // bouton « Acquitter » sous les alertes
	client           *http.Client
}

func (t *telegram) Nom() string { return NomTelegram }

// appel : une méthode de l'API Bot ; resultat (facultatif) reçoit le champ
// « result » de la réponse.
func (t *telegram) appel(ctx context.Context, methode string, corps, resultat any) error {
	var b []byte
	if corps != nil {
		var err error
		if b, err = json.Marshal(corps); err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.api+"/bot"+t.jeton+"/"+methode, bytes.NewReader(b))
	if err != nil {
		return errors.New("telegram : requête impossible")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.client.Do(req)
	if err != nil {
		return fmt.Errorf("telegram injoignable : %w", sansSecret(err))
	}
	defer resp.Body.Close()
	var r struct {
		OK          bool            `json:"ok"`
		Description string          `json:"description"`
		Result      json.RawMessage `json:"result"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&r); err != nil || !r.OK {
		d := r.Description
		if len(d) > 200 {
			d = d[:200]
		}
		return fmt.Errorf("telegram : HTTP %d %s", resp.StatusCode, d)
	}
	if resultat != nil && len(r.Result) > 0 {
		if err := json.Unmarshal(r.Result, resultat); err != nil {
			return errors.New("telegram : réponse illisible")
		}
	}
	return nil
}

// MaxBoutons : boutons « Acquitter » au plus sous un message.
const MaxBoutons = 8

// boutons : un bouton par alerte active déjà enregistrée (numéro connu),
// sauf dans un message de fin.
func boutons(m *Message) []map[string]string {
	if m.Type == TypeResolution || m.Systeme != "" {
		return nil
	}
	var ligne []map[string]string
	for _, a := range m.Actives {
		if a.ID <= 0 || a.AcquitteePar != "" || len(ligne) == MaxBoutons {
			continue
		}
		texte := "✔ Acquitter"
		if len(m.Actives) > 1 {
			texte = "✔ " + a.Regle + " " + a.Machine()
			if len(texte) > 40 {
				texte = texte[:40]
			}
		}
		ligne = append(ligne, map[string]string{"text": strings.ToValidUTF8(texte, ""), "callback_data": fmt.Sprintf("ack:%d", a.ID)})
	}
	return ligne
}

func (t *telegram) Envoyer(ctx context.Context, m *Message, o Mise) error {
	o.HTML, o.Max = true, 4000 // limite Telegram : 4 096 caractères
	corps := map[string]any{"chat_id": t.chat, "text": Texte(m, o), "parse_mode": "HTML",
		"disable_notification": !m.Son, "link_preview_options": map[string]bool{"is_disabled": true}}
	if b := boutons(m); t.acquitter && len(b) > 0 {
		// Un bouton par ligne : lisible sur téléphone.
		clavier := make([][]map[string]string, len(b))
		for i := range b {
			clavier[i] = b[i : i+1]
		}
		corps["reply_markup"] = map[string]any{"inline_keyboard": clavier}
	}
	return t.appel(ctx, "sendMessage", corps, nil)
}

// Verifier : getMe (aucun message ; détecte un blocage réseau ou un jeton révoqué).
func (t *telegram) Verifier(ctx context.Context) error { return t.appel(ctx, "getMe", nil, nil) }

// clicTelegram : un appui sur un bouton (seul type de mise à jour demandé :
// le bot ne lit jamais les messages du groupe).
type clicTelegram struct {
	UpdateID int64 `json:"update_id"`
	Callback *struct {
		ID   string `json:"id"`
		Data string `json:"data"`
		De   struct {
			ID       int64  `json:"id"`
			Username string `json:"username"`
			Prenom   string `json:"first_name"`
		} `json:"from"`
		Message *struct {
			ID   int64 `json:"message_id"`
			Chat struct {
				ID int64 `json:"id"`
			} `json:"chat"`
		} `json:"message"`
	} `json:"callback_query"`
}

// clics : appuis sur les boutons depuis offset (sans attente : timeout 0).
func (t *telegram) clics(ctx context.Context, offset int64) ([]clicTelegram, error) {
	var out []clicTelegram
	err := t.appel(ctx, "getUpdates", map[string]any{"offset": offset, "limit": 20, "timeout": 0,
		"allowed_updates": []string{"callback_query"}}, &out)
	return out, err
}

// repondreClic : petit message affiché à la personne qui a appuyé ; le
// bouton est retiré si l'acquittement a réussi.
func (t *telegram) repondreClic(ctx context.Context, idClic, texte string, chat, message int64, retirer bool) {
	_ = t.appel(ctx, "answerCallbackQuery", map[string]any{"callback_query_id": idClic, "text": texte}, nil)
	if retirer && message != 0 {
		_ = t.appel(ctx, "editMessageReplyMarkup", map[string]any{"chat_id": chat, "message_id": message,
			"reply_markup": map[string]any{"inline_keyboard": [][]any{}}}, nil)
	}
}

// --- ntfy ----------------------------------------------------------------------------

type ntfy struct {
	url, jeton string
	client     *http.Client
}

func (n *ntfy) Nom() string { return NomNtfy }

func (n *ntfy) requete(ctx context.Context, methode, adresse string, corps io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, methode, adresse, corps)
	if err != nil {
		return nil, errors.New("ntfy : requête impossible")
	}
	if n.jeton != "" {
		req.Header.Set("Authorization", "Bearer "+n.jeton)
	}
	return req, nil
}

func (n *ntfy) Envoyer(ctx context.Context, m *Message, o Mise) error {
	o.HTML, o.Max = false, 4000
	req, err := n.requete(ctx, http.MethodPost, n.url, strings.NewReader(Texte(m, o)))
	if err != nil {
		return err
	}
	titre := "mirador"
	if len(m.Actives) > 0 {
		titre += " : " + m.Actives[0].Regle + " " + m.Actives[0].Machine()
	} else if len(m.Resolues) > 0 {
		titre += " : résolu " + m.Resolues[0].Regle
	}
	req.Header.Set("Title", mime.BEncoding.Encode("utf-8", titre))
	prio := map[string]string{alertes.Critique: "urgent", alertes.Avertissement: "default", alertes.Info: "low"}[m.Gravite]
	if !m.Son && prio == "urgent" {
		prio = "high"
	}
	req.Header.Set("Priority", prio)
	req.Header.Set("Tags", map[string]string{alertes.Critique: "rotating_light", alertes.Avertissement: "warning"}[m.Gravite])
	r, err := n.client.Do(req)
	if err != nil {
		return fmt.Errorf("ntfy injoignable : %w", sansSecret(err))
	}
	defer r.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 64<<10))
	if r.StatusCode/100 != 2 {
		return fmt.Errorf("ntfy : HTTP %d", r.StatusCode)
	}
	return nil
}

// Verifier : /v1/health du serveur ntfy.
func (n *ntfy) Verifier(ctx context.Context) error {
	u, _ := url.Parse(n.url)
	req, err := n.requete(ctx, http.MethodGet, u.Scheme+"://"+u.Host+"/v1/health", nil)
	if err != nil {
		return err
	}
	r, err := n.client.Do(req)
	if err != nil {
		return fmt.Errorf("ntfy injoignable : %w", sansSecret(err))
	}
	defer r.Body.Close()
	var h struct {
		Healthy bool `json:"healthy"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&h); err != nil || !h.Healthy {
		return fmt.Errorf("ntfy : serveur pas en bonne santé (HTTP %d)", r.StatusCode)
	}
	return nil
}

// --- E-mail ---------------------------------------------------------------------------

type email struct {
	cfg   Email
	mdp   string
	prive bool
}

func (e *email) Nom() string { return NomEmail }

// session ouvre une session SMTP chiffrée et authentifiée (30 s au plus).
func (e *email) session(ctx context.Context) (*smtp.Client, error) {
	hote, _, _ := net.SplitHostPort(e.cfg.Serveur)
	d := numeroteur(e.prive)
	conn, err := d.DialContext(ctx, "tcp", e.cfg.Serveur)
	if err != nil {
		return nil, fmt.Errorf("smtp injoignable : %w", err)
	}
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	tc := &tls.Config{ServerName: hote, MinVersion: tls.VersionTLS12}
	if e.cfg.Securite == "tls" {
		conn = tls.Client(conn, tc)
	}
	c, err := smtp.NewClient(conn, hote)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("smtp : %w", err)
	}
	if e.cfg.Securite == "starttls" {
		if err := c.StartTLS(tc); err != nil {
			_ = c.Close()
			return nil, fmt.Errorf("smtp starttls : %w", err)
		}
	}
	if e.cfg.Utilisateur != "" {
		if err := c.Auth(smtp.PlainAuth("", e.cfg.Utilisateur, e.mdp, hote)); err != nil {
			_ = c.Close()
			return nil, errors.New("smtp : authentification refusée")
		}
	}
	return c, nil
}

func (e *email) Envoyer(ctx context.Context, m *Message, o Mise) error {
	o.HTML, o.Max = false, 64<<10
	c, err := e.session(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	de, _ := mail.ParseAddress(e.cfg.De)
	if err := c.Mail(de.Address); err != nil {
		return fmt.Errorf("smtp MAIL : %w", err)
	}
	var a []string
	for _, x := range e.cfg.A {
		ad, _ := mail.ParseAddress(x)
		if err := c.Rcpt(ad.Address); err != nil {
			return fmt.Errorf("smtp RCPT : %w", err)
		}
		a = append(a, ad.String())
	}
	sujet := "[mirador] "
	if len(m.Actives) > 0 {
		sujet += m.Actives[0].Gravite + " " + m.Actives[0].Regle + " " + m.Actives[0].Machine()
	} else if len(m.Resolues) > 0 {
		sujet += "résolu " + m.Resolues[0].Regle + " " + m.Resolues[0].Machine()
	}
	id := make([]byte, 12)
	_, _ = rand.Read(id)
	hote, _, _ := net.SplitHostPort(e.cfg.Serveur)
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("smtp DATA : %w", err)
	}
	corps := Texte(m, o)
	fmt.Fprintf(w, "From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\nMessage-ID: <%s@%s>\r\nMIME-Version: 1.0\r\n"+
		"Content-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\n%s\r\n",
		de.String(), strings.Join(a, ", "), mime.QEncoding.Encode("utf-8", sujet), m.Le.Format(time.RFC1123Z),
		hex.EncodeToString(id), hote, strings.ReplaceAll(corps, "\n", "\r\n"))
	if err := w.Close(); err != nil {
		return fmt.Errorf("smtp : message refusé : %w", err)
	}
	return c.Quit()
}

// Verifier : connexion, chiffrement, authentification, sans message.
func (e *email) Verifier(ctx context.Context) error {
	c, err := e.session(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	return c.Quit()
}

// --- Fabrication à partir des réglages ------------------------------------------------

// fabriquer : canaux actifs, secrets déchiffrés (jamais gardés ailleurs qu'ici,
// en mémoire).
func fabriquer(c *Canaux, cle []byte) (map[string]Canal, error) {
	out := map[string]Canal{}
	if t := c.Telegram; t != nil && t.Actif {
		j, err := secrets.Dechiffrer(cle, usageTelegram, t.Jeton)
		if err != nil {
			return nil, fmt.Errorf("telegram.jeton : %w", err)
		}
		// Adresse de l'API figée dans le code : pas de risque SSRF ici.
		out[NomTelegram] = &telegram{api: APITelegram, jeton: j, chat: t.Chat, acquitter: t.Acquitter, client: clientHTTP(c.ReseauPrive)}
	}
	if n := c.Ntfy; n != nil && n.Actif {
		j := ""
		if n.Jeton != "" {
			var err error
			if j, err = secrets.Dechiffrer(cle, usageNtfy, n.Jeton); err != nil {
				return nil, fmt.Errorf("ntfy.jeton : %w", err)
			}
		}
		out[NomNtfy] = &ntfy{url: n.URL, jeton: j, client: clientHTTP(c.ReseauPrive)}
	}
	if e := c.Email; e != nil && e.Actif {
		m := ""
		if e.MotDePasse != "" {
			var err error
			if m, err = secrets.Dechiffrer(cle, usageEmail, e.MotDePasse); err != nil {
				return nil, fmt.Errorf("email.mot_de_passe : %w", err)
			}
		}
		out[NomEmail] = &email{cfg: *e, mdp: m, prive: c.ReseauPrive}
	}
	// Olvid : daemon sur le réseau Docker interne (adresse privée par nature,
	// donnée par l'administrateur) ; jamais exposé.
	if o := c.Olvid; o != nil && o.Actif {
		k, err := secrets.Dechiffrer(cle, usageOlvid, o.Cle)
		if err != nil {
			return nil, fmt.Errorf("olvid.cle : %w", err)
		}
		ov, err := nouvelOlvid(*o, k)
		if err != nil {
			return nil, err
		}
		out[NomOlvid] = ov
	}
	return out, nil
}
