// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"mirador-stack/internal/server/flotte"
)

// Réglages › Sites, types et groupes, page revue le 21/11 (maquette v2
// validée, SPEC § 107) : un onglet par liste, machines de chaque valeur,
// « Aussi utilisé par », renommer (avec ce qui change), fusionner,
// « Machines… » pour ranger plusieurs machines d'un coup.

// listeMachines : ce qui distingue les trois listes.
type listeMachines struct {
	Cle       string // sites, types, groupes (adresse et formulaires)
	Titre     string // « Sites »
	Un        string // « site » (dans les phrases)
	Etiquette string // site, env, groupe (étiquette des données)
}

var listesMachines = []listeMachines{
	{"sites", "Sites", "site", "site"},
	{"types", "Types de machine", "type", "env"},
	{"groupes", "Groupes de configuration", "groupe", "groupe"},
}

func trouverListe(cle string) (listeMachines, bool) {
	for _, l := range listesMachines {
		if l.Cle == cle {
			return l, true
		}
	}
	return listeMachines{}, false
}

// valeurs : la liste choisie dans ListesMachines.
func (l *ListesMachines) valeurs(cle string) *[]string {
	switch cle {
	case "types":
		return &l.Types
	case "groupes":
		return &l.Groupes
	}
	return &l.Sites
}

// valeurDe : la valeur de la machine pour cette liste.
func valeurDe(m flotte.InfoMachine, cle string) string {
	switch cle {
	case "types":
		return m.Env
	case "groupes":
		return m.Groupe
	}
	return m.Site
}

// machinesVisibles : nombre de noms montrés en pastilles avant « +N ».
const machinesVisibles = 4

type ligneListe struct {
	Valeur   string
	Machines int      // machines qui l'utilisent
	Noms     []string // les premières (pastilles)
	Plus     int      // les autres (lien vers la liste filtrée)
	Lien     string   // /machines?q=…
	Aussi    []aussi  // autres usages (maintenances, réglages d'agent…)
}

type aussi struct{ Texte, Lien string }

type ongletListe struct {
	listeMachines
	Nombre int
	Actif  bool
}

type choixMachine struct {
	Nom, Actuelle string
	Coche         bool
}

type donneesSites struct {
	Liste    listeMachines
	Onglets  []ongletListe
	Lignes   []ligneListe
	Renommer *panneauRenommer
	Choisir  string // valeur dont on range les machines (« Machines… »)
	Choix    []choixMachine
}

type panneauRenommer struct {
	Ancien, Nouveau string
	Fusion          bool // le nouveau nom existe déjà : confirmation
	Impact          impactRenommage
}

func (c *Console) pageSites(q *req) {
	ctx := q.r.Context()
	u := q.r.URL.Query()
	liste, ok := trouverListe(u.Get("liste"))
	if !ok {
		liste = listesMachines[0]
	}
	l := c.lireListes(ctx)
	ms, _ := flotte.ListerMachines(ctx, c.db)
	d := donneesSites{Liste: liste}
	for _, x := range listesMachines {
		d.Onglets = append(d.Onglets, ongletListe{x, len(*l.valeurs(x.Cle)), x.Cle == liste.Cle})
	}
	usages := c.usages(ctx, liste)
	for _, v := range *l.valeurs(liste.Cle) {
		lg := ligneListe{Valeur: v, Lien: "/machines?q=" + url.QueryEscape(v), Aussi: usages[v]}
		for _, m := range ms {
			if valeurDe(m, liste.Cle) == v {
				lg.Machines++
				if len(lg.Noms) < machinesVisibles {
					lg.Noms = append(lg.Noms, m.Nom)
				}
			}
		}
		lg.Plus = lg.Machines - len(lg.Noms)
		d.Lignes = append(d.Lignes, lg)
	}
	if a := u.Get("renommer"); a != "" && slices.Contains(*l.valeurs(liste.Cle), a) {
		p := &panneauRenommer{Ancien: a, Nouveau: strings.TrimSpace(u.Get("nouveau"))}
		p.Fusion = p.Nouveau != "" && p.Nouveau != a && slices.Contains(*l.valeurs(liste.Cle), p.Nouveau)
		p.Impact = c.impact(ctx, liste, a)
		d.Renommer = p
	}
	if v := u.Get("machines"); v != "" && slices.Contains(*l.valeurs(liste.Cle), v) {
		d.Choisir = v
		for _, m := range ms {
			if m.Statut == "refusee" {
				continue
			}
			act := valeurDe(m, liste.Cle)
			d.Choix = append(d.Choix, choixMachine{m.Nom, act, act == v})
		}
	}
	c.rendre(q, "sites", "Sites, types et groupes", "reglages", d)
}

