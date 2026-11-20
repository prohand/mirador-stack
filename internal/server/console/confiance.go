// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"net/http"
	"time"

	"mirador-stack/internal/server/auth"
)

// Appareils de confiance (SPEC § 78) : « Rester connecté sur cet appareil ».
// La session est aussi gardée sur disque (empreinte seulement) et le cookie
// est daté : la PWA ne redemande plus la connexion à chaque ouverture, ni
// après une mise à jour du serveur.

// confianceVueDisque : intervalle d'écriture du dernier usage sur disque.
const confianceVueDisque = 10 * time.Minute

// confiancePermise : « Rester connecté » demandé et permis par les réglages.
func (c *Console) confiancePermise(r *http.Request) bool {
	return r.PostFormValue("confiance") == "1" && !c.securite.Load().SansConfiance
}

// noterConfiance : connexion complète d'un appareil de confiance ; garde
// l'empreinte sur disque et pose un cookie daté. Renvoie false si la base
// refuse (le cookie reste alors un cookie de session ordinaire).
func (c *Console) noterConfiance(w http.ResponseWriter, r *http.Request, id string, s *auth.Session, ip string, t time.Time) bool {
	_, max := c.securite.Load().DureesConfiance()
	a := auth.Confiance{Empreinte: auth.EmpreinteDe(id), UtilisateurID: s.UtilisateurID, Generation: s.Generation,
		CreeLe: t, VuLe: t, Adresse: ip, Appareil: auth.AppareilDe(r.UserAgent())}
	if err := auth.EnregistrerConfiance(r.Context(), c.db, a); err != nil {
		c.journal.Warn("appareil de confiance non enregistré", "erreur", err)
		return false
	}
	c.sessions.Modifier(a.Empreinte, func(x *auth.Session) { x.Confiance, x.VueDisque = true, t })
	auth.PoserCookieConfiance(w, id, max)
	return true
}

// restaurerConfiance : cookie inconnu en mémoire (PWA rouverte, serveur
// redémarré) : la session d'un appareil de confiance valide est remise en
// mémoire. nil sinon (l'appareil expiré ou refusé est oublié).
func (c *Console) restaurerConfiance(ctx context.Context, id, ip string, t time.Time) *auth.Session {
	sec := c.securite.Load()
	if sec.SansConfiance {
		return nil
	}
	a, ok := auth.LireConfiance(ctx, c.db, id)
	if !ok {
		return nil
	}
	inactivite, max := sec.DureesConfiance()
	u, err := c.compte(ctx, a.UtilisateurID, t)
	if auth.ConfianceExpiree(a, t, inactivite, max) || err != nil || u.Desactive || u.Generation != a.Generation || u.TOTP == "" {
		_ = auth.OublierConfiance(ctx, c.db, a.Empreinte)
		return nil
	}
	_ = auth.ToucherConfiance(ctx, c.db, a.Empreinte, t)
	return c.sessions.Restaurer(id, &auth.Session{UtilisateurID: u.ID, Nom: u.Nom, Generation: u.Generation, Creee: a.CreeLe,
		Adresse: ip, VueDisque: t}, t)
}

// toucherConfiance : dernier usage noté sur disque, toutes les 10 min au plus.
func (c *Console) toucherConfiance(ctx context.Context, s *auth.Session, t time.Time) {
	if !s.Confiance || t.Sub(s.VueDisque) < confianceVueDisque {
		return
	}
	if auth.ToucherConfiance(ctx, c.db, s.Empreinte, t) == nil {
		c.sessions.Modifier(s.Empreinte, func(x *auth.Session) { x.VueDisque = t })
	}
}

// fermerSessionsCompte : toutes les sessions d'un compte, en mémoire et sur
// disque, sauf celle de garder (mot de passe changé, 2FA refaite, compte
// modifié par un admin).
func (c *Console) fermerSessionsCompte(ctx context.Context, utilisateur int64, garder string) {
	c.sessions.FermerUtilisateur(utilisateur, garder)
	if err := auth.OublierConfianceUtilisateur(ctx, c.db, utilisateur, garder); err != nil {
		c.journal.Warn("appareils de confiance non oubliés", "erreur", err)
	}
}

// expiree : durées des appareils de confiance pour leurs sessions.
func (c *Console) expiree(s *auth.Session, t time.Time) bool {
	sec := c.securite.Load()
	if s.Confiance {
		in, max := sec.DureesConfiance()
		return auth.Expiree(s, t, in, max)
	}
	return auth.Expiree(s, t, time.Duration(sec.Inactivite), time.Duration(sec.DureeMax))
}
