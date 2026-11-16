// SPDX-License-Identifier: GPL-3.0-or-later

package notif

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"mirador-stack/internal/server/alertes"
)

// Site injoignable (SPEC.md § 39, demande du 16/11 après la coupure
// d'internet du site de l'utilisateur).
//
// Quand toutes les machines d'un site se taisent en même temps, c'est en
// général le lien du site (internet, box, courant) et pas les machines. Au
// lieu d'un message « machine muette » par machine, un seul message :
// « Site X injoignable », puis un seul au retour.
//
// Règles simples :
//   - machines d'un site : agents et collecteurs approuvés, avec un site, vus
//     dans les dernières 24 h (une machine éteinte depuis longtemps ne compte
//     pas) ; les équipements relevés par un collecteur ne comptent pas (ils
//     suivent déjà leur collecteur, par la dépendance) ;
//   - au moins 2 machines dans le site, sinon rien ne change ;
//   - le site est injoignable quand **chacune** de ses machines a une
//     alerte de panne active (réglage « règles de panne » : AgentMuet…) ;
//   - pour laisser aux autres machines le temps de se taire aussi, la panne
//     d'une machine d'un tel site n'est annoncée qu'après AttenteSite ;
//   - au retour, les machines reviennent à quelques secondes d'écart : le
//     message de retour attend AttenteRetourSite que toutes soient revenues.
//
// Coût : une requête SQL de plus à la relecture de la sourdine (chaque
// minute), et un parcours des alertes actives toutes les 5 s (quelques
// microsecondes pour quelques centaines d'alertes). Rien en base.

// AttenteSite : délai avant d'annoncer la panne d'une machine qui partage
// son site avec d'autres. AgentMuet est évaluée chaque minute et les agents
// envoient toutes les 10 s : 2 min suffisent pour que toutes les machines
// coupées en même temps aient leur alerte. Coût : une machine seule en panne
// est annoncée 2 min plus tard qu'avant.
const AttenteSite = 2 * time.Minute

// AttenteRetourSite : au retour du lien, délai laissé aux autres machines
// pour revenir avant d'envoyer le message de retour.
const AttenteRetourSite = 3 * time.Minute

// prefixeSite : clé des groupes de site (jamais produite par cleGroupe, qui
// écrit toujours « nom=valeur » avec un nom d'étiquette, jamais « site:… »).
const prefixeSite = "site:"

// SiteInjoignable : site dont toutes les machines sont en panne (affichage).
type SiteInjoignable struct {
	Site     string
	Machines []string  // machines du site (triées)
	Depuis   time.Time // première panne du site
}

