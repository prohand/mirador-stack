// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"mirador-stack/internal/server/flotte"
	"mirador-stack/internal/server/servicesmetier"
	"mirador-stack/internal/server/stockage/journaux"
)

// Incidents (SPEC § 54, idée reprise de New Relic « issues ») : alertes qui
// forment un même problème, avec une frise (journal d'événements, § 53),
// les logs d'erreur et des notes.
//
// Calculés à partir de l'historique des alertes, sans rien garder de plus :
// une alerte rejoint un incident si elle commence au plus 10 min après la
// fin (ou pendant) d'une alerte de l'incident, et touche la même machine,
// le même site ou un même service. Numéro de l'incident : celui de sa
// première alerte (les notes y sont attachées).

const (
	ecartIncident    = 10 * time.Minute
	periodeIncidents = 30 * 24 * time.Hour
	maxAlertesLues   = 3000
	tailleMaxNote    = 1000
)

type alerteIncident struct {
	ID                   int64
	Regle, Gravite       string
	Machine, Site, Sonde string
	Service              string    // service touché (ServiceEnPanne…)
	Debut, Fin           time.Time // Fin zéro : en cours
	Resume               string
}

type incident struct {
	ID         int64
	Titre      string
	Alertes    []alerteIncident
	Debut, Fin time.Time // Fin zéro : en cours
	Gravite    string
	Machines   []string
	Services   []string
	Prise      string // « pris en charge par admin à 03:24 »
	DureeTexte string
}

func (i *incident) EnCours() bool { return i.Fin.IsZero() }
func (i *incident) Duree(t time.Time) string {
	f := i.Fin
	if f.IsZero() {
		f = t
	}
	return dureeLisible(f.Sub(i.Debut))
}

// cles : ce qui relie une alerte aux autres (machine, site, services).
func (a alerteIncident) cles(services map[string][]string) []string {
	var out []string
	if a.Machine != "" {
		out = append(out, "m:"+a.Machine)
		for _, s := range services["m:"+a.Machine] {
			out = append(out, "v:"+s)
		}
	}
	if a.Site != "" {
		out = append(out, "s:"+a.Site)
	}
	if a.Sonde != "" {
		for _, s := range services["o:"+a.Sonde] {
			out = append(out, "v:"+s)
		}
	}
	if a.Service != "" {
		out = append(out, "v:"+a.Service)
	}
	return out
}