// usages : « Aussi utilisé par » pour chaque valeur de la liste.
func (c *Console) usages(ctx context.Context, liste listeMachines) map[string][]aussi {
	r := map[string][]aussi{}
	prefixe := liste.Un + ":" // maintenances : site:NOM, groupe:NOM
	if liste.Cle != "types" {
		if rows, err := c.db.QueryContext(ctx, `SELECT cible, COUNT(*) FROM maintenances WHERE cible LIKE ? GROUP BY cible`, prefixe+"%"); err == nil {
			for rows.Next() {
				var cible string
				var n int
				if rows.Scan(&cible, &n) == nil {
					v := strings.TrimPrefix(cible, prefixe)
					r[v] = append(r[v], aussi{pluriel(n, "maintenance"), "/sourdine"})
				}
			}
			_ = rows.Close()
		}
	}
	switch liste.Cle {
	case "sites":
		if ms, err := flotte.ListerMachines(ctx, c.db); err == nil {
			for _, m := range ms {
				if m.Type == "collecteur" && m.Site != "" {
					r[m.Site] = append(r[m.Site], aussi{m.Nom + " (collecteur du site)", "/machines/" + url.PathEscape(m.Nom)})
				}
			}
		}
	case "groupes":
		if rows, err := c.db.QueryContext(ctx, `SELECT cle FROM reglages WHERE cle LIKE 'config_agent:groupe:%'`); err == nil {
			for rows.Next() {
				var cle string
				if rows.Scan(&cle) == nil {
					v := strings.TrimPrefix(cle, "config_agent:groupe:")
					r[v] = append(r[v], aussi{"réglages d'agent", "/reglages/agents?portee=groupe:" + url.QueryEscape(v)})
				}
			}
			_ = rows.Close()
		}
		if rows, err := c.db.QueryContext(ctx, `SELECT portee, COUNT(*) FROM modeles_attribues WHERE portee LIKE 'groupe:%' AND etat = 'actif' GROUP BY portee`); err == nil {
			for rows.Next() {
				var portee string
				var n int
				if rows.Scan(&portee, &n) == nil {
					v := strings.TrimPrefix(portee, "groupe:")
					r[v] = append(r[v], aussi{pluriel(n, "modèle"), "/reglages/agents?portee=groupe:" + url.QueryEscape(v)})
				}
			}
			_ = rows.Close()
		}
	}
	return r
}

func (c *Console) postSites(q *req) {
	ctx := q.r.Context()
	f := champs{q}
	liste, ok := trouverListe(f.texte("liste"))
	if !ok {
		c.retour(q, "/reglages/sites", errors.New("liste inconnue"), "")
		return
	}
	retour := "/reglages/sites?liste=" + liste.Cle
	l := c.lireListes(ctx)
	cible := l.valeurs(liste.Cle)
	nom := liste.Un
	v := strings.TrimSpace(f.texte("valeur"))
	message := "liste enregistrée"
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
					if valeurDe(m, liste.Cle) == v {
						return fmt.Errorf("%s %q utilisé par %s : changer d'abord ses machines", nom, v, m.Nom)
					}
				}
			}
			*cible = slices.Delete(*cible, i, i+1)
		case "renommer":
			nouveau := strings.TrimSpace(f.texte("nouveau"))
			// Nom déjà pris et fusion pas encore confirmée : on revient au
			// panneau, qui demande la confirmation.
			if nouveau != v && slices.Contains(*cible, nouveau) && !f.coche("fusionner") {
				retour += "&renommer=" + url.QueryEscape(v) + "&nouveau=" + url.QueryEscape(nouveau)
				return fmt.Errorf("« %s » existe déjà : cocher « Fusionner » pour réunir les deux", nouveau)
			}
			n, err := c.renommerValeur(ctx, liste, v, nouveau, q)
			if err != nil {
				retour += "&renommer=" + url.QueryEscape(v)
				return err
			}
			message = fmt.Sprintf("« %s » renommé en « %s » (%s)", v, nouveau, pluriel(n, "machine"))
			return nil
		case "machines":
			if !slices.Contains(*cible, v) {
				return fmt.Errorf("%s %q inconnu", nom, v)
			}
			n, err := c.rangerMachines(ctx, liste, v, q.r.PostForm["machine"], q)
			message = fmt.Sprintf("%s changée(s) pour « %s »", pluriel(n, "machine"), v)
			return err
		default:
			return errors.New("action inconnue")
		}
		return c.ecrireListes(ctx, l, q)
	}()
	c.retour(q, retour, err, message)
}

// rangerMachines : machines cochées mises sur la valeur ; celles qui
// l'avaient et ne sont plus cochées la perdent (valeur vide). Une ligne
// d'audit par machine changée (comme dans sa fiche).
func (c *Console) rangerMachines(ctx context.Context, liste listeMachines, v string, cochees []string, q *req) (int, error) {
	ms, err := flotte.ListerMachines(ctx, c.db)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, m := range ms {
		avant := valeurDe(m, liste.Cle)
		apres := avant
		if slices.Contains(cochees, m.Nom) {
			apres = v
		} else if avant == v {
			apres = ""
		}
		if apres == avant {
			continue
		}
		site, env, groupe := m.Site, m.Env, m.Groupe
		switch liste.Cle {
		case "sites":
			site = apres
		case "types":
			env = apres
		case "groupes":
			groupe = apres
		}
		if err := flotte.DefinirEtiquettes(ctx, c.db, m.Nom, site, env, groupe, q.qui(), q.t); err != nil {
			return n, err
		}
		n++
	}
	c.oublierEtiquettes()
	return n, nil
}

// oublierEtiquettes : les étiquettes des machines sont gardées en mémoire
// par l'ingestion ; effet immédiat sur les prochaines données.
func (c *Console) oublierEtiquettes() {
	if c.opt.Ingestion != nil {
		c.opt.Ingestion.Auth().Oublier()
		c.opt.Ingestion.OublierEquipements()
	}
	flotte.SignalerConfig()
}
