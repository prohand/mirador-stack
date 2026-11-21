// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"encoding/json"
	"mirador-stack/internal/protocole"
	"mirador-stack/internal/server/flotte"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"mirador-stack/internal/server/changements"
	"mirador-stack/internal/server/sauvegarde"
)

// Journal d'événements (SPEC § 53, idée reprise de New Relic) : tout ce qui
// s'est passé, dans l'ordre, lu dans les tables existantes (rien de plus en
// base) : alertes (début et fin), changements, maintenances, réglages
// (audit), sauvegardes, notes d'incident. Un seul point d'entrée
// (journalEvenements) pour la page Événements, la frise des incidents et
// les repères des courbes.

// Types d'événement.
const (
	evAlerte      = "alerte"
	evResolu      = "resolu"
	evSonde       = "sonde"
	evChangement  = "changement"
	evMaintenance = "maintenance"
	evReglage     = "reglage"
	evSauvegarde  = "sauvegarde"
	evNote        = "note"
	evPanne       = "panne" // vue tout de suite par l'agent (SPEC § 98)
)

// titresEvenements : libellé et couleur de la puce de chaque type.
var titresEvenements = map[string][2]string{
	evAlerte: {"Alerte", "c-al"}, evResolu: {"Résolu", "c-ok"}, evSonde: {"Sonde", "c-so"},
	evChangement: {"Changement", "c-ch"}, evMaintenance: {"Maintenance", "c-ma"}, evReglage: {"Réglage", "c-av"},
	evSauvegarde: {"Sauvegarde", "c-ok"}, evNote: {"Note", "c-lg"}, evPanne: {"Panne", "c-al"},
}

// evenement : une ligne du journal.
type evenement struct {
	Le                 time.Time
	Type               string
	Gravite            string // critical, warning… (alertes)
	Texte, Objet, Lien string
	Machine            string // pour le filtre (vide : général)
	Auto               bool   // réglage fait par mirador lui-même
}

// Titre et Classe : pour l'affichage.
func (e evenement) Titre() string  { return titresEvenements[e.Type][0] }
func (e evenement) Classe() string { return titresEvenements[e.Type][1] }

// filtreEvenements : machine (vide : toutes), types (vide : tous).
type filtreEvenements struct {
	Debut, Fin time.Time
	Machine    string
	Types      map[string]bool
	SansAuto   bool
	Max        int
	// Cite : seulement les événements dont le texte cite ce nom (fiche d'un
	// élément, « element= » de la page). Filtré aussi dans les requêtes,
	// avant la limite (relecture du 21/11).
	Cite string
}

func (f filtreEvenements) veut(e evenement) bool {
	if f.Machine != "" && e.Machine != f.Machine {
		return false
	}
	if len(f.Types) > 0 && !f.Types[e.Type] {
		return false
	}
	if f.Cite != "" && !strings.Contains(e.Texte, f.Cite) {
		return false
	}
	return !(f.SansAuto && e.Auto)
}

