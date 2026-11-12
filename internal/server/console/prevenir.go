// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"mirador-stack/internal/server/auth"
	"sync"
	"time"
)

// Messages de sécurité envoyés à l'administrateur par les canaux de
// notification (SPEC § 18.28) :
//   - code 2FA faux après un bon mot de passe (mot de passe sans doute volé) ;
//   - compte verrouillé ;
//   - connexion réussie depuis une nouvelle adresse.
//
// Règles :
//   - l'envoi se fait à part (goroutine) : la réponse de la page de
//     connexion n'attend pas le canal et sa durée ne dit rien ;
//   - un message au plus par heure pour le même sujet (un attaquant ne peut
//     pas noyer l'administrateur sous les messages) ;
//   - jamais de secret dans le texte (ni mot de passe, ni code).

const (
	delaiEntreMessages = time.Hour
	messagesSuivisMax  = 1000
	delaiEnvoiMessage  = 30 * time.Second
)

// antiRafale : dernier envoi par sujet.
type antiRafale struct {
	mu      sync.Mutex
	dernier map[string]time.Time
}

// permis : vrai si aucun message n'est parti pour ce sujet depuis une heure.
func (a *antiRafale) permis(sujet string, t time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if le, ok := a.dernier[sujet]; ok && t.Sub(le) < delaiEntreMessages {
		return false
	}
	if a.dernier == nil || len(a.dernier) >= messagesSuivisMax {
		a.dernier = map[string]time.Time{} // borne mémoire ; au pire un message en double
	}
	a.dernier[sujet] = t
	return true
}

// prevenirSecurite envoie le message (si un canal existe), sans bloquer.
func (c *Console) prevenirSecurite(sujet, texte string, t time.Time) {
	if c.prevenir == nil || !c.messages.permis(sujet, t) {
		return
	}
	go func() {
		ctx, annuler := context.WithTimeout(context.Background(), delaiEnvoiMessage)
		defer annuler()
		c.prevenir(ctx, "securite="+sujet, texte)
	}()
}

// prevenirVerrou : compte verrouillé. Seulement si le compte existe : un
// inconnu qui essaie des noms au hasard ne déclenche aucun message.
func (c *Console) prevenirVerrou(nom, ip string, duree time.Duration, t time.Time) {
	if c.prevenir == nil || auth.VerifierNom(nom) != nil {
		return
	}
	if _, err := auth.LireUtilisateur(context.Background(), c.db, nom); err != nil {
		return
	}
	c.prevenirSecurite("verrou|"+nom, "🔒 mirador : compte « "+nom+" » verrouillé pour "+duree.String()+" après trop d'échecs de connexion.\nDernière adresse : "+ip+
		"\nSi ce n'est pas vous : vérifier le journal et la page Sécurité.", t)
}
