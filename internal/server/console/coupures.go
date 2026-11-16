// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"time"

	"mirador-stack/internal/server/alertes"
	"mirador-stack/internal/server/flotte"
	"mirador-stack/internal/server/notif"
)

// Page « Coupures » (SPEC.md § 39.2, demande du 16/11 après la coupure
// d'internet du site de l'utilisateur) : quand un site entier s'est tu,
// les données ont-elles été rattrapées ?
//
// Pendant une coupure, chaque agent garde ses lots en mémoire (8 Mio de
// métriques, 4 Mio de logs, réglages par défaut) et les renvoie au retour ;
// le serveur accepte jusqu'à 24 h de retard. Cette page le vérifie, machine
// par machine, sans rien stocker de plus :
//   - les coupures sont retrouvées dans l'historique des alertes : au moins
//     2 machines d'un même site en panne (AgentMuet…) à moins de 10 min
//     d'écart ; cela marche aussi pour une coupure d'avant cette page ;
//   - le bilan est lu dans la base de métriques : relevés reçus pendant la
//     coupure (comparés à l'heure d'avant), taille maximale de la file
//     d'attente de l'agent, points jetés (file pleine), autonomie estimée.
//
// Coût : une lecture de l'historique des alertes (30 jours, index sur
// active_le non nécessaire : quelques milliers de lignes au plus) et
// 7 requêtes instantanées par machine de la coupure, seulement à
// l'ouverture de la page.

// ecartCoupure : pannes à moins de cet écart = même coupure.
const ecartCoupure = 10 * time.Minute

// Tampons par défaut de l'agent (internal/agent/config) : octets compressés.
const (
	tamponMetriquesDefaut = 8 << 20
	tamponLogsDefaut      = 4 << 20
)

// sansSite : nom affiché pour les machines sans site.
const sansSite = "(sans site)"

type coupureSite struct {
	Site       string
	Debut, Fin time.Time // Fin zéro : en cours
	Machines   []string
	pannes     []alertes.Alerte
}

// Cle : identifiant dans l'adresse (site et début en secondes).
func (c coupureSite) Lien() string {
	return "/coupures?site=" + url.QueryEscape(c.Site) + "&debut=" + strconv.FormatInt(c.Debut.Unix(), 10)
}

// Duree : durée (jusqu'à maintenant si en cours).
func (c coupureSite) Duree(maintenant time.Time) time.Duration {
	if c.Fin.IsZero() {
		return maintenant.Sub(c.Debut)
	}
	return c.Fin.Sub(c.Debut)
}

// ligneBilan : une machine de la coupure.
type ligneBilan struct {
	Machine          string
	Muette, Revenue  time.Time // alerte de panne (Revenue zéro : encore muette)
	Couverture       int       // % des relevés reçus pour la coupure (-1 : inconnu)
	FileMax          string    // taille maximale de la file d'attente
	FilePct          int       // en % du tampon (-1 : inconnu)
	Jetes, LogsJetes int64     // points / lots jetés (file pleine) ; -1 : inconnu
	Autonomie        string    // durée estimée avant que le tampon ne déborde
	Limite           time.Time // début + autonomie
	Etat, Classe     string
}

type donneesCoupures struct {
	Coupures []coupureSite
	Choisie  *coupureSite
	Bilan    []ligneBilan
	Site     string
	Sites    []notif.SiteInjoignable // sites injoignables en ce moment
	Le       time.Time
}