// journalEvenements : événements de la période, les plus récents d'abord.
func (c *Console) journalEvenements(ctx context.Context, f filtreEvenements) []evenement {
	if f.Max <= 0 {
		f.Max = 500
	}
	var out []evenement
	ajouter := func(e evenement) {
		if !e.Le.Before(f.Debut) && !e.Le.After(f.Fin) && f.veut(e) {
			out = append(out, e)
		}
	}
	// Filtre machine appliqué dans la requête, avant la limite (relecture
	// du 21/11 : sinon, sur un parc chargé, les événements d'une machine
	// calme étaient coupés par ceux des autres).
	sqlAlerte, sqlChgt := "", ""
	argsAlerte := []any{f.Debut.UnixMilli(), f.Fin.UnixMilli(), f.Debut.UnixMilli(), f.Fin.UnixMilli()}
	argsChgt := []any{f.Debut.Unix(), f.Fin.Unix()}
	if f.Machine != "" {
		sqlAlerte = ` AND COALESCE(NULLIF(json_extract(etiquettes, '$.instance'), ''), json_extract(etiquettes, '$.host')) = ?`
		argsAlerte = append(argsAlerte, f.Machine)
		sqlChgt = ` AND machine = ?`
		argsChgt = append(argsChgt, f.Machine)
	}
	// Tri grossier dans la requête (LIKE, sans casse) ; le tri exact se
	// fait sur le texte final (veut).
	sqlAuditCite, argsAuditCite := "", []any{}
	if f.Cite != "" {
		m := "%" + echapperLike(f.Cite) + "%"
		sqlAlerte += ` AND (resume LIKE ? ESCAPE '\' OR regle LIKE ? ESCAPE '\' OR etiquettes LIKE ? ESCAPE '\')`
		argsAlerte = append(argsAlerte, m, m, m)
		sqlChgt += ` AND (detail LIKE ? ESCAPE '\' OR machine LIKE ? ESCAPE '\' OR type LIKE ? ESCAPE '\')`
		argsChgt = append(argsChgt, m, m, m)
		sqlAuditCite = ` AND (objet LIKE ? ESCAPE '\' OR action LIKE ? ESCAPE '\' OR qui LIKE ? ESCAPE '\')`
		argsAuditCite = append(argsAuditCite, m, m, m)
	}
	// Alertes : début (passage en alerte) et fin.
	if rows, err := c.db.QueryContext(ctx, `SELECT id, regle, etiquettes, gravite, active_le, fin, resume FROM alertes
		WHERE active_le > 0 AND (active_le BETWEEN ? AND ? OR fin BETWEEN ? AND ?)`+sqlAlerte+` ORDER BY active_le DESC LIMIT ?`,
		append(argsAlerte, 4*f.Max)...); err == nil {
		for rows.Next() {
			var id, active, fin int64
			var regle, etq, gravite, resume string
			if rows.Scan(&id, &regle, &etq, &gravite, &active, &fin, &resume) != nil {
				continue
			}
			var e map[string]string
			_ = json.Unmarshal([]byte(etq), &e)
			machine := e["instance"]
			if machine == "" {
				machine = e["host"]
			}
			typ := evAlerte
			if strings.HasPrefix(regle, "Sonde") {
				typ = evSonde
			}
			texte := libelleRegle(regle)
			if resume != "" {
				texte += " — " + resume
			}
			lien := "/alertes/" + strconv.FormatInt(id, 10)
			ajouter(evenement{Le: time.UnixMilli(active), Type: typ, Gravite: gravite, Texte: texte, Objet: machine, Lien: lien, Machine: machine})
			if fin > 0 {
				ajouter(evenement{Le: time.UnixMilli(fin), Type: evResolu, Texte: libelleRegle(regle) + " : terminée (" + dureeLisible(time.Duration(fin-active)*time.Millisecond) + ")",
					Objet: machine, Lien: lien, Machine: machine})
			}
		}
		_ = rows.Close()
	}
	// Pannes vues tout de suite par l'agent (SPEC § 98).
	if ps, err := flotte.ListerPannes(ctx, c.db, f.Machine, f.Debut, f.Fin, f.Max); err == nil {
		for _, p := range ps {
			ajouter(evenement{Le: p.Le, Type: evPanne, Gravite: "critical", Texte: textePanne(p), Objet: p.Machine,
				Lien: "/machines/" + url.PathEscape(p.Machine), Machine: p.Machine})
		}
	}
	// Changements repérés (SPEC § 47).
	if rows, err := c.db.QueryContext(ctx, `SELECT machine, le, type, detail FROM changements WHERE le BETWEEN ? AND ?`+sqlChgt+` ORDER BY le DESC LIMIT ?`,
		append(argsChgt, f.Max)...); err == nil {
		for rows.Next() {
			var x changements.Changement
			var le int64
			if rows.Scan(&x.Machine, &le, &x.Type, &x.Detail) != nil {
				continue
			}
			x.Le = time.Unix(le, 0)
			ajouter(evenement{Le: x.Le, Type: evChangement, Texte: changements.Titre(x.Type) + " : " + x.Resume(), Objet: x.Machine,
				Lien: "/machines/" + url.PathEscape(x.Machine) + "#changements", Machine: x.Machine})
		}
		_ = rows.Close()
	}
	// Maintenances déclarées (début).
	if rows, err := c.db.QueryContext(ctx, `SELECT cible, debut, fin, auteur, commentaire FROM maintenances WHERE recurrence = '' AND debut BETWEEN ? AND ?`,
		f.Debut.Unix(), f.Fin.Unix()); err == nil {
		for rows.Next() {
			var cible, auteur, com string
			var d, fi int64
			if rows.Scan(&cible, &d, &fi, &auteur, &com) != nil {
				continue
			}
			machine := strings.TrimPrefix(cible, "machine:")
			if machine == cible {
				machine = ""
			}
			texte := "Maintenance de " + cible + " jusqu'à " + time.Unix(fi, 0).In(fuseau.Load()).Format("02/01 15:04") + " (" + auteur + ")"
			if com != "" {
				texte += " : " + com
			}
			ajouter(evenement{Le: time.Unix(d, 0), Type: evMaintenance, Texte: texte, Objet: cible, Lien: "/sourdine", Machine: machine})
		}
		_ = rows.Close()
	}
	// Réglages (audit) ; machine retrouvée par « machine:ID » ou par le nom.
	noms := map[string]string{}
	if rows, err := c.db.QueryContext(ctx, `SELECT id, nom FROM machines`); err == nil {
		for rows.Next() {
			var id int64
			var nom string
			if rows.Scan(&id, &nom) == nil {
				noms["machine:"+strconv.FormatInt(id, 10)] = nom
				noms[nom] = nom
			}
		}
		_ = rows.Close()
	}
	sqlAudit, argsAudit := "", []any{f.Debut.Unix(), f.Fin.Unix()}
	if f.Machine != "" {
		// Objet « machine:ID … » ou « nom … » (premier mot).
		var ids []string
		for k, v := range noms {
			if v == f.Machine && k != v {
				ids = append(ids, k)
			}
		}
		sqlAudit = ` AND (objet = ? OR objet LIKE ? ESCAPE '\'`
		argsAudit = append(argsAudit, f.Machine, echapperLike(f.Machine)+" %")
		for _, id := range ids {
			sqlAudit += ` OR objet = ? OR objet LIKE ? ESCAPE '\'`
			argsAudit = append(argsAudit, id, echapperLike(id)+" %")
		}
		sqlAudit += `)`
	}
	sqlAudit += sqlAuditCite
	argsAudit = append(argsAudit, argsAuditCite...)
	if rows, err := c.db.QueryContext(ctx, `SELECT le, qui, action, objet FROM audit WHERE le BETWEEN ? AND ?`+sqlAudit+` ORDER BY le DESC LIMIT ?`, // #nosec G202 -- morceaux SQL fixes, valeurs en paramètres
		append(argsAudit, 2*f.Max)...); err == nil {
		for rows.Next() {
			var le int64
			var qui, action, objet string
			if rows.Scan(&le, &qui, &action, &objet) != nil {
				continue
			}
			machine := noms[strings.SplitN(objet, " ", 2)[0]]
			texte := remplacerPortees(phraseAudit(action, objet), noms)
			if qui != "" {
				texte += " (" + strings.TrimPrefix(qui, "web:") + ")"
			}
			ajouter(evenement{Le: time.Unix(le, 0), Type: evReglage, Texte: texte, Objet: objet, Lien: "/audit", Machine: machine,
				Auto: qui == "mirador" || qui == "auto"})
		}
		_ = rows.Close()
	}
	// Sauvegardes.
	if h, err := sauvegarde.Historique(ctx, c.db, 60); err == nil {
		for _, s := range h {
			if s.Fin == 0 {
				continue
			}
			texte := "Sauvegarde réussie (" + tailleLisible(s.Taille) + ")"
			if s.Statut != "ok" {
				texte = "Sauvegarde en échec : " + s.Erreur
			}
			ajouter(evenement{Le: time.Unix(s.Fin, 0), Type: evSauvegarde, Texte: texte, Objet: "serveur", Lien: "/reglages/sauvegardes"})
		}
	}
	// Notes d'incident (SPEC § 54).
	if rows, err := c.db.QueryContext(ctx, `SELECT incident, le, qui, texte FROM notes_incidents WHERE le BETWEEN ? AND ?`, f.Debut.Unix(), f.Fin.Unix()); err == nil {
		for rows.Next() {
			var inc, le int64
			var qui, txt string
			if rows.Scan(&inc, &le, &qui, &txt) == nil {
				ajouter(evenement{Le: time.Unix(le, 0), Type: evNote, Texte: strings.TrimPrefix(qui, "web:") + " : « " + txt + " »",
					Objet: "incident " + strconv.FormatInt(inc, 10), Lien: "/incidents/" + strconv.FormatInt(inc, 10)})
			}
		}
		_ = rows.Close()
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Le.After(out[j].Le) })
	if len(out) > f.Max {
		out = out[:f.Max]
	}
	return out
}

