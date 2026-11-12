// SPDX-License-Identifier: GPL-3.0-or-later

package auth

import (
	"net/netip"
	"sync"
	"time"
)

// Limitation des tentatives de connexion (brief ; revue de sécurité du
// 11/11, SPEC § 18.22) :
//   - par adresse : après Echecs échecs en 15 min, plus aucun essai pendant
//     la durée de verrouillage. Une adresse IPv6 compte pour son /64 (un
//     attaquant qui dispose d'un /64 ne peut pas changer d'adresse à chaque
//     essai) ;
//   - par compte : même règle avec un seuil 3 fois plus haut (essais venus
//     de beaucoup d'adresses). Pour qu'un inconnu ne puisse pas bloquer
//     l'administrateur, le verrou d'un compte ne s'applique pas aux adresses
//     depuis lesquelles ce compte s'est déjà connecté (30 jours) ;
//   - les tables sont bornées (10 000 entrées) ; quand l'une est pleine, on
//     oublie d'abord les compteurs périmés, puis les plus anciens compteurs
//     NON verrouillés : un verrou actif n'est jamais effacé par une
//     avalanche de noms ou d'adresses ;
//   - verrou progressif : chaque nouveau verrou de la même adresse ou du
//     même compte dure deux fois plus longtemps que le précédent (15 min,
//     30 min, 1 h… 24 h au plus). La série repart de zéro après 24 h sans
//     verrou, ou après une connexion réussie pour le compte ;
//   - essais en cours : un essai est « réservé » avant la vérification du
//     mot de passe ou du code (debuter). Sans cela, des requêtes envoyées
//     toutes en même temps passaient le contrôle avant que le premier échec
//     soit compté (40 mots de passe essayés au lieu de 5, mesuré le 11/11).
// Rien n'est écrit sur disque : chaque échec est écrit dans le journal du
// serveur, qu'un outil comme CrowdSec ou fail2ban peut lire pour bannir l'adresse. La commande
// « utilisateur-secours » lève tous les verrous (Tour).

const (
	fenetreEchecs  = 15 * time.Minute
	verrouMax      = 24 * time.Hour // plafond du verrou progressif
	oubliSerie     = 24 * time.Hour // sans verrou pendant ce temps : la série repart de zéro
	entreesMax     = 10000
	confianceMax   = 5000
	dureeConfiance = 30 * 24 * time.Hour
)

type compteur struct {
	echecs    int
	premier   time.Time
	bloqueAvt time.Time
	serie     int // verrous de suite (verrou progressif)
}

type table map[string]*compteur

type Verrous struct {
	mu        sync.Mutex
	adresses  table
	comptes   table
	confiance map[string]time.Time // « compte|adresse » → dernière connexion réussie
	enCours   map[string]int       // essais en cours de vérification (« a|adresse », « c|compte »)
}

func NouveauxVerrous() *Verrous {
	return &Verrous{adresses: table{}, comptes: table{}, confiance: map[string]time.Time{}, enCours: map[string]int{}}
}

// debuter réserve un essai (mot de passe ou code) pour cette adresse et ce
// compte, AVANT la vérification. Refusé (faux) si les échecs récents plus
// les essais déjà en cours atteignent le seuil : une rafale de requêtes
// simultanées ne permet donc pas plus d'essais que des requêtes à la suite.
// Si l'essai est accepté, appeler finir quand la vérification est terminée
// (et comptée). La table des essais en cours ne contient que les requêtes
// en cours de traitement : elle se vide toute seule.
func (v *Verrous) Debuter(nom, ip string, seuil int, t time.Time) (finir func(), ok bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	adr := CleAdresse(ip)
	ka, kc := "a|"+adr, "c|"+nom
	if v.adresses.recents(adr, t)+v.enCours[ka] >= seuil {
		return nil, false
	}
	// Compte : seuil 3 fois plus haut, et jamais pour une adresse de
	// confiance (mêmes règles que bloqueCompte et echecCompte).
	le, connue := v.confiance[nom+"|"+adr]
	if !(connue && t.Sub(le) < dureeConfiance) && v.comptes.recents(nom, t)+v.enCours[kc] >= 3*seuil {
		return nil, false
	}
	v.enCours[ka]++
	v.enCours[kc]++
	return func() {
		v.mu.Lock()
		defer v.mu.Unlock()
		for _, k := range []string{ka, kc} {
			if v.enCours[k]--; v.enCours[k] <= 0 {
				delete(v.enCours, k)
			}
		}
	}, true
}

// CleAdresse : l'adresse (IPv4) ou son /64 (IPv6).
func CleAdresse(ip string) string {
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return ip
	}
	a = a.Unmap()
	if a.Is6() {
		if p, err := a.Prefix(64); err == nil {
			return p.String()
		}
	}
	return a.String()
}

