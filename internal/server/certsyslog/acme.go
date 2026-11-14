// SPDX-License-Identifier: GPL-3.0-or-later

package certsyslog

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/crypto/acme"

	"mirador-stack/internal/server/secrets"
)

// --- deSEC (défi DNS-01) --------------------------------------------------------

// BaseDeSEC : API de deSEC.
const BaseDeSEC = "https://desec.io/api/v1"

// DeSEC : enregistrements TXT du défi, par l'API de deSEC (un seul appel
// HTTPS, écrit à la main).
type DeSEC struct {
	Base, Jeton, Domaine string
	Client               *http.Client
}

// poser : enregistrement TXT (valeurs vides = supprimé).
func (d DeSEC) poser(ctx context.Context, sousNom string, valeurs []string) error {
	enregistrements := make([]string, 0, len(valeurs))
	for _, v := range valeurs {
		enregistrements = append(enregistrements, `"`+v+`"`)
	}
	corps, err := json.Marshal([]map[string]any{{"subname": sousNom, "type": "TXT", "ttl": 3600, "records": enregistrements}})
	if err != nil {
		return err
	}
	base := d.Base
	if base == "" {
		base = BaseDeSEC
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, base+"/domains/"+url.PathEscape(d.Domaine)+"/rrsets/", bytes.NewReader(corps))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Token "+d.Jeton)
	req.Header.Set("Content-Type", "application/json")
	client := d.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("deSEC : %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("deSEC : réponse %d (%s)", resp.StatusCode, strings.TrimSpace(string(detail)))
	}
	return nil
}

// --- ACME ---------------------------------------------------------------------------

// Emetteur : obtient un certificat pour une CSR (ACME en vrai, faux dans
// les tests du gestionnaire).
type Emetteur interface {
	Obtenir(ctx context.Context, nom string, csrDER []byte) (chainePEM string, err error)
}

// ACME : démarche ACME avec le défi DNS-01 chez deSEC.
type ACME struct {
	DB     *sql.DB
	Cle    []byte // clé du serveur (secrets chiffrés)
	DeSEC  DeSEC  // Base et Client ; jeton et zone lus dans les réglages
	Client *http.Client
	// Attente : délai entre la pose du TXT et la demande de validation
	// (propagation chez deSEC ; 60 s par défaut).
	Attente time.Duration
}

// Obtenir : compte (créé au premier usage), commande, défi DNS-01, CSR,
// chaîne de certificats (PEM).
func (a *ACME) Obtenir(ctx context.Context, nom string, csrDER []byte) (string, error) {
	r, err := LireReglages(ctx, a.DB)
	if err != nil {
		return "", err
	}
	if r.JetonChiffre == "" {
		return "", errors.New("jeton deSEC pas encore saisi (page du collecteur › Certificat)")
	}
	jeton, err := secrets.Dechiffrer(a.Cle, UsageJeton, r.JetonChiffre)
	if err != nil {
		return "", fmt.Errorf("jeton deSEC illisible : %w", err)
	}
	sousNom, err := nomDansDomaine(nom, r.Domaine)
	if err != nil {
		return "", err
	}
	compte, err := a.compte(ctx, &r)
	if err != nil {
		return "", err
	}
	c := &acme.Client{Key: compte, DirectoryURL: r.AnnuaireEffectif(), HTTPClient: a.Client, UserAgent: "mirador-server"}
	if _, err := c.Register(ctx, &acme.Account{}, acme.AcceptTOS); err != nil && !errors.Is(err, acme.ErrAccountAlreadyExists) {
		return "", fmt.Errorf("compte ACME : %w", err)
	}
	commande, err := c.AuthorizeOrder(ctx, acme.DomainIDs(nom))
	if err != nil {
		return "", fmt.Errorf("commande ACME : %w", err)
	}
	dns := a.DeSEC
	dns.Jeton, dns.Domaine = jeton, r.Domaine
	for _, u := range commande.AuthzURLs {
		z, err := c.GetAuthorization(ctx, u)
		if err != nil {
			return "", err
		}
		if z.Status == acme.StatusValid {
			continue
		}
		var defi *acme.Challenge
		for _, ch := range z.Challenges {
			if ch.Type == "dns-01" {
				defi = ch
			}
		}
		if defi == nil {
			return "", errors.New("ACME : pas de défi DNS-01 proposé")
		}
		valeur, err := c.DNS01ChallengeRecord(defi.Token)
		if err != nil {
			return "", err
		}
		if err := dns.poser(ctx, sousNom, []string{valeur}); err != nil {
			return "", err
		}
		defer func() { _ = dns.poser(context.WithoutCancel(ctx), sousNom, nil) }() // nettoyage
		attente := a.Attente
		if attente == 0 {
			attente = time.Minute
		}
		select {
		case <-time.After(attente):
		case <-ctx.Done():
			return "", ctx.Err()
		}
		if _, err := c.Accept(ctx, defi); err != nil {
			return "", fmt.Errorf("ACME : défi refusé : %w", err)
		}
		if _, err := c.WaitAuthorization(ctx, z.URI); err != nil {
			return "", fmt.Errorf("ACME : validation : %w", err)
		}
	}
	if commande, err = c.WaitOrder(ctx, commande.URI); err != nil {
		return "", fmt.Errorf("ACME : commande : %w", err)
	}
	ders, _, err := c.CreateCertFromOrder(ctx, commande, csrDER, true)
	if err != nil {
		return "", fmt.Errorf("ACME : certificat : %w", err)
	}
	var sortie bytes.Buffer
	for _, d := range ders {
		_ = pem.Encode(&sortie, &pem.Block{Type: "CERTIFICATE", Bytes: d})
	}
	return sortie.String(), nil
}

