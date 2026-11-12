// SPDX-License-Identifier: GPL-3.0-or-later

package console

import "net/http"

// routes : toutes les adresses de l'interface. Lecture = tout compte
// connecté ; admin = rôle admin (et code 2FA pour les actions sensibles,
// vérifié dans chaque action).
func (c *Console) routes() {
	m := c.mux
	// Connexion (publiques).
	m.HandleFunc("GET /connexion", c.pageConnexion)
	m.HandleFunc("POST /connexion", c.postConnexion)
	m.HandleFunc("GET /connexion/code", c.pageCode)
	m.HandleFunc("POST /connexion/code", c.postCode)
	m.HandleFunc("POST /connexion/passkey", c.postPasskey)
	m.HandleFunc("GET /connexion/2fa-creer", c.pageCreer2FA)
	m.HandleFunc("POST /connexion/2fa-creer", c.postCreer2FA)
	m.HandleFunc("POST /deconnexion", c.postDeconnexion)
	m.HandleFunc("GET /premier-admin", c.pagePremier)
	m.HandleFunc("POST /premier-admin", c.postPremier)

	// Compte et sécurité.
	m.HandleFunc("GET /compte", c.lecture(c.pageCompte))
	m.HandleFunc("POST /compte/mot-de-passe", c.lecture(c.postMotDePasse))
	m.HandleFunc("POST /compte/2fa", c.lecture(c.post2FANouvelle))
	m.HandleFunc("POST /compte/2fa-confirmer", c.lecture(c.post2FAConfirmer))
	m.HandleFunc("POST /compte/2fa-abandonner", c.lecture(c.post2FAAbandonner))
	m.HandleFunc("POST /compte/codes-secours", c.lecture(c.postCodesSecours))
	m.HandleFunc("POST /compte/passkey/preparer", c.lecture(c.postPasskeyPreparer))
	m.HandleFunc("POST /compte/passkey/enregistrer", c.lecture(c.postPasskeyEnregistrer))
	m.HandleFunc("POST /compte/passkey/supprimer", c.lecture(c.postPasskeySupprimer))
	m.HandleFunc("POST /confirmer", c.lecture(c.postConfirmer))
	m.HandleFunc("GET /utilisateurs", c.admin(c.pageUtilisateurs))
	m.HandleFunc("POST /utilisateurs", c.admin(c.postUtilisateur))
	m.HandleFunc("GET /securite", c.admin(c.pageSecurite))
	m.HandleFunc("POST /securite", c.admin(c.postSecurite))
	m.HandleFunc("POST /securite/fermer-session", c.admin(c.postFermerSession))

	c.routesPages()
	c.routesTableaux()
	c.routesRequetes()

	// Tout le reste : page introuvable (après la session : on ne dit pas
	// à un inconnu quelles adresses existent).
	m.HandleFunc("/", c.lecture(func(q *req) {
		if q.r.URL.Path == "/" {
			c.pageAccueil(q)
			return
		}
		c.rendreCode(q, http.StatusNotFound, "introuvable", "Page introuvable", "", nil)
	}))
}
