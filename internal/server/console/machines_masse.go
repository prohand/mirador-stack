// SPDX-License-Identifier: GPL-3.0-or-later

package console

// Actions de masse de la page Machines (SPEC § 141, maquette validée le
// 27/11) : approuver toutes les machines en attente (site, type et groupe
// gardés), actualiser, changer site / type / groupe, assigner une version
// (code 2FA, comme sur la page des mises à jour). Rien n'est exécuté sur les
// machines : seuls la configuration et l'inventaire du serveur changent.

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"mirador-stack/internal/server/flotte"
)

// garderValeur : choix « ne pas changer » des listes de l'action de masse.
const garderValeur = "="

// maxMasse : machines traitées par une action (formulaire borné).
const maxMasse = 5000

func (c *Console) postMachinesMasse(q *req) {
	ctx := q.r.Context()
	f := func(k string) string { return strings.TrimSpace(q.r.PostFormValue(k)) }
	noms := q.r.PostForm["m"]
	if len(noms) > maxMasse {
		c.retour(q, "/machines", fmt.Errorf("au plus %d machines par action", maxMasse), "")
		return
	}
	ms, err := flotte.ListerMachines(ctx, c.db)
	if err != nil {
		c.retour(q, "/machines", err, "")
		return
	}
	parNom := map[string]flotte.InfoMachine{}
	for _, m := range ms {
		parNom[m.Nom] = m
	}
	choisies := func() ([]flotte.InfoMachine, error) {
		var out []flotte.InfoMachine
		for _, n := range noms {
			m, ok := parNom[n]
			if !ok || m.Statut != "approuvee" {
				return nil, fmt.Errorf("machine %q inconnue ou pas approuvée", n)
			}
			out = append(out, m)
		}
		if len(out) == 0 {
			return nil, errors.New("aucune machine cochée")
		}
		return out, nil
	}
	var errs []error
	n := 0
	ok := ""
	switch f("action") {
	case "approuver_tout":
		for _, m := range ms {
			if m.Statut == "en_attente" {
				if e := flotte.Approuver(ctx, c.db, m.Nom, true, q.qui(), q.t); e != nil {
					errs = append(errs, fmt.Errorf("%s : %w", m.Nom, e))
					continue
				}
				n++
			}
		}
		ok = fmt.Sprintf("%d %s : %s token à la prochaine demande", n, accorder(n, "machine(s) approuvée(s)"), map[bool]string{true: "elle reçoit son", false: "elles reçoivent leur"}[n == 1])
	case "actualiser":
		sel, e := choisies()
		if e != nil {
			c.retour(q, "/machines", e, "")
			return
		}
		for _, m := range sel {
			flotte.DemanderActualisation(m.ID, q.t)
			n++
		}
		ok = fmt.Sprintf("%d %s : configuration relue dans la seconde (agents en attente)", n, accorder(n, "machine(s) actualisée(s)"))
	case "etiquettes":
		sel, e := choisies()
		if e != nil {
			c.retour(q, "/machines", e, "")
			return
		}
		l := c.lireListes(ctx)
		for _, m := range sel {
			site, env, groupe := f("site"), f("env"), f("groupe")
			if site == garderValeur {
				site = m.Site
			}
			if env == garderValeur {
				env = m.Env
			}
			if groupe == garderValeur {
				groupe = m.Groupe
			}
			if site == m.Site && env == m.Env && groupe == m.Groupe {
				continue
			}
			if e := l.verifierChoix(site, env, groupe, m.Site, m.Env, m.Groupe); e != nil {
				errs = append(errs, e)
				break
			}
			if e := flotte.DefinirEtiquettes(ctx, c.db, m.Nom, site, env, groupe, q.qui(), q.t); e != nil {
				errs = append(errs, fmt.Errorf("%s : %w", m.Nom, e))
				continue
			}
			n++
		}
		ok = fmt.Sprintf("site, type ou groupe changé sur %d %s", n, accorder(n, "machine(s)"))
	case "version":
		sel, e := choisies()
		if e == nil {
			e = c.verifierCode(q)
		}
		v := f("version")
		if e == nil && v != "" {
			vs, _ := flotte.ListerVersions(ctx, c.db)
			if !slices.ContainsFunc(vs, func(x flotte.VersionPubliee) bool { return x.Version == v }) {
				e = fmt.Errorf("version %q inconnue", v)
			}
		}
		if e != nil {
			c.retour(q, "/machines", e, "")
			return
		}
		for _, m := range sel {
			if err := flotte.DefinirCible(ctx, c.db, m.ID, v, q.qui(), q.t); err != nil {
				errs = append(errs, fmt.Errorf("%s : %w", m.Nom, err))
				continue
			}
			n++
		}
		ok = fmt.Sprintf("version %s assignée à %d %s (mise à jour dans la fenêtre horaire)", v, n, accorder(n, "machine(s)"))
		if v == "" {
			ok = fmt.Sprintf("version propre retirée sur %d %s : elles suivent la version du parc", n, accorder(n, "machine(s)"))
		}
	default:
		errs = append(errs, errors.New("action inconnue"))
	}
	c.retour(q, "/machines", errors.Join(errs...), ok)
}
