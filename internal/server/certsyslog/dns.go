// SPDX-License-Identifier: GPL-3.0-or-later

package certsyslog

import (
	"bytes"
	"context"
	"crypto/sha1" // #nosec G505 -- imposé par la signature des requêtes de l'API OVH, pas un usage cryptographique choisi
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Fournisseurs DNS du défi DNS-01 (SPEC § 23.17), ceux des clients ACME
// courants : deSEC, Cloudflare, OVHcloud, Gandi, Infomaniak. Chacun pose
// puis retire un enregistrement TXT par son API (appels HTTPS écrits à la
// main, aucune dépendance). Les identifiants restent sur le serveur,
// chiffrés.

// FournisseurDNS : pose et retire l'enregistrement TXT du défi.
type FournisseurDNS interface {
	Poser(ctx context.Context, sousNom, valeur string) error
	Retirer(ctx context.Context, sousNom, valeur string) error
}

// Champ : identifiant demandé par un fournisseur.
type Champ struct {
	Nom, Titre string
	Secret     bool     // jamais réaffiché
	Choix      []string // liste fermée (point d'accès OVH)
}

// InfoFournisseur : un fournisseur proposé dans l'interface.
type InfoFournisseur struct {
	Nom, Titre, Aide string
	Champs           []Champ
}

// Fournisseurs : fournisseurs proposés (ordre d'affichage).
var Fournisseurs = []InfoFournisseur{
	{"desec", "deSEC", "jeton limité à l'enregistrement _acme-challenge (règle de jeton)",
		[]Champ{{Nom: "jeton", Titre: "Jeton", Secret: true}}},
	{"cloudflare", "Cloudflare", "jeton d'API : Zone › DNS › Modifier, limité à cette zone",
		[]Champ{{Nom: "jeton", Titre: "Jeton d'API", Secret: true}}},
	{"ovh", "OVHcloud", "clés créées sur la page « createToken » de l'API : GET/POST/DELETE sur /domain/zone/<zone>/*",
		[]Champ{{Nom: "point", Titre: "Point d'accès", Choix: []string{"ovh-eu", "ovh-ca", "ovh-us"}},
			{Nom: "cle_application", Titre: "Clé d'application"},
			{Nom: "secret_application", Titre: "Secret d'application", Secret: true},
			{Nom: "cle_consommateur", Titre: "Clé consommateur", Secret: true}}},
	{"gandi", "Gandi (LiveDNS)", "jeton d'accès personnel, droit « Gérer la configuration technique des domaines »",
		[]Champ{{Nom: "jeton", Titre: "Jeton d'accès personnel", Secret: true}}},
	{"infomaniak", "Infomaniak", "jeton d'API avec le droit « domain:read, dns:write »",
		[]Champ{{Nom: "jeton", Titre: "Jeton d'API", Secret: true}}},
}

// InfoDe : description d'un fournisseur (nil s'il est inconnu).
func InfoDe(nom string) *InfoFournisseur {
	for i := range Fournisseurs {
		if Fournisseurs[i].Nom == nom {
			return &Fournisseurs[i]
		}
	}
	return nil
}

// Bases par défaut des API (remplaçables dans les tests).
var basesDefaut = map[string]string{
	"desec":      "https://desec.io/api/v1",
	"cloudflare": "https://api.cloudflare.com/client/v4",
	"ovh-eu":     "https://eu.api.ovh.com/1.0",
	"ovh-ca":     "https://ca.api.ovh.com/1.0",
	"ovh-us":     "https://api.us.ovhcloud.com/1.0",
	"gandi":      "https://api.gandi.net/v5/livedns",
	"infomaniak": "https://api.infomaniak.com",
}

// nouveauFournisseur : client du fournisseur choisi.
func nouveauFournisseur(nom string, ids map[string]string, zone string, bases map[string]string, client *http.Client) (FournisseurDNS, error) {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	base := func(cle string) string {
		if b, ok := bases[cle]; ok {
			return b
		}
		return basesDefaut[cle]
	}
	a := appelant{client: client}
	switch nom {
	case "desec", "":
		return &deSEC{a: a, base: base("desec"), jeton: ids["jeton"], zone: zone}, nil
	case "cloudflare":
		return &cloudflare{a: a, base: base("cloudflare"), jeton: ids["jeton"], zone: zone}, nil
	case "ovh":
		b := base(ids["point"])
		if b == "" {
			return nil, errors.New("OVH : point d'accès inconnu")
		}
		return &ovh{a: a, base: b, ak: ids["cle_application"], as: ids["secret_application"], ck: ids["cle_consommateur"], zone: zone, ids: map[string]int64{}}, nil
	case "gandi":
		return &gandi{a: a, base: base("gandi"), jeton: ids["jeton"], zone: zone}, nil
	case "infomaniak":
		return &infomaniak{a: a, base: base("infomaniak"), jeton: ids["jeton"], zone: zone, ids: map[string]string{}}, nil
	}
	return nil, fmt.Errorf("fournisseur DNS %q inconnu", nom)
}

// --- Appels HTTP communs ----------------------------------------------------------------

type appelant struct{ client *http.Client }

// appeler : requête JSON ; réponse 2xx décodée dans out (si non nil).
func (a appelant) appeler(ctx context.Context, methode, adresse string, entetes map[string]string, corps, out any) error {
	var lecteur io.Reader
	var brut []byte
	if corps != nil {
		var err error
		if brut, err = json.Marshal(corps); err != nil {
			return err
		}
		lecteur = bytes.NewReader(brut)
	}
	req, err := http.NewRequestWithContext(ctx, methode, adresse, lecteur)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if corps != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range entetes {
		req.Header.Set(k, v)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	reponse, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		detail := strings.TrimSpace(string(reponse))
		if len(detail) > 300 {
			detail = detail[:300]
		}
		return fmt.Errorf("réponse %d (%s)", resp.StatusCode, detail)
	}
	if out != nil && len(reponse) > 0 {
		return json.Unmarshal(reponse, out)
	}
	return nil
}

func enrobe(fournisseur string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s : %w", fournisseur, err)
}

// --- deSEC ---------------------------------------------------------------------------------

type deSEC struct {
	a                 appelant
	base, jeton, zone string
}

func (d *deSEC) poser(ctx context.Context, sousNom string, valeurs []string) error {
	enr := make([]string, 0, len(valeurs))
	for _, v := range valeurs {
		enr = append(enr, `"`+v+`"`)
	}
	corps := []map[string]any{{"subname": sousNom, "type": "TXT", "ttl": 3600, "records": enr}}
	return enrobe("deSEC", d.a.appeler(ctx, http.MethodPatch, d.base+"/domains/"+url.PathEscape(d.zone)+"/rrsets/",
		map[string]string{"Authorization": "Token " + d.jeton}, corps, nil))
}

func (d *deSEC) Poser(ctx context.Context, s, v string) error { return d.poser(ctx, s, []string{v}) }
func (d *deSEC) Retirer(ctx context.Context, s, _ string) error {
	return d.poser(ctx, s, nil)
}

// --- Cloudflare ----------------------------------------------------------------------------

type cloudflare struct {
	a                 appelant
	base, jeton, zone string
	idZone            string
}

func (c *cloudflare) entetes() map[string]string {
	return map[string]string{"Authorization": "Bearer " + c.jeton}
}

func (c *cloudflare) zoneID(ctx context.Context) (string, error) {
	if c.idZone != "" {
		return c.idZone, nil
	}
	var r struct {
		Result []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"result"`
	}
	if err := c.a.appeler(ctx, http.MethodGet, c.base+"/zones?name="+url.QueryEscape(c.zone), c.entetes(), nil, &r); err != nil {
		return "", err
	}
	for _, z := range r.Result {
		if z.Name == c.zone {
			c.idZone = z.ID
			return z.ID, nil
		}
	}
	return "", fmt.Errorf("zone %s introuvable (jeton limité à une autre zone ?)", c.zone)
}

func (c *cloudflare) Poser(ctx context.Context, sousNom, valeur string) error {
	id, err := c.zoneID(ctx)
	if err != nil {
		return enrobe("Cloudflare", err)
	}
	corps := map[string]any{"type": "TXT", "name": sousNom + "." + c.zone, "content": `"` + valeur + `"`, "ttl": 120}
	return enrobe("Cloudflare", c.a.appeler(ctx, http.MethodPost, c.base+"/zones/"+url.PathEscape(id)+"/dns_records", c.entetes(), corps, nil))
}

func (c *cloudflare) Retirer(ctx context.Context, sousNom, valeur string) error {
	id, err := c.zoneID(ctx)
	if err != nil {
		return enrobe("Cloudflare", err)
	}
	var r struct {
		Result []struct {
			ID      string `json:"id"`
			Content string `json:"content"`
		} `json:"result"`
	}
	nom := sousNom + "." + c.zone
	if err := c.a.appeler(ctx, http.MethodGet, c.base+"/zones/"+url.PathEscape(id)+"/dns_records?type=TXT&name="+url.QueryEscape(nom), c.entetes(), nil, &r); err != nil {
		return enrobe("Cloudflare", err)
	}
	for _, e := range r.Result {
		if strings.Trim(e.Content, `"`) == valeur {
			if err := c.a.appeler(ctx, http.MethodDelete, c.base+"/zones/"+url.PathEscape(id)+"/dns_records/"+url.PathEscape(e.ID), c.entetes(), nil, nil); err != nil {
				return enrobe("Cloudflare", err)
			}
		}
	}
	return nil
}

