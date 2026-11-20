// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"errors"
	"html/template"
	"math"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"mirador-stack/internal/server/alertes"
	"mirador-stack/internal/server/flotte"
	"mirador-stack/internal/server/servicesmetier"
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
	// Couleurs (retour du 19/11, SPEC § 70) : trait de la ligne (ok, lente,
	// echec), pastille du type, disponibilité (ok, attention, erreur).
	ClasseLigne string
	DispoClasse string
	Code        string // « Code 200 » sous l'état (HTTP)
	CodeClasse  string // ok, info, attention, erreur
	DispoPct    int
}

type donneesSondes struct {
	Lignes                    []ligneSonde
	Total, OK, Echecs, Lentes int
	Mediane                   string
	Depuis                    []string // serveur + collecteurs (formulaire)
	Toutes                    []string // machines approuvées (« Machine liée »)
	FiltreSur                 filtreSur
	Ajout                     ajoutSonde
}

// ajoutSonde : panneau « + Ajouter une sonde » en haut de la page (retour du
// 19/11, SPEC § 69) : « Depuis une machine » (même bloc que la page machine)
// ou « À la main ».
type ajoutSonde struct {
	Ouvert   bool
	Machines []string          // machines avec agent (approuvées)
	Choisie  string            // machine choisie (?ajouter=NOM)
	Bloc     *vueSondesMachine // ses adresses et ports proposés
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
		"code":   `max by (instance, sonde) (mirador_sonde_code_http)`, // HTTP(S) : code reçu (retour du 19/11)
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
	d := donneesSondes{Depuis: []string{essais.ServeurLocal}, Toutes: c.nomsMachines(ctx)}
	if cs, err := flotte.Collecteurs(ctx, c.db); err == nil {
		d.Depuis = append(d.Depuis, cs...)
	}
	liste, err := flotte.LireSondes(ctx, c.db)
	if err != nil {
		c.flash(q, "", err.Error())
	}
	// Depuis une fiche (§ 68) : une seule sonde, ou celles d'une machine.
	if nom, machine := q.r.URL.Query().Get("sonde"), q.r.URL.Query().Get("machine"); nom != "" || machine != "" {
		garde := liste[:0]
		for _, s := range liste {
			if (nom != "" && s.Nom == nom) || (machine != "" && s.Machine == machine) {
				garde = append(garde, s)
			}
		}
		liste = garde
		d.FiltreSur = filtreSur{"la sonde « " + nom + " »", "/sondes"}
		if nom == "" {
			d.FiltreSur.Texte = "la machine « " + machine + " »"
		}
	}
	// Panneau d'ajout : machine choisie → son bloc (adresses, ports proposés).
	if ms, err := flotte.ListerMachines(ctx, c.db); err == nil {
		choisie := q.r.URL.Query().Get("ajouter")
		d.Ajout.Ouvert = q.r.URL.Query().Get("ajout") != "" || choisie != "" // bouton « + Ajouter une sonde »
		for _, m := range ms {
			if m.Type != "agent" || m.Statut != "approuvee" {
				continue
			}
			d.Ajout.Machines = append(d.Ajout.Machines, m.Nom)
			if m.Nom == choisie {
				d.Ajout.Choisie, d.Ajout.Ouvert = m.Nom, true
				_, releveur, _ := flotte.CollecteurDe(ctx, c.db, m.ID)
				d.Ajout.Bloc = c.sondesMachine(ctx, m, releveur, q.t)
			}
		}
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
		var bilan int
		l.Etat, l.ClasseEtat, bilan = etatSondeLue(lues, s)
		l.SansDonnee, l.Probleme = bilan == bilanSansEssai, bilan == bilanLente || bilan == bilanEchec
		switch bilan {
		case bilanOK:
			d.OK++
		case bilanLente:
			d.Lentes++
		case bilanEchec:
			d.Echecs++
		}
		if bilan == bilanOK || bilan == bilanLente {
			durees = append(durees, lues["duree"][k].Valeur*1000)
		}
		l.ClasseLigne = map[int]string{bilanOK: "ok", bilanLente: "lente", bilanEchec: "echec"}[bilan]
		l.Code, l.CodeClasse = codeSondeLu(lues, s)
		if v, ok := lues["dispo"][k]; ok && !math.IsNaN(v.Valeur) {
			l.Dispo = virgule(strconv.FormatFloat(100*v.Valeur, 'f', dispoDecimales(v.Valeur), 64)) + " %"
			l.DispoPct = int(math.Round(100 * v.Valeur))
			switch {
			case v.Valeur >= 0.999:
				l.DispoClasse = "ok"
			case v.Valeur >= 0.99:
				l.DispoClasse = "attention"
			default:
				l.DispoClasse = "erreur"
			}
		}
		if cb, ok := courbes[k]; ok {
			g := ""
			switch l.ClasseEtat {
			case "erreur":
				g = alertes.Critique
			case "ok":
				g = "ok" // courbe verte (§ 70)
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

// sondeDuFormulaire : sonde lue dans le formulaire d'ajout ou de
// modification (vérifiée ensuite par flotte.EnregistrerSondes).
func sondeDuFormulaire(f champs) (essais.Sonde, error) {
	code := 0
	if v := f.texte("code_http"); v != "" {
		var err error
		if code, err = strconv.Atoi(v); err != nil {
			return essais.Sonde{}, errors.New("code attendu : un nombre (ex. 200)")
		}
	}
	certificat := 0
	if f.coche("certificat") {
		certificat = 14
		if v := f.texte("certificat_jours"); v != "" {
			var err error
			if certificat, err = strconv.Atoi(v); err != nil {
				return essais.Sonde{}, errors.New("alerte certificat : un nombre de jours (ex. 14)")
			}
		}
	}
	return essais.Sonde{Nom: f.texte("nom"), Type: f.texte("type"), Cible: f.texte("cible"), Depuis: f.texte("depuis"),
		Intervalle: f.texte("intervalle"), Lente: f.texte("lente"), Code: code, Contenu: f.texte("contenu"),
		NonVerifie: f.coche("non_verifie"), Question: f.texte("question"), Machine: f.texte("machine"),
		ChampJSON: f.texte("champ_json"), ValeurJSON: f.texte("valeur_json"), Certificat: certificat}, nil
}

// renommerSonde : le nouveau nom suit dans les services (§ 50) et les
// étiquettes (§ 59). Les séries déjà enregistrées gardent l'ancien nom
// (l'historique des mesures repart sous le nouveau).
func (c *Console) renommerSonde(ctx context.Context, q *req, ancien, nouveau string) {
	if svcs, err := servicesmetier.Lire(ctx, c.db); err == nil {
		change := false
		for i := range svcs {
			for j := range svcs[i].Composants {
				if x := &svcs[i].Composants[j]; x.Sorte == servicesmetier.Sonde && x.Nom == ancien {
					x.Nom, change = nouveau, true
				}
			}
		}
		if change {
			_ = servicesmetier.Enregistrer(ctx, c.db, svcs, q.qui(), q.ip.String(), q.t)
		}
	}
	if etq := c.lireEtiquettes(ctx)["s:"+ancien]; len(etq) > 0 {
		if c.enregistrerEtiquettes(ctx, q, "s:"+nouveau, etq, q.t) == nil {
			_ = c.enregistrerEtiquettes(ctx, q, "s:"+ancien, nil, q.t)
		}
	}
}

// codeSondeLu : HTTP(S), code de retour du dernier essai (« Code 200 »),
// montré sous l'état (retour du 19/11), et sa couleur : ok (2xx), info
// (3xx), attention (4xx), erreur (5xx). Vides sinon.
func codeSondeLu(lues map[string]map[string]alertes.Echantillon, s essais.Sonde) (texte, classe string) {
	if s.Type != essais.HTTP {
		return "", ""
	}
	c, ok := lues["code"][cleSonde(instanceDe(s), s.Nom)]
	if !ok || c.Valeur < 100 || c.Valeur > 599 {
		return "", ""
	}
	code := int(c.Valeur)
	switch {
	case code >= 500:
		classe = "erreur"
	case code >= 400:
		classe = "attention"
	case code >= 300:
		classe = "info"
	case code >= 200:
		classe = "ok"
	}
	return "Code " + strconv.Itoa(code), classe
}

// Bilan du dernier essai d'une sonde.
const (
	bilanSansEssai = iota
	bilanOK
	bilanLente
	bilanEchec
)

// etatSondeLue : texte et couleur du dernier essai (« OK · 12 ms »,
// « Échec : délai dépassé »…).
func etatSondeLue(lues map[string]map[string]alertes.Echantillon, s essais.Sonde) (etat, classe string, bilan int) {
	k := cleSonde(instanceDe(s), s.Nom)
	succes, ok := lues["succes"][k]
	switch {
	case !ok:
		return "Pas encore d'essai", "", bilanSansEssai
	case succes.Valeur == 1:
		ms := strconv.FormatFloat(lues["duree"][k].Valeur*1000, 'f', 0, 64)
		if lues["lente"][k].Valeur == 1 {
			return "Lente · " + ms + " ms", "attention", bilanLente
		}
		return "OK · " + ms + " ms", "ok", bilanOK
	}
	raison := "échec"
	if e, ok := lues["erreur"][k]; ok {
		raison = e.Etiquettes.Get("erreur")
	}
	return "Échec : " + raison, "erreur", bilanEchec
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
		if s.ChampJSON != "" {
			if s.ValeurJSON != "" {
				p = append(p, "JSON "+s.ChampJSON+" = « "+s.ValeurJSON+" »")
			} else {
				p = append(p, "JSON "+s.ChampJSON+" présent")
			}
		}
		if s.NonVerifie {
			p = append(p, "certificat non vérifié")
		}
		if s.Certificat > 0 {
			p = append(p, "alerte "+strconv.Itoa(s.Certificat)+" j avant l'expiration du certificat")
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
		var s essais.Sonde
		if s, err = sondeDuFormulaire(f); err != nil {
			c.retour(q, "/sondes", err, "")
			return
		}
		liste = append(liste, s)
		err = flotte.EnregistrerSondes(ctx, c.db, liste, q.qui(), q.t)
		c.retour(q, "/sondes", err, "sonde « "+s.Nom+" » ajoutée : premier essai dans la minute")
	case "modifier":
		// Modifier une sonde (retour du 19/11 : cible HTTPS à changer,
		// sonde à renommer) : mêmes vérifications qu'à l'ajout ; la machine
		// liée se change aussi (retour du 20/11).
		ancien := f.texte("ancien")
		retour := "/sondes/" + url.PathEscape(ancien)
		i := slices.IndexFunc(liste, func(x essais.Sonde) bool { return x.Nom == ancien })
		if i < 0 {
			c.retour(q, "/sondes", errors.New("sonde introuvable"), "")
			return
		}
		s, err := sondeDuFormulaire(f)
		if err != nil {
			c.retour(q, retour+"#modifier", err, "")
			return
		}
		if _, envoye := q.r.PostForm["machine"]; !envoye {
			s.Machine = liste[i].Machine // ancien formulaire : machine gardée
		}
		liste[i] = s
		if err = flotte.EnregistrerSondes(ctx, c.db, liste, q.qui(), q.t); err != nil {
			c.retour(q, retour+"#modifier", err, "")
			return
		}
		ok := "sonde « " + s.Nom + " » modifiée : prise en compte dans la minute"
		if s.Nom != ancien {
			c.renommerSonde(ctx, q, ancien, s.Nom)
			ok += " ; l'historique des mesures repart sous le nouveau nom"
		}
		c.retour(q, "/sondes/"+url.PathEscape(s.Nom), nil, ok)
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

// nomsMachines : machines approuvées, triées (liste « Machine liée »).
func (c *Console) nomsMachines(ctx context.Context) []string {
	var out []string
	if ms, err := flotte.ListerMachines(ctx, c.db); err == nil {
		for _, m := range ms {
			if m.Statut == "approuvee" {
				out = append(out, m.Nom)
			}
		}
	}
	sort.Strings(out)
	return out
}
