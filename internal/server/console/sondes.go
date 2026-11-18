// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"errors"
	"html/template"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"mirador-stack/internal/server/alertes"
	"mirador-stack/internal/server/flotte"
	essais "mirador-stack/internal/sondes"
)

// Page « Sondes » (SPEC § 48, idée reprise de New Relic « synthetics ») :
// liste des sondes, dernier essai, temps de réponse sur 1 h, disponibilité
// sur 24 h ; ajout et retrait (administrateurs, code 2FA).
//
// Coût à l'ouverture : 6 requêtes (5 instantanées, 1 sur 1 h au pas de
// 1 min), quelques dizaines de séries.

// instanceServeur : nom des séries écrites par le serveur lui-même.
const instanceServeur = "mirador-server"

type ligneSonde struct {
	S                essais.Sonde
	Attendu          string // « code 200, contient … »
	Depuis           string // « serveur » ou nom du collecteur
	Etat, ClasseEtat string
	Courbe           template.HTML
	Dispo            string
	Probleme         bool
	SansDonnee       bool
}

type donneesSondes struct {
	Lignes                    []ligneSonde
	Total, OK, Echecs, Lentes int
	Mediane                   string
	Depuis                    []string // serveur + collecteurs (formulaire)
}

// cleSonde : séries d'une sonde (instance = depuis, sonde = nom).
func cleSonde(instance, nom string) string { return instance + "\x00" + nom }

func instanceDe(s essais.Sonde) string {
	if s.Depuis == essais.ServeurLocal {
		return instanceServeur
	}
	return s.Depuis
}

func (c *Console) sondesLues(ctx context.Context, t time.Time) map[string]map[string]alertes.Echantillon {
	out := map[string]map[string]alertes.Echantillon{}
	if c.lecteur == nil {
		return out
	}
	for nom, expr := range map[string]string{
		"succes": `max by (instance, sonde) (mirador_sonde_succes)`,
		"duree":  `max by (instance, sonde) (mirador_sonde_duree_secondes)`,
		"lente":  `max by (instance, sonde) (mirador_sonde_lente)`,
		"erreur": `max by (instance, sonde, erreur) (mirador_sonde_erreur)`,
		"dispo":  `avg by (instance, sonde) (avg_over_time(mirador_sonde_succes[24h]))`,
	} {
		ech, err := c.instant(ctx, expr, t)
		if err != nil {
			continue
		}
		m := map[string]alertes.Echantillon{}
		for _, e := range ech {
			m[cleSonde(e.Etiquettes.Get("instance"), e.Etiquettes.Get("sonde"))] = e
		}
		out[nom] = m
	}
	return out
}

// nbSondesEnEchec : compteur du menu.
func (c *Console) nbSondesEnEchec(ctx context.Context, t time.Time) int {
	if c.lecteur == nil {
		return 0
	}
	ech, err := c.instant(ctx, `count(max by (instance, sonde) (mirador_sonde_succes) == 0) or vector(0)`, t)
	if err != nil || len(ech) == 0 || math.IsNaN(ech[0].Valeur) {
		return 0
	}
	return int(ech[0].Valeur)
}