// incidentsDe : alertes de la période regroupées, les plus récents d'abord.
func (c *Console) incidentsDe(ctx context.Context, debut, fin time.Time) []*incident {
	sites := map[string]string{}
	if ms, err := flotte.ListerMachines(ctx, c.db); err == nil {
		for _, m := range ms {
			sites[m.Nom] = m.Site
		}
	}
	// Liens des services (SPEC § 50) : machine, conteneur (sa machine), sonde → services.
	services := map[string][]string{}
	if svcs, err := servicesmetier.Lire(ctx, c.db); err == nil {
		for _, s := range svcs {
			for _, x := range s.Composants {
				switch x.Sorte {
				case servicesmetier.Machine:
					services["m:"+x.Nom] = append(services["m:"+x.Nom], s.Nom)
				case servicesmetier.Conteneur:
					m, _, _ := strings.Cut(x.Nom, "/")
					services["m:"+m] = append(services["m:"+m], s.Nom)
				case servicesmetier.Sonde:
					services["o:"+x.Nom] = append(services["o:"+x.Nom], s.Nom)
				}
			}
		}
	}
	// Sonde rangée sous une machine (SPEC § 60) : son alerte va avec la machine.
	machineSonde := map[string]string{}
	if ss, err := flotte.LireSondes(ctx, c.db); err == nil {
		for _, s := range ss {
			if s.Machine != "" {
				machineSonde[s.Nom] = s.Machine
			}
		}
	}
	rows, err := c.db.QueryContext(ctx, `SELECT id, regle, etiquettes, gravite, active_le, fin, resume, acquittee_par, acquittee_le FROM alertes
		WHERE active_le > 0 AND active_le <= ? AND (fin = 0 OR fin >= ?) AND gravite IN ('critical', 'warning')
		ORDER BY active_le LIMIT ?`, fin.UnixMilli(), debut.UnixMilli(), maxAlertesLues)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var tous []*incident
	ouverts := map[string]*incident{} // clé → incident encore « joignable »
	finIncident := map[*incident]time.Time{}
	for rows.Next() {
		var a alerteIncident
		var etq, par string
		var d, f, ackLe int64
		if rows.Scan(&a.ID, &a.Regle, &etq, &a.Gravite, &d, &f, &a.Resume, &par, &ackLe) != nil {
			continue
		}
		var e map[string]string
		_ = json.Unmarshal([]byte(etq), &e)
		a.Machine, a.Sonde, a.Service = e["instance"], e["sonde"], e["service"]
		if a.Machine == "mirador-server" && a.Sonde != "" {
			a.Machine = ""
		}
		if m := machineSonde[a.Sonde]; a.Sonde != "" && m != "" {
			a.Machine = m
		}
		a.Site = sites[a.Machine]
		a.Debut = time.UnixMilli(d)
		if f > 0 {
			a.Fin = time.UnixMilli(f)
		}
		finA := a.Fin
		if finA.IsZero() {
			finA = fin
		}
		// Incident joignable : une de ses clés, fini il y a moins de 10 min.
		var cible *incident
		for _, k := range a.cles(services) {
			if inc := ouverts[k]; inc != nil && !a.Debut.After(finIncident[inc].Add(ecartIncident)) {
				cible = inc
				break
			}
		}
		if cible == nil {
			cible = &incident{ID: a.ID, Debut: a.Debut}
			tous = append(tous, cible)
		}
		cible.Alertes = append(cible.Alertes, a)
		if finA.After(finIncident[cible]) {
			finIncident[cible] = finA
		}
		for _, k := range a.cles(services) {
			ouverts[k] = cible
		}
		if par != "" && cible.Prise == "" {
			cible.Prise = "pris en charge par " + strings.TrimPrefix(par, "web:") + " à " + time.UnixMilli(ackLe).In(fuseau.Load()).Format("15:04")
		}
	}
	for _, inc := range tous {
		c.completerIncident(inc, services)
		inc.DureeTexte = inc.Duree(fin)
	}
	sort.SliceStable(tous, func(i, j int) bool {
		if tous[i].EnCours() != tous[j].EnCours() {
			return tous[i].EnCours()
		}
		return tous[i].Debut.After(tous[j].Debut)
	})
	return tous
}

// completerIncident : fin, gravité, machines, services, titre.
func (c *Console) completerIncident(inc *incident, services map[string][]string) {
	enCours := false
	vusM, vusS := map[string]bool{}, map[string]bool{}
	sites := map[string]bool{}
	muettes := 0
	var service string
	for _, a := range inc.Alertes {
		if a.Fin.IsZero() {
			enCours = true
		} else if a.Fin.After(inc.Fin) {
			inc.Fin = a.Fin
		}
		if a.Gravite == "critical" {
			inc.Gravite = "critical"
		} else if inc.Gravite == "" {
			inc.Gravite = a.Gravite
		}
		if a.Machine != "" && !vusM[a.Machine] {
			vusM[a.Machine] = true
			inc.Machines = append(inc.Machines, a.Machine)
		}
		for _, k := range a.cles(services) {
			if s, ok := strings.CutPrefix(k, "v:"); ok && !vusS[s] {
				vusS[s] = true
				inc.Services = append(inc.Services, s)
			}
			if s, ok := strings.CutPrefix(k, "s:"); ok {
				sites[s] = true
			}
		}
		if a.Regle == "AgentMuet" {
			muettes++
		}
		if strings.HasPrefix(a.Regle, "ServiceEnPanne") && a.Service != "" {
			service = a.Service
		}
	}
	if enCours {
		inc.Fin = time.Time{}
	}
	premiere := inc.Alertes[0]
	switch {
	case service != "":
		inc.Titre = service + " en panne"
	case muettes >= 2 && len(sites) == 1:
		for s := range sites {
			inc.Titre = "Site " + s + " injoignable (" + strconv.Itoa(muettes) + " machines)"
		}
	case len(inc.Services) == 1 && len(inc.Alertes) > 1:
		inc.Titre = inc.Services[0] + " : " + premiere.Regle
	case premiere.Machine != "":
		inc.Titre = premiere.Machine + " : " + premiere.Regle
	case premiere.Sonde != "":
		inc.Titre = "Sonde " + premiere.Sonde + " : " + premiere.Regle
	default:
		inc.Titre = premiere.Regle
	}
	if n := len(inc.Alertes); n > 1 {
		inc.Titre += fmt.Sprintf(" · %d alertes", n)
	}
}

