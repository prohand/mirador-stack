// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"mirador-stack/internal/server/flotte"
)

// Sites, types et groupes de machines (demandes du 10 et 11/11, SPEC
// § 18.15 et § 18.23) : listes définies dans Réglages › Sites, types et
// groupes, proposées en listes déroulantes à la création d'un jeton, à
// l'approbation et dans la fiche d'une machine. Le type est l'étiquette
// « env » des données ; le groupe est l'étiquette « groupe » ET la portée
// de configuration des agents (réglages communs à ses machines).

// ListesMachines : clé de réglage « machines.listes ».
type ListesMachines struct {
	Sites   []string `json:"sites"`
	Types   []string `json:"types"`
	Groupes []string `json:"groupes,omitempty"` // groupes de configuration (11/11)
}

const cleListesMachines = "machines.listes"

// typesDefaut : proposés tant que rien n'est enregistré.
var typesDefaut = []string{"conteneur", "nas", "physique", "vm", "vps"}

var reValeurListe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

// lireListes : réglage enregistré ; sinon valeurs déjà utilisées par les
// machines (+ types par défaut), pour ne rien perdre au premier passage.
func (c *Console) lireListes(ctx context.Context) ListesMachines {
	var l ListesMachines
	enregistre, err := lireJSONReglage(ctx, c.db, cleListesMachines, &l)
	if err != nil {
		enregistre = false
		l = ListesMachines{}
	}
	if !enregistre {
		l.Types = slices.Clone(typesDefaut)
	}
	// Valeurs déjà portées par des machines : toujours proposées (premier
	// passage, ou groupes créés avant que la liste existe).
	if ms, err := flotte.ListerMachines(ctx, c.db); err == nil {
		for _, m := range ms {
			if !enregistre && m.Site != "" && !slices.Contains(l.Sites, m.Site) {
				l.Sites = append(l.Sites, m.Site)
			}
			if !enregistre && m.Env != "" && !slices.Contains(l.Types, m.Env) {
				l.Types = append(l.Types, m.Env)
			}
			if m.Groupe != "" && !slices.Contains(l.Groupes, m.Groupe) {
				l.Groupes = append(l.Groupes, m.Groupe)
			}
		}
	}
	sort.Strings(l.Sites)
	sort.Strings(l.Types)
	sort.Strings(l.Groupes)
	return l
}

// ecrireListes : enregistrement + audit (lecture de l'ancienne valeur avant
// la transaction : la base n'a qu'une connexion).
func (c *Console) ecrireListes(ctx context.Context, l ListesMachines, q *req) error {
	sort.Strings(l.Sites)
	sort.Strings(l.Types)
	sort.Strings(l.Groupes)
	avant, _ := json.Marshal(c.lireListes(ctx))
	b, _ := json.Marshal(l)
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // sans effet après Commit
	if err := ecrireReglage(ctx, tx, cleListesMachines, string(b), q.qui(), q.t); err != nil {
		return err
	}
	if err := Auditer(ctx, tx, q.qui(), "sites_types", cleListesMachines, string(avant), string(b), q.ip.String(), q.t); err != nil {
		return err
	}
	return tx.Commit()
}

// verifierChoix : site, type et groupe pris dans les listes (vide permis) ;
// une valeur déjà portée par la machine reste permise (ancienne valeur).
func (l ListesMachines) verifierChoix(site, env, groupe, siteActuel, envActuel, groupeActuel string) error {
	if groupe != "" && groupe != groupeActuel && !slices.Contains(l.Groupes, groupe) {
		return fmt.Errorf("groupe %q inconnu : le créer d'abord dans Réglages › Sites, types et groupes", groupe)
	}
	if site != "" && site != siteActuel && !slices.Contains(l.Sites, site) {
		return fmt.Errorf("site %q inconnu : le créer d'abord dans Réglages › Sites, types et groupes", site)
	}
	if env != "" && env != envActuel && !slices.Contains(l.Types, env) {
		return fmt.Errorf("type %q inconnu : le créer d'abord dans Réglages › Sites, types et groupes", env)
	}
	return nil
}

type ligneListe struct {
	Valeur   string
	Machines int // machines qui l'utilisent
}

type donneesSites struct {
	Sites, Types, Groupes []ligneListe
}

func (c *Console) pageSites(q *req) {
	ctx := q.r.Context()
	l := c.lireListes(ctx)
	nbSite, nbType, nbGroupe := map[string]int{}, map[string]int{}, map[string]int{}
	if ms, err := flotte.ListerMachines(ctx, c.db); err == nil {
		for _, m := range ms {
			nbSite[m.Site]++
			nbType[m.Env]++
			nbGroupe[m.Groupe]++
		}
	}
	var d donneesSites
	for _, s := range l.Sites {
		d.Sites = append(d.Sites, ligneListe{s, nbSite[s]})
	}
	for _, s := range l.Types {
		d.Types = append(d.Types, ligneListe{s, nbType[s]})
	}
	for _, s := range l.Groupes {
		d.Groupes = append(d.Groupes, ligneListe{s, nbGroupe[s]})
	}
	c.rendre(q, "sites", "Sites, types et groupes", "reglages", d)
}

func (c *Console) postSites(q *req) {
	ctx := q.r.Context()
	f := champs{q}
	l := c.lireListes(ctx)
	quelle := f.texte("liste")
	cible := &l.Sites
	nom := "site"
	switch quelle {
	case "sites":
	case "types":
		cible, nom = &l.Types, "type"
	case "groupes":
		cible, nom = &l.Groupes, "groupe"
	default:
		c.retour(q, "/reglages/sites", errors.New("liste inconnue"), "")
		return
	}
	v := strings.TrimSpace(f.texte("valeur"))
	err := func() error {
		switch f.texte("action") {
		case "ajouter":
			if !reValeurListe.MatchString(v) {
				return fmt.Errorf("%s %q : lettres, chiffres, . _ - (63 au plus, sans espace)", nom, v)
			}
			if slices.Contains(*cible, v) {
				return fmt.Errorf("%s %q existe déjà", nom, v)
			}
			if len(*cible) >= 100 {
				return errors.New("100 valeurs au plus")
			}
			*cible = append(*cible, v)
		case "supprimer":
			i := slices.Index(*cible, v)
			if i < 0 {
				return fmt.Errorf("%s %q inconnu", nom, v)
			}
			if ms, err := flotte.ListerMachines(ctx, c.db); err == nil {
				for _, m := range ms {
					if (quelle == "sites" && m.Site == v) || (quelle == "types" && m.Env == v) || (quelle == "groupes" && m.Groupe == v) {
						return fmt.Errorf("%s %q utilisé par %s : changer d'abord ses machines", nom, v, m.Nom)
					}
				}
			}
			*cible = slices.Delete(*cible, i, i+1)
		default:
			return errors.New("action inconnue")
		}
		return c.ecrireListes(ctx, l, q)
	}()
	c.retour(q, "/reglages/sites", err, "liste enregistrée")
}
