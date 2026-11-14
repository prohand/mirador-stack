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
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/crypto/acme"

	"mirador-stack/internal/server/secrets"
)

// --- ACME ---------------------------------------------------------------------------

// Emetteur : obtient un certificat pour une CSR (ACME en vrai, faux dans
// les tests du gestionnaire).
type Emetteur interface {
	Obtenir(ctx context.Context, nom string, csrDER []byte) (chainePEM string, err error)
}

// ACME : démarche ACME avec le défi DNS-01 chez le fournisseur DNS choisi.
type ACME struct {
	DB     *sql.DB
	Cle    []byte       // clé du serveur (secrets chiffrés)
	Client *http.Client // vers l'annuaire ACME
	// BasesDNS, ClientDNS : adresses des API DNS et client HTTP (tests ;
	// vides = vraies API). Fournisseur, zone et identifiants sont lus
	// dans les réglages.
	BasesDNS  map[string]string
	ClientDNS *http.Client
	// Attente : délai entre la pose du TXT et la demande de validation
	// (propagation chez le fournisseur ; 60 s par défaut).
	Attente time.Duration
}

// Obtenir : compte (créé au premier usage), commande, défi DNS-01, CSR,
// chaîne de certificats (PEM).
func (a *ACME) Obtenir(ctx context.Context, nom string, csrDER []byte) (string, error) {
	r, err := LireReglages(ctx, a.DB)
	if err != nil {
		return "", err
	}
	ids, err := r.identifiants(a.Cle)
	if err != nil {
		return "", err
	}
	dns, err := nouveauFournisseur(r.FournisseurEffectif(), ids, r.Domaine, a.BasesDNS, a.ClientDNS)
	if err != nil {
		return "", err
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
		if err := dns.Poser(ctx, sousNom, valeur); err != nil {
			return "", err
		}
		defer func() { _ = dns.Retirer(context.WithoutCancel(ctx), sousNom, valeur) }() // nettoyage
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

// Saisie : réglages du certificat saisis dans l'interface.
type Saisie struct {
	Fournisseur, Domaine, Annuaire string
	// Identifiants : champs du fournisseur (secret vide = garder l'actuel,
	// si le fournisseur ne change pas).
	Identifiants map[string]string
}

// DefinirReglages : fournisseur DNS, zone, annuaire et identifiants
// (secrets chiffrés, jamais réaffichés).
func DefinirReglages(ctx context.Context, db *sql.DB, cle []byte, s Saisie, qui string, maintenant time.Time) error {
	r, err := LireReglages(ctx, db)
	if err != nil {
		return err
	}
	info := InfoDe(s.Fournisseur)
	if info == nil {
		return errors.New("fournisseur DNS inconnu")
	}
	domaine := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(s.Domaine), "."))
	if _, err := nomDansDomaine(domaine, domaine); err != nil || len(domaine) > 253 || strings.ContainsAny(domaine, " /") {
		return errors.New("zone DNS invalide (ex. exemple.org)")
	}
	annuaire := strings.TrimSpace(s.Annuaire)
	if annuaire != "" {
		u, err := url.Parse(annuaire)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return errors.New("annuaire ACME : adresse https:// attendue")
		}
	}
	if annuaire != r.Annuaire {
		r.CompteChiffre = "" // nouvel annuaire : nouveau compte
	}
	// Identifiants : ceux déjà rangés servent si le fournisseur ne change pas.
	anciens := map[string]string{}
	if r.FournisseurEffectif() == info.Nom {
		if anciens, err = r.identifiants(cle); err != nil {
			anciens = map[string]string{}
		}
	}
	params, secretsClairs := map[string]string{}, map[string]string{}
	for _, ch := range info.Champs {
		v := strings.TrimSpace(s.Identifiants[ch.Nom])
		if v == "" {
			v = anciens[ch.Nom]
		}
		switch {
		case v == "":
			return fmt.Errorf("%s : « %s » obligatoire", info.Titre, ch.Titre)
		case len(v) > 300 || strings.ContainsAny(v, " \r\n\t\"'"):
			return fmt.Errorf("%s : « %s » invalide", info.Titre, ch.Titre)
		case ch.Choix != nil && !contient(ch.Choix, v):
			return fmt.Errorf("%s : « %s » inconnu", info.Titre, ch.Titre)
		}
		if ch.Secret {
			secretsClairs[ch.Nom] = v
		} else {
			params[ch.Nom] = v
		}
	}
	brut, err := json.Marshal(secretsClairs)
	if err != nil {
		return err
	}
	if r.IdentifiantsChiffre, err = secrets.Chiffrer(cle, UsageIdentifiants, string(brut)); err != nil {
		return err
	}
	r.Fournisseur, r.Parametres, r.JetonChiffre = info.Nom, params, ""
	r.Domaine, r.Annuaire = domaine, annuaire
	return ecrireReglages(ctx, db, r, qui, maintenant)
}

func contient(liste []string, v string) bool {
	for _, x := range liste {
		if x == v {
			return true
		}
	}
	return false
}
