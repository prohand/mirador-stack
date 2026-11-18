// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"mirador-stack/internal/server/flotte"
	essais "mirador-stack/internal/sondes"
)

// Vue « ruche » de la vue d'ensemble (SPEC § 51, idée reprise de New Relic
// « Navigator », choix B du 18/11 : onglet de la vue d'ensemble). Une case
// par élément, regroupées (site, machine du conteneur, origine de la
// sonde), couleur = pire état en cours. Rien de plus à lire que la vue
// d'ensemble, la page Conteneurs et la page Sondes.

// parLigneRuche : cases par rangée (rangées paires décalées : nid
// d'abeille).
const parLigneRuche = 8

type caseRuche struct {
	Nom, Sous, Classe, Lien, Titre string
	rang                           int
}

type groupeRuche struct {
	Titre, Resume string
	Lignes        [][]caseRuche
}

type vueRuche struct {
	Filtre  string // machines, conteneurs, sondes, tout
	Groupes []groupeRuche
}

// rangsRuche : ordre des cases (problèmes d'abord).
var rangsRuche = map[string]int{"cr": 0, "av": 1, "mu": 2, "ma": 3, "ok": 4}

// grouperRuche : groupes triés par titre, cases par état puis nom, en rangées.
func grouperRuche(parGroupe map[string][]caseRuche) []groupeRuche {
	titres := make([]string, 0, len(parGroupe))
	for t := range parGroupe {
		titres = append(titres, t)
	}
	sort.Strings(titres)
	var out []groupeRuche
	for _, t := range titres {
		cs := parGroupe[t]
		sort.SliceStable(cs, func(i, j int) bool {
			if cs[i].rang != cs[j].rang {
				return cs[i].rang < cs[j].rang
			}
			return cs[i].Nom < cs[j].Nom
		})
		n := map[string]int{}
		for _, c := range cs {
			n[c.Classe]++
		}
		g := groupeRuche{Titre: t, Resume: resumeRuche(len(cs), n)}
		for k := 0; k < len(cs); k += parLigneRuche {
			g.Lignes = append(g.Lignes, cs[k:min(k+parLigneRuche, len(cs))])
		}
		out = append(out, g)
	}
	return out
}

func resumeRuche(total int, n map[string]int) string {
	s := strconv.Itoa(total) + " " + accordNombre(total, "élément", "éléments")
	if n["cr"] > 0 {
		s += " · " + strconv.Itoa(n["cr"]) + " " + accordNombre(n["cr"], "critique", "critiques")
	}
	if n["av"] > 0 {
		s += " · " + strconv.Itoa(n["av"]) + " " + accordNombre(n["av"], "avertissement", "avertissements")
	}
	if n["mu"] > 0 {
		s += " · " + strconv.Itoa(n["mu"]) + " " + accordNombre(n["mu"], "muet ou inconnu", "muets ou inconnus")
	}
	if n["cr"]+n["av"]+n["mu"] == 0 {
		s += " · tout va bien"
	}
	return s
}

func accordNombre(n int, un, plusieurs string) string {
	if n > 1 {
		return plusieurs
	}
	return un
}

func nouvelleCase(nom, sous, classe, lien, titre string) caseRuche {
	return caseRuche{Nom: nom, Sous: sous, Classe: classe, Lien: lien, Titre: titre, rang: rangsRuche[classe]}
}

// machinesEnMaintenance : maintenances ponctuelles « machine:NOM » en cours.
func (c *Console) machinesEnMaintenance(ctx context.Context, t time.Time) map[string]bool {
	out := map[string]bool{}
	rows, err := c.db.QueryContext(ctx, `SELECT cible FROM maintenances WHERE recurrence = '' AND cible LIKE 'machine:%' AND debut <= ? AND fin > ?`, t.Unix(), t.Unix())
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var cible string
		if rows.Scan(&cible) == nil {
			out[cible[len("machine:"):]] = true
		}
	}
	return out
}

func (c *Console) ruche(ctx context.Context, filtre string, parc []ligneParc, t time.Time) *vueRuche {
	switch filtre {
	case "conteneurs", "sondes", "tout":
	default:
		filtre = "machines"
	}
	v := &vueRuche{Filtre: filtre}
	if filtre == "machines" || filtre == "tout" {
		maint := c.machinesEnMaintenance(ctx, t)
		g := map[string][]caseRuche{}
		for _, m := range parc {
			classe, sous := "ok", ""
			if m.CPU != "—" {
				sous = m.CPU + " CPU"
			}
			switch {
			case maint[m.Nom]:
				classe, sous = "ma", "maintenance"
			case m.Muette:
				classe, sous = "mu", "muette "+strings.TrimPrefix(depuis(m.VuLe*1000), "il y a ")
			case m.ClasseEtat == "erreur":
				classe = "cr"
			case m.ClasseEtat == "attention":
				classe = "av"
			}
			titre := m.Nom + " : " + m.Etat
			if m.Alertes > 0 {
				titre += " · " + strconv.Itoa(m.Alertes) + " alerte(s)"
			}
			site := m.Site
			if site == "" {
				site = "Sans site"
			}
			if filtre == "tout" {
				site = "Machines · " + site
			}
			g[site] = append(g[site], nouvelleCase(m.Nom, sous, classe, "/machines/"+url.PathEscape(m.Nom), titre))
		}
		v.Groupes = append(v.Groupes, grouperRuche(g)...)
	}
	if filtre == "conteneurs" || filtre == "tout" {
		g := map[string][]caseRuche{}
		for _, l := range c.conteneursDuParc(ctx, t) {
			classe := "ok"
			switch {
			case l.Probleme && l.ClasseEtat == "erreur" || l.ClasseSante == "erreur" || l.OOM:
				classe = "cr"
			case l.Etat != "En marche":
				classe = "mu"
			}
			sous := l.Etat
			if l.Sante != "" && l.Etat == "En marche" {
				sous = l.Sante
			}
			titre := "Conteneur " + l.Nom + " sur " + l.Machine + " : " + l.Etat
			g["Conteneurs · "+l.Machine] = append(g["Conteneurs · "+l.Machine],
				nouvelleCase(l.Nom, sous, classe, "/conteneurs?machine="+url.QueryEscape(l.Machine), titre))
		}
		v.Groupes = append(v.Groupes, grouperRuche(g)...)
	}
	if filtre == "sondes" || filtre == "tout" {
		g := map[string][]caseRuche{}
		if ss, err := flotte.LireSondes(ctx, c.db); err == nil && len(ss) > 0 {
			lues := c.sondesLues(ctx, t)
			for _, s := range ss {
				k := cleSonde(instanceDe(s), s.Nom)
				classe, sous := "mu", "pas d'essai"
				if e, ok := lues["succes"][k]; ok {
					ms := strconv.FormatFloat(lues["duree"][k].Valeur*1000, 'f', 0, 64) + " ms"
					switch {
					case e.Valeur == 0:
						classe, sous = "cr", "échec"
					case lues["lente"][k].Valeur == 1:
						classe, sous = "av", "lente · "+ms
					default:
						classe, sous = "ok", ms
					}
				}
				depuisSonde := "Sondes depuis le serveur"
				if s.Depuis != essais.ServeurLocal {
					depuisSonde = "Sondes depuis " + s.Depuis
				}
				g[depuisSonde] = append(g[depuisSonde], nouvelleCase(s.Nom, sous, classe, "/sondes", "Sonde "+s.Nom+" ("+s.Cible+")"))
			}
		}
		v.Groupes = append(v.Groupes, grouperRuche(g)...)
	}
	return v
}
