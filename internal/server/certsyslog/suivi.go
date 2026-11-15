// SPDX-License-Identifier: GPL-3.0-or-later

package certsyslog

import (
	"context"
	"time"
)

// Avancement de la démarche Let's Encrypt (SPEC § 32) : chaque étape est
// notée en base, l'interface la montre à l'administrateur.

// Étapes de la démarche, dans l'ordre.
const (
	EtapeCompte      = "compte"      // connexion au compte Let's Encrypt
	EtapeCommande    = "commande"    // commande du certificat
	EtapeDNS         = "dns"         // enregistrement TXT posé chez le fournisseur DNS
	EtapePropagation = "propagation" // attente de la publication du TXT
	EtapeValidation  = "validation"  // Let's Encrypt vérifie le TXT
	EtapeEmission    = "emission"    // Let's Encrypt signe le certificat
)

// EtapesACME : ordre et libellés des étapes (affichage).
var EtapesACME = []struct{ Code, Titre string }{
	{EtapeCompte, "Connexion au compte Let's Encrypt"},
	{EtapeCommande, "Commande du certificat"},
	{EtapeDNS, "Enregistrement DNS du défi posé"},
	{EtapePropagation, "Attente de la publication DNS"},
	{EtapeValidation, "Vérification par Let's Encrypt"},
	{EtapeEmission, "Signature du certificat"},
}

// Suivi : reçoit chaque étape (jusqua : fin prévue d'une attente, sinon zéro).
type Suivi func(etape string, jusqua time.Time)

type cleSuivi struct{}

// AvecSuivi : contexte qui transmet les étapes de la démarche à f.
func AvecSuivi(ctx context.Context, f Suivi) context.Context {
	return context.WithValue(ctx, cleSuivi{}, f)
}

// suivre : note une étape (rien sans suivi dans le contexte).
func suivre(ctx context.Context, etape string, jusqua time.Time) {
	if f, ok := ctx.Value(cleSuivi{}).(Suivi); ok && f != nil {
		f(etape, jusqua)
	}
}