// trouverCoupures : coupures des 30 derniers jours, plus récentes d'abord.
func trouverCoupures(as []alertes.Alerte, reglesPanne []string, sites map[string]string) []coupureSite {
	panne := map[string]bool{}
	for _, r := range reglesPanne {
		panne[r] = true
	}
	parSite := map[string][]alertes.Alerte{}
	for _, a := range as {
		m := a.Machine()
		if !panne[a.Regle] || m == "" || a.ActiveLe.IsZero() {
			continue
		}
		site, connue := sites[m]
		if !connue {
			continue // machine supprimée depuis
		}
		if site == "" {
			site = sansSite
		}
		parSite[site] = append(parSite[site], a)
	}
	var out []coupureSite
	for site, ps := range parSite {
		sort.Slice(ps, func(i, j int) bool { return ps[i].ActiveLe.Before(ps[j].ActiveLe) })
		for i := 0; i < len(ps); {
			c := coupureSite{Site: site, Debut: ps[i].ActiveLe}
			j := i
			for ; j < len(ps) && ps[j].ActiveLe.Sub(c.Debut) <= ecartCoupure; j++ {
				c.pannes = append(c.pannes, ps[j])
			}
			i = j
			vues := map[string]bool{}
			enCours := false
			for _, a := range c.pannes {
				if !vues[a.Machine()] {
					vues[a.Machine()] = true
					c.Machines = append(c.Machines, a.Machine())
				}
				if a.Fin.IsZero() {
					enCours = true
				} else if a.Fin.After(c.Fin) {
					c.Fin = a.Fin
				}
			}
			if len(c.Machines) < 2 {
				continue // une seule machine : pas une coupure du site
			}
			if enCours {
				c.Fin = time.Time{}
			}
			sort.Strings(c.Machines)
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Debut.After(out[j].Debut) })
	return out
}

func (c *Console) pageCoupures(q *req) {
	ctx := q.r.Context()
	d := donneesCoupures{Site: q.r.URL.Query().Get("site"), Le: q.t}
	if c.opt.Aiguilleur != nil {
		d.Sites = c.opt.Aiguilleur.SitesInjoignables()
	}
	r, err := notif.LireReglages(ctx, c.db)
	if err != nil {
		c.erreurCoupures(q, err)
		return
	}
	ms, err := flotte.ListerMachines(ctx, c.db)
	if err != nil {
		c.erreurCoupures(q, err)
		return
	}
	sites := map[string]string{}
	for _, m := range ms {
		if m.Statut == "approuvee" {
			sites[m.Nom] = m.Site
		}
	}
	as, err := alertes.HistoriquePeriode(ctx, c.db, q.t.Add(-30*24*time.Hour), q.t, 20000)
	if err != nil {
		c.erreurCoupures(q, err)
		return
	}
	d.Coupures = trouverCoupures(as, r.ReglesPanne, sites)
	debut, _ := strconv.ParseInt(q.r.URL.Query().Get("debut"), 10, 64)
	for i := range d.Coupures {
		x := &d.Coupures[i]
		if d.Site != "" && x.Site != d.Site || debut > 0 && x.Debut.Unix() != debut {
			continue
		}
		d.Choisie = x // la plus récente qui correspond
		break
	}
	if d.Choisie != nil {
		d.Bilan = c.bilanCoupure(ctx, d.Choisie, q.t)
	}
	if len(d.Coupures) > 100 {
		d.Coupures = d.Coupures[:100]
	}
	c.rendre(q, "coupures", "Coupures de site", "alertes", d)
}