// --- OVHcloud ------------------------------------------------------------------------------

type ovh struct {
	a                      appelant
	base, ak, as, ck, zone string
	mu                     sync.Mutex
	ids                    map[string]int64 // valeur → identifiant de l'enregistrement
}

// signer : en-têtes signés de l'API OVH (« $1$ » + SHA-1 de
// secret+clé consommateur+méthode+URL+corps+horodatage).
func (o *ovh) signer(ctx context.Context, methode, adresse string, corps any) (map[string]string, error) {
	var t int64
	if err := o.a.appeler(ctx, http.MethodGet, o.base+"/auth/time", nil, nil, &t); err != nil {
		return nil, err
	}
	brut := ""
	if corps != nil {
		b, err := json.Marshal(corps)
		if err != nil {
			return nil, err
		}
		brut = string(b)
	}
	ts := strconv.FormatInt(t, 10)
	h := sha1.Sum([]byte(o.as + "+" + o.ck + "+" + methode + "+" + adresse + "+" + brut + "+" + ts)) // #nosec G401 -- signature imposée par l'API OVH
	return map[string]string{"X-Ovh-Application": o.ak, "X-Ovh-Consumer": o.ck, "X-Ovh-Timestamp": ts,
		"X-Ovh-Signature": "$1$" + hex.EncodeToString(h[:])}, nil
}

