// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"mirador-stack/internal/server/auth"
	"mirador-stack/internal/server/notif"
	"mirador-stack/internal/server/webpush"
)

// Notifications push sur un appareil (SPEC § 35) : chaque compte abonne ses
// propres appareils dans « Mon compte ». Les envois sont faits par le canal
// « application » (paquet notif).

func (c *Console) routesPush() {
	m := c.mux
	m.HandleFunc("POST /compte/push/abonner", c.lecture(c.postPushAbonner))
	m.HandleFunc("POST /compte/push/retirer", c.lecture(c.postPushRetirer))
	m.HandleFunc("POST /compte/push/tester", c.lecture(c.postPushTester))
}

// servirSW : le service worker des notifications. À la racine du site : un
// service worker ne peut agir que sous le dossier d'où il est servi. Public
// comme les autres scripts (rien de secret) ; jamais gardé en cache par le
// navigateur, pour qu'une mise à jour du serveur soit prise tout de suite.
func servirSW(w http.ResponseWriter, r *http.Request) {
	s := statiques["/statique/sw.js"]
	if s == nil || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/javascript; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("Service-Worker-Allowed", "/")
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(s.brut)
}

// nomAppareil : nom lisible tiré du navigateur (« iPhone », « Chrome sur
// Android »…), pour reconnaître ses appareils dans la liste. Jamais plus :
// le texte complet envoyé par le navigateur n'est pas gardé.
func nomAppareil(ua string) string {
	systeme := ""
	switch {
	case strings.Contains(ua, "iPhone"):
		return "iPhone"
	case strings.Contains(ua, "iPad"):
		return "iPad"
	case strings.Contains(ua, "Android"):
		systeme = "Android"
	case strings.Contains(ua, "Windows"):
		systeme = "Windows"
	case strings.Contains(ua, "Mac OS X"), strings.Contains(ua, "Macintosh"):
		systeme = "Mac"
	case strings.Contains(ua, "Linux"):
		systeme = "Linux"
	}
	navigateur := "Navigateur"
	switch {
	case strings.Contains(ua, "Firefox/"):
		navigateur = "Firefox"
	case strings.Contains(ua, "Edg/"), strings.Contains(ua, "EdgA/"):
		navigateur = "Edge"
	case strings.Contains(ua, "Chrome/"):
		navigateur = "Chrome"
	case strings.Contains(ua, "Safari/"):
		navigateur = "Safari"
	}
	if systeme == "" {
		return navigateur
	}
	return navigateur + " sur " + systeme
}

// postPushAbonner : l'abonnement que le navigateur vient de créer. Code 2FA
// ou mode administration : un nouvel appareil va recevoir le texte des
// alertes (une session volée ne suffit pas à s'abonner).
func (c *Console) postPushAbonner(q *req) {
	err := func() error {
		if err := c.verifierCode(q); err != nil {
			return err
		}
		if _, _, err := auth.DomaineEtOrigine(q.r.Host); err != nil {
			return errors.New("adresse de l'interface invalide")
		}
		a := webpush.Abonnement{Adresse: strings.TrimSpace(q.r.PostFormValue("adresse"))}
		var e1, e2 error
		a.P256dh, e1 = base64.RawURLEncoding.DecodeString(strings.TrimRight(q.r.PostFormValue("p256dh"), "="))
		a.Auth, e2 = base64.RawURLEncoding.DecodeString(strings.TrimRight(q.r.PostFormValue("auth"), "="))
		if e1 != nil || e2 != nil || len(q.r.PostFormValue("p256dh")) > 200 || len(q.r.PostFormValue("auth")) > 100 {
			return errors.New("abonnement illisible : recommencer")
		}
		appareil := nomAppareil(q.r.UserAgent())
		// Contact annoncé au service de push : l'adresse de cette interface.
		if err := notif.AbonnerPush(q.r.Context(), c.db, q.u.ID, a, "https://"+q.r.Host, appareil, q.t); err != nil {
			return err
		}
		return Auditer(q.r.Context(), c.db, q.qui(), "push_abonner", q.u.Nom, "", appareil, q.ip.String(), q.t)
	}()
	c.retour(q, "/compte#push", err, "cet appareil recevra les notifications : « Envoyer une notification d'essai » pour vérifier")
}

func (c *Console) postPushRetirer(q *req) {
	id, _ := strconv.ParseInt(q.r.PostFormValue("id"), 10, 64)
	nom, err := notif.RetirerAppareilPush(q.r.Context(), c.db, q.u.ID, id)
	if err == nil {
		err = Auditer(q.r.Context(), c.db, q.qui(), "push_retirer", q.u.Nom, nom, "", q.ip.String(), q.t)
	}
	c.retour(q, "/compte#push", err, "appareil retiré : il ne recevra plus de notification")
}

// postPushTester : notification d'essai aux appareils de CE compte (au plus
// une toutes les 10 s par session : pas d'avalanche sur les téléphones).
func (c *Console) postPushTester(q *req) {
	if q.t.Sub(q.s.EssaiPushLe) < delaiEssaiPush {
		c.retour(q, "/compte#push", errors.New("un essai vient d'être envoyé : attendre quelques secondes"), "")
		return
	}
	c.sessions.Modifier(q.s.Empreinte, func(s *auth.Session) { s.EssaiPushLe = q.t })
	n, err := notif.TesterPush(q.r.Context(), c.db, c.opt.Cle, q.u.ID, q.t)
	c.retour(q, "/compte#push", err, accorder(n, "notification d'essai envoyée à "+strconv.Itoa(n)+" appareil(s)"))
}