// bilanCoupure : une ligne par machine (voir l'en-tête du fichier).
func (c *Console) bilanCoupure(ctx context.Context, x *coupureSite, maintenant time.Time) []ligneBilan {
	fin := x.Fin
	if fin.IsZero() {
		fin = maintenant
	}
	duree := fin.Sub(x.Debut)
	// Les relevés de la coupure arrivent au retour ; la file et les pertes
	// sont lues jusqu'à 1 h après (le rattrapage prend quelques minutes).
	apres := fin.Add(time.Hour)
	if apres.After(maintenant) {
		apres = maintenant
	}
	une := func(expr string, t time.Time) (float64, bool) {
		if c.lecteur == nil {
			return 0, false
		}
		ech, err := c.lecteur.Instant(ctx, expr, t)
		if err != nil || len(ech) == 0 || math.IsNaN(ech[0].Valeur) || math.IsInf(ech[0].Valeur, 0) {
			return 0, false
		}
		return ech[0].Valeur, true
	}
	fenetre := func(d time.Duration) string { return strconv.FormatInt(int64(math.Max(60, d.Seconds())), 10) + "s" }
	var out []ligneBilan
	for _, m := range x.Machines {
		l := ligneBilan{Machine: m, Couverture: -1, FilePct: -1, Jetes: -1, LogsJetes: -1, FileMax: "—", Autonomie: "—"}
		for _, a := range x.pannes {
			if a.Machine() != m {
				continue
			}
			if l.Muette.IsZero() || a.ActiveLe.Before(l.Muette) {
				l.Muette = a.ActiveLe
			}
			if a.Fin.IsZero() {
				l.Revenue = time.Time{}
				break
			}
			if a.Fin.After(l.Revenue) {
				l.Revenue = a.Fin
			}
		}
		if !reValeurVariable.MatchString(m) {
			out = append(out, l)
			continue
		}
		i := `{instance="` + m + `"}`
		// Relevés reçus pour la coupure, comparés à l'heure d'avant.
		avant, okA := une(`count_over_time(node_uname_info`+i+`[1h])`, x.Debut)
		pendant, _ := une(`count_over_time(node_uname_info`+i+`[`+fenetre(duree)+`])`, fin)
		if okA && avant > 0 {
			attendus := avant / 3600 * duree.Seconds()
			l.Couverture = int(math.Min(100, math.Round(100*pendant/math.Max(attendus, 1))))
		}
		w := fenetre(apres.Sub(x.Debut) + 5*time.Minute)
		if v, ok := une(`max_over_time(mirador_agent_queue_bytes`+i+`[`+w+`])`, apres); ok {
			l.FileMax = virgule(tailleLisible(int64(v)))
			l.FilePct = int(math.Min(100, math.Round(100*v/tamponMetriquesDefaut)))
		}
		if v, ok := une(`increase(mirador_agent_queue_dropped_total`+i+`[`+w+`])`, apres); ok {
			l.Jetes = int64(math.Round(v))
		}
		if v, ok := une(`increase(mirador_logs_queue_dropped_total`+i+`[`+w+`])`, apres); ok {
			l.LogsJetes = int64(math.Round(v))
		}
		// Autonomie : tampon par défaut / débit envoyé dans l'heure d'avant.
		if v, ok := une(`rate(mirador_agent_sent_bytes_total`+i+`[1h])`, x.Debut); ok && v > 0 {
			auto := time.Duration(float64(tamponMetriquesDefaut) / v * float64(time.Second))
			l.Autonomie, l.Limite = dureeLisible(auto.Round(time.Minute)), x.Debut.Add(auto)
		}
		l.Etat, l.Classe = etatBilan(l, maintenant)
		out = append(out, l)
	}
	return out
}

// etatBilan : phrase courte et couleur de la ligne.
func etatBilan(l ligneBilan, maintenant time.Time) (string, string) {
	switch {
	case l.Revenue.IsZero() && !l.Limite.IsZero() && maintenant.After(l.Limite):
		return "Encore muette ; tampon sans doute plein : les plus anciens points sont jetés", "erreur"
	case l.Revenue.IsZero():
		return "Encore muette ; données gardées dans le tampon de l'agent", "attention"
	case l.Jetes > 0:
		return fmt.Sprintf("Trou : %d lot(s) jeté(s), tampon plein", l.Jetes), "erreur"
	case l.Couverture >= 95:
		return "Rattrapé", "ok"
	case l.Couverture >= 0 && maintenant.Sub(l.Revenue) < 15*time.Minute:
		return "Rattrapage en cours", "attention"
	case l.Couverture >= 0:
		return fmt.Sprintf("Incomplet : %d %% des relevés reçus", l.Couverture), "erreur"
	}
	return "Inconnu (pas de relevés de l'agent)", ""
}

// erreurCoupures : erreur de lecture en base (détail dans le journal seulement).
func (c *Console) erreurCoupures(q *req, err error) {
	if c.opt.Journal != nil {
		c.opt.Journal.Error("page des coupures", "erreur", err)
	}
	http.Error(q.w, "erreur interne (détail dans le journal du serveur)", http.StatusInternalServerError)
}
