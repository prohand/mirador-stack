// SPDX-License-Identifier: GPL-3.0-or-later

package notif

import (
	"context"
	"strings"
	"time"
	"unicode"

	"mirador-stack/internal/server/alertes"
)

// Extraits de logs dans les messages (SPEC.md § 41, idée reprise de Beszel
// le 17/11 : idée seulement, aucun code repris).
//
// Quand un service échoue, qu'un conteneur va mal ou que le noyau tue un
// processus faute de mémoire, le message contient les dernières lignes de
// logs de ce service ou de ce conteneur : la cause se lit sur le téléphone,
// sans ouvrir l'interface.
//
// Règles simples :
//   - seulement pour une alerte **nouvelle** d'un message d'alerte (pas les
//     rappels, ni les fins, ni le message d'un site injoignable) ;
//   - 3 alertes au plus par message, 5 lignes au plus par alerte, 160
//     caractères au plus par ligne ;
//   - la recherche est choisie d'après les étiquettes de l'alerte (voir
//     requeteExtrait) ; sans correspondance, rien n'est ajouté ;
//   - les logs sont déjà masqués par l'agent (mots de passe, jetons) avant
//     d'arriver au serveur ; le réglage « sans_extraits_logs » coupe tout.
//
// Coût : une recherche de logs par alerte nouvelle concernée (quelques
// blocs lus grâce à l'index par machine et par unité), bornée à 3 s par
// message. Rien en base.

// Bornes des extraits.
const (
	ExtraitLignes     = 5
	extraitLongueur   = 160
	extraitAlertesMax = 3
	extraitAvant      = 10 * time.Minute // logs lus depuis le début de la condition, moins ce délai
	extraitDureeMax   = 3 * time.Second  // pour toutes les recherches d'un message
	extraitPeriodeMax = 24 * time.Hour   // une alerte très ancienne : seulement les dernières 24 h
	suffixeService    = ".service"
)

// RequeteExtrait : quels logs chercher pour une alerte.
type RequeteExtrait struct {
	Champs     map[string]string // égalités exactes : host, unit, container
	Mots       string            // mots entiers tous présents (vide : tous)
	Debut, Fin time.Time
}

// ChercheurLogs : renvoie au plus n lignes, les plus récentes d'abord, déjà
// mises en forme (« 15:04:05 message »). Fourni par le serveur.
type ChercheurLogs func(ctx context.Context, r RequeteExtrait, n int) ([]string, error)

// requeteExtrait : la recherche qui correspond à l'alerte, ou faux.
//   - étiquette name en « .service » (règles des services systemd) : logs
//     de cette unité sur la machine ;
//   - étiquette name sans « .service » (règles des conteneurs Docker) : logs
//     de ce conteneur sur la machine ;
//   - OomKiller : lignes du noyau « Out of memory: Killed process … ».
func requeteExtrait(a *alertes.Alerte, t time.Time) (RequeteExtrait, bool) {
	hote := a.Machine()
	if hote == "" {
		return RequeteExtrait{}, false
	}
	debut := a.Debut.Add(-extraitAvant)
	if a.Debut.IsZero() || t.Sub(debut) > extraitPeriodeMax {
		debut = t.Add(-extraitPeriodeMax)
	}
	r := RequeteExtrait{Champs: map[string]string{"host": hote}, Debut: debut, Fin: t}
	nom := a.Etiquettes.Get("name")
	switch {
	case a.Regle == "OomKiller":
		r.Mots = "killed memory"
	case strings.HasSuffix(nom, suffixeService):
		r.Champs["unit"] = nom
	case nom != "":
		r.Champs["container"] = nom
	default:
		return RequeteExtrait{}, false
	}
	return r, true
}

// nettoyerLigne : une ligne courte, sans caractère de contrôle (pas de
// retour à la ligne qui casserait le message).
func nettoyerLigne(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, strings.TrimSpace(s))
	if r := []rune(s); len(r) > extraitLongueur {
		s = string(r[:extraitLongueur-1]) + "…"
	}
	return s
}

// ajouterExtraits (hors verrou) : remplit m.Extraits pour les alertes
// nouvelles du message. Une erreur de recherche n'empêche jamais l'envoi.
func (g *Aiguilleur) ajouterExtraits(ctx context.Context, m *Message, r *Reglages, t time.Time) {
	if g.opt.Logs == nil || r.SansExtraitsLogs || m.Type != TypeAlerte || m.Site != "" || m.Nouvelles == 0 {
		return
	}
	ctx, fin := context.WithTimeout(ctx, extraitDureeMax)
	defer fin()
	n := 0
	for i := 0; i < m.Nouvelles && i < len(m.Actives) && n < extraitAlertesMax; i++ {
		a := &m.Actives[i]
		req, ok := requeteExtrait(a, t)
		if !ok {
			continue
		}
		n++
		lignes, err := g.opt.Logs(ctx, req, ExtraitLignes)
		if err != nil {
			g.opt.Journal.Warn("notifications : extrait de logs", "alerte", a.Regle, "machine", a.Machine(), "erreur", err)
			continue
		}
		if len(lignes) == 0 {
			continue
		}
		propres := make([]string, 0, len(lignes))
		for k := len(lignes) - 1; k >= 0; k-- { // ordre chronologique dans le message
			if l := nettoyerLigne(lignes[k]); l != "" {
				propres = append(propres, l)
			}
		}
		if m.Extraits == nil {
			m.Extraits = map[uint64][]string{}
		}
		m.Extraits[a.Etiquettes.Hash()] = propres
	}
}

// ajouterChangements (hors verrou) : dernier changement de la machine avant
// chaque alerte nouvelle du message (SPEC § 47).
func (g *Aiguilleur) ajouterChangements(ctx context.Context, m *Message) {
	if g.opt.Changement == nil || m.Type != TypeAlerte || m.Site != "" || m.Nouvelles == 0 {
		return
	}
	for i := 0; i < m.Nouvelles && i < len(m.Actives) && i < extraitAlertesMax; i++ {
		a := &m.Actives[i]
		debut := a.Debut
		if debut.IsZero() {
			debut = a.ActiveLe
		}
		if a.Machine() == "" || debut.IsZero() {
			continue
		}
		quand, texte, ok := g.opt.Changement(ctx, a.Machine(), debut)
		if !ok {
			continue
		}
		if m.Changements == nil {
			m.Changements = map[uint64]string{}
		}
		ecart := debut.Sub(quand).Round(time.Minute)
		avant := "juste avant"
		if ecart >= time.Minute {
			avant = strings.TrimSuffix(dureeCourte(ecart), " 0s") + " avant"
		}
		m.Changements[a.Etiquettes.Hash()] = avant + " : " + texte
	}
}
