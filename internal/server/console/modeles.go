// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"time"

	"mirador-stack/internal/server/flotte"
	"mirador-stack/internal/server/modeles"
)

// Modèles de surveillance dans l'interface (SPEC § 24) : bloc « Modèles » de
// la page d'une machine (attribués, proposés par la détection, à ajouter),
// et page « Modèles » (catalogue, attribution aux groupes).

func (c *Console) routesModeles() {
	c.mux.HandleFunc("GET /modeles", c.lecture(c.pageModeles))
	c.mux.HandleFunc("POST /modeles", c.admin(c.postModeles))
}

// vueParam : un paramètre et sa valeur effective.
type vueParam struct {
	modeles.Parametre
	Valeur    float64
	Surcharge bool // réglé pour cette portée (sinon hérité)
}

// vueModeleAttribue : un modèle appliqué.
type vueModeleAttribue struct {
	M        *modeles.Modele
	DuGroupe bool
	Params   []vueParam
}

// vueModeles : bloc « Modèles » de la page d'une machine.
type vueModeles struct {
	Attribues   []vueModeleAttribue
	Proposes    []*modeles.Modele
	Disponibles []*modeles.Modele
	Ignores     []*modeles.Modele
	Groupe      string
}

// detectes : modèle → instances où sa détection répond (cache du lecteur).
func (c *Console) detectes(ctx context.Context, t time.Time) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	for _, m := range modeles.Fournis() {
		if m.Detection == "" {
			continue
		}
		ech, err := c.instant(ctx, m.Detection, t)
		if err != nil {
			continue
		}
		vus := map[string]bool{}
		for _, e := range ech {
			if i := e.Etiquettes.Get("instance"); i != "" {
				vus[i] = true
			}
		}
		out[m.Nom] = vus
	}
	return out
}

// modelesProposes : modèles détectés sur la machine, ni attribués ni ignorés.
func (c *Console) modelesProposes(ctx context.Context, m flotte.InfoMachine, t time.Time) []*modeles.Modele {
	effs, err := modeles.Effectifs(ctx, c.db, m.ID, m.Groupe)
	if err != nil {
		return nil
	}
	ign, err := modeles.Ignores(ctx, c.db, m.ID)
	if err != nil {
		return nil
	}
	actifs := map[string]bool{}
	for _, e := range effs {
		actifs[e.Modele.Nom] = true
	}
	det := c.detectes(ctx, t)
	var out []*modeles.Modele
	for _, mod := range modeles.Fournis() {
		if det[mod.Nom][m.Nom] && !actifs[mod.Nom] && !ign[mod.Nom] {
			out = append(out, mod)
		}
	}
	return out
}

// vueModelesMachine : le bloc « Modèles » d'une machine.
func (c *Console) vueModelesMachine(ctx context.Context, m flotte.InfoMachine, t time.Time) vueModeles {
	v := vueModeles{Groupe: m.Groupe}
	effs, err := modeles.Effectifs(ctx, c.db, m.ID, m.Groupe)
	if err != nil {
		return v
	}
	ign, _ := modeles.Ignores(ctx, c.db, m.ID)
	pris := map[string]bool{}
	for _, e := range effs {
		va := vueModeleAttribue{M: e.Modele, DuGroupe: e.DuGroupe}
		for _, p := range e.Modele.Parametres {
			va.Params = append(va.Params, vueParam{Parametre: p, Valeur: e.Parametres[p.Nom], Surcharge: e.Surcharges[p.Nom]})
		}
		v.Attribues = append(v.Attribues, va)
		pris[e.Modele.Nom] = true
	}
	v.Proposes = c.modelesProposes(ctx, m, t)
	for _, p := range v.Proposes {
		pris[p.Nom] = true
	}
	for _, mod := range modeles.Fournis() {
		switch {
		case ign[mod.Nom]:
			v.Ignores = append(v.Ignores, mod)
		case !pris[mod.Nom]:
			v.Disponibles = append(v.Disponibles, mod)
		}
	}
	return v
}