// --- Pages ------------------------------------------------------------------------

type donneesIncidents struct {
	Incidents []*incident
	EnCours   int
}

func (c *Console) pageIncidents(q *req) {
	d := donneesIncidents{Incidents: c.incidentsDe(q.r.Context(), q.t.Add(-periodeIncidents), q.t)}
	for _, i := range d.Incidents {
		if i.EnCours() {
			d.EnCours++
		}
	}
	c.rendre(q, "incidents", "Incidents", "alertes", d)
}

type pointFrise struct {
	X                 float64 // 0 à 100 (%)
	Haut              bool
	Heure, Titre, Txt string
	Classe, Lien      string
}

type noteIncident struct {
	Le       time.Time
	Qui, Txt string
}

type donneesIncident struct {
	I                    *incident
	Points               []pointFrise
	BandeX, BandeL       float64
	Journal              []evenement
	Logs                 []ligneLogIncident
	Notes                []noteIncident
	DebutFrise, FinFrise string
}

type ligneLogIncident struct {
	Heure, Hote, Msg string
}

// incidentPar : incident qui contient l'alerte id (30 derniers jours).
func (c *Console) incidentPar(ctx context.Context, id int64, t time.Time) *incident {
	for _, i := range c.incidentsDe(ctx, t.Add(-periodeIncidents), t) {
		for _, a := range i.Alertes {
			if a.ID == id {
				return i
			}
		}
	}
	return nil
}