func (c *Console) pageSondes(q *req) {
	ctx := q.r.Context()
	d := donneesSondes{Depuis: []string{essais.ServeurLocal}}
	if cs, err := flotte.Collecteurs(ctx, c.db); err == nil {
		d.Depuis = append(d.Depuis, cs...)
	}
	liste, err := flotte.LireSondes(ctx, c.db)
	if err != nil {
		c.flash(q, "", err.Error())
	}
	lues := c.sondesLues(ctx, q.t)
	courbes := map[string]alertes.Courbe{}
	if c.lecteur != nil && len(liste) > 0 {
		if cs, _, err := c.lecteur.Plage(ctx, `max by (instance, sonde) (mirador_sonde_duree_secondes)`, q.t.Add(-time.Hour), q.t, time.Minute); err == nil {
			for _, cb := range cs {
				courbes[cleSonde(cb.Etiquettes.Get("instance"), cb.Etiquettes.Get("sonde"))] = cb
			}
		}
	}
	var durees []float64
	for _, s := range liste {
		k := cleSonde(instanceDe(s), s.Nom)
		l := ligneSonde{S: s, Depuis: s.Depuis, Attendu: attenduSonde(s), Dispo: "—"}
		succes, ok := lues["succes"][k]
		switch {
		case !ok:
			l.Etat, l.SansDonnee = "Pas encore d'essai", true
		case succes.Valeur == 1:
			ms := lues["duree"][k].Valeur * 1000
			durees = append(durees, ms)
			l.Etat, l.ClasseEtat = "OK · "+strconv.FormatFloat(ms, 'f', 0, 64)+" ms", "ok"
			if lues["lente"][k].Valeur == 1 {
				l.Etat, l.ClasseEtat, l.Probleme = "Lente · "+strconv.FormatFloat(ms, 'f', 0, 64)+" ms", "attention", true
				d.Lentes++
			} else {
				d.OK++
			}
		default:
			raison := "échec"
			for kk, e := range lues["erreur"] {
				if kk == k {
					raison = e.Etiquettes.Get("erreur")
				}
			}
			l.Etat, l.ClasseEtat, l.Probleme = "Échec : "+raison, "erreur", true
			d.Echecs++
		}
		if v, ok := lues["dispo"][k]; ok && !math.IsNaN(v.Valeur) {
			l.Dispo = virgule(strconv.FormatFloat(100*v.Valeur, 'f', dispoDecimales(v.Valeur), 64)) + " %"
		}
		if cb, ok := courbes[k]; ok {
			g := ""
			if l.ClasseEtat == "erreur" {
				g = alertes.Critique
			}
			// Échelle : de 0 au seuil « lente » au moins (pointillé) ; un
			// petit écart de quelques ms ne fait pas une montagne.
			_, lente := s.Durees()
			l.Courbe = svgCourbeEchelle(cb.T, cb.V, q.t.Add(-time.Hour), q.t, lente.Seconds(), true, g, 120, 28, true)
		}
		d.Lignes = append(d.Lignes, l)
	}
	sort.SliceStable(d.Lignes, func(i, j int) bool { return d.Lignes[i].Probleme && !d.Lignes[j].Probleme })
	d.Total = len(liste)
	if len(durees) > 0 {
		sort.Float64s(durees)
		d.Mediane = strconv.FormatFloat(durees[len(durees)/2], 'f', 0, 64) + " ms"
	}
	c.rendre(q, "sondes", "Sondes", "sondes", d)
}

func dispoDecimales(v float64) int {
	if v >= 0.9995 || v == 0 {
		return 0
	}
	return 1
}

// attenduSonde : ce qui est vérifié, en clair.
func attenduSonde(s essais.Sonde) string {
	var p []string
	switch s.Type {
	case essais.HTTP:
		if s.Code != 0 {
			p = append(p, "code "+strconv.Itoa(s.Code))
		} else {
			p = append(p, "code 200 à 399")
		}
		if s.Contenu != "" {
			p = append(p, "contient « "+s.Contenu+" »")
		}
		if s.NonVerifie {
			p = append(p, "certificat non vérifié")
		}
	case essais.DNS:
		q := "demande " + s.Question
		if s.Contenu != "" {
			q += " → " + s.Contenu
		}
		p = append(p, q)
	}
	iv, lente := s.Durees()
	rythme := "chaque minute"
	if iv != time.Minute {
		rythme = "toutes les " + dureeLisible(iv)
	}
	p = append(p, rythme, "lente au-delà de "+dureeLisible(lente))
	return strings.Join(p, " · ")
}

func (c *Console) postSondes(q *req) {
	ctx := q.r.Context()
	f := champs{q}
	if err := c.verifierCode(q); err != nil {
		c.retour(q, "/sondes", err, "")
		return
	}
	liste, err := flotte.LireSondes(ctx, c.db)
	if err != nil {
		c.retour(q, "/sondes", err, "")
		return
	}
	switch f.texte("action") {
	case "ajouter":
		code := 0
		if v := f.texte("code_http"); v != "" {
			if code, err = strconv.Atoi(v); err != nil {
				c.retour(q, "/sondes", errors.New("code attendu : un nombre (ex. 200)"), "")
				return
			}
		}
		s := essais.Sonde{Nom: f.texte("nom"), Type: f.texte("type"), Cible: f.texte("cible"), Depuis: f.texte("depuis"),
			Intervalle: f.texte("intervalle"), Lente: f.texte("lente"), Code: code, Contenu: f.texte("contenu"),
			NonVerifie: f.coche("non_verifie"), Question: f.texte("question")}
		liste = append(liste, s)
		err = flotte.EnregistrerSondes(ctx, c.db, liste, q.qui(), q.t)
		c.retour(q, "/sondes", err, "sonde « "+s.Nom+" » ajoutée : premier essai dans la minute")
	case "supprimer":
		nom := f.texte("nom")
		garde := liste[:0]
		for _, s := range liste {
			if s.Nom != nom {
				garde = append(garde, s)
			}
		}
		err = flotte.EnregistrerSondes(ctx, c.db, garde, q.qui(), q.t)
		c.retour(q, "/sondes", err, "sonde « "+nom+" » retirée")
	default:
		c.retour(q, "/sondes", errors.New("action inconnue"), "")
	}
}
