// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"time"
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
	// Éléments du registre (SPEC § 55), groupés selon le filtre.
	parFiltre := map[string]map[string][]caseRuche{}
	for _, e := range c.elementsDuParc(ctx, parc, t) {
		var f, groupe string
		switch strings.SplitN(e.Sorte, " ", 2)[0] {
		case sorteMachine, sorteCollecteur, sorteEquipement:
			f, groupe = "machines", e.Groupe
			if filtre == "tout" {
				groupe = "Machines · " + groupe
			}
		case sorteConteneur:
			f, groupe = "conteneurs", "Conteneurs · "+e.Groupe
		case sorteSonde:
			f, groupe = "sondes", e.Groupe
		default:
			continue // services : page Services
		}
		if filtre != "tout" && f != filtre {
			continue
		}
		if parFiltre[f] == nil {
			parFiltre[f] = map[string][]caseRuche{}
		}
		titre := e.Sorte + " " + e.Nom + " : " + e.Etat
		parFiltre[f][groupe] = append(parFiltre[f][groupe], nouvelleCase(e.Nom, e.Sous, e.Classe, e.Lien, titre))
	}
	for _, f := range []string{"machines", "conteneurs", "sondes"} {
		if g := parFiltre[f]; g != nil {
			v.Groupes = append(v.Groupes, grouperRuche(g)...)
		}
	}
	return v
}
