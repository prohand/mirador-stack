// SPDX-License-Identifier: GPL-3.0-or-later

package checks

import (
	"encoding/json"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"time"

	"mirador-stack/internal/agent/config"
)

// Garde du « contient » des checks http (revue de sécurité du 28/11, M4).
//
// Un check http avec « contient » répond oui ou non : le texte est-il dans
// la page ? Un serveur compromis pourrait s'en servir comme d'un oracle et
// lire une page du LAN morceau par morceau en changeant le texte attendu à
// chaque configuration. Deux garde-fous, décidés sur la machine :
//   - « MIRADOR_CHECKS_CONTIENT=non » dans le fichier local (root) : ces
//     checks ne sont jamais faits ;
//   - sinon, un « contient » nouveau ou changé n'est pris en compte qu'une
//     heure au moins après le précédent changement accepté (l'ancien texte
//     reste en service en attendant ; un check nouveau attend). L'état est
//     gardé sur disque : l'agent redémarre à chaque configuration reçue.
//
// Coût : un petit fichier JSON (50 entrées au plus), relu au démarrage,
// réécrit seulement quand un changement est accepté.

// DelaiContient : délai minimal entre deux changements de « contient ».
const DelaiContient = time.Hour

// Options : réglages locaux du moteur.
type Options struct {
	// SansContient : MIRADOR_CHECKS_CONTIENT=non (checks avec « contient »
	// jamais faits).
	SansContient bool
	// FichierContient : état du délai entre deux changements ("" : pas de
	// délai, configuration locale seule, sans serveur).
	FichierContient string
	Journal         *slog.Logger
}

// FichierContient : fichier d'état, rangé à côté de la configuration
// distante gardée ("" sans elle).
func FichierContient(fichierConfigDistante string) string {
	if fichierConfigDistante == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(fichierConfigDistante), "checks-contient.json")
}

// etatContient : textes attendus acceptés et date du dernier changement.
type etatContient struct {
	Le      int64             `json:"le"`      // dernier changement accepté (secondes Unix)
	Valeurs map[string]string `json:"valeurs"` // check → texte attendu accepté
}

// cleContient : un check (changer de cible compte comme un changement).
func cleContient(c config.Check) string {
	return c.Type + "|" + c.Nom + "|" + c.Modele + "|" + c.Cible
}

// filtrerContient : checks à faire. eff a la longueur de recus ; actifs[i]
// faux : check mis de côté. revoir : heure où un changement en attente
// pourra être accepté (zéro : rien en attente).
func filtrerContient(recus []config.Check, etat etatContient, maintenant time.Time) (eff []config.Check, actifs []bool, nouvel etatContient, revoir time.Time) {
	eff = append([]config.Check(nil), recus...)
	actifs = make([]bool, len(recus))
	courants := map[string]string{}
	change := false
	for i, c := range recus {
		actifs[i] = true
		if c.Contient == "" {
			continue
		}
		courants[cleContient(c)] = c.Contient
		change = change || etat.Valeurs[cleContient(c)] != c.Contient
	}
	// Horloge revenue en arrière (date future) : le délai est compté comme passé.
	ecart := maintenant.Sub(time.Unix(etat.Le, 0))
	if !change || ecart >= DelaiContient || ecart < 0 {
		nouvel = etatContient{Le: etat.Le, Valeurs: courants}
		if change {
			nouvel.Le = maintenant.Unix()
		}
		return eff, actifs, nouvel, time.Time{}
	}
	// Changement trop tôt : textes déjà acceptés gardés, nouveaux mis de côté.
	nouvel = etatContient{Le: etat.Le, Valeurs: map[string]string{}}
	for i, c := range recus {
		if c.Contient == "" {
			continue
		}
		ancien, connu := etat.Valeurs[cleContient(c)]
		switch {
		case connu && ancien == c.Contient:
		case connu:
			eff[i].Contient = ancien // l'ancien texte reste en service
		default:
			actifs[i] = false // check nouveau : il attend
			continue
		}
		nouvel.Valeurs[cleContient(c)] = eff[i].Contient
	}
	return eff, actifs, nouvel, time.Unix(etat.Le, 0).Add(DelaiContient)
}

// lireEtatContient : état gardé (vide s'il manque ou s'il est illisible).
func lireEtatContient(chemin string) etatContient {
	var e etatContient
	if b, err := os.ReadFile(chemin); err == nil { // #nosec G304 -- chemin fixé par l'agent (dossier de la configuration distante)
		_ = json.Unmarshal(b, &e)
	}
	return e
}

// ecrireEtatContient : écriture atomique (0600).
func ecrireEtatContient(chemin string, e etatContient) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(chemin), ".checks-contient-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), chemin)
}

// appliquerContient : liste effective du moteur (au démarrage, puis à
// l'heure où un changement en attente devient permis).
func (m *Moteur) appliquerContient(maintenant time.Time) {
	m.eff = append([]config.Check(nil), m.checks...)
	m.actifs = make([]bool, len(m.checks))
	m.revoir = time.Time{}
	for i := range m.actifs {
		m.actifs[i] = true
	}
	if m.opt.SansContient {
		n := 0
		for i, c := range m.checks {
			if c.Contient != "" {
				m.actifs[i] = false
				n++
			}
		}
		if n > 0 && m.opt.Journal != nil {
			m.opt.Journal.Warn("checks avec « contient » ignorés (MIRADOR_CHECKS_CONTIENT=non)", "nombre", n)
		}
		return
	}
	if m.opt.FichierContient == "" {
		return
	}
	etat := lireEtatContient(m.opt.FichierContient)
	var nouvel etatContient
	m.eff, m.actifs, nouvel, m.revoir = filtrerContient(m.checks, etat, maintenant)
	if nouvel.Le != etat.Le || !maps.Equal(nouvel.Valeurs, etat.Valeurs) {
		if err := ecrireEtatContient(m.opt.FichierContient, nouvel); err != nil && m.opt.Journal != nil {
			m.opt.Journal.Warn("checks : état du « contient » non enregistré", "erreur", err)
		}
	}
	if !m.revoir.IsZero() && m.opt.Journal != nil {
		m.opt.Journal.Warn("checks : « contient » nouveau ou changé mis de côté (un changement par heure au plus)",
			"jusqu_a", m.revoir.Format(time.RFC3339))
	}
}
