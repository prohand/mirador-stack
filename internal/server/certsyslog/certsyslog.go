// SPDX-License-Identifier: GPL-3.0-or-later

// Package certsyslog : certificat du syslog TLS de chaque mirador-collector
// (SPEC § 23.8, § 23.14). Le collecteur crée sa clé et envoie une demande
// de certificat (CSR) avec son battement ; le serveur fait la démarche ACME
// (Let's Encrypt, défi DNS-01 chez deSEC : le jeton deSEC reste sur le
// serveur, chiffré) puis met le certificat dans la configuration du
// collecteur. Renouvellement automatique 30 jours avant l'échéance ;
// « Renouveler maintenant » dans l'interface.
package certsyslog

import (
	"context"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
	"time"

	"mirador-stack/internal/server/secrets"
)

// Bornes et rythmes.
const (
	TailleMaxCSR      = 4 << 10
	RenouvelerAvant   = 30 * 24 * time.Hour
	AttenteApresEchec = 6 * time.Hour
)

// LireCSR : CSR PEM vérifiée (signature, clé ECDSA P-256 / P-384 ou RSA ≥ 2048
// bits, un seul nom DNS).
func LireCSR(csrPEM string) (*x509.CertificateRequest, error) {
	if len(csrPEM) > TailleMaxCSR {
		return nil, errors.New("demande de certificat trop grande")
	}
	bloc, _ := pem.Decode([]byte(csrPEM))
	if bloc == nil || bloc.Type != "CERTIFICATE REQUEST" {
		return nil, errors.New("demande de certificat illisible")
	}
	req, err := x509.ParseCertificateRequest(bloc.Bytes)
	if err != nil {
		return nil, err
	}
	if err := req.CheckSignature(); err != nil {
		return nil, fmt.Errorf("demande de certificat : %w", err)
	}
	switch k := req.PublicKey.(type) {
	case *ecdsa.PublicKey:
		if n := k.Curve.Params().BitSize; n != 256 && n != 384 {
			return nil, errors.New("demande de certificat : courbe non acceptée")
		}
	case *rsa.PublicKey:
		if k.N.BitLen() < 2048 {
			return nil, errors.New("demande de certificat : clé RSA trop courte")
		}
	default:
		return nil, errors.New("demande de certificat : type de clé non accepté")
	}
	if len(req.DNSNames) != 1 || len(req.IPAddresses) > 0 || len(req.EmailAddresses) > 0 || len(req.URIs) > 0 {
		return nil, errors.New("demande de certificat : un seul nom DNS attendu")
	}
	return req, nil
}

// NoterCSR : demande reçue avec le battement d'un collecteur (enregistrée si
// elle a changé).
func NoterCSR(ctx context.Context, db *sql.DB, machine int64, csrPEM string, maintenant time.Time) error {
	if _, err := LireCSR(csrPEM); err != nil {
		return err
	}
	_, err := db.ExecContext(ctx, `INSERT INTO certificats_syslog(machine_id, csr, csr_recue_le) VALUES (?, ?, ?)
		ON CONFLICT(machine_id) DO UPDATE SET csr = excluded.csr, csr_recue_le = excluded.csr_recue_le WHERE certificats_syslog.csr != excluded.csr`,
		machine, csrPEM, maintenant.Unix())
	return err
}

// Etat : certificat d'un collecteur, pour l'interface et les mesures.
type Etat struct {
	Nom                    string // nom du dernier certificat obtenu
	CSR                    bool   // demande reçue
	CSRRecueLe             int64
	Fin, ObtenuLe, EssaiLe int64
	Erreur                 string
	DemandeLe              int64 // « Renouveler maintenant »
	Certificat             string
	// Etape : étape de la démarche en cours, ou celle de l'échec (vide =
	// pas de démarche) ; EtapeLe : début ; EtapeJusqua : fin d'une attente.
	Etape                string
	EtapeInfo            string // ex. « visible sur 1/2 serveurs DNS »
	EtapeLe, EtapeJusqua int64
}

