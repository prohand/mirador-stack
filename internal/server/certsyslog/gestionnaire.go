// SPDX-License-Identifier: GPL-3.0-or-later

package certsyslog

import (
	"context"
	"crypto/x509"
	"database/sql"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"time"
)

// Gestionnaire : passe toutes les minutes sur les collecteurs et obtient
// (ou renouvelle) les certificats qui en ont besoin, un à la fois.
type Gestionnaire struct {
	DB       *sql.DB
	Emetteur Emetteur
	Journal  *slog.Logger
	// NomDe : nom du certificat réglé pour ce collecteur ('' : syslog pas
	// réglé) ; lu dans sa configuration (fourni par le serveur).
	NomDe      func(ctx context.Context, machine int64) (string, error)
	Maintenant func() time.Time
	// Apres : appelé après un nouveau certificat (configuration renvoyée).
	Apres func()
}

// Tourner : un passage par minute jusqu'à l'arrêt.
func (g *Gestionnaire) Tourner(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		g.Passer(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Passer : un passage sur tous les collecteurs qui ont envoyé une demande.
func (g *Gestionnaire) Passer(ctx context.Context) {
	maintenant := time.Now()
	if g.Maintenant != nil {
		maintenant = g.Maintenant()
	}
	rows, err := g.DB.QueryContext(ctx, `SELECT c.machine_id, c.csr, c.certificat, c.nom, c.fin, c.obtenu_le, c.essai_le, c.demande_le
		FROM certificats_syslog c JOIN machines m ON m.id = c.machine_id WHERE c.csr != '' AND m.type = 'collecteur' AND m.statut = 'approuvee'`)
	if err != nil {
		return
	}
	type ligne struct {
		id                          int64
		csr, cert, nom              string
		fin, obtenu, essai, demande int64
	}
	var lignes []ligne
	for rows.Next() {
		var l ligne
		if rows.Scan(&l.id, &l.csr, &l.cert, &l.nom, &l.fin, &l.obtenu, &l.essai, &l.demande) == nil {
			lignes = append(lignes, l)
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return
	}
	for _, l := range lignes {
		nom, err := g.NomDe(ctx, l.id)
		if err != nil || nom == "" {
			continue // syslog pas (ou plus) réglé
		}
		force := l.demande > l.obtenu && l.demande > l.essai
		if !force && !aRenouveler(l.csr, l.cert, l.nom, nom, l.fin, maintenant) {
			continue
		}
		if !force && l.essai > l.obtenu && maintenant.Sub(time.Unix(l.essai, 0)) < AttenteApresEchec {
			continue // échec récent : nouvel essai plus tard
		}
		g.obtenir(ctx, l.id, nom, l.csr, maintenant)
	}
}

// aRenouveler : pas de certificat, autre nom, autre clé, ou échéance dans
// moins de 30 jours.
func aRenouveler(csrPEM, certPEM, nomCert, nom string, fin int64, maintenant time.Time) bool {
	if certPEM == "" || nomCert != nom || time.Unix(fin, 0).Sub(maintenant) < RenouvelerAvant {
		return true
	}
	req, err1 := LireCSR(csrPEM)
	bloc, _ := pem.Decode([]byte(certPEM))
	if err1 != nil || bloc == nil {
		return true
	}
	cert, err := x509.ParseCertificate(bloc.Bytes)
	return err != nil || !reflect.DeepEqual(cert.PublicKey, req.PublicKey)
}

// obtenir : démarche ACME pour un collecteur ; résultat (ou erreur) en base.
func (g *Gestionnaire) obtenir(ctx context.Context, machine int64, nom, csrPEM string, maintenant time.Time) {
	enregistrerEchec := func(err error) {
		msg := err.Error()
		if len(msg) > 500 {
			msg = msg[:500]
		}
		_, _ = g.DB.ExecContext(ctx, `UPDATE certificats_syslog SET essai_le = ?, erreur = ? WHERE machine_id = ?`, maintenant.Unix(), msg, machine)
		if g.Journal != nil {
			g.Journal.Warn("certificat du syslog non obtenu", "collecteur", machine, "erreur", msg)
		}
	}
	req, err := LireCSR(csrPEM)
	if err != nil {
		enregistrerEchec(err)
		return
	}
	if req.DNSNames[0] != nom {
		enregistrerEchec(fmt.Errorf("la demande du collecteur est pour %s, pas pour %s (il en refait une à son redémarrage)", req.DNSNames[0], nom))
		return
	}
	ctxA, fin := context.WithTimeout(ctx, 10*time.Minute)
	defer fin()
	chaine, err := g.Emetteur.Obtenir(ctxA, nom, req.Raw)
	if err != nil {
		enregistrerEchec(err)
		return
	}
	finCert, err := verifierChaine(chaine, req, nom)
	if err != nil {
		enregistrerEchec(err)
		return
	}
	if _, err := g.DB.ExecContext(ctx, `UPDATE certificats_syslog SET nom = ?, certificat = ?, fin = ?, obtenu_le = ?, essai_le = ?, erreur = '' WHERE machine_id = ?`,
		nom, chaine, finCert.Unix(), maintenant.Unix(), maintenant.Unix(), machine); err != nil {
		enregistrerEchec(err)
		return
	}
	if g.Journal != nil {
		g.Journal.Info("certificat du syslog obtenu", "collecteur", machine, "nom", nom, "fin", finCert)
	}
	if g.Apres != nil {
		g.Apres()
	}
}

// verifierChaine : la chaîne reçue est bien pour cette clé et ce nom.
func verifierChaine(chaine string, req *x509.CertificateRequest, nom string) (time.Time, error) {
	bloc, _ := pem.Decode([]byte(chaine))
	if bloc == nil {
		return time.Time{}, errors.New("certificat reçu illisible")
	}
	cert, err := x509.ParseCertificate(bloc.Bytes)
	if err != nil {
		return time.Time{}, err
	}
	if !reflect.DeepEqual(cert.PublicKey, req.PublicKey) {
		return time.Time{}, errors.New("certificat reçu pour une autre clé")
	}
	if err := cert.VerifyHostname(nom); err != nil {
		return time.Time{}, err
	}
	return cert.NotAfter, nil
}