// --- Page « Événements » --------------------------------------------------------------

type jourEvenements struct {
	Titre  string
	Lignes []evenement
}

type donneesEvenements struct {
	Jours         []jourEvenements
	Machines      []string
	Machine, Type string
	Periode       string
	SansAuto      bool
	Types         [][2]string // code, titre
	Total         int
	Element       string // événements qui citent cet élément (fiches, § 68)
	FiltreSur     filtreSur
}

func (c *Console) pageEvenements(q *req) {
	ctx := q.r.Context()
	v := q.r.URL.Query()
	d := donneesEvenements{Machine: v.Get("machine"), Type: v.Get("type"), Periode: v.Get("periode"), SansAuto: v.Get("auto") != "1"}
	duree := 7 * 24 * time.Hour
	switch d.Periode {
	case "24h":
		duree = 24 * time.Hour
	case "30j":
		duree = 30 * 24 * time.Hour
	default:
		d.Periode = "7j"
	}
	f := filtreEvenements{Debut: q.t.Add(-duree), Fin: q.t, Machine: d.Machine, SansAuto: d.SansAuto}
	if d.Type != "" {
		f.Types = map[string]bool{d.Type: true}
		if d.Type == evAlerte {
			f.Types[evResolu] = true
		}
	}
	if d.Element = strings.TrimSpace(v.Get("element")); d.Element != "" && len(d.Element) <= 200 {
		f.Cite = d.Element
		d.FiltreSur = filtreSur{"« " + d.Element + " »", "/evenements"}
	} else {
		d.Element = ""
	}
	evs := c.journalEvenements(ctx, f)
	d.Total = len(evs)
	loc := fuseau.Load()
	for _, e := range evs {
		titre := jourLisible(e.Le.In(loc), q.t.In(loc))
		if n := len(d.Jours); n == 0 || d.Jours[n-1].Titre != titre {
			d.Jours = append(d.Jours, jourEvenements{Titre: titre})
		}
		d.Jours[len(d.Jours)-1].Lignes = append(d.Jours[len(d.Jours)-1].Lignes, e)
	}
	if rows, err := c.db.QueryContext(ctx, `SELECT nom FROM machines WHERE statut = 'approuvee' ORDER BY nom`); err == nil {
		for rows.Next() {
			var n string
			if rows.Scan(&n) == nil {
				d.Machines = append(d.Machines, n)
			}
		}
		_ = rows.Close()
	}
	for _, t := range []string{evAlerte, evSonde, evChangement, evMaintenance, evReglage, evSauvegarde, evNote} {
		d.Types = append(d.Types, [2]string{t, titresEvenements[t][0]})
	}
	c.rendre(q, "evenements", "Événements", "alertes", d)
}