// LireEtat : état du certificat d'un collecteur (zéro si rien).
func LireEtat(ctx context.Context, db *sql.DB, machine int64) (Etat, error) {
	var e Etat
	var csr string
	err := db.QueryRowContext(ctx, `SELECT nom, csr, csr_recue_le, certificat, fin, obtenu_le, essai_le, erreur, demande_le,
		etape, etape_info, etape_le, etape_jusqua FROM certificats_syslog WHERE machine_id = ?`, machine).Scan(&e.Nom, &csr, &e.CSRRecueLe,
		&e.Certificat, &e.Fin, &e.ObtenuLe, &e.EssaiLe, &e.Erreur, &e.DemandeLe, &e.Etape, &e.EtapeInfo, &e.EtapeLe, &e.EtapeJusqua)
	if errors.Is(err, sql.ErrNoRows) {
		return Etat{}, nil
	}
	e.CSR = csr != ""
	return e, err
}

// CertificatCollecteur : un collecteur approuvé et l'état de son certificat
// (page Réglages › Certificats).
type CertificatCollecteur struct {
	ID   int64
	Nom  string // nom du collecteur
	Etat Etat
}

// ListerCollecteurs : tous les collecteurs approuvés, par nom, avec l'état
// de leur certificat (zéro sans demande).
func ListerCollecteurs(ctx context.Context, db *sql.DB) ([]CertificatCollecteur, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, nom FROM machines WHERE type = 'collecteur' AND statut = 'approuvee' ORDER BY nom`)
	if err != nil {
		return nil, err
	}
	var out []CertificatCollecteur
	for rows.Next() {
		var c CertificatCollecteur
		if err := rows.Scan(&c.ID, &c.Nom); err != nil {
			_ = rows.Close()
			return nil, err
		}
		out = append(out, c)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].Etat, err = LireEtat(ctx, db, out[i].ID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Demander : « Renouveler maintenant » (fait au prochain passage, sous une
// minute). Audit.
func Demander(ctx context.Context, db *sql.DB, machine int64, qui string, maintenant time.Time) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // sans effet après Commit
	res, err := tx.ExecContext(ctx, `UPDATE certificats_syslog SET demande_le = ? WHERE machine_id = ? AND csr != ''`, maintenant.Unix(), machine)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("le collecteur n'a pas encore envoyé de demande de certificat (syslog réglé ? collecteur à jour ?)")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO audit(le, qui, action, objet, avant, apres, adresse) VALUES (?, ?, 'certificat_syslog_demande', ?, '', '', '')`,
		maintenant.Unix(), qui, fmt.Sprint("machine:", machine)); err != nil {
		return err
	}
	return tx.Commit()
}

// --- Réglages (fournisseur DNS, ACME) -----------------------------------------------------

// cleReglages : réglage JSON (les identifiants DNS y sont chiffrés).
const cleReglages = "certsyslog.reglages"

// UsageJeton, UsageCompte : usages du chiffrement (internal/server/secrets).
const (
	UsageJeton        = "certsyslog.desec" // ancien jeton deSEC (avant le choix du fournisseur)
	UsageIdentifiants = "certsyslog.dns"
	UsageCompte       = "certsyslog.compte"
)

// AnnuaireLetsEncrypt : annuaire ACME par défaut.
const AnnuaireLetsEncrypt = "https://acme-v02.api.letsencrypt.org/directory"

// Reglages : démarche ACME.
type Reglages struct {
	// Fournisseur : fournisseur DNS du défi (vide = deSEC).
	Fournisseur string `json:"fournisseur,omitempty"`
	// Domaine : zone DNS gérée chez le fournisseur (ex. exemple.org) ; le
	// nom du certificat doit en faire partie.
	Domaine string `json:"domaine"`
	// Annuaire ACME (défaut : Let's Encrypt).
	Annuaire string `json:"annuaire,omitempty"`
	// Parametres : champs non secrets du fournisseur (point d'accès OVH…).
	Parametres map[string]string `json:"parametres,omitempty"`
	// IdentifiantsChiffre : champs secrets du fournisseur (JSON chiffré,
	// jamais réaffiché).
	IdentifiantsChiffre string `json:"identifiants_chiffre,omitempty"`
	// JetonChiffre : ancien jeton deSEC chiffré (réglages d'avant le choix
	// du fournisseur ; repris tel quel).
	JetonChiffre string `json:"jeton_chiffre,omitempty"`
	// CompteChiffre : clé du compte ACME (PEM chiffré), créée au premier usage.
	CompteChiffre string `json:"compte_chiffre,omitempty"`
	// AttenteMax : maximum d'attente de la publication DNS, en secondes
	// (0 = selon le fournisseur), SPEC § 32.
	AttenteMax int `json:"attente_max,omitempty"`
}

// AnnuaireEffectif : annuaire ACME (Let's Encrypt si vide).
func (r Reglages) AnnuaireEffectif() string {
	if r.Annuaire == "" {
		return AnnuaireLetsEncrypt
	}
	return r.Annuaire
}

// FournisseurEffectif : fournisseur DNS (deSEC si vide).
func (r Reglages) FournisseurEffectif() string {
	if r.Fournisseur == "" {
		return "desec"
	}
	return r.Fournisseur
}

// IdentifiantsEnregistres : des identifiants sont déjà rangés.
func (r Reglages) IdentifiantsEnregistres() bool {
	return r.IdentifiantsChiffre != "" || r.JetonChiffre != ""
}

// identifiants : champs du fournisseur, secrets déchiffrés.
func (r Reglages) identifiants(cle []byte) (map[string]string, error) {
	ids := map[string]string{}
	for k, v := range r.Parametres {
		ids[k] = v
	}
	switch {
	case r.IdentifiantsChiffre != "":
		clair, err := secrets.Dechiffrer(cle, UsageIdentifiants, r.IdentifiantsChiffre)
		if err != nil {
			return nil, fmt.Errorf("identifiants DNS illisibles : %w", err)
		}
		var s map[string]string
		if err := json.Unmarshal([]byte(clair), &s); err != nil {
			return nil, fmt.Errorf("identifiants DNS illisibles : %w", err)
		}
		for k, v := range s {
			ids[k] = v
		}
	case r.JetonChiffre != "":
		jeton, err := secrets.Dechiffrer(cle, UsageJeton, r.JetonChiffre)
		if err != nil {
			return nil, fmt.Errorf("jeton deSEC illisible : %w", err)
		}
		ids["jeton"] = jeton
	default:
		return nil, errors.New("identifiants du fournisseur DNS pas encore saisis (page du collecteur › Certificat)")
	}
	return ids, nil
}

// LireReglages : réglages enregistrés (zéro si rien).
func LireReglages(ctx context.Context, db *sql.DB) (Reglages, error) {
	var v string
	var r Reglages
	err := db.QueryRowContext(ctx, `SELECT valeur FROM reglages WHERE cle = ?`, cleReglages).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return r, nil
	}
	if err != nil {
		return r, err
	}
	return r, json.Unmarshal([]byte(v), &r)
}

// ecrireReglages : enregistre (audit sans aucun secret).
func ecrireReglages(ctx context.Context, db *sql.DB, r Reglages, qui string, maintenant time.Time) error {
	brut, err := json.Marshal(r)
	if err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // sans effet après Commit
	if _, err := tx.ExecContext(ctx, `INSERT INTO reglages(cle, valeur, modifie_le, modifie_par) VALUES (?, ?, ?, ?)
		ON CONFLICT(cle) DO UPDATE SET valeur = excluded.valeur, modifie_le = excluded.modifie_le, modifie_par = excluded.modifie_par`,
		cleReglages, string(brut), maintenant.Unix(), qui); err != nil {
		return err
	}
	if qui != "" {
		if _, err := tx.ExecContext(ctx, `INSERT INTO audit(le, qui, action, objet, avant, apres, adresse) VALUES (?, ?, 'certsyslog_reglages', ?, '', ?, '')`,
			maintenant.Unix(), qui, r.Domaine, "fournisseur "+r.FournisseurEffectif()+", annuaire "+r.AnnuaireEffectif()+", identifiants ***"); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// nomDansDomaine : le nom du certificat appartient à la zone DNS ; renvoie
// le sous-nom de l'enregistrement du défi (« _acme-challenge.syslog.lan »).
func nomDansDomaine(nom, domaine string) (string, error) {
	nom, domaine = strings.ToLower(nom), strings.ToLower(strings.TrimSuffix(domaine, "."))
	switch {
	case domaine == "":
		return "", errors.New("zone DNS pas réglée")
	case nom == domaine:
		return "_acme-challenge", nil
	case strings.HasSuffix(nom, "."+domaine):
		return "_acme-challenge." + strings.TrimSuffix(nom, "."+domaine), nil
	}
	return "", fmt.Errorf("le nom %s n'appartient pas à la zone %s", nom, domaine)
}