// lireParametres : valeurs saisies « p.<nom> » ; vide = hérité (pas de surcharge).
func lireParametres(mod *modeles.Modele, f func(string) string) (map[string]float64, error) {
	out := map[string]float64{}
	for _, p := range mod.Parametres {
		texte := strings.TrimSpace(f("p." + p.Nom))
		if texte == "" {
			continue
		}
		v, err := mod.LireValeur(p.Nom, texte)
		if err != nil {
			return nil, err
		}
		out[p.Nom] = v
	}
	return out, nil
}

// actionModele : actions du bloc « Modèles » de la page d'une machine. Faux
// si l'action n'est pas de ce bloc.
func (c *Console) actionModele(q *req, m flotte.InfoMachine, action string) (ok string, err error, traitee bool) {
	ctx := q.r.Context()
	f := q.r.PostFormValue
	mod := modeles.Trouver(f("modele"))
	if !strings.HasPrefix(action, "modele_") {
		return "", nil, false
	}
	if mod == nil {
		return "", errors.New("modèle inconnu"), true
	}
	portee := modeles.PorteeMachine(m.ID)
	switch action {
	case "modele_ajouter":
		err = modeles.Definir(ctx, c.db, portee, mod.Nom, modeles.EtatActif, nil, q.qui(), q.t)
		ok = "modèle « " + mod.Titre + " » attribué : configuration envoyée à l'agent, règles actives sous une minute"
	case "modele_ignorer":
		err = modeles.Definir(ctx, c.db, portee, mod.Nom, modeles.EtatIgnore, nil, q.qui(), q.t)
		ok = "modèle « " + mod.Titre + " » ignoré pour cette machine (plus proposé)"
	case "modele_reprendre":
		err = modeles.Definir(ctx, c.db, portee, mod.Nom, "", nil, q.qui(), q.t)
		ok = "modèle « " + mod.Titre + " » de nouveau proposé ou hérité du groupe"
	case "modele_retirer":
		// Modèle du groupe : ignoré pour cette machine seulement ; sinon la
		// ligne de la machine est effacée.
		etat := ""
		if g := m.Groupe; g != "" {
			if as, e := modeles.Lister(ctx, c.db, modeles.PorteeGroupe(g)); e == nil {
				for _, a := range as {
					if a.Modele == mod.Nom && a.Etat == modeles.EtatActif {
						etat = modeles.EtatIgnore
					}
				}
			}
		}
		err = modeles.Definir(ctx, c.db, portee, mod.Nom, etat, nil, q.qui(), q.t)
		ok = "modèle « " + mod.Titre + " » retiré de cette machine"
	case "modele_parametres":
		var params map[string]float64
		if params, err = lireParametres(mod, f); err == nil {
			err = modeles.Definir(ctx, c.db, portee, mod.Nom, modeles.EtatActif, params, q.qui(), q.t)
		}
		ok = "seuils de « " + mod.Titre + " » enregistrés pour cette machine (pris en compte sous une minute)"
	default:
		err = errors.New("action inconnue")
	}
	c.oublierAValider()
	return ok, err, true
}

// --- Page « Modèles » ------------------------------------------------------------

// vueCatalogue : un modèle du catalogue et les groupes qui le portent.
type vueCatalogue struct {
	M       *modeles.Modele
	Groupes []vueGroupeModele
	Libres  []string // groupes qui ne le portent pas
	NbMach  int      // machines qui le portent (directement ou par groupe)
	NbDetec int      // machines où il est détecté sans être attribué
}

type vueGroupeModele struct {
	Groupe string
	Params []vueParam
}

type donneesModeles struct {
	Catalogue []vueCatalogue
}

