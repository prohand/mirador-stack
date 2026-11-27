// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"time"

	"mirador-stack/internal/server/flotte"
	"mirador-stack/internal/server/modeles"
)

// La collecte suit les modèles (retour du 14/11, SPEC § 27) : retirer un
// modèle d'une machine coupe aussi ce qu'il faisait relever à l'agent
// (métriques Docker, systemd…), même si c'était déjà réglé avant les
// modèles ; cocher un modèle lève une coupure posée pour la machine. Une
// clé allumée par un autre modèle encore porté reste allumée.

// clesModele : clés de configuration d'agent qu'un modèle allume (true).
func clesModele(m *modeles.Modele) []string {
	if m == nil || len(m.Config) == 0 {
		return nil
	}
	var cfg map[string]any
	if json.Unmarshal(m.Config, &cfg) != nil {
		return nil
	}
	var out []string
	for k, v := range cfg {
		if b, ok := v.(bool); ok && b {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// effectifsDe : modèles portés par une machine (nom → modèle).
func (c *Console) effectifsDe(ctx context.Context, m flotte.InfoMachine) (map[string]*modeles.Modele, error) {
	effs, err := modeles.Effectifs(ctx, c.db, m.ID, m.Groupe)
	if err != nil {
		return nil, err
	}
	out := make(map[string]*modeles.Modele, len(effs))
	for _, e := range effs {
		out[e.Modele.Nom] = e.Modele
	}
	return out, nil
}

// suivreModeles : après un changement de modèles d'une machine (« avant » :
// modèles portés avant le changement), règle la collecte pour cette machine :
// clés des modèles retirés à false (sauf si un modèle encore porté les
// allume), coupures des clés des modèles ajoutés levées.
func (c *Console) suivreModeles(ctx context.Context, m flotte.InfoMachine, avant map[string]*modeles.Modele, qui string, t time.Time) error {
	apres, err := c.effectifsDe(ctx, m)
	if err != nil {
		return err
	}
	fournies, allumer, couper := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for nom, mod := range apres {
		for _, k := range clesModele(mod) {
			fournies[k] = true
			if avant[nom] == nil {
				allumer[k] = true
			}
		}
	}
	for nom, mod := range avant {
		if apres[nom] != nil {
			continue
		}
		for _, k := range clesModele(mod) {
			if !fournies[k] {
				couper[k] = true
			}
		}
	}
	portee := "machine:" + strconv.FormatInt(m.ID, 10)
	actuel, err := flotte.LireConfigPortee(ctx, c.db, portee)
	if err != nil {
		return err
	}
	besoin := false
	for k := range allumer {
		if v, ok := actuel[k].(bool); ok && !v {
			besoin = true
		}
	}
	for k := range couper {
		if v, ok := actuel[k].(bool); !ok || v {
			besoin = true
		}
	}
	if !besoin {
		return nil
	}
	return flotte.ModifierConfig(ctx, c.db, portee, func(d map[string]any) {
		for k := range allumer {
			if v, ok := d[k].(bool); ok && !v {
				delete(d, k)
			}
		}
		for k := range couper {
			d[k] = false
		}
	}, qui, t)
}

// photoModeles : modèles portés par chaque machine (agents approuvés), à
// prendre avant un changement qui touche plusieurs machines.
func (c *Console) photoModeles(ctx context.Context) ([]flotte.InfoMachine, map[int64]map[string]*modeles.Modele, error) {
	tous, err := c.machines(ctx)
	if err != nil {
		return nil, nil, err
	}
	var ms []flotte.InfoMachine
	avant := map[int64]map[string]*modeles.Modele{}
	for _, m := range tous {
		if m.Statut != "approuvee" || m.Type != "agent" {
			continue
		}
		e, err := c.effectifsDe(ctx, m)
		if err != nil {
			return nil, nil, err
		}
		ms = append(ms, m)
		avant[m.ID] = e
	}
	return ms, avant, nil
}

// suivreTous : suivreModeles pour chaque machine de la photo.
func (c *Console) suivreTous(ctx context.Context, ms []flotte.InfoMachine, avant map[int64]map[string]*modeles.Modele, qui string, t time.Time) error {
	var errs []error
	for _, m := range ms {
		if err := c.suivreModeles(ctx, m, avant[m.ID], qui, t); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