// bloqueAdresse : l'adresse (ou son /64) est-elle verrouillée ?
func (v *Verrous) BloqueAdresse(ip string, t time.Time) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.adresses.bloque(CleAdresse(ip), t)
}

// bloqueCompte : le compte est-il verrouillé pour cette adresse ? (jamais
// pour une adresse depuis laquelle il s'est déjà connecté)
func (v *Verrous) BloqueCompte(nom, ip string, t time.Time) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	if le, ok := v.confiance[nom+"|"+CleAdresse(ip)]; ok && t.Sub(le) < dureeConfiance {
		return false
	}
	return v.comptes.bloque(nom, t)
}

// echecAdresse, echecCompte : un échec. Si la clé vient d'être verrouillée,
// renvoie la durée du verrou (0 sinon).
func (v *Verrous) EchecAdresse(ip string, seuil int, duree time.Duration, t time.Time) time.Duration {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.adresses.echec(CleAdresse(ip), seuil, duree, t)
}

func (v *Verrous) EchecCompte(nom string, seuil int, duree time.Duration, t time.Time) time.Duration {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.comptes.echec(nom, seuil, duree, t)
}

// adresseConnue : ce compte s'est-il déjà connecté depuis cette adresse
// (30 jours, depuis le démarrage du serveur) ?
func (v *Verrous) AdresseConnue(nom, ip string, t time.Time) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	le, ok := v.confiance[nom+"|"+CleAdresse(ip)]
	return ok && t.Sub(le) < dureeConfiance
}

// reussite : connexion complète réussie. Le compteur du COMPTE est effacé et
// l'adresse devient « de confiance » pour ce compte. Celui de l'ADRESSE est
// gardé : se connecter à son propre compte ne remet pas à zéro les essais
// faits depuis la même adresse sur un autre.
func (v *Verrous) Reussite(nom, ip string, t time.Time) {
	v.mu.Lock()
	defer v.mu.Unlock()
	delete(v.comptes, nom)
	if len(v.confiance) >= confianceMax {
		for k, le := range v.confiance {
			if t.Sub(le) >= dureeConfiance {
				delete(v.confiance, k)
			}
		}
		if len(v.confiance) >= confianceMax {
			clear(v.confiance) // au pire : le verrou de compte s'applique à tous
		}
	}
	v.confiance[nom+"|"+CleAdresse(ip)] = t
}

// Lever : efface tous les compteurs et verrous (commande de secours).
func (v *Verrous) Lever() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.adresses, v.comptes = table{}, table{}
}

func (tb table) bloque(cle string, t time.Time) bool {
	c := tb[cle]
	return c != nil && t.Before(c.bloqueAvt)
}

// recents : échecs comptés dans la fenêtre en cours.
func (tb table) recents(cle string, t time.Time) int {
	c := tb[cle]
	if c == nil || t.Sub(c.premier) > fenetreEchecs {
		return 0
	}
	return c.echecs
}

func (tb table) echec(cle string, seuil int, duree time.Duration, t time.Time) time.Duration {
	c := tb[cle]
	if c == nil {
		if len(tb) >= entreesMax {
			tb.faireDeLaPlace(t)
		}
		c = &compteur{premier: t}
		tb[cle] = c
	}
	if t.Sub(c.premier) > fenetreEchecs {
		c.echecs, c.premier = 0, t
	}
	c.echecs++
	if c.echecs >= seuil {
		// Verrou progressif : durée doublée à chaque verrou de la série.
		if t.Sub(c.bloqueAvt) > oubliSerie {
			c.serie = 0
		}
		for i := 0; i < c.serie && duree < verrouMax; i++ {
			duree *= 2
		}
		duree = min(duree, verrouMax)
		c.serie++
		c.bloqueAvt = t.Add(duree)
		c.echecs, c.premier = 0, t
		return duree
	}
	return 0
}

// faireDeLaPlace : compteurs périmés d'abord, puis le plus ancien compteur
// non verrouillé ; en dernier recours (table pleine de verrous actifs), le
// verrou qui finit le plus tôt.
func (tb table) faireDeLaPlace(t time.Time) {
	for k, c := range tb {
		// Périmé : plus d'échec récent, plus de verrou, et série oubliée.
		if t.Sub(c.premier) > fenetreEchecs && !t.Before(c.bloqueAvt) && (c.serie == 0 || t.Sub(c.bloqueAvt) > oubliSerie) {
			delete(tb, k)
		}
	}
	for len(tb) >= entreesMax {
		var vieux, verrou string
		var tVieux, tVerrou time.Time
		for k, c := range tb {
			if !t.Before(c.bloqueAvt) {
				if vieux == "" || c.premier.Before(tVieux) {
					vieux, tVieux = k, c.premier
				}
			} else if verrou == "" || c.bloqueAvt.Before(tVerrou) {
				verrou, tVerrou = k, c.bloqueAvt
			}
		}
		if vieux != "" {
			delete(tb, vieux)
		} else {
			delete(tb, verrou)
		}
	}
}