func (c *Console) pageModeles(q *req) {
	ctx := q.r.Context()
	l := c.lireListes(ctx)
	ms, _ := flotte.ListerMachines(ctx, c.db)
	det := c.detectes(ctx, q.t)
	var d donneesModeles
	for _, mod := range modeles.Fournis() {
		vc := vueCatalogue{M: mod}
		for _, g := range l.Groupes {
			as, err := modeles.Lister(ctx, c.db, modeles.PorteeGroupe(g))
			if err != nil {
				continue
			}
			trouve := false
			for _, a := range as {
				if a.Modele == mod.Nom && a.Etat == modeles.EtatActif {
					trouve = true
					vg := vueGroupeModele{Groupe: g}
					for _, p := range mod.Parametres {
						v, sur := p.Defaut, false
						if x, ok := a.Parametres[p.Nom]; ok {
							v, sur = x, true
						}
						vg.Params = append(vg.Params, vueParam{Parametre: p, Valeur: v, Surcharge: sur})
					}
					vc.Groupes = append(vc.Groupes, vg)
				}
			}
			if !trouve {
				vc.Libres = append(vc.Libres, g)
			}
		}
		for _, m := range ms {
			if m.Statut != "approuvee" {
				continue
			}
			effs, err := modeles.Effectifs(ctx, c.db, m.ID, m.Groupe)
			if err != nil {
				continue
			}
			porte := false
			for _, e := range effs {
				if e.Modele.Nom == mod.Nom {
					porte = true
				}
			}
			if porte {
				vc.NbMach++
			} else if det[mod.Nom][m.Nom] {
				vc.NbDetec++
			}
		}
		d.Catalogue = append(d.Catalogue, vc)
	}
	c.rendre(q, "modeles", "Modèles de surveillance", "machines", d)
}

// postModeles : attribution aux groupes (code 2FA : change la configuration
// des agents et les alertes de toutes les machines du groupe).
func (c *Console) postModeles(q *req) {
	ctx := q.r.Context()
	f := q.r.PostFormValue
	err := c.verifierCode(q)
	ok := "fait"
	mod := modeles.Trouver(f("modele"))
	groupe := strings.TrimSpace(f("groupe"))
	if err == nil && mod == nil {
		err = errors.New("modèle inconnu")
	}
	if err == nil {
		l := c.lireListes(ctx)
		found := false
		for _, g := range l.Groupes {
			found = found || g == groupe
		}
		if !found {
			err = errors.New("groupe inconnu : le créer d'abord dans Réglages › Sites, types et groupes")
		}
	}
	if err == nil {
		portee := modeles.PorteeGroupe(groupe)
		switch f("action") {
		case "ajouter":
			err = modeles.Definir(ctx, c.db, portee, mod.Nom, modeles.EtatActif, nil, q.qui(), q.t)
			ok = "« " + mod.Titre + " » attribué au groupe " + groupe
		case "retirer":
			err = modeles.Definir(ctx, c.db, portee, mod.Nom, "", nil, q.qui(), q.t)
			ok = "« " + mod.Titre + " » retiré du groupe " + groupe
		case "parametres":
			var params map[string]float64
			if params, err = lireParametres(mod, f); err == nil {
				err = modeles.Definir(ctx, c.db, portee, mod.Nom, modeles.EtatActif, params, q.qui(), q.t)
			}
			ok = "seuils de « " + mod.Titre + " » enregistrés pour le groupe " + groupe
		default:
			err = errors.New("action inconnue")
		}
	}
	c.oublierAValider()
	c.retour(q, "/modeles#"+url.PathEscape(f("modele")), err, ok)
}

// nbModelesProposes : propositions par machine (liste des machines).
func (c *Console) nbModelesProposes(ctx context.Context, ms []flotte.InfoMachine, t time.Time) map[string]int {
	out := map[string]int{}
	det := c.detectes(ctx, t)
	for _, m := range ms {
		if m.Statut != "approuvee" {
			continue
		}
		effs, err := modeles.Effectifs(ctx, c.db, m.ID, m.Groupe)
		if err != nil {
			continue
		}
		ign, err := modeles.Ignores(ctx, c.db, m.ID)
		if err != nil {
			continue
		}
		actifs := map[string]bool{}
		for _, e := range effs {
			actifs[e.Modele.Nom] = true
		}
		for nom, vus := range det {
			if vus[m.Nom] && !actifs[nom] && !ign[nom] {
				out[m.Nom]++
			}
		}
	}
	return out
}

// formatSeuil : valeur d'un paramètre pour un champ de formulaire.
func formatSeuil(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }
