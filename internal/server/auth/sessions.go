// SPDX-License-Identifier: GPL-3.0-or-later

package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"sync"
	"time"
)

// Sessions gardées en mémoire, côté serveur (SPEC § 18.2) :
//   - le navigateur n'a qu'un identifiant aléatoire de 256 bits ; le serveur
//     ne garde que son empreinte SHA-256 (un vidage mémoire ne donne pas de
//     cookie utilisable) ;
//   - rien n'est écrit sur le disque à chaque requête ;
//   - un redémarrage du serveur ferme toutes les sessions (il faut se
//     reconnecter avec la 2FA : acceptable, et même plus sûr) ;
//   - nouvel identifiant à chaque changement de droits (mot de passe accepté,
//     2FA acceptée) : une session « volée » avant la connexion ne sert à rien.

// Étapes de la connexion.
const (
	EtapeCode    = "code"     // mot de passe accepté, code 2FA attendu
	EtapeCreer   = "creer"    // mot de passe accepté, 2FA à créer
	EtapeComplet = "complete" // connecté
)

// NomCookie : préfixe __Host- = cookie Secure, sans domaine, chemin / ;
// un sous-domaine ne peut pas le poser ni l'écraser.
const NomCookie = "__Host-mirador"

// Bornes.
const (
	sessionsMax      = 1000
	partiellesMax    = 3               // sessions « avant le code » par compte
	DureeAvantCode   = 5 * time.Minute // entre le mot de passe et le code
	EssaisCodeMax    = 5               // codes faux avant de tout reprendre
	tailleIdentifant = 32
)

// Session : état d'une connexion.
type Session struct {
	Empreinte     string // empreinte SHA-256 de l\'identifiant du cookie
	UtilisateurID int64
	Nom           string
	Generation    int64
	Etape         string
	CSRF          string
	SecretPropose string // 2FA en cours de création (jamais en base avant confirmation)
	EssaisCode    int
	// EssaisSensibles : codes 2FA ou mots de passe faux pendant une session
	// complète (actions sensibles, changement de mot de passe) ; 5 = fermée.
	EssaisSensibles int
	Creee, Vue      time.Time
	Adresse         string
	Flash, Alerte   string // message affiché une fois (succès / erreur)
}

// Ref : début de l'empreinte, pour désigner une session dans la page
// « Sécurité » (jamais l'identifiant du cookie).
func (s Session) Ref() string { return s.Empreinte[:16] }

type Magasin struct {
	mu   sync.Mutex
	tout map[string]*Session
}

func NouveauMagasin() *Magasin { return &Magasin{tout: map[string]*Session{}} }

func Aleatoire(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // le système n'a plus d'aléa : rien de sûr n'est possible
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func empreinteID(id string) string {
	e := sha256.Sum256([]byte(id))
	return hex.EncodeToString(e[:])
}

// creer ajoute une session et renvoie l'identifiant à mettre dans le cookie.
func (m *Magasin) Creer(s *Session, t time.Time) string {
	id := Aleatoire(tailleIdentifant)
	s.Empreinte = empreinteID(id)
	s.CSRF = Aleatoire(24)
	s.Creee, s.Vue = t, t
	m.mu.Lock()
	defer m.mu.Unlock()
	if s.Etape != EtapeComplet {
		m.limiterPartielles(s.UtilisateurID)
	}
	if len(m.tout) >= sessionsMax {
		m.evincer()
	}
	m.tout[s.Empreinte] = s
	return id
}

// limiterPartielles : au plus partiellesMax sessions « avant le code » par
// compte (appelé sous verrou, avant d'en ajouter une). La plus ancienne est
// fermée : quelqu'un qui connaît le mot de passe ne peut pas en ouvrir des
// centaines pour essayer des codes sur chacune.
func (m *Magasin) limiterPartielles(utilisateur int64) {
	for {
		var vieille *Session
		n := 0
		for _, s := range m.tout {
			if s.UtilisateurID != utilisateur || s.Etape == EtapeComplet {
				continue
			}
			n++
			if vieille == nil || s.Creee.Before(vieille.Creee) {
				vieille = s
			}
		}
		if n < partiellesMax {
			return
		}
		delete(m.tout, vieille.Empreinte)
	}
}

// evincer retire la session la moins récemment vue (appelé sous verrou).
func (m *Magasin) evincer() {
	var vieille *Session
	for _, s := range m.tout {
		if vieille == nil || s.Vue.Before(vieille.Vue) {
			vieille = s
		}
	}
	if vieille != nil {
		delete(m.tout, vieille.Empreinte)
	}
}

// lire renvoie une COPIE de la session (nil si inconnue).
func (m *Magasin) Lire(id string) *Session {
	if id == "" || len(id) > 64 {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.tout[empreinteID(id)]
	if s == nil {
		return nil
	}
	c := *s
	return &c
}

// modifier applique f à la session (sous verrou).
func (m *Magasin) Modifier(empreinte string, f func(*Session)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s := m.tout[empreinte]; s != nil {
		f(s)
	}
}

// renouveler remplace l'identifiant (changement de droits) ; renvoie le
// nouvel identifiant.
func (m *Magasin) Renouveler(empreinte string, t time.Time, f func(*Session)) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.tout[empreinte]
	if s == nil {
		return ""
	}
	delete(m.tout, empreinte)
	id := Aleatoire(tailleIdentifant)
	s.Empreinte = empreinteID(id)
	s.CSRF = Aleatoire(24)
	s.Vue = t
	f(s)
	m.tout[s.Empreinte] = s
	return id
}

func (m *Magasin) Supprimer(empreinte string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.tout, empreinte)
}

// fermerUtilisateur ferme toutes les sessions d'un compte (sauf garder).
func (m *Magasin) FermerUtilisateur(id int64, garder string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, s := range m.tout {
		if s.UtilisateurID == id && k != garder {
			delete(m.tout, k)
		}
	}
}

// nettoyer retire les sessions expirées.
func (m *Magasin) Nettoyer(t time.Time, inactivite, max time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, s := range m.tout {
		if Expiree(s, t, inactivite, max) {
			delete(m.tout, k)
		}
	}
}

// Liste des sessions ouvertes (page « Sécurité »).
func (m *Magasin) Lister() []Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Session, 0, len(m.tout))
	for _, s := range m.tout {
		if s.Etape == EtapeComplet {
			out = append(out, *s)
		}
	}
	return out
}

func Expiree(s *Session, t time.Time, inactivite, max time.Duration) bool {
	if s.Etape != EtapeComplet {
		return t.Sub(s.Creee) > DureeAvantCode
	}
	return t.Sub(s.Vue) > inactivite || t.Sub(s.Creee) > max
}

// PoserCookie : HttpOnly, Secure, SameSite=Strict (brief). Pas de date
// d'expiration : le cookie disparaît à la fermeture du navigateur, et le
// serveur applique de toute façon ses propres durées.
func PoserCookie(w http.ResponseWriter, id string) {
	http.SetCookie(w, &http.Cookie{Name: NomCookie, Value: id, Path: "/", HttpOnly: true, Secure: true,
		SameSite: http.SameSiteStrictMode})
}

func EffacerCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: NomCookie, Value: "", Path: "/", HttpOnly: true, Secure: true,
		SameSite: http.SameSiteStrictMode, MaxAge: -1})
}