// jourLisible : « Aujourd'hui · mercredi 18/11 », « Hier · … », « lundi 16/11 ».
func jourLisible(t, maintenant time.Time) string {
	jours := []string{"dimanche", "lundi", "mardi", "mercredi", "jeudi", "vendredi", "samedi"}
	s := jours[t.Weekday()] + " " + t.Format("02/01")
	a, b := t.Format("2006-01-02"), maintenant.Format("2006-01-02")
	switch {
	case a == b:
		return "Aujourd'hui · " + s
	case a == maintenant.AddDate(0, 0, -1).Format("2006-01-02"):
		return "Hier · " + s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// textePanne : « nginx.service est passé en échec », « processus tué faute de mémoire (1) ».
func textePanne(p flotte.Panne) string {
	switch p.Type {
	case protocole.EvenementService:
		return p.Objet + " est passé en échec (vu par l'agent en direct)"
	case protocole.EvenementOOM:
		return "Processus tué faute de mémoire : " + p.Texte
	}
	return p.Type + " " + p.Objet
}

// echapperLike protège %, _ et \ d'un texte pour « LIKE … ESCAPE '\' ».
func echapperLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(s)
}

// fusionnerEvenements : deux listes (les plus récents d'abord), sans
// doublon, max éléments au plus.
func fusionnerEvenements(a, b []evenement, max int) []evenement {
	vus := map[evenement]bool{}
	var out []evenement
	for _, e := range append(append([]evenement{}, a...), b...) {
		if !vus[e] {
			vus[e] = true
			out = append(out, e)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Le.After(out[j].Le) })
	if len(out) > max {
		out = out[:max]
	}
	return out
}