func (o *ovh) appel(ctx context.Context, methode, chemin string, corps, out any) error {
	adresse := o.base + chemin
	e, err := o.signer(ctx, methode, adresse, corps)
	if err != nil {
		return err
	}
	return o.a.appeler(ctx, methode, adresse, e, corps, out)
}

func (o *ovh) Poser(ctx context.Context, sousNom, valeur string) error {
	var r struct {
		ID int64 `json:"id"`
	}
	zone := "/domain/zone/" + url.PathEscape(o.zone)
	if err := o.appel(ctx, http.MethodPost, zone+"/record", map[string]any{"fieldType": "TXT", "subDomain": sousNom, "target": valeur, "ttl": 60}, &r); err != nil {
		return enrobe("OVH", err)
	}
	o.mu.Lock()
	o.ids[valeur] = r.ID
	o.mu.Unlock()
	return enrobe("OVH", o.appel(ctx, http.MethodPost, zone+"/refresh", nil, nil))
}

func (o *ovh) Retirer(ctx context.Context, _, valeur string) error {
	o.mu.Lock()
	id, ok := o.ids[valeur]
	delete(o.ids, valeur)
	o.mu.Unlock()
	if !ok {
		return nil
	}
	zone := "/domain/zone/" + url.PathEscape(o.zone)
	if err := o.appel(ctx, http.MethodDelete, zone+"/record/"+strconv.FormatInt(id, 10), nil, nil); err != nil {
		return enrobe("OVH", err)
	}
	return enrobe("OVH", o.appel(ctx, http.MethodPost, zone+"/refresh", nil, nil))
}