func (c *Console) pageIncident(q *req) {
	ctx := q.r.Context()
	id, _ := strconv.ParseInt(q.r.PathValue("id"), 10, 64)
	inc := c.incidentPar(ctx, id, q.t)
	if inc == nil {
		c.rendreCode(q, 404, "introuvable", "Incident introuvable", "alertes", nil)
		return
	}
	d := donneesIncident{I: inc}
	fin := inc.Fin
	if fin.IsZero() {
		fin = q.t
	}
	// Fenêtre de la frise : 30 min avant (les changements qui ont précédé),
	// 5 min après.
	f0, f1 := inc.Debut.Add(-30*time.Minute), fin.Add(5*time.Minute)
	loc := fuseau.Load()
	d.DebutFrise, d.FinFrise = f0.In(loc).Format("15:04"), f1.In(loc).Format("15:04")
	pos := func(t time.Time) float64 { return 100 * float64(t.Sub(f0)) / float64(f1.Sub(f0)) }
	d.BandeX, d.BandeL = pos(inc.Debut), pos(fin)-pos(inc.Debut)
	machines := map[string]bool{}
	for _, m := range inc.Machines {
		machines[m] = true
	}
	ids := map[string]bool{}
	for _, a := range inc.Alertes {
		ids["/alertes/"+strconv.FormatInt(a.ID, 10)] = true
	}
	notes := c.notesIncident(ctx, inc.ID)
	for _, e := range c.journalEvenements(ctx, filtreEvenements{Debut: f0, Fin: f1, SansAuto: true, Max: 300}) {
		garder := ids[e.Lien] || (e.Machine != "" && machines[e.Machine] && e.Type != evReglage) || e.Objet == "incident "+strconv.FormatInt(inc.ID, 10)
		if garder {
			d.Journal = append(d.Journal, e)
		}
	}
	// Frise : 14 points au plus, du plus ancien au plus récent, alternés.
	pts := append([]evenement(nil), d.Journal...)
	sort.SliceStable(pts, func(i, j int) bool { return pts[i].Le.Before(pts[j].Le) })
	if len(pts) > 14 {
		pts = pts[len(pts)-14:]
	}
	for k, e := range pts {
		d.Points = append(d.Points, pointFrise{X: min(94, max(6, pos(e.Le))), Haut: k%2 == 0, // textes entiers près des bords Heure: e.Le.In(loc).Format("15:04"), Titre: e.Titre(),
			Txt: raccourcirTexte(e.Texte, 60), Classe: e.Classe(), Lien: e.Lien})
	}
	// Logs d'erreur des machines de l'incident pendant la fenêtre (20 au plus).
	for _, m := range inc.Machines {
		if c.opt.Logs == nil || len(d.Logs) >= 20 || len(machines) > 5 { // pas de logs (tests) ou trop de machines
			break
		}
		res, err := c.chercherLogs(ctx, journaux.Requete{Debut: f0.UnixMilli(), Fin: f1.UnixMilli(), Champs: map[string]string{"host": m, "level": "err"},
			Variantes: map[string][]string{"level": niveauxErreur}, Limite: 20})
		if err != nil {
			continue
		}
		for _, l := range res.Lignes {
			d.Logs = append(d.Logs, ligneLogIncident{Heure: time.UnixMilli(l.T).In(loc).Format("15:04:05"), Hote: m, Msg: raccourcirTexte(l.Msg, 300)})
		}
	}
	sort.SliceStable(d.Logs, func(i, j int) bool { return d.Logs[i].Heure < d.Logs[j].Heure })
	if len(d.Logs) > 20 {
		d.Logs = d.Logs[len(d.Logs)-20:]
	}
	d.Notes = notes
	c.rendre(q, "incident", "Incident "+strconv.FormatInt(inc.ID, 10), "alertes", d)
}

func raccourcirTexte(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

func (c *Console) notesIncident(ctx context.Context, id int64) []noteIncident {
	var out []noteIncident
	rows, err := c.db.QueryContext(ctx, `SELECT le, qui, texte FROM notes_incidents WHERE incident = ? ORDER BY le`, id)
	if err != nil {
		return nil
	}
	defer rows.Close()
	for rows.Next() {
		var n noteIncident
		var le int64
		if rows.Scan(&le, &n.Qui, &n.Txt) == nil {
			n.Le, n.Qui = time.Unix(le, 0), strings.TrimPrefix(n.Qui, "web:")
			out = append(out, n)
		}
	}
	return out
}

// postNoteIncident : ajouter une note (administrateurs).
func (c *Console) postNoteIncident(q *req) {
	id, _ := strconv.ParseInt(q.r.PathValue("id"), 10, 64)
	lien := "/incidents/" + strconv.FormatInt(id, 10)
	txt := strings.TrimSpace(q.r.PostFormValue("note"))
	if txt == "" || len([]rune(txt)) > tailleMaxNote {
		c.retour(q, lien, errors.New("note vide ou trop longue (1 000 caractères au plus)"), "")
		return
	}
	if c.incidentPar(q.r.Context(), id, q.t) == nil {
		c.retour(q, "/incidents", errors.New("incident introuvable"), "")
		return
	}
	_, err := c.db.ExecContext(q.r.Context(), `INSERT INTO notes_incidents(incident, le, qui, texte) VALUES (?, ?, ?, ?)`, id, q.t.Unix(), q.qui(), txt)
	c.retour(q, lien, err, "note ajoutée")
}

// lienIncident : page de l'incident d'une alerte (page de l'alerte).
func lienIncident(id int64) string { return "/incidents/" + url.PathEscape(strconv.FormatInt(id, 10)) }
