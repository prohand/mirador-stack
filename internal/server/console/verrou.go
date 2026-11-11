// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"database/sql"
	"net/netip"
	"strconv"
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
//   - essais en cours : un essai est « réservé » avant la vérification du
//     mot de passe ou du code (debuter). Sans cela, des requêtes envoyées
//     toutes en même temps passaient le contrôle avant que le premier échec
//     soit compté (40 mots de passe essayés au lieu de 5, mesuré le 11/11).
// Rien n'est écrit sur disque : chaque échec est écrit dans le journal du
// serveur, qu'un outil comme CrowdSec ou fail2ban peut lire pour bannir l'adresse. La commande
// « utilisateur-secours » lève tous les verrous (Tour).

const (
	fenetreEchecs  = 15 * time.Minute
	entreesMax     = 10000
	confianceMax   = 5000
	dureeConfiance = 30 * 24 * time.Hour
)

type compteur struct {
	echecs    int
	premier   time.Time
	bloqueAvt time.Time
}

type table map[string]*compteur

type verrous struct {
	mu        sync.Mutex
	adresses  table
	comptes   table
	confiance map[string]time.Time // « compte|adresse » → dernière connexion réussie
	enCours   map[string]int       // essais en cours de vérification (« a|adresse », « c|compte »)
}

func nouveauxVerrous() *verrous {
	return &verrous{adresses: table{}, comptes: table{}, confiance: map[string]time.Time{}, enCours: map[string]int{}}
}

// debuter réserve un essai (mot de passe ou code) pour cette adresse et ce
// compte, AVANT la vérification. Refusé (faux) si les échecs récents plus
// les essais déjà en cours atteignent le seuil : une rafale de requêtes
// simultanées ne permet donc pas plus d'essais que des requêtes à la suite.
// Si l'essai est accepté, appeler finir quand la vérification est terminée
// (et comptée). La table des essais en cours ne contient que les requêtes
// en cours de traitement : elle se vide toute seule.
func (v *verrous) debuter(nom, ip string, seuil int, t time.Time) (finir func(), ok bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	adr := cleAdresse(ip)
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

// cleAdresse : l'adresse (IPv4) ou son /64 (IPv6).
func cleAdresse(ip string) string {
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
func (v *verrous) bloqueAdresse(ip string, t time.Time) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.adresses.bloque(cleAdresse(ip), t)
}

// bloqueCompte : le compte est-il verrouillé pour cette adresse ? (jamais
// pour une adresse depuis laquelle il s'est déjà connecté)
func (v *verrous) bloqueCompte(nom, ip string, t time.Time) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	if le, ok := v.confiance[nom+"|"+cleAdresse(ip)]; ok && t.Sub(le) < dureeConfiance {
		return false
	}
	return v.comptes.bloque(nom, t)
}

// echecAdresse, echecCompte : un échec ; vrai si la clé vient d'être verrouillée.
func (v *verrous) echecAdresse(ip string, seuil int, duree time.Duration, t time.Time) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.adresses.echec(cleAdresse(ip), seuil, duree, t)
}

func (v *verrous) echecCompte(nom string, seuil int, duree time.Duration, t time.Time) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.comptes.echec(nom, seuil, duree, t)
}

// reussite : connexion complète réussie. Le compteur du COMPTE est effacé et
// l'adresse devient « de confiance » pour ce compte. Celui de l'ADRESSE est
// gardé : se connecter à son propre compte ne remet pas à zéro les essais
// faits depuis la même adresse sur un autre.
func (v *verrous) reussite(nom, ip string, t time.Time) {
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
	v.confiance[nom+"|"+cleAdresse(ip)] = t
}

// lever : efface tous les compteurs et verrous (commande de secours).
func (v *verrous) lever() {
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

func (tb table) echec(cle string, seuil int, duree time.Duration, t time.Time) bool {
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
		c.bloqueAvt = t.Add(duree)
		c.echecs, c.premier = 0, t
		return true
	}
	return false
}

// faireDeLaPlace : compteurs périmés d'abord, puis le plus ancien compteur
// non verrouillé ; en dernier recours (table pleine de verrous actifs), le
// verrou qui finit le plus tôt.
func (tb table) faireDeLaPlace(t time.Time) {
	for k, c := range tb {
		if t.Sub(c.premier) > fenetreEchecs && !t.Before(c.bloqueAvt) {
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

// cleVerrousLeves : réglage écrit par la commande « utilisateur-secours »
// (autre processus) ; le serveur en marche lève ses verrous en le voyant.
const cleVerrousLeves = "console.verrous_leves"

// DemanderLeveeVerrous : à appeler par une commande de secours.
func DemanderLeveeVerrous(ctx context.Context, db *sql.DB, qui string, t time.Time) error {
	return ecrireReglage(ctx, db, cleVerrousLeves, strconv.FormatInt(t.UnixNano(), 10), qui, t)
}

// leverSiDemande : vu à chaque tour (5 s).
func (c *Console) leverSiDemande(ctx context.Context) {
	var v string
	if err := c.db.QueryRowContext(ctx, `SELECT valeur FROM reglages WHERE cle = ?`, cleVerrousLeves).Scan(&v); err != nil {
		return
	}
	n, _ := strconv.ParseInt(v, 10, 64)
	if n > c.verrousLevesLe.Load() {
		c.verrousLevesLe.Store(n)
		c.verrous.lever()
		c.journal.Warn("console : verrous de connexion levés (commande de secours)")
	}
}