// compte : clé du compte ACME (créée et enregistrée chiffrée au premier usage).
func (a *ACME) compte(ctx context.Context, r *Reglages) (*ecdsa.PrivateKey, error) {
	if r.CompteChiffre != "" {
		p, err := secrets.Dechiffrer(a.Cle, UsageCompte, r.CompteChiffre)
		if err != nil {
			return nil, err
		}
		bloc, _ := pem.Decode([]byte(p))
		if bloc == nil {
			return nil, errors.New("clé du compte ACME illisible")
		}
		return x509.ParseECPrivateKey(bloc.Bytes)
	}
	cle, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalECPrivateKey(cle)
	if err != nil {
		return nil, err
	}
	if r.CompteChiffre, err = secrets.Chiffrer(a.Cle, UsageCompte, string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}))); err != nil {
		return nil, err
	}
	return cle, ecrireReglages(ctx, a.DB, *r, "", time.Now())
}

// DefinirReglages : zone deSEC, annuaire et (s'il est donné) jeton deSEC,
// chiffré. Jeton vide = garder l'actuel.
func DefinirReglages(ctx context.Context, db *sql.DB, cle []byte, domaine, annuaire, jeton, qui string, maintenant time.Time) error {
	r, err := LireReglages(ctx, db)
	if err != nil {
		return err
	}
	domaine = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(domaine), "."))
	if _, err := nomDansDomaine(domaine, domaine); err != nil || len(domaine) > 253 || strings.ContainsAny(domaine, " /") {
		return errors.New("zone deSEC invalide (ex. exemple.org)")
	}
	if annuaire = strings.TrimSpace(annuaire); annuaire != "" {
		u, err := url.Parse(annuaire)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return errors.New("annuaire ACME : adresse https:// attendue")
		}
	}
	if annuaire != r.Annuaire {
		r.CompteChiffre = "" // nouvel annuaire : nouveau compte
	}
	r.Domaine, r.Annuaire = domaine, annuaire
	if jeton = strings.TrimSpace(jeton); jeton != "" {
		if len(jeton) > 200 || strings.ContainsAny(jeton, " \r\n\t") {
			return errors.New("jeton deSEC invalide")
		}
		if r.JetonChiffre, err = secrets.Chiffrer(cle, UsageJeton, jeton); err != nil {
			return err
		}
	}
	return ecrireReglages(ctx, db, r, qui, maintenant)
}