// --- Gandi (LiveDNS) -----------------------------------------------------------------------

type gandi struct {
	a                 appelant
	base, jeton, zone string
}

func (g *gandi) chemin(sousNom string) string {
	return g.base + "/domains/" + url.PathEscape(g.zone) + "/records/" + url.PathEscape(sousNom) + "/TXT"
}

func (g *gandi) Poser(ctx context.Context, sousNom, valeur string) error {
	return enrobe("Gandi", g.a.appeler(ctx, http.MethodPut, g.chemin(sousNom), map[string]string{"Authorization": "Bearer " + g.jeton},
		map[string]any{"rrset_values": []string{`"` + valeur + `"`}, "rrset_ttl": 300}, nil))
}

func (g *gandi) Retirer(ctx context.Context, sousNom, _ string) error {
	return enrobe("Gandi", g.a.appeler(ctx, http.MethodDelete, g.chemin(sousNom), map[string]string{"Authorization": "Bearer " + g.jeton}, nil, nil))
}

// --- Infomaniak ----------------------------------------------------------------------------

type infomaniak struct {
	a                 appelant
	base, jeton, zone string
	idDomaine         int64
	mu                sync.Mutex
	ids               map[string]string // valeur → identifiant de l'enregistrement
}

func (i *infomaniak) entetes() map[string]string {
	return map[string]string{"Authorization": "Bearer " + i.jeton}
}

func (i *infomaniak) domaine(ctx context.Context) (int64, error) {
	if i.idDomaine != 0 {
		return i.idDomaine, nil
	}
	var r struct {
		Data []struct {
			ID           int64  `json:"id"`
			CustomerName string `json:"customer_name"`
		} `json:"data"`
	}
	if err := i.a.appeler(ctx, http.MethodGet, i.base+"/1/product?service_name=domain&customer_name="+url.QueryEscape(i.zone), i.entetes(), nil, &r); err != nil {
		return 0, err
	}
	for _, d := range r.Data {
		if d.CustomerName == i.zone {
			i.idDomaine = d.ID
			return d.ID, nil
		}
	}
	return 0, fmt.Errorf("domaine %s introuvable pour ce jeton", i.zone)
}

func (i *infomaniak) Poser(ctx context.Context, sousNom, valeur string) error {
	id, err := i.domaine(ctx)
	if err != nil {
		return enrobe("Infomaniak", err)
	}
	var r struct {
		Data json.RawMessage `json:"data"`
	}
	if err := i.a.appeler(ctx, http.MethodPost, i.base+"/1/domain/"+strconv.FormatInt(id, 10)+"/dns/record", i.entetes(),
		map[string]any{"source": sousNom, "type": "TXT", "target": valeur, "ttl": 300}, &r); err != nil {
		return enrobe("Infomaniak", err)
	}
	i.mu.Lock()
	i.ids[valeur] = strings.Trim(string(r.Data), `"`)
	i.mu.Unlock()
	return nil
}

func (i *infomaniak) Retirer(ctx context.Context, _, valeur string) error {
	i.mu.Lock()
	rid, ok := i.ids[valeur]
	delete(i.ids, valeur)
	i.mu.Unlock()
	if !ok || rid == "" {
		return nil
	}
	id, err := i.domaine(ctx)
	if err != nil {
		return enrobe("Infomaniak", err)
	}
	return enrobe("Infomaniak", i.a.appeler(ctx, http.MethodDelete, i.base+"/1/domain/"+strconv.FormatInt(id, 10)+"/dns/record/"+url.PathEscape(rid), i.entetes(), nil, nil))
}