// chargerSites lit les machines de chaque site (voir les règles plus haut).
// Seuls les sites d'au moins 2 machines sont gardés.
func (s *Sourdine) chargerSites(ctx context.Context, db *sql.DB, t time.Time) error {
	rows, err := db.QueryContext(ctx, `SELECT nom, site FROM machines
		WHERE statut = 'approuvee' AND site != '' AND type IN ('agent', 'collecteur') AND vu_le >= ?
		  AND id NOT IN (SELECT machine_id FROM equipements_collecteur)`, t.Add(-24*time.Hour).Unix())
	if err != nil {
		return err
	}
	defer rows.Close()
	parSite := map[string][]string{}
	for rows.Next() {
		var n, site string
		if err := rows.Scan(&n, &site); err != nil {
			return err
		}
		parSite[site] = append(parSite[site], n)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	s.sites, s.parSite = map[string]string{}, map[string][]string{}
	for site, ms := range parSite {
		if len(ms) < 2 {
			continue
		}
		sort.Strings(ms)
		s.parSite[site] = ms
		for _, m := range ms {
			s.sites[m] = site
		}
	}
	return nil
}

// siteDe : site (d'au moins 2 machines) de la machine, "" sinon.
func (s *Sourdine) siteDe(machine string) string {
	if s == nil {
		return ""
	}
	return s.sites[machine]
}

// estPanne : l'alerte veut-elle dire « machine en panne » ?
func (s *Sourdine) estPanne(a *alertes.Alerte) bool {
	return s != nil && s.reglesPanne[a.Regle]
}

// sitesInjoignables : sites dont chaque machine a une alerte de panne active.
func (s *Sourdine) sitesInjoignables(actives []alertes.Alerte) map[string]SiteInjoignable {
	if s == nil || len(s.parSite) == 0 {
		return nil
	}
	// Première panne de chaque machine d'un site surveillé.
	pannes := map[string]time.Time{}
	for i := range actives {
		a := &actives[i]
		m := a.Machine()
		if !s.estPanne(a) || s.sites[m] == "" {
			continue
		}
		d := debutActif(a)
		if p, ok := pannes[m]; !ok || d.Before(p) {
			pannes[m] = d
		}
	}
	var out map[string]SiteInjoignable
	for site, ms := range s.parSite {
		var depuis time.Time
		toutes := true
		for _, m := range ms {
			d, ok := pannes[m]
			if !ok {
				toutes = false
				break
			}
			if depuis.IsZero() || d.Before(depuis) {
				depuis = d
			}
		}
		if !toutes {
			continue
		}
		if out == nil {
			out = map[string]SiteInjoignable{}
		}
		out[site] = SiteInjoignable{Site: site, Machines: ms, Depuis: depuis}
	}
	return out
}

// debutActif : moment où l'alerte est devenue active (à défaut, son début).
func debutActif(a *alertes.Alerte) time.Time {
	if !a.ActiveLe.IsZero() {
		return a.ActiveLe
	}
	return a.Debut
}

// groupeSite : nom du site si c'est un groupe de site, "" sinon.
func groupeSite(cle string) string {
	site, _ := strings.CutPrefix(cle, prefixeSite)
	if site == cle {
		return ""
	}
	return site
}

// retenue (sous verrou) : la panne de cette machine attend-elle encore de
// savoir si tout son site tombe ? Seulement hors des groupes de site, pour
// une panne pas encore annoncée.
func (g *Aiguilleur) retenue(gr *groupe, s *suivi, t time.Time) bool {
	if s.notifiee || groupeSite(gr.cle) != "" || !g.sourdine.estPanne(&s.a) || g.sourdine.siteDe(s.a.Machine()) == "" {
		return false
	}
	return t.Before(debutActif(&s.a).Add(AttenteSite))
}

// regrouperSites (sous verrou) : les pannes des machines d'un site
// injoignable passent dans le groupe du site (un seul message). Une alerte
// déplacée y reste jusqu'à sa fin (son retour est annoncé avec le site).
func (g *Aiguilleur) regrouperSites(bas map[string]SiteInjoignable, t time.Time) {
	if len(bas) == 0 {
		return
	}
	for cle, gr := range g.groupes {
		if groupeSite(cle) != "" {
			continue
		}
		for h, s := range gr.alertes {
			if s.resolue || !g.sourdine.estPanne(&s.a) {
				continue
			}
			site := g.sourdine.siteDe(s.a.Machine())
			if _, ok := bas[site]; !ok {
				continue
			}
			cleSite := prefixeSite + site
			gs := g.groupes[cleSite]
			if gs == nil {
				// Toutes les machines du site sont déjà là : pas d'attente de
				// regroupement en plus (le groupe date de la première panne).
				gs = &groupe{cle: cleSite, cree: debutActif(&s.a), alertes: map[uint64]*suivi{}}
				g.groupes[cleSite] = gs
			}
			if d := debutActif(&s.a); d.Before(gs.cree) {
				gs.cree = d
			}
			gs.alertes[h] = s
			delete(gr.alertes, h)
			g.deplacees[h] = cleSite
		}
		if len(gr.alertes) == 0 {
			delete(g.groupes, cle)
		}
	}
}

// retourEnCours (sous verrou) : dans un groupe de site, des machines sont
// revenues mais pas toutes, depuis moins de AttenteRetourSite : on attend
// les autres pour n'envoyer qu'un message de retour.
func retourEnCours(resolues, encore []alertes.Alerte, t time.Time) bool {
	if len(resolues) == 0 || len(encore) == 0 {
		return false
	}
	premiere := resolues[0].Fin
	for _, a := range resolues[1:] {
		if a.Fin.Before(premiere) {
			premiere = a.Fin
		}
	}
	return t.Before(premiere.Add(AttenteRetourSite))
}

// SitesInjoignables : sites injoignables au dernier tour (affichage), triés.
func (g *Aiguilleur) SitesInjoignables() []SiteInjoignable {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]SiteInjoignable, 0, len(g.sitesBas))
	for _, s := range g.sitesBas {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Site < out[j].Site })
	return out
}

// TitreSite : titre court d'un message de site (push, ntfy, e-mail), "" sinon.
func (m *Message) TitreSite() string {
	switch {
	case m.Site == "":
		return ""
	case len(m.Actives) == 0:
		return "Site " + m.Site + " de nouveau joignable"
	}
	return fmt.Sprintf("Site %s injoignable (%d machines)", m.Site, len(m.Actives))
}
